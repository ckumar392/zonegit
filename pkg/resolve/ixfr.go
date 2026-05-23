package resolve

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/history"
	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/zone"
)

// serveIXFR responds to incremental zone transfer queries (RFC 1995).
//
// The client puts its current SOA in the request Authority section. We
// find the historical commit on the active branch whose apex SOA has
// that serial, then compute the diff between that commit's tree and
// HEAD's tree using pkg/history.Diff. The result is emitted as a single
// IXFR delta block (old SOA → removals → new SOA → additions → new SOA),
// which is RFC-compliant and what secondaries expect when only one step
// has happened (the common case for an IXFR).
//
// Falls back to a full AXFR-shape response when:
//   - the client's SOA is absent or unparseable
//   - the historical commit can't be located (e.g. after reset --hard)
//   - the client's serial equals ours (empty IXFR — just our SOA)
//
// RFC 1995 §4 says falling back to AXFR shape is correct behaviour for
// any of these.
func (r *Resolver) serveIXFR(w dns.ResponseWriter, req *dns.Msg, rp *repo.Repo) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	head, err := r.resolveHead(ctx, rp, req.Question[0], r.cfg.Zone)
	if err != nil {
		log.Printf("ixfr: head: %v", err)
		_ = sendAXFRError(w, req)
		return
	}

	latestSOAset, err := rp.Lookup(ctx, head, "@", "SOA")
	if err != nil || len(latestSOAset.RRs) != 1 {
		log.Printf("ixfr: no apex SOA at HEAD: %v", err)
		_ = sendAXFRError(w, req)
		return
	}
	latestSOA, _ := latestSOAset.RRs[0].(*dns.SOA)
	if latestSOA == nil {
		_ = sendAXFRError(w, req)
		return
	}

	clientSerial, ok := clientIXFRSerial(req)
	if !ok {
		// No client SOA in Authority section — fall back to AXFR.
		r.serveAXFR(w, req, rp)
		return
	}

	if clientSerial == latestSOA.Serial {
		// Already current. RFC 1995 §2: respond with just the current SOA.
		emitTransfer(w, req, []dns.RR{latestSOA})
		return
	}

	// Find the historical commit whose apex SOA has the client's serial.
	histCommit, histSOASet, err := findCommitBySOASerial(ctx, rp, head, clientSerial)
	if err != nil {
		log.Printf("ixfr: serial %d not found in history: %v — falling back to AXFR", clientSerial, err)
		r.serveAXFR(w, req, rp)
		return
	}
	if len(histSOASet.RRs) != 1 {
		r.serveAXFR(w, req, rp)
		return
	}
	histSOA, _ := histSOASet.RRs[0].(*dns.SOA)
	if histSOA == nil {
		r.serveAXFR(w, req, rp)
		return
	}

	// Compute the diff between the historical tree and HEAD's tree.
	histTree := treeOf(ctx, rp.Storage(), histCommit)
	headTree := treeOf(ctx, rp.Storage(), head)
	changes, err := history.Diff(ctx, rp.Storage(), histTree, headTree)
	if err != nil {
		log.Printf("ixfr: diff: %v — falling back to AXFR", err)
		r.serveAXFR(w, req, rp)
		return
	}

	var removed, added []dns.RR
	for _, ch := range changes {
		// Skip the apex SOA — its movement is implicit in the IXFR
		// framing (old SOA … new SOA), so secondaries reject duplicates.
		if len(ch.Path) == 0 && ch.RRType == "SOA" {
			continue
		}
		if !ch.OldBlob.IsZero() {
			rrs, err := loadRRs(ctx, rp.Storage(), ch.OldBlob)
			if err != nil {
				log.Printf("ixfr: load old %s: %v", ch.FQDN(), err)
				continue
			}
			removed = append(removed, rrs...)
		}
		if !ch.NewBlob.IsZero() {
			rrs, err := loadRRs(ctx, rp.Storage(), ch.NewBlob)
			if err != nil {
				log.Printf("ixfr: load new %s: %v", ch.FQDN(), err)
				continue
			}
			added = append(added, rrs...)
		}
	}

	// Build the IXFR single-delta envelope (RFC 1995 §4 form):
	//   <latest SOA> <old SOA> <removals> <latest SOA> <additions> <latest SOA>
	rrs := make([]dns.RR, 0, 3+len(removed)+len(added)+2)
	rrs = append(rrs, latestSOA)
	rrs = append(rrs, histSOA)
	rrs = append(rrs, removed...)
	rrs = append(rrs, latestSOA)
	rrs = append(rrs, added...)
	rrs = append(rrs, latestSOA)
	emitTransfer(w, req, rrs)
}

