package resolve_test

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/resolve"
)

const testZone = "foo.com."

func bg() context.Context { return context.Background() }

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	if err != nil {
		t.Fatalf("NewRR(%q): %v", s, err)
	}
	return rr
}

// seedRepo returns an in-memory repo with a small committed zone (serial 1).
func seedRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Open(repo.Options{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Init(bg(), testZone); err != nil {
		t.Fatal(err)
	}
	// Import loads heterogeneous RRsets in one pass (Set takes a single
	// RRset). Explicit SOA serial 1, so the IXFR test has a known baseline.
	zonefile := `$ORIGIN foo.com.
$TTL 300
@   IN SOA ns1.foo.com. admin.foo.com. 1 7200 3600 1209600 300
@   IN NS  ns1.foo.com.
ns1 IN A   10.0.0.1
api IN A   1.2.3.4
api IN AAAA 2001:db8::1
www IN CNAME api.foo.com.
`
	if _, err := r.Import(bg(), strings.NewReader(zonefile)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(bg(), object.Identity{Name: "test", Email: "t@t"}, "seed"); err != nil {
		t.Fatal(err)
	}
	return r
}

func newResolver(t *testing.T, r *repo.Repo, cfg resolve.Config) *resolve.Resolver {
	t.Helper()
	if cfg.Zone == "" {
		cfg.Zone = testZone
	}
	return resolve.New(&resolve.StaticSnapshotter{R: r}, cfg)
}

// fakeRW is a dns.ResponseWriter that captures every message written to it,
// including each envelope of an AXFR/IXFR (dns.Transfer.Out writes via WriteMsg).
type fakeRW struct {
	mu     sync.Mutex
	msgs   []*dns.Msg
	remote net.Addr
}

func (f *fakeRW) WriteMsg(m *dns.Msg) error {
	f.mu.Lock()
	f.msgs = append(f.msgs, m)
	f.mu.Unlock()
	return nil
}
func (f *fakeRW) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeRW) Close() error                { return nil }
func (f *fakeRW) TsigStatus() error           { return nil }
func (f *fakeRW) TsigTimersOnly(bool)         {}
func (f *fakeRW) Hijack()                     {}
func (f *fakeRW) LocalAddr() net.Addr         { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }
func (f *fakeRW) RemoteAddr() net.Addr {
	if f.remote != nil {
		return f.remote
	}
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 5555}
}

func (f *fakeRW) first(t *testing.T) *dns.Msg {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.msgs) == 0 {
		t.Fatal("no response written")
	}
	return f.msgs[0]
}

// xfrRRs concatenates the Answer sections of every written message. For
// AXFR/IXFR this yields the full transfer (the daemon's deferred empty reply
// contributes no answers).
func (f *fakeRW) xfrRRs() []dns.RR {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dns.RR
	for _, m := range f.msgs {
		out = append(out, m.Answer...)
	}
	return out
}

func ask(t *testing.T, res *resolve.Resolver, qname string, qtype uint16, opts ...func(*dns.Msg)) *dns.Msg {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(qname), qtype)
	for _, o := range opts {
		o(req)
	}
	w := &fakeRW{}
	res.Handle(w, req)
	return w.first(t)
}

