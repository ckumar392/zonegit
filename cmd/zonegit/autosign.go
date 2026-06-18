package main

import (
	"context"

	"github.com/miekg/dns"

	"github.com/ckumar392/zonegit/pkg/dnssec"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/zonesign"
)

// autoSignTouched re-signs the RRsets just staged via r.Set (the `set
// --auto-sign` path) so resolvers keep validating after the commit lands.
//
// It loads the zone's keys from <repo>/keys/ and delegates the actual
// signing to pkg/zonesign. If the zone has no DNSSEC keys this is a silent
// no-op: the caller asked to auto-sign but nothing's configured, so we
// don't fail.
func autoSignTouched(ctx context.Context, r *repo.Repo, touched []dns.RR) error {
	zoneName := r.ActiveZone()
	if zoneName == "" || !dnssec.HasKeys(keysDir(), zoneName) {
		return nil
	}
	keys, err := dnssec.LoadFromDir(keysDir(), zoneName)
	if err != nil {
		return err
	}
	return zonesign.AutoSign(ctx, r, keys, touched)
}
