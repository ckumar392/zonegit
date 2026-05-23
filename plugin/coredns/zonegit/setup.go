package zonegit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"

	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/resolve"
	"github.com/ckumar392/zonegit/pkg/route"
	"github.com/ckumar392/zonegit/pkg/store"
)

func init() {
	plugin.Register("zonegit", setup)
}

// setup is called once per Corefile `zonegit` block. It parses the
// block, opens a snapshotter against the named repo, builds a
// Resolver, and inserts it into the CoreDNS plugin chain.
func setup(c *caddy.Controller) error {
	repoPath, branch, canary, salt, atRefish, err := parseCorefile(c)
	if err != nil {
		return plugin.Error("zonegit", err)
	}

	cfg := dnsserver.GetConfig(c)
	if cfg.Zone == "" {
		return plugin.Error("zonegit", fmt.Errorf("must be mounted under exactly one zone in the Corefile"))
	}
	zoneName := canonZone(cfg.Zone)

	watched := []string{refs.BranchRef(zoneName, branch)}
	var router resolve.Router
	if canary != "" {
		br, err := route.NewBucketRouter(branch, canary, salt)
		if err != nil {
			return plugin.Error("zonegit", fmt.Errorf("parse canary: %w", err))
		}
		router = br
		watched = append(watched, refs.BranchRef(zoneName, br.CanaryBranch))
	}

	snap, err := resolve.NewPollingSnapshotter(repoPath, watched, 200*time.Millisecond)
	if err != nil {
		return plugin.Error("zonegit", fmt.Errorf("snapshotter: %w", err))
	}

	var pinned store.Hash
	if atRefish != "" {
		probe, err := snap.Snapshot()
		if err != nil {
			return plugin.Error("zonegit", fmt.Errorf("snapshot for at: %w", err))
		}
		h, err := probe.Resolve(context.Background(), atRefish)
		if err != nil {
			return plugin.Error("zonegit", fmt.Errorf("resolve at %q: %w", atRefish, err))
		}
		pinned = h
	}

	resolver := resolve.New(snap, resolve.Config{
		Zone:          zoneName,
		DefaultBranch: branch,
		PinnedAt:      pinned,
		Router:        router,
	})

	c.OnShutdown(func() error { return snap.Close() })

	cfg.AddPlugin(func(next plugin.Handler) plugin.Handler {
		return &Zonegit{Next: next, Zone: zoneName, Resolver: resolver}
	})
	return nil
}

// parseCorefile reads the directive block syntax described in the
// package doc.
func parseCorefile(c *caddy.Controller) (repoPath, branch, canary, salt, atRefish string, err error) {
	branch = "main"
	salt = "zonegit"
	for c.Next() {
		args := c.RemainingArgs()
		if len(args) != 1 {
			return "", "", "", "", "", c.ArgErr()
		}
		repoPath = args[0]
		for c.NextBlock() {
			switch c.Val() {
			case "branch":
				v := c.RemainingArgs()
				if len(v) != 1 {
					return "", "", "", "", "", c.ArgErr()
				}
				branch = v[0]
			case "canary":
				v := c.RemainingArgs()
				if len(v) != 1 {
					return "", "", "", "", "", c.ArgErr()
				}
				canary = v[0]
			case "canary-salt":
				v := c.RemainingArgs()
				if len(v) != 1 {
					return "", "", "", "", "", c.ArgErr()
				}
				salt = v[0]
			case "at":
				v := c.RemainingArgs()
				if len(v) != 1 {
					return "", "", "", "", "", c.ArgErr()
				}
				atRefish = v[0]
			default:
				return "", "", "", "", "", c.Errf("unknown directive %q", c.Val())
			}
		}
	}
	if repoPath == "" {
		return "", "", "", "", "", c.Errf("zonegit requires a repo path argument")
	}
	return repoPath, branch, canary, salt, atRefish, nil
}

func canonZone(z string) string {
	z = strings.ToLower(z)
	if z == "" {
		return ""
	}
	if !strings.HasSuffix(z, ".") {
		z += "."
	}
	return z
}
