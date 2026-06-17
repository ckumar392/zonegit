// Package resolve is the DNS query path. Given a query plus the active
// branch (selected by a Router), it walks the branch's tree and returns
// the matching RRset.
//
// This package owns:
//   - The Resolver type and its dns.HandlerFunc adapter.
//   - The Snapshotter contract that hides the per-process repo handle
//     lifecycle (see snapshot.go).
//   - CNAME chase, NODATA vs NXDOMAIN classification, SOA-at-apex authority
//     attachment for negative answers.
//   - AXFR (full-zone transfer) — see axfr.go.
//   - Time-travel: when PinnedAt is non-zero, the resolver serves a frozen
//     historical commit instead of following a branch.
//
// The package is pure of CLI / flag plumbing — that lives in cmd/zonegitd.
package resolve

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// Router decides which branch to serve a query from. A nil Router means
// "always serve cfg.Branch" (the v0 behaviour).
//
// Implementations live in pkg/route. The interface is here to avoid a
// circular import — pkg/route would otherwise need to depend on pkg/resolve
// for the QueryContext, which would be silly.
type Router interface {
	// Route returns the branch name to serve this query from. The default
	// branch should be returned when no rule matches.
	Route(QueryContext) string
}

// QueryContext is the per-packet input to a Router.
type QueryContext struct {
	// ClientIP is the source IP of the query (or the EDNS Client Subnet
	// when present). The bucket-hash router keys off the /24 of this.
	ClientIP string
	// QName is the lowercased FQDN.
	QName string
	// QType is the dig-style mnemonic ("A", "AAAA", ...).
	QType string
	// Now is the wall clock at packet receive time.
	Now time.Time
}

// Config configures a Resolver. Construct once at startup; the resolver
// reads it read-only thereafter.
type Config struct {
	Zone          string     // lowercase FQDN with trailing dot
	DefaultBranch string     // branch served when Router is nil or returns ""
	PinnedAt      store.Hash // non-zero => freeze to this commit (time-travel)
	Router        Router     // optional canary routing
	MetricsHook   MetricsHook
}

// MetricsHook is called once per handled DNS query, after the response is
// fully constructed but before it is written to the wire. Implementations
// must not retain pointers into the message.
type MetricsHook interface {
	Observe(qtype string, rcode int)
}

// Resolver is a stateless adapter: every call to Handle pulls a fresh
// Snapshot() and answers from it. Concurrent calls are safe.
type Resolver struct {
	cfg  Config
	snap Snapshotter
}

// New builds a Resolver. cfg.Zone must be set; cfg.DefaultBranch defaults
// to "main".
func New(snap Snapshotter, cfg Config) *Resolver {
	if cfg.DefaultBranch == "" {
		cfg.DefaultBranch = "main"
	}
	cfg.Zone = strings.ToLower(dns.Fqdn(cfg.Zone))
	return &Resolver{cfg: cfg, snap: snap}
}

