// Command zonegitd is the authoritative DNS responder backed by a zonegit
// repository.
//
// Scope:
//   - UDP + TCP listener
//   - One zone per process (--zone, auto-loaded from the repo if persisted)
//   - Serves the HEAD of --branch by default
//   - --at <refish> pins serving to a historical commit (time-travel)
//   - --canary <branch>:<pct> splits traffic between --branch and a canary
//   - Responds to AXFR with the full zone
//   - Optional /metrics endpoint
//
// Hot reload: the daemon does NOT reopen Badger per query. A background
// poller (pkg/resolve.PollingSnapshotter) reopens the read-only handle
// only when a watched branch's tip hash changes. Per-query cost is one
// atomic pointer load.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/resolve"
	"github.com/ckumar392/zonegit/pkg/route"
	"github.com/ckumar392/zonegit/pkg/store"
)

// canonZone lower-cases and ensures a trailing dot.
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

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		repoPath      = flag.String("repo", envOr("ZONEGIT_REPO", "./.zonegit"), "path to zonegit repository")
		zoneFlag      = flag.String("zone", envOr("ZONEGIT_ZONE", ""), "zone name (auto-loaded from repo if persisted)")
		listen        = flag.String("listen", "127.0.0.1:5353", "DNS listen address (UDP+TCP)")
		branch        = flag.String("branch", "main", "branch to serve")
		atRefish      = flag.String("at", "", "if set, pin serving to this historical commit (refish: hash, branch, HEAD~N, tag)")
		canarySpec    = flag.String("canary", "", "canary spec, e.g. \"canary:20\" — sends 20% of traffic (by client /24) to the 'canary' branch")
		canarySalt    = flag.String("canary-salt", "zonegit", "hash salt for canary bucketing")
		metricsListen = flag.String("metrics-listen", "", "if set (e.g. \":9353\"), serve Prometheus metrics on this address")
		showVersion   = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("zonegitd %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	// Initial zone enumeration. We need at least one zone present for the
	// daemon to be useful; new zones added later are picked up by the
	// background reconciler below.
	probe, err := repo.Open(repo.Options{Path: *repoPath, ReadOnly: true})
	if err != nil {
		fatal("open repo: %v", err)
	}
	startupZones, err := probe.Zones(context.Background())
	if err != nil {
		fatal("list zones: %v", err)
	}
	_ = probe.Close()
	if len(startupZones) == 0 {
		fatal("repo has no registered zones; run `zonegit init <zone>` first")
	}

	// Build the canary router once (shared across zones — the same
	// branch:pct rule applies to every zone we serve). Zones that don't
	// have the canary branch will fall back to the default branch via
	// the resolver's lookup error path.
	var router resolve.Router
	if *canarySpec != "" {
		br, err := route.NewBucketRouter(*branch, *canarySpec, *canarySalt)
		if err != nil {
			fatal("parse canary: %v", err)
		}
		router = br
	}

	snap, err := resolve.NewPollingSnapshotter(*repoPath, nil, 200*time.Millisecond)
	if err != nil {
		fatal("snapshotter: %v", err)
	}
	defer snap.Close()

	// Time-travel pin: --at applies uniformly to all zones.
	var pinned store.Hash
	if *atRefish != "" {
		probe, err := snap.Snapshot()
		if err != nil {
			fatal("resolve --at: snapshot: %v", err)
		}
		h, err := probe.Resolve(context.Background(), *atRefish)
		if err != nil {
			fatal("resolve --at %q: %v", *atRefish, err)
		}
		pinned = h
	}

	metrics := resolve.NewMetrics()
	switch {
	case !pinned.IsZero():
		metrics.SetActiveBranch("pinned@" + pinned.Short())
	case router != nil:
		metrics.SetActiveBranch(fmt.Sprintf("%s + %s", *branch, *canarySpec))
	default:
		metrics.SetActiveBranch(*branch)
	}

	// Build the reconciler that owns the dns.HandleFunc registrations and
	// the snapshotter's watched-ref list. Called once at startup, then
	// periodically so zones added at runtime (`zonegit zone add ...`) get
	// picked up without restarting the daemon.
	zoneFilter := canonZone(*zoneFlag) // "" → all zones
	reconcileMu := &sync.Mutex{}
	registered := map[string]bool{}
	reconcile := func() {
		// Open a fresh read-only repo every tick. We cannot reuse
		// snap.Snapshot() here because the snapshotter's cached handle is
		// only refreshed when a watched ref changes — and zone-marker refs
		// are NOT watched (they don't affect query answers, only the set
		// of zones to register handlers for). Opening fresh costs one
		// Badger Open per second; acceptable for the discovery path.
		rp, err := repo.Open(repo.Options{Path: *repoPath, ReadOnly: true})
		if err != nil {
			return
		}
		defer rp.Close()
		all, err := rp.Zones(context.Background())
		if err != nil {
			return
		}
		desired := all
		if zoneFilter != "" {
			desired = nil
			for _, z := range all {
				if z == zoneFilter {
					desired = append(desired, z)
				}
			}
		}

		reconcileMu.Lock()
		defer reconcileMu.Unlock()

		desiredSet := make(map[string]bool, len(desired))
		for _, z := range desired {
			desiredSet[z] = true
		}
		for _, z := range desired {
			if registered[z] {
				continue
			}
			cfg := resolve.Config{
				Zone:          z,
				DefaultBranch: *branch,
				PinnedAt:      pinned,
				Router:        router,
				MetricsHook:   metrics,
			}
			r := resolve.New(snap, cfg)
			dns.HandleFunc(z, r.HandleWithRemote)
			registered[z] = true
			log.Printf("zonegitd: registered zone %s", z)
		}
		for z := range registered {
			if !desiredSet[z] {
				dns.HandleRemove(z)
				delete(registered, z)
				log.Printf("zonegitd: unregistered zone %s (no longer in repo)", z)
			}
		}

		refsToWatch := make([]string, 0, len(desired)*2)
		for _, z := range desired {
			refsToWatch = append(refsToWatch, refs.BranchRef(z, *branch))
			if br, ok := router.(*route.BucketRouter); ok && router != nil {
				refsToWatch = append(refsToWatch, refs.BranchRef(z, br.CanaryBranch))
			}
		}
		snap.SetWatchedRefs(refsToWatch)
	}

	reconcile() // initial registration

	stopReconciler := make(chan struct{})
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopReconciler:
				return
			case <-t.C:
				reconcile()
			}
		}
	}()
	defer close(stopReconciler)

	udp := &dns.Server{Addr: *listen, Net: "udp"}
	tcp := &dns.Server{Addr: *listen, Net: "tcp"}

	switch {
	case !pinned.IsZero():
		log.Printf("zonegitd: serving %d zone(s) %v from %s on %s (pinned at %s)", len(startupZones), startupZones, *repoPath, *listen, pinned.Short())
	case router != nil:
		log.Printf("zonegitd: serving %d zone(s) %v from %s on %s (default=%s, canary=%s)", len(startupZones), startupZones, *repoPath, *listen, *branch, *canarySpec)
	default:
		log.Printf("zonegitd: serving %d zone(s) %v from %s on %s (branch=%s)", len(startupZones), startupZones, *repoPath, *listen, *branch)
	}

	go func() {
		if err := udp.ListenAndServe(); err != nil {
			fatal("udp listener: %v", err)
		}
	}()
	go func() {
		if err := tcp.ListenAndServe(); err != nil {
			fatal("tcp listener: %v", err)
		}
	}()

	if *metricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics)
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "zonegitd %s — /metrics for stats\n", version)
		})
		go func() {
			log.Printf("zonegitd: metrics on %s/metrics", *metricsListen)
			if err := http.ListenAndServe(*metricsListen, mux); err != nil {
				log.Printf("metrics listener: %v", err)
			}
		}()
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	<-sigc
	log.Printf("zonegitd: shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = udp.ShutdownContext(ctx)
	_ = tcp.ShutdownContext(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "zonegitd: "+format+"\n", args...)
	os.Exit(1)
}