func TestResolverQueries(t *testing.T) {
	res := newResolver(t, seedRepo(t), resolve.Config{})

	tests := []struct {
		name      string
		qname     string
		qtype     uint16
		wantRcode int
		check     func(t *testing.T, m *dns.Msg)
	}{
		{"A", "api.foo.com.", dns.TypeA, dns.RcodeSuccess, func(t *testing.T, m *dns.Msg) {
			if len(m.Answer) != 1 {
				t.Fatalf("answers = %d, want 1", len(m.Answer))
			}
			a, ok := m.Answer[0].(*dns.A)
			if !ok || a.A.String() != "1.2.3.4" {
				t.Errorf("answer = %v", m.Answer[0])
			}
			if !m.Authoritative {
				t.Error("AA bit not set")
			}
			if m.RecursionAvailable {
				t.Error("RA bit should be false")
			}
			if len(m.Ns) != 0 {
				t.Errorf("authority section should be empty on a positive answer, got %v", m.Ns)
			}
		}},
		{"AAAA", "api.foo.com.", dns.TypeAAAA, dns.RcodeSuccess, func(t *testing.T, m *dns.Msg) {
			if len(m.Answer) != 1 {
				t.Fatalf("answers = %d", len(m.Answer))
			}
			if _, ok := m.Answer[0].(*dns.AAAA); !ok {
				t.Errorf("answer = %v", m.Answer[0])
			}
		}},
		{"CNAME chase", "www.foo.com.", dns.TypeA, dns.RcodeSuccess, func(t *testing.T, m *dns.Msg) {
			var sawCNAME, sawA bool
			for _, rr := range m.Answer {
				switch v := rr.(type) {
				case *dns.CNAME:
					sawCNAME = true
				case *dns.A:
					if v.A.String() == "1.2.3.4" {
						sawA = true
					}
				}
			}
			if !sawCNAME || !sawA {
				t.Errorf("want CNAME + target A, got %v", m.Answer)
			}
		}},
		{"direct CNAME query", "www.foo.com.", dns.TypeCNAME, dns.RcodeSuccess, func(t *testing.T, m *dns.Msg) {
			if len(m.Answer) != 1 {
				t.Fatalf("answers = %d", len(m.Answer))
			}
			if _, ok := m.Answer[0].(*dns.CNAME); !ok {
				t.Errorf("answer = %v", m.Answer[0])
			}
		}},
		{"NODATA", "api.foo.com.", dns.TypeTXT, dns.RcodeSuccess, func(t *testing.T, m *dns.Msg) {
			if len(m.Answer) != 0 {
				t.Errorf("NODATA should have no answers, got %v", m.Answer)
			}
			if len(m.Ns) == 0 {
				t.Error("NODATA should carry SOA in authority")
			}
		}},
		{"NXDOMAIN", "nope.foo.com.", dns.TypeA, dns.RcodeNameError, func(t *testing.T, m *dns.Msg) {
			if len(m.Ns) == 0 {
				t.Error("NXDOMAIN should carry SOA in authority")
			}
		}},
		{"REFUSED out of zone", "bar.com.", dns.TypeA, dns.RcodeRefused, nil},
		{"SOA apex", "foo.com.", dns.TypeSOA, dns.RcodeSuccess, func(t *testing.T, m *dns.Msg) {
			if len(m.Answer) != 1 {
				t.Fatalf("answers = %d", len(m.Answer))
			}
			if _, ok := m.Answer[0].(*dns.SOA); !ok {
				t.Errorf("answer = %v", m.Answer[0])
			}
		}},
		{"NS apex", "foo.com.", dns.TypeNS, dns.RcodeSuccess, func(t *testing.T, m *dns.Msg) {
			if len(m.Answer) != 1 {
				t.Fatalf("answers = %d", len(m.Answer))
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ask(t, res, tc.qname, tc.qtype)
			if m.Rcode != tc.wantRcode {
				t.Fatalf("rcode = %s, want %s", dns.RcodeToString[m.Rcode], dns.RcodeToString[tc.wantRcode])
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

func TestResolverEmptyQuestion(t *testing.T) {
	res := newResolver(t, seedRepo(t), resolve.Config{})
	w := &fakeRW{}
	res.Handle(w, new(dns.Msg)) // no question
	if got := w.first(t).Rcode; got != dns.RcodeFormatError {
		t.Errorf("rcode = %s, want FORMERR", dns.RcodeToString[got])
	}
}

// TestResolverNODATAForUnprobedType pins NODATA-vs-NXDOMAIN classification:
// a name that exists with *any* record type — even one outside the small
// set the resolver used to probe — must return NOERROR/NODATA, not NXDOMAIN.
func TestResolverNODATAForUnprobedType(t *testing.T) {
	r := seedRepo(t)
	// HINFO is a real record type that was absent from the old hardcoded
	// probe list, so before the fix this name looked nonexistent.
	if err := r.Set(bg(), []dns.RR{mustRR(t, `weird.foo.com. 300 IN HINFO "cpu" "os"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(bg(), object.Identity{Name: "t", Email: "t@t"}, "add hinfo"); err != nil {
		t.Fatal(err)
	}
	res := newResolver(t, r, resolve.Config{})

	m := ask(t, res, "weird.foo.com.", dns.TypeA)
	if m.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR (NODATA): the name exists with a HINFO record", dns.RcodeToString[m.Rcode])
	}
	if len(m.Answer) != 0 {
		t.Errorf("NODATA must have no answers, got %v", m.Answer)
	}
	if len(m.Ns) == 0 {
		t.Error("NODATA should carry the apex SOA in authority")
	}
}

func TestResolverDNSSEC(t *testing.T) {
	r := seedRepo(t)
	// Stage an RRSIG covering api A. The resolver only looks it up by
	// TypeCovered; it does not validate the (placeholder) signature.
	sig := &dns.RRSIG{
		Hdr:         dns.RR_Header{Name: "api.foo.com.", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 300},
		TypeCovered: dns.TypeA,
		Algorithm:   15, // Ed25519
		Labels:      3,
		OrigTtl:     300,
		Expiration:  2000000000,
		Inception:   1000000000,
		KeyTag:      12345,
		SignerName:  "foo.com.",
		Signature:   "ZHVtbXlzaWc=", // base64("dummysig")
	}
	if err := r.Set(bg(), []dns.RR{sig}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(bg(), object.Identity{Name: "t", Email: "t@t"}, "sign api A"); err != nil {
		t.Fatal(err)
	}
	res := newResolver(t, r, resolve.Config{})

	// With the DO bit, the RRSIG is appended alongside the answer.
	withDO := ask(t, res, "api.foo.com.", dns.TypeA, func(req *dns.Msg) { req.SetEdns0(4096, true) })
	var sawSig bool
	for _, rr := range withDO.Answer {
		if _, ok := rr.(*dns.RRSIG); ok {
			sawSig = true
		}
	}
	if !sawSig {
		t.Errorf("DO=1 query should include RRSIG, got %v", withDO.Answer)
	}

	// Without the DO bit, no RRSIG.
	noDO := ask(t, res, "api.foo.com.", dns.TypeA)
	for _, rr := range noDO.Answer {
		if _, ok := rr.(*dns.RRSIG); ok {
			t.Errorf("DO=0 query should not include RRSIG, got %v", noDO.Answer)
		}
	}
}

func TestResolverTimeTravel(t *testing.T) {
	r := seedRepo(t)
	_, _, c1, err := r.Head(bg())
	if err != nil {
		t.Fatal(err)
	}
	// Change api A and commit a second version.
	if err := r.Set(bg(), []dns.RR{mustRR(t, "api.foo.com. 300 IN A 9.9.9.9")}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(bg(), object.Identity{Name: "t", Email: "t@t"}, "failover api"); err != nil {
		t.Fatal(err)
	}

	// Pinned at the first commit → old value.
	pinned := newResolver(t, r, resolve.Config{PinnedAt: c1})
	if a := ask(t, pinned, "api.foo.com.", dns.TypeA); a.Answer[0].(*dns.A).A.String() != "1.2.3.4" {
		t.Errorf("pinned answer = %v, want 1.2.3.4", a.Answer[0])
	}
	// Following HEAD → new value.
	head := newResolver(t, r, resolve.Config{})
	if a := ask(t, head, "api.foo.com.", dns.TypeA); a.Answer[0].(*dns.A).A.String() != "9.9.9.9" {
		t.Errorf("head answer = %v, want 9.9.9.9", a.Answer[0])
	}
}

type fixedRouter struct{ branch string }

func (f fixedRouter) Route(resolve.QueryContext) string { return f.branch }

func TestResolverRouting(t *testing.T) {
	r := seedRepo(t)

	t.Run("router selects existing branch", func(t *testing.T) {
		res := newResolver(t, r, resolve.Config{Router: fixedRouter{branch: "main"}})
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetQuestion("api.foo.com.", dns.TypeA)
		res.HandleWithRemote(w, req)
		if w.first(t).Rcode != dns.RcodeSuccess {
			t.Errorf("rcode = %s", dns.RcodeToString[w.first(t).Rcode])
		}
	})

	t.Run("empty route falls back to default branch", func(t *testing.T) {
		res := newResolver(t, r, resolve.Config{Router: fixedRouter{branch: ""}})
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetQuestion("api.foo.com.", dns.TypeA)
		res.HandleWithRemote(w, req)
		if w.first(t).Rcode != dns.RcodeSuccess {
			t.Errorf("rcode = %s", dns.RcodeToString[w.first(t).Rcode])
		}
	})

	t.Run("nonexistent branch → SERVFAIL", func(t *testing.T) {
		res := newResolver(t, r, resolve.Config{Router: fixedRouter{branch: "ghost"}})
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetQuestion("api.foo.com.", dns.TypeA)
		res.HandleWithRemote(w, req)
		if got := w.first(t).Rcode; got != dns.RcodeServerFailure {
			t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[got])
		}
	})

	t.Run("EDNS client subnet drives the router", func(t *testing.T) {
		res := newResolver(t, r, resolve.Config{Router: fixedRouter{branch: "main"}})
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetQuestion("api.foo.com.", dns.TypeA)
		opt := &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}}
		ecs := &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: net.ParseIP("203.0.113.5")}
		opt.Option = append(opt.Option, ecs)
		req.Extra = append(req.Extra, opt)
		res.HandleWithRemote(w, req)
		if w.first(t).Rcode != dns.RcodeSuccess {
			t.Errorf("rcode = %s", dns.RcodeToString[w.first(t).Rcode])
		}
	})
}

func TestResolverAXFR(t *testing.T) {
	res := newResolver(t, seedRepo(t), resolve.Config{})
	w := &fakeRW{}
	req := new(dns.Msg)
	req.SetAxfr(testZone)
	res.Handle(w, req)

	rrs := w.xfrRRs()
	if len(rrs) < 3 {
		t.Fatalf("AXFR returned %d RRs, want the zone bracketed by SOAs", len(rrs))
	}
	if _, ok := rrs[0].(*dns.SOA); !ok {
		t.Errorf("first RR = %v, want leading SOA", rrs[0])
	}
	if _, ok := rrs[len(rrs)-1].(*dns.SOA); !ok {
		t.Errorf("last RR = %v, want trailing SOA", rrs[len(rrs)-1])
	}
	var sawAPI bool
	for _, rr := range rrs {
		if a, ok := rr.(*dns.A); ok && a.Header().Name == "api.foo.com." && a.A.String() == "1.2.3.4" {
			sawAPI = true
		}
	}
	if !sawAPI {
		t.Errorf("AXFR missing api A record; got %v", rrs)
	}
}

func TestResolverAXFRError(t *testing.T) {
	// A router pointing at a branch that does not exist makes resolveHead
	// fail, so the transfer is answered with SERVFAIL rather than a partial zone.
	res := newResolver(t, seedRepo(t), resolve.Config{Router: fixedRouter{branch: "ghost"}})
	w := &fakeRW{}
	req := new(dns.Msg)
	req.SetAxfr(testZone)
	res.Handle(w, req)
	if got := w.first(t).Rcode; got != dns.RcodeServerFailure {
		t.Errorf("rcode = %s, want SERVFAIL", dns.RcodeToString[got])
	}
}

func TestResolverIXFR(t *testing.T) {
	r := seedRepo(t) // serial 1
	if err := r.Set(bg(), []dns.RR{mustRR(t, "api.foo.com. 300 IN A 9.9.9.9")}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(bg(), object.Identity{Name: "t", Email: "t@t"}, "change api"); err != nil {
		t.Fatal(err) // auto-bumps SOA to serial 2
	}
	res := newResolver(t, r, resolve.Config{})

	soaSerials := func(rrs []dns.RR) []uint32 {
		var out []uint32
		for _, rr := range rrs {
			if s, ok := rr.(*dns.SOA); ok {
				out = append(out, s.Serial)
			}
		}
		return out
	}

	t.Run("delta from older serial", func(t *testing.T) {
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetIxfr(testZone, 1, "ns1.foo.com.", "admin.foo.com.")
		res.Handle(w, req)
		rrs := w.xfrRRs()
		var sawOld, sawNew bool
		for _, rr := range rrs {
			if a, ok := rr.(*dns.A); ok && a.Header().Name == "api.foo.com." {
				if a.A.String() == "1.2.3.4" {
					sawOld = true
				}
				if a.A.String() == "9.9.9.9" {
					sawNew = true
				}
			}
		}
		if !sawOld || !sawNew {
			t.Errorf("IXFR delta should contain removed 1.2.3.4 and added 9.9.9.9; got %v", rrs)
		}
		if len(soaSerials(rrs)) < 2 {
			t.Errorf("IXFR delta should be SOA-framed; serials = %v", soaSerials(rrs))
		}
	})

	t.Run("current serial → single SOA", func(t *testing.T) {
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetIxfr(testZone, 2, "ns1.foo.com.", "admin.foo.com.")
		res.Handle(w, req)
		serials := soaSerials(w.xfrRRs())
		if len(serials) != 1 || serials[0] != 2 {
			t.Errorf("up-to-date IXFR should be a single current SOA; serials = %v", serials)
		}
	})

	t.Run("no client SOA falls back to AXFR", func(t *testing.T) {
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetQuestion(testZone, dns.TypeIXFR) // no SOA in authority
		res.Handle(w, req)
		rrs := w.xfrRRs()
		if len(rrs) < 3 {
			t.Fatalf("fallback AXFR returned %d RRs", len(rrs))
		}
		if _, ok := rrs[0].(*dns.SOA); !ok {
			t.Error("AXFR fallback should lead with SOA")
		}
	})

	t.Run("unknown serial falls back to AXFR", func(t *testing.T) {
		w := &fakeRW{}
		req := new(dns.Msg)
		req.SetIxfr(testZone, 99999, "ns1.foo.com.", "admin.foo.com.")
		res.Handle(w, req)
		if len(w.xfrRRs()) < 3 {
			t.Errorf("unknown-serial IXFR should fall back to a full AXFR")
		}
	})
}

func TestResolverMetricsHook(t *testing.T) {
	m := resolve.NewMetrics()
	res := newResolver(t, seedRepo(t), resolve.Config{MetricsHook: m})
	ask(t, res, "api.foo.com.", dns.TypeA)

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `qtype="A"`) {
		t.Errorf("metrics did not record the A query:\n%s", rec.Body.String())
	}
}

func TestMetrics(t *testing.T) {
	m := resolve.NewMetrics()
	m.Observe("A", dns.RcodeSuccess)
	m.Observe("A", dns.RcodeSuccess)
	m.Observe("AAAA", dns.RcodeNameError)
	m.Observe("", 9999) // empty qtype → UNKNOWN; unmapped rcode → RCODE9999
	m.SetActiveBranch("main")

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`zonegit_dns_queries_total{qtype="A",rcode="NOERROR"} 2`,
		`zonegit_dns_queries_total{qtype="AAAA",rcode="NXDOMAIN"} 1`,
		`qtype="UNKNOWN"`,
		`rcode="RCODE9999"`,
		`zonegit_repo_active_branch{branch="main"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
}

func TestStaticSnapshotter(t *testing.T) {
	r := seedRepo(t)
	s := &resolve.StaticSnapshotter{R: r}
	got, err := s.Snapshot()
	if err != nil || got != r {
		t.Fatalf("Snapshot() = %v, %v; want the wrapped repo", got, err)
	}

	var nilSnap resolve.StaticSnapshotter
	if _, err := nilSnap.Snapshot(); err == nil {
		t.Error("Snapshot() on nil repo should error")
	}
	if err := nilSnap.Close(); err != nil {
		t.Errorf("Close() on nil repo = %v, want nil", err)
	}
}

func TestPollingSnapshotter(t *testing.T) {
	dir := t.TempDir() + "/repo"
	w, err := repo.Open(repo.Options{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Init(bg(), testZone); err != nil {
		t.Fatal(err)
	}
	if err := w.Set(bg(), []dns.RR{mustRR(t, "api.foo.com. 300 IN A 1.2.3.4")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(bg(), object.Identity{Name: "t", Email: "t@t"}, "seed"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close() // release the writer so the read-only snapshotter can open

	// A 10s interval ensures the background watcher never ticks during this
	// sub-second test, so there is no concurrent Badger reopen to race on.
	ps, err := resolve.NewPollingSnapshotter(dir, []string{"refs/heads/foo.com./main"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()

	snap, err := ps.Snapshot()
	if err != nil || snap == nil {
		t.Fatalf("Snapshot() = %v, %v", snap, err)
	}

	res := resolve.New(ps, resolve.Config{Zone: testZone})
	if a := ask(t, res, "api.foo.com.", dns.TypeA); a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
		t.Fatalf("query through polling snapshotter: rcode=%s answers=%d", dns.RcodeToString[a.Rcode], len(a.Answer))
	}

	ps.SetWatchedRefs([]string{"refs/heads/foo.com./main", "refs/heads/foo.com./canary"})
}
