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
	"syscall"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/resolve"
	"github.com/ckumar392/zonegit/pkg/route"
	"github.com/ckumar392/zonegit/pkg/store"
)

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

	// Resolve the zone: explicit flag wins; otherwise read from the repo.
	zoneName := *zoneFlag
	if zoneName == "" {
		r, err := repo.Open(repo.Options{Path: *repoPath, ReadOnly: true})
		if err != nil {
			fatal("open repo: %v", err)
		}
		zoneName = r.Zone()
		_ = r.Close()
		if zoneName == "" {
			fatal("--zone is required (and no zone is persisted in the repo)")
		}
	}
	if !strings.HasSuffix(zoneName, ".") {
		zoneName += "."
	}
	zoneName = strings.ToLower(zoneName)

	// Determine which branches the snapshotter needs to watch.
	watched := []string{*branch}
	var router resolve.Router
	if *canarySpec != "" {
		br, err := route.NewBucketRouter(*branch, *canarySpec, *canarySalt)
		if err != nil {
			fatal("parse canary: %v", err)
		}
		router = br
		watched = append(watched, br.CanaryBranch)
	}

	snap, err := resolve.NewPollingSnapshotter(*repoPath, watched, 200*time.Millisecond)
	if err != nil {
		fatal("snapshotter: %v", err)
	}
	defer snap.Close()

	// Time-travel: resolve --at once at startup and pin the resolver to it.
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
	if pinned.IsZero() {
		if *canarySpec != "" {
			metrics.SetActiveBranch(fmt.Sprintf("%s + %s", *branch, *canarySpec))
		} else {
			metrics.SetActiveBranch(*branch)
		}
	} else {
		metrics.SetActiveBranch("pinned@" + pinned.Short())
	}

	cfg := resolve.Config{
		Zone:          zoneName,
		DefaultBranch: *branch,
		PinnedAt:      pinned,
		Router:        router,
		MetricsHook:   metrics,
	}
	r := resolve.New(snap, cfg)

	dns.HandleFunc(zoneName, r.HandleWithRemote)

	udp := &dns.Server{Addr: *listen, Net: "udp"}
	tcp := &dns.Server{Addr: *listen, Net: "tcp"}

	switch {
	case !pinned.IsZero():
		log.Printf("zonegitd: serving zone %s from %s on %s (pinned at %s)", zoneName, *repoPath, *listen, pinned.Short())
	case router != nil:
		log.Printf("zonegitd: serving zone %s from %s on %s (default=%s, canary=%s)", zoneName, *repoPath, *listen, *branch, *canarySpec)
	default:
		log.Printf("zonegitd: serving zone %s from %s on %s (branch=%s)", zoneName, *repoPath, *listen, *branch)
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
