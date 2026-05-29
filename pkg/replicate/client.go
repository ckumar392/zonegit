package replicate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// Client is a secondary's pull-replication client. Construct one per
// (primary, local-repo) pair and call Run to start the polling loop.
//
// One Pull() invocation fetches every diverged branch in one pass:
//
//  1. GET /v0/refs to learn the primary's current branch tips.
//  2. For each (zone, branch) where the local hash differs, request a
//     Merkle walk to determine what objects are missing.
//  3. Fetch each missing object and store it locally.
//  4. Move local refs to match the primary atomically.
//
// The local repo must be writable (not opened ReadOnly). The polling
// loop is single-threaded — one Pull at a time — which avoids ref
// race-conditions without needing a multi-writer protocol.
type Client struct {
	PrimaryURL string
	Local      *repo.Repo
	Interval   time.Duration
	HTTP       *http.Client

	mu   sync.Mutex
	stop chan struct{}
	wg   sync.WaitGroup
}

// NewClient builds a Client with sensible defaults: 5s poll interval,
// 30s per-request HTTP timeout.
func NewClient(primaryURL string, local *repo.Repo) *Client {
	return &Client{
		PrimaryURL: primaryURL,
		Local:      local,
		Interval:   5 * time.Second,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
}

// Run starts the background polling loop. Returns when Stop is called.
// Errors per cycle are logged but don't terminate the loop — transient
// network failures shouldn't take down a secondary.
func (c *Client) Run(ctx context.Context) {
	c.stop = make(chan struct{})
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		t := time.NewTicker(c.Interval)
		defer t.Stop()
		// Immediate first pull so the secondary catches up at startup
		// instead of after one Interval.
		if err := c.Pull(ctx); err != nil {
			log.Printf("replicate: initial pull: %v", err)
		}
		for {
			select {
			case <-c.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.Pull(ctx); err != nil {
					log.Printf("replicate: pull: %v", err)
				}
			}
		}
	}()
}

// Stop terminates the polling loop and waits for the goroutine to exit.
func (c *Client) Stop() {
	if c.stop != nil {
		close(c.stop)
	}
	c.wg.Wait()
}

// Pull performs one full sync cycle: refs → missing-walk → fetch → ref
// advance. Safe to call directly (without Run) for tests or one-shot
// secondary catchups.
func (c *Client) Pull(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	remote, err := c.getRefs(ctx)
	if err != nil {
		return fmt.Errorf("get refs: %w", err)
	}

	// Make sure every zone the primary knows about is registered locally.
	for _, z := range remote.Zones {
		if err := c.Local.AddZone(ctx, z); err != nil {
			return fmt.Errorf("register zone %s: %w", z, err)
		}
	}

	for _, b := range remote.Branches {
		primaryHash, err := store.ParseHash(b.Hash)
		if err != nil {
			log.Printf("replicate: bad hash %q from primary: %v", b.Hash, err)
			continue
		}
		localHash, _ := c.Local.Refs().GetBranch(ctx, b.Zone, b.Name)
		if localHash == primaryHash {
			continue // already in sync
		}
		if err := c.pullBranch(ctx, b, primaryHash, localHash); err != nil {
			log.Printf("replicate: pull %s/%s: %v", b.Zone, b.Name, err)
			continue
		}
	}
	return nil
}

func (c *Client) pullBranch(ctx context.Context, b Branch, primaryHash, localHash store.Hash) error {
	known := []string{}
	if !localHash.IsZero() {
		known = []string{hashesToHex([]store.Hash{localHash})[0]}
	}
	walk, err := c.walkMissing(ctx, []string{b.Hash}, known)
	if err != nil {
		return fmt.Errorf("walk: %w", err)
	}
	// Fetch in any order — content addressing means the local store
	// validates each object's hash on insert.
	for _, hexHash := range walk.Missing {
		h, err := store.ParseHash(hexHash)
		if err != nil {
			return fmt.Errorf("bad hash from primary: %w", err)
		}
		// Skip if we already have it (race between walk and fetch).
		if ok, _ := c.Local.Storage().HasObject(ctx, h); ok {
			continue
		}
		obj, err := c.getObject(ctx, hexHash)
		if err != nil {
			return fmt.Errorf("get object %s: %w", h.Short(), err)
		}
		if err := c.Local.Storage().PutObject(ctx, h, obj); err != nil {
			return fmt.Errorf("put object %s: %w", h.Short(), err)
		}
	}

	if localHash.IsZero() {
		if err := c.Local.Refs().CreateBranch(ctx, b.Zone, b.Name, primaryHash); err != nil {
			return fmt.Errorf("create branch: %w", err)
		}
	} else {
		if err := c.Local.Refs().UpdateBranch(ctx, b.Zone, b.Name, localHash, primaryHash); err != nil {
			return fmt.Errorf("update branch: %w", err)
		}
	}
	log.Printf("replicate: %s/%s → %s (was %s)", b.Zone, b.Name, primaryHash.Short(), localHash.Short())
	return nil
}

// --- HTTP helpers ---

func (c *Client) getRefs(ctx context.Context) (*RefsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.PrimaryURL+BasePath+"/refs", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out RefsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) walkMissing(ctx context.Context, roots, known []string) (*WalkResponse, error) {
	body, _ := json.Marshal(WalkRequest{Roots: roots, Known: known})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.PrimaryURL+BasePath+"/objects/walk", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(b))
	}
	var out WalkResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getObject(ctx context.Context, hexHash string) (store.Object, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.PrimaryURL+BasePath+"/objects/"+hexHash, nil)
	if err != nil {
		return store.Object{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return store.Object{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return store.Object{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	kind := resp.Header.Get("X-Object-Kind")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return store.Object{}, err
	}
	return store.Object{Kind: kind, Payload: body}, nil
}
