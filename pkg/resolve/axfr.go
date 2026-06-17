package resolve

import (
	"context"
	"log"
	"time"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/zone"
)

// serveAXFR streams a full zone transfer to the requester. The transfer
// always starts and ends with the apex SOA (RFC 5936 §2.2). RRsets between
// the two SOAs are emitted in canonical tree-walk order.
//
// AXFR makes this daemon a credible drop-in for BIND/Knot/PowerDNS:
// secondaries pull the zone over standard TCP and stay in sync via their
// own NOTIFY/refresh loop. Incremental transfer (IXFR) is handled
// separately in serveIXFR.
//
// It returns the response code recorded for metrics: NOERROR once the
// transfer is streamed, SERVFAIL if the zone could not be assembled.
func (r *Resolver) serveAXFR(w dns.ResponseWriter, req *dns.Msg, rp *repo.Repo) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	head, err := r.resolveHead(ctx, rp, req.Question[0], r.cfg.Zone)
	if err != nil {
		log.Printf("axfr: head: %v", err)
		_ = sendAXFRError(w, req)
		return dns.RcodeServerFailure
	}

	soa, err := rp.Lookup(ctx, head, "@", "SOA")
	if err != nil {
		log.Printf("axfr: no apex SOA: %v", err)
		_ = sendAXFRError(w, req)
		return dns.RcodeServerFailure
	}

	// Collect all RRsets into a single envelope. The miekg/dns Transfer.Out
	// will chunk as needed.
	commitObj, err := rp.Storage().GetObject(ctx, head)
	if err != nil {
		log.Printf("axfr: load commit: %v", err)
		_ = sendAXFRError(w, req)
		return dns.RcodeServerFailure
	}
	commit, err := object.DecodeCommit(commitObj.Payload)
	if err != nil {
		log.Printf("axfr: decode commit: %v", err)
		_ = sendAXFRError(w, req)
		return dns.RcodeServerFailure
	}

	var all []dns.RR
	all = append(all, soa.RRs...) // leading SOA
	err = object.WalkAllLeaves(ctx, rp.Storage(), commit.Tree, func(path []string, rrtype string, blobHash store.Hash) error {
		// Skip the apex SOA — we already emitted it as the lead record.
		if len(path) == 0 && rrtype == "SOA" {
			return nil
		}
		bobj, err := rp.Storage().GetObject(ctx, blobHash)
		if err != nil {
			return err
		}
		rs, err := zone.DecodeRRset(bobj.Payload)
		if err != nil {
			return err
		}
		all = append(all, rs.RRs...)
		return nil
	})
	if err != nil {
		log.Printf("axfr: walk: %v", err)
		_ = sendAXFRError(w, req)
		return dns.RcodeServerFailure
	}
	all = append(all, soa.RRs...) // trailing SOA closes the transfer

	tr := new(dns.Transfer)
	ch := make(chan *dns.Envelope, 1)
	go func() {
		ch <- &dns.Envelope{RR: all}
		close(ch)
	}()
	if err := tr.Out(w, req, ch); err != nil {
		log.Printf("axfr: out: %v", err)
	}
	return dns.RcodeSuccess
}

func sendAXFRError(w dns.ResponseWriter, req *dns.Msg) error {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Rcode = dns.RcodeServerFailure
	return w.WriteMsg(resp)
}