// Handle implements dns.HandlerFunc. Register it with dns.HandleFunc(zone, r.Handle).
func (r *Resolver) Handle(w dns.ResponseWriter, req *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true
	resp.RecursionAvailable = false

	defer func() {
		if r.cfg.MetricsHook != nil && len(req.Question) > 0 {
			r.cfg.MetricsHook.Observe(dns.TypeToString[req.Question[0].Qtype], resp.Rcode)
		}
		_ = w.WriteMsg(resp)
	}()

	if len(req.Question) == 0 {
		resp.Rcode = dns.RcodeFormatError
		return
	}
	q := req.Question[0]
	qname := strings.ToLower(dns.Fqdn(q.Name))
	if !strings.HasSuffix(qname, r.cfg.Zone) {
		resp.Rcode = dns.RcodeRefused
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rp, err := r.snap.Snapshot()
	if err != nil {
		log.Printf("resolve: snapshot: %v", err)
		resp.Rcode = dns.RcodeServerFailure
		return
	}
	// The resolver owns its own zone identity (set at construction); the
	// Repo handle is shared across all zones served from this snapshotter.

	// Zone-transfer queries stream their response and skip the leaf-walk
	// path below.
	switch q.Qtype {
	case dns.TypeAXFR:
		r.serveAXFR(w, req, rp)
		return
	case dns.TypeIXFR:
		r.serveIXFR(w, req, rp)
		return
	}

	head, err := r.resolveHead(ctx, rp, q, qname)
	if err != nil {
		log.Printf("resolve: head: %v", err)
		resp.Rcode = dns.RcodeServerFailure
		return
	}

	rel := strings.TrimSuffix(qname, "."+r.cfg.Zone)
	if qname == r.cfg.Zone {
		rel = "@"
	}
	qtype := dns.TypeToString[q.Qtype]

	// CNAME wins over any other type (RFC 1034 §3.6.2) except when the
	// query is explicitly for CNAME.
	if q.Qtype != dns.TypeCNAME {
		if cname, err := rp.Lookup(ctx, head, rel, "CNAME"); err == nil {
			resp.Answer = append(resp.Answer, cname.RRs...)
			if c, ok := cname.RRs[0].(*dns.CNAME); ok {
				target := strings.ToLower(dns.Fqdn(c.Target))
				if strings.HasSuffix(target, r.cfg.Zone) {
					trel := strings.TrimSuffix(target, "."+r.cfg.Zone)
					if target == r.cfg.Zone {
						trel = "@"
					}
					if rs, err := rp.Lookup(ctx, head, trel, qtype); err == nil {
						resp.Answer = append(resp.Answer, rs.RRs...)
					}
				}
			}
			r.attachSOA(ctx, rp, head, resp)
			return
		}
	}

	rs, err := rp.Lookup(ctx, head, rel, qtype)
	switch {
	case err == nil:
		resp.Answer = append(resp.Answer, rs.RRs...)
		if dnssecOK(req) {
			r.attachRRSIG(ctx, rp, head, rel, q.Qtype, resp)
		}
		r.attachSOA(ctx, rp, head, resp)
	case errors.Is(err, store.ErrNotFound):
		if r.nameExists(ctx, rp, head, rel) {
			resp.Rcode = dns.RcodeSuccess // NODATA
		} else {
			resp.Rcode = dns.RcodeNameError // NXDOMAIN
		}
		r.attachSOA(ctx, rp, head, resp)
	default:
		log.Printf("resolve: lookup %s %s: %v", rel, qtype, err)
		resp.Rcode = dns.RcodeServerFailure
	}
}

// dnssecOK reports whether the requester set the DNSSEC-OK (DO) bit in
// the OPT pseudo-record. When set, RFC 4035 §3.2.1 requires the server
// to include the relevant RRSIG(s) alongside the answer.
func dnssecOK(req *dns.Msg) bool {
	opt := req.IsEdns0()
	return opt != nil && opt.Do()
}

// attachRRSIG looks up the RRSIG RRset at the answer's owner and
// appends any RRSIG whose TypeCovered matches the query type. Multiple
// RRsets at the same owner each contribute one RRSIG, all stored
// together as a single RRSIG RRset in the object model.
func (r *Resolver) attachRRSIG(ctx context.Context, rp *repo.Repo, head store.Hash, rel string, qtype uint16, resp *dns.Msg) {
	sigSet, err := rp.Lookup(ctx, head, rel, "RRSIG")
	if err != nil {
		return // no RRSIG present — zone isn't signed
	}
	for _, rr := range sigSet.RRs {
		if sig, ok := rr.(*dns.RRSIG); ok && sig.TypeCovered == qtype {
			resp.Answer = append(resp.Answer, sig)
		}
	}
}

// resolveHead returns the commit hash to answer this query against,
// honoring (in order): PinnedAt, the Router's choice, the default branch.
//
// The branch lookup is scoped to the resolver's zone (refs/heads/<zone>/<branch>).
func (r *Resolver) resolveHead(ctx context.Context, rp *repo.Repo, q dns.Question, qname string) (store.Hash, error) {
	if !r.cfg.PinnedAt.IsZero() {
		return r.cfg.PinnedAt, nil
	}
	branch := r.cfg.DefaultBranch
	if r.cfg.Router != nil {
		qctx := QueryContext{
			ClientIP: extractClientIP(qname), // fallback; the daemon overrides via remoteAddr
			QName:    qname,
			QType:    dns.TypeToString[q.Qtype],
			Now:      time.Now(),
		}
		if b := r.cfg.Router.Route(qctx); b != "" {
			branch = b
		}
	}
	h, err := rp.Refs().GetBranch(ctx, r.cfg.Zone, branch)
	if err != nil {
		return store.ZeroHash, err
	}
	return h, nil
}

// nameExists reports whether rel names an existing node in the zone — a
// name with some RRset, or an empty non-terminal with descendants. It is
// what separates NODATA from NXDOMAIN: a name that exists but lacks the
// queried type gets NODATA; a name with no node at all gets NXDOMAIN.
func (r *Resolver) nameExists(ctx context.Context, rp *repo.Repo, head store.Hash, rel string) bool {
	exists, err := rp.NameExists(ctx, head, rel)
	if err != nil {
		log.Printf("resolve: nameExists %s: %v", rel, err)
		return false
	}
	return exists
}

func (r *Resolver) attachSOA(ctx context.Context, rp *repo.Repo, head store.Hash, resp *dns.Msg) {
	if len(resp.Answer) > 0 {
		return
	}
	soa, err := rp.Lookup(ctx, head, "@", "SOA")
	if err != nil {
		return
	}
	resp.Ns = append(resp.Ns, soa.RRs...)
}

// HandleWithRemote is an adapter that captures the remote address before
// calling Handle, so the Router sees the real client IP (the bare
// dns.HandlerFunc shape does not provide it directly).
//
// Daemons register this instead of Handle when they care about routing
// by client subnet (canary, geo, etc.).
func (r *Resolver) HandleWithRemote(w dns.ResponseWriter, req *dns.Msg) {
	if r.cfg.Router != nil {
		ip := remoteIP(w, req)
		// Stash on the request via a thread-local-ish trick would be ugly;
		// instead we just inline a routing pass here and pass the IP via
		// a per-call clone of the resolver config.
		if ip != "" {
			clone := *r
			clone.cfg.Router = wrappedRouter{base: r.cfg.Router, ip: ip}
			clone.Handle(w, req)
			return
		}
	}
	r.Handle(w, req)
}

type wrappedRouter struct {
	base Router
	ip   string
}

func (wr wrappedRouter) Route(q QueryContext) string {
	q.ClientIP = wr.ip
	return wr.base.Route(q)
}

func remoteIP(w dns.ResponseWriter, req *dns.Msg) string {
	// Prefer EDNS Client Subnet if present and trusted (we accept any ECS
	// here; the docs/SELECTORS.md spec calls this out as a config knob to
	// add later).
	if opt := req.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if ecs, ok := o.(*dns.EDNS0_SUBNET); ok && ecs.Address != nil {
				return ecs.Address.String()
			}
		}
	}
	if w == nil || w.RemoteAddr() == nil {
		return ""
	}
	// RemoteAddr() returns "ip:port" for both UDP and TCP.
	addr := w.RemoteAddr().String()
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// extractClientIP is a last-resort fallback used when the daemon entered
// via Handle (not HandleWithRemote); it returns "" so a Router that needs
// a client IP simply matches nothing.
func extractClientIP(_ string) string { return "" }
