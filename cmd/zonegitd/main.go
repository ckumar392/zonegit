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
		configPath    = flag.String("config", "", "path to per-zone YAML config (overrides --branch/--canary/--at on a per-zone basis)")
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

	// Load per-zone config (if --config given). Fall back to flag values
	// as the daemon-level defaults so the simple "one zone, one branch"
	// invocation still works without a config file.
	cfgFile, err := loadDaemonConfig(*configPath)
	if err != nil {
		fatal("config: %v", err)
	}
	if cfgFile.DefaultBranch == "" {
		cfgFile.DefaultBranch = *branch
	}
	if cfgFile.DefaultAt == "" {
		cfgFile.DefaultAt = *atRefish
	}
	if cfgFile.DefaultCanary == "" {
		cfgFile.DefaultCanary = *canarySpec
	}
	if cfgFile.DefaultSalt == "" {
		cfgFile.DefaultSalt = *canarySalt
	}

	snap, err := resolve.NewPollingSnapshotter(*repoPath, nil, 200*time.Millisecond)
	if err != nil {
		fatal("snapshotter: %v", err)
	}
	defer snap.Close()

	metrics := resolve.NewMetrics()
	// active-branch info gauge is multi-zone-aware now: show the file path
	// if a config is loaded, otherwise the default branch / canary string.
	switch {
	case *configPath != "":
		metrics.SetActiveBranch("config=" + *configPath)
	case cfgFile.DefaultCanary != "":
		metrics.SetActiveBranch(fmt.Sprintf("%s + %s", cfgFile.DefaultBranch, cfgFile.DefaultCanary))
	default:
		metrics.SetActiveBranch(cfgFile.DefaultBranch)
	}

	// resolvedPin caches the result of resolving a refish to a commit
	// hash. Per-zone --at refishes are resolved once on first encounter.
	pinCache := map[string]store.Hash{}
	resolvePin := func(refish string) store.Hash {
		if refish == "" {
			return store.ZeroHash
		}
		if h, ok := pinCache[refish]; ok {
			return h
		}
		probe, err := snap.Snapshot()
		if err != nil {
			log.Printf("resolve --at %q: snapshot: %v", refish, err)
			return store.ZeroHash
		}
		h, err := probe.Resolve(context.Background(), refish)
		if err != nil {
			log.Printf("resolve --at %q: %v", refish, err)
			return store.ZeroHash
		}
		pinCache[refish] = h
		return h
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
		refsToWatch := make([]string, 0, len(desired)*2)
		for _, z := range desired {
			rule := cfgFile.ruleFor(z)
			refsToWatch = append(refsToWatch, refs.BranchRef(z, rule.Branch))

			var router resolve.Router
			if rule.Canary != "" {
				br, err := route.NewBucketRouter(rule.Branch, rule.Canary, rule.Salt)
				if err != nil {
					log.Printf("zonegitd: zone %s: bad canary %q: %v — skipping canary for this zone", z, rule.Canary, err)
				} else {
					router = br
					refsToWatch = append(refsToWatch, refs.BranchRef(z, br.CanaryBranch))
				}
			}

			if registered[z] {
				continue
			}
			cfg := resolve.Config{
				Zone:          z,
				DefaultBranch: rule.Branch,
				PinnedAt:      resolvePin(rule.At),
				Router:        router,
				MetricsHook:   metrics,
			}
			r := resolve.New(snap, cfg)
			dns.HandleFunc(z, r.HandleWithRemote)
			registered[z] = true
			log.Printf("zonegitd: registered zone %s (%s)", z, rule)
		}
		for z := range registered {
			if !desiredSet[z] {
				dns.HandleRemove(z)
				delete(registered, z)
				log.Printf("zonegitd: unregistered zone %s (no longer in repo)", z)
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

	if *configPath != "" {
		log.Printf("zonegitd: serving %d zone(s) %v from %s on %s (config=%s)", len(startupZones), startupZones, *repoPath, *listen, *configPath)
	} else {
		log.Printf("zonegitd: serving %d zone(s) %v from %s on %s (default branch=%s)", len(startupZones), startupZones, *repoPath, *listen, cfgFile.DefaultBranch)
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
