// Command dnsdbd is a minimal authoritative DNS responder backed by a dnsdb
// repository. It always serves the current HEAD of the configured branch.
//
// Scope (v0):
//   - UDP + TCP listener
//   - One zone per process (--zone)
//   - Answers from HEAD; SOA + NS at apex
//   - NODATA / NXDOMAIN distinction is best-effort (NODATA when name has any
//     RRtype, otherwise NXDOMAIN). DNSSEC, AXFR, NOTIFY are out of scope.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ckumar392/dnsdb/pkg/repo"
	"github.com/ckumar392/dnsdb/pkg/store"
	"github.com/miekg/dns"
)

func main() {
	var (
		repoPath = flag.String("repo", envOr("DNSDB_REPO", "./.dnsdb"), "path to dnsdb repository")
		zone     = flag.String("zone", envOr("DNSDB_ZONE", ""), "zone name (e.g. foo.com.)")
		listen   = flag.String("listen", "127.0.0.1:5353", "address to listen on (UDP+TCP)")
		branch   = flag.String("branch", "main", "branch to serve")
	)
	flag.Parse()

	if *zone == "" {
		fatal("--zone is required")
	}
	if !strings.HasSuffix(*zone, ".") {
		*zone += "."
	}

	r, err := repo.Open(repo.Options{Path: *repoPath, ReadOnly: true})
	if err != nil {
		fatal("open repo: %v", err)
	}
	defer r.Close()
	r.SetZone(*zone)

	srv := &server{
		repoPath: *repoPath,
		zone:     strings.ToLower(*zone),
		branch:   *branch,
		repo:     r,
	}

	dns.HandleFunc(*zone, srv.handle)

	udp := &dns.Server{Addr: *listen, Net: "udp"}
	tcp := &dns.Server{Addr: *listen, Net: "tcp"}

	go func() {
		log.Printf("dnsdbd: serving zone %s from %s on %s/udp", *zone, *repoPath, *listen)
		if err := udp.ListenAndServe(); err != nil {
			fatal("udp listener: %v", err)
		}
	}()
	go func() {
		if err := tcp.ListenAndServe(); err != nil {
			fatal("tcp listener: %v", err)
		}
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	<-sigc
	log.Printf("dnsdbd: shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = udp.ShutdownContext(ctx)
	_ = tcp.ShutdownContext(ctx)
}

type server struct {
	repoPath string
	zone     string // lowercase, trailing dot
	branch   string

	mu   sync.Mutex
	repo *repo.Repo // current read-only handle, swapped on demand
}

// snapshot returns a Repo whose Badger handle was opened after lastHead was
// observed; it closes the previous handle. This is how the daemon picks up
// writes without restart. Cost: a Badger Open (~10–50 ms on a small repo).
func (s *server) snapshot() (*repo.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := repo.Open(repo.Options{Path: s.repoPath, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	r.SetZone(s.zone)
	old := s.repo
	s.repo = r
	if old != nil {
		_ = old.Close()
	}
	return r, nil
}

func (s *server) handle(w dns.ResponseWriter, req *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	resp.RecursionAvailable = false

	if len(req.Question) == 0 {
		resp.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(resp)
		return
	}
	q := req.Question[0]
	qname := strings.ToLower(dns.Fqdn(q.Name))

	// Reject queries outside our zone.
	if !strings.HasSuffix(qname, s.zone) {
		resp.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(resp)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Re-open the read-only Badger handle for each query so we pick up any
	// commits made by the writer process. Cheap enough for v0; a real
	// deployment would use SIGHUP-triggered reload or Badger Subscribe.
	r, err := s.snapshot()
	if err != nil {
		log.Printf("dnsdbd: snapshot: %v", err)
		resp.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(resp)
		return
	}

	// Resolve HEAD of branch.
	head, err := r.Resolve(ctx, "refs/heads/"+s.branch)
	if err != nil {
		log.Printf("dnsdbd: resolve %s: %v", s.branch, err)
		resp.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(resp)
		return
	}

	rel := strings.TrimSuffix(qname, "."+s.zone)
	if qname == s.zone {
		rel = "@"
	}
	qtype := dns.TypeToString[q.Qtype]

	// CNAME first (RFC 1034 §3.6.2): a CNAME wins over any other type for
	// non-CNAME queries, except when qtype == CNAME itself.
	if q.Qtype != dns.TypeCNAME {
		if cname, err := r.Lookup(ctx, head, rel, "CNAME"); err == nil {
			resp.Answer = append(resp.Answer, cname.RRs...)
			// In-zone CNAME chase (best-effort, single hop).
			if len(cname.RRs) > 0 {
				if c, ok := cname.RRs[0].(*dns.CNAME); ok {
					target := strings.ToLower(dns.Fqdn(c.Target))
					if strings.HasSuffix(target, s.zone) {
						trel := strings.TrimSuffix(target, "."+s.zone)
						if target == s.zone {
							trel = "@"
						}
						if rs, err := r.Lookup(ctx, head, trel, qtype); err == nil {
							resp.Answer = append(resp.Answer, rs.RRs...)
						}
					}
				}
			}
			s.attachSOA(ctx, r, head, resp)
			_ = w.WriteMsg(resp)
			return
		}
	}

	rs, err := r.Lookup(ctx, head, rel, qtype)
	switch {
	case err == nil:
		resp.Answer = append(resp.Answer, rs.RRs...)
		s.attachSOA(ctx, r, head, resp)
	case errors.Is(err, store.ErrNotFound):
		// Distinguish NODATA from NXDOMAIN: probe a few common types.
		if s.nameExists(ctx, r, head, rel) {
			// NODATA: name exists but no RRset of this type.
			resp.Rcode = dns.RcodeSuccess
		} else {
			resp.Rcode = dns.RcodeNameError
		}
		s.attachSOA(ctx, r, head, resp)
	default:
		log.Printf("dnsdbd: lookup %s %s: %v", rel, qtype, err)
		resp.Rcode = dns.RcodeServerFailure
	}

	_ = w.WriteMsg(resp)
}

// nameExists probes a small set of common types to detect NODATA vs NXDOMAIN.
// A more thorough check would walk the tree at this name; this is a v0
// approximation sufficient for the demo.
func (s *server) nameExists(ctx context.Context, r *repo.Repo, head store.Hash, rel string) bool {
	for _, t := range []string{"A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV", "PTR", "SOA"} {
		if _, err := r.Lookup(ctx, head, rel, t); err == nil {
			return true
		}
	}
	return false
}

func (s *server) attachSOA(ctx context.Context, r *repo.Repo, head store.Hash, resp *dns.Msg) {
	if len(resp.Answer) > 0 {
		return // not needed in positive answer
	}
	soa, err := r.Lookup(ctx, head, "@", "SOA")
	if err != nil {
		return
	}
	resp.Ns = append(resp.Ns, soa.RRs...)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dnsdbd: "+format+"\n", args...)
	os.Exit(1)
}
