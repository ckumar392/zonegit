// Package zonegit is a CoreDNS plugin that serves authoritative DNS
// from a zonegit repository.
//
// It is a thin shim over pkg/resolve.Resolver: the resolver already
// owns the entire query-answering path (CNAME chase, NODATA/NXDOMAIN
// classification, RRSIG attachment when DO bit is set, AXFR, IXFR), so
// the plugin only needs to pick the right resolver per query based on
// the zone matched in the Corefile.
//
// Corefile syntax:
//
//	foo.com. {
//	    zonegit /path/to/repo {
//	        branch main
//	        canary canary:20
//	        canary-salt api-rollout
//	    }
//	}
//
// All Corefile directives map 1:1 to the same flags `zonegitd` accepts.
//
// Build: see ./README.md. This package is a separate Go module so it
// can carry the (large) CoreDNS dependency tree without polluting the
// main zonegit module.
package zonegit

import (
	"context"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/resolve"
)

// Zonegit is one mounted zonegit-backed zone. CoreDNS instantiates one
// per Corefile block.
type Zonegit struct {
	Next     plugin.Handler
	Zone     string             // canonical zone with trailing dot
	Resolver *resolve.Resolver
}

// Name satisfies plugin.Handler. CoreDNS uses this for routing in the
// directive chain and for metrics labels.
func (z *Zonegit) Name() string { return "zonegit" }

// ServeDNS routes the query to our Resolver if the question name falls
// under this plugin's zone. Otherwise it chains to the next plugin per
// the standard CoreDNS pattern.
//
// We return (0, nil) after the resolver writes — telling CoreDNS the
// response is already on the wire and no further processing is needed.
func (z *Zonegit) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	if !plugin.Name(z.Zone).Matches(state.Name()) {
		return plugin.NextOrFailure(z.Name(), z.Next, ctx, w, r)
	}
	z.Resolver.HandleWithRemote(w, r)
	return 0, nil
}
