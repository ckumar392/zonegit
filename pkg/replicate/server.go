package replicate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// Server exposes a zonegit repo to secondaries over HTTP. It is
// stateless in the sense that every request opens against the current
// snapshot of the underlying repo; correctness comes from
// content-addressed object bodies, not from session state.
//
// The repo handle must be opened read-only — Server never mutates.
type Server struct {
	// SnapshotFn returns a fresh read-only Repo for the current state.
	// Used per request so secondaries see the latest commits the
	// writer has produced. Implementations typically wire this to
	// resolve.PollingSnapshotter or open a fresh handle on every call.
	SnapshotFn func() (*repo.Repo, error)
}

// RegisterHandlers wires the protocol endpoints onto mux under BasePath.
// Callers can mount the same server alongside their existing /metrics
// endpoint by passing http.DefaultServeMux or any sub-mux.
func (s *Server) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc(BasePath+"/refs", s.handleRefs)
	mux.HandleFunc(BasePath+"/objects/walk", s.handleWalk)
	mux.HandleFunc(BasePath+"/objects/", s.handleObject)
}

func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	rp, err := s.SnapshotFn()
	if err != nil {
		http.Error(w, fmt.Sprintf("snapshot: %v", err), http.StatusInternalServerError)
		return
	}
	zones, err := rp.Zones(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := RefsResponse{Zones: zones}
	for _, z := range zones {
		names, err := rp.Refs().ListBranches(ctx, z)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, n := range names {
			h, err := rp.Refs().GetBranch(ctx, z, n)
			if err != nil {
				continue
			}
			resp.Branches = append(resp.Branches, Branch{
				Zone: z, Name: n, Hash: hashesToHex([]store.Hash{h})[0],
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleWalk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req WalkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	roots, err := hexesToHashes(req.Roots)
	if err != nil {
		http.Error(w, fmt.Sprintf("roots: %v", err), http.StatusBadRequest)
		return
	}
	known, err := hexesToHashes(req.Known)
	if err != nil {
		http.Error(w, fmt.Sprintf("known: %v", err), http.StatusBadRequest)
		return
	}

	rp, err := s.SnapshotFn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Expand `known` into the full set of object hashes the secondary
	// already has — anything reachable from a known root is implicitly
	// present locally.
	knownSet := make(map[store.Hash]bool, 64)
	for _, k := range known {
		if err := walkReachable(r.Context(), rp.Storage(), k, knownSet); err != nil {
			// Missing-known is fine — secondary may have lost an object;
			// we just don't get to skip its descendants.
			if !errors.Is(err, store.ErrNotFound) {
				http.Error(w, fmt.Sprintf("walk known: %v", err), http.StatusInternalServerError)
				return
			}
		}
	}

	// Walk roots, collecting hashes not in knownSet.
	missing := make(map[store.Hash]bool, 64)
	for _, root := range roots {
		if err := walkMissing(r.Context(), rp.Storage(), root, knownSet, missing); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, fmt.Sprintf("root %s not in primary", root.Short()), http.StatusNotFound)
				return
			}
			http.Error(w, fmt.Sprintf("walk: %v", err), http.StatusInternalServerError)
			return
		}
	}

	out := make([]string, 0, len(missing))
	for h := range missing {
		out = append(out, hashesToHex([]store.Hash{h})[0])
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(WalkResponse{Missing: out})
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	hexHash := strings.TrimPrefix(r.URL.Path, BasePath+"/objects/")
	h, err := store.ParseHash(hexHash)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad hash: %v", err), http.StatusBadRequest)
		return
	}
	rp, err := s.SnapshotFn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	obj, err := rp.Storage().GetObject(r.Context(), h)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Object-Kind", obj.Kind)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(obj.Payload)
}

// walkReachable adds h and every object reachable from it to visited.
// Idempotent — re-visiting a hash is cheap.
//
// Walks: commits → tree + parents; trees → child trees + leaf blobs;
// blobs and symrefs are leaves. The same shape is used for both
// expanding `known` and walking `roots`.
func walkReachable(ctx context.Context, s store.Storage, h store.Hash, visited map[store.Hash]bool) error {
	if h.IsZero() || visited[h] {
		return nil
	}
	visited[h] = true
	obj, err := s.GetObject(ctx, h)
	if err != nil {
		return err
	}
	return walkChildren(ctx, s, obj, func(child store.Hash) error {
		return walkReachable(ctx, s, child, visited)
	})
}

// walkMissing walks from root, skipping any hash in known. Collects
// every object the secondary needs to fetch.
func walkMissing(ctx context.Context, s store.Storage, h store.Hash, known, missing map[store.Hash]bool) error {
	if h.IsZero() || known[h] || missing[h] {
		return nil
	}
	missing[h] = true
	obj, err := s.GetObject(ctx, h)
	if err != nil {
		return err
	}
	return walkChildren(ctx, s, obj, func(child store.Hash) error {
		return walkMissing(ctx, s, child, known, missing)
	})
}

// walkChildren invokes fn for every direct child object of obj.
//
// This is the one place that has to know about every object Kind:
// commits reference a tree + parents; trees reference their entry
// hashes; tags reference a target. Symrefs hold a ref path, not a
// hash, so they have no children.
func walkChildren(ctx context.Context, s store.Storage, obj store.Object, fn func(store.Hash) error) error {
	switch obj.Kind {
	case "commit":
		c, err := object.DecodeCommit(obj.Payload)
		if err != nil {
			return err
		}
		if err := fn(c.Tree); err != nil {
			return err
		}
		for _, p := range c.Parents {
			if err := fn(p); err != nil {
				return err
			}
		}
	case "tree":
		t, err := object.DecodeTree(obj.Payload)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			if err := fn(e.Hash); err != nil {
				return err
			}
		}
	case "blob", "symref", "tag":
		// Leaves — no children.
	}
	return nil
}

// Suppress unused-import linting when refs is only referenced by the
// handleRefs ListBranches call above on some platforms.
var _ = refs.BranchPrefix
