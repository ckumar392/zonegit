package resolve

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"
)

// Metrics is a tiny, allocation-free Prometheus-format exporter that
// avoids pulling in github.com/prometheus/client_golang.
//
// Series exposed:
//
//	zonegit_dns_queries_total{qtype="A",rcode="NOERROR"} N
//	zonegit_repo_head_branch  (info gauge, value is always 1, label "branch")
//
// Cardinality is bounded by (qtypes seen) × (rcodes seen) and stays small
// even on adversarial input because the qtype/rcode strings are drawn
// from a fixed dns.TypeToString / dns.RcodeToString set.
type Metrics struct {
	mu      sync.RWMutex
	queries map[queryKey]*atomic.Uint64

	branchLabel atomic.Value // string
}

type queryKey struct {
	qtype string
	rcode string
}

// NewMetrics returns a Metrics with empty counters.
func NewMetrics() *Metrics {
	m := &Metrics{queries: make(map[queryKey]*atomic.Uint64)}
	m.branchLabel.Store("")
	return m
}

// Observe implements MetricsHook.
func (m *Metrics) Observe(qtype string, rcode int) {
	if qtype == "" {
		qtype = "UNKNOWN"
	}
	rcodeStr, ok := dns.RcodeToString[rcode]
	if !ok {
		rcodeStr = fmt.Sprintf("RCODE%d", rcode)
	}
	k := queryKey{qtype: qtype, rcode: rcodeStr}

	// Fast path: counter already exists.
	m.mu.RLock()
	c, exists := m.queries[k]
	m.mu.RUnlock()
	if exists {
		c.Add(1)
		return
	}

	// Slow path: allocate the counter.
	m.mu.Lock()
	if c = m.queries[k]; c == nil {
		c = new(atomic.Uint64)
		m.queries[k] = c
	}
	m.mu.Unlock()
	c.Add(1)
}

// SetActiveBranch records the branch the daemon is currently serving (or
// the canary rule summary). Shown as a labeled info gauge so operators
// can confirm at a glance which branch the process is bound to.
func (m *Metrics) SetActiveBranch(label string) {
	m.branchLabel.Store(label)
}

// ServeHTTP renders metrics in Prometheus exposition format.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	m.mu.RLock()
	keys := make([]queryKey, 0, len(m.queries))
	for k := range m.queries {
		keys = append(keys, k)
	}
	m.mu.RUnlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].qtype != keys[j].qtype {
			return keys[i].qtype < keys[j].qtype
		}
		return keys[i].rcode < keys[j].rcode
	})

	var buf strings.Builder
	buf.WriteString("# HELP zonegit_dns_queries_total DNS queries answered, partitioned by query type and response code.\n")
	buf.WriteString("# TYPE zonegit_dns_queries_total counter\n")
	for _, k := range keys {
		m.mu.RLock()
		c := m.queries[k]
		m.mu.RUnlock()
		fmt.Fprintf(&buf, "zonegit_dns_queries_total{qtype=%q,rcode=%q} %d\n", k.qtype, k.rcode, c.Load())
	}

	buf.WriteString("# HELP zonegit_repo_active_branch Branch the daemon is currently serving (info gauge — value is always 1).\n")
	buf.WriteString("# TYPE zonegit_repo_active_branch gauge\n")
	if branch, _ := m.branchLabel.Load().(string); branch != "" {
		fmt.Fprintf(&buf, "zonegit_repo_active_branch{branch=%q} 1\n", branch)
	}

	_, _ = w.Write([]byte(buf.String()))
}