// findCommitBySOASerial searches the full commit DAG reachable from
// head for a commit whose apex SOA has the given serial. Uses BFS with
// a visited set so merge ancestors are considered, not just first
// parents (a zone where serial N landed via a merge into a side branch
// must still be IXFR-able from N).
func findCommitBySOASerial(ctx context.Context, rp *repo.Repo, head store.Hash, serial uint32) (store.Hash, zone.RRset, error) {
	if head.IsZero() {
		return store.ZeroHash, zone.RRset{}, fmt.Errorf("serial %d: head is zero", serial)
	}
	// Bound the walk at 10_000 commits to defend against pathological
	// graphs. Real zone histories don't get anywhere near this.
	const maxCommits = 10000
	visited := make(map[store.Hash]bool, 64)
	queue := []store.Hash{head}
	visited[head] = true
	for len(queue) > 0 && len(visited) < maxCommits {
		cur := queue[0]
		queue = queue[1:]
		if cur.IsZero() {
			continue
		}
		soaSet, err := rp.Lookup(ctx, cur, "@", "SOA")
		if err == nil && len(soaSet.RRs) == 1 {
			if soa, ok := soaSet.RRs[0].(*dns.SOA); ok && soa.Serial == serial {
				return cur, soaSet, nil
			}
		}
		commitObj, err := rp.Storage().GetObject(ctx, cur)
		if err != nil {
			return store.ZeroHash, zone.RRset{}, err
		}
		c, err := object.DecodeCommit(commitObj.Payload)
		if err != nil {
			return store.ZeroHash, zone.RRset{}, err
		}
		for _, p := range c.Parents {
			if !visited[p] {
				visited[p] = true
				queue = append(queue, p)
			}
		}
	}
	return store.ZeroHash, zone.RRset{}, fmt.Errorf("serial %d not in reachable history (walked %d commits)", serial, len(visited))
}

// clientIXFRSerial extracts the SOA serial from an IXFR query's
// Authority section. RFC 1995 §3 mandates one SOA RR there.
func clientIXFRSerial(req *dns.Msg) (uint32, bool) {
	for _, rr := range req.Ns {
		if soa, ok := rr.(*dns.SOA); ok {
			return soa.Serial, true
		}
	}
	return 0, false
}

// loadRRs reads an RRset blob and returns its RRs.
func loadRRs(ctx context.Context, s store.Storage, blobHash store.Hash) ([]dns.RR, error) {
	obj, err := s.GetObject(ctx, blobHash)
	if err != nil {
		return nil, err
	}
	rs, err := zone.DecodeRRset(obj.Payload)
	if err != nil {
		return nil, err
	}
	return rs.RRs, nil
}

// treeOf returns the tree hash inside the commit at h (or ZeroHash on
// error — callers should have already validated h points at a commit).
func treeOf(ctx context.Context, s store.Storage, h store.Hash) store.Hash {
	if h.IsZero() {
		return store.ZeroHash
	}
	obj, err := s.GetObject(ctx, h)
	if err != nil {
		return store.ZeroHash
	}
	c, err := object.DecodeCommit(obj.Payload)
	if err != nil {
		return store.ZeroHash
	}
	return c.Tree
}

// emitTransfer sends rrs as a single Transfer.Out envelope. Used by both
// IXFR and the "client already has current serial" short-circuit.
func emitTransfer(w dns.ResponseWriter, req *dns.Msg, rrs []dns.RR) {
	tr := new(dns.Transfer)
	ch := make(chan *dns.Envelope, 1)
	go func() {
		ch <- &dns.Envelope{RR: rrs}
		close(ch)
	}()
	if err := tr.Out(w, req, ch); err != nil {
		log.Printf("ixfr/axfr out: %v", err)
	}
}
