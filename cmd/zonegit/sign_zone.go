package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ckumar392/zonegit/pkg/dnssec"
	"github.com/ckumar392/zonegit/pkg/zonesign"
)

// keysDir returns the directory where DNSSEC keys for this repo live.
// Default: <repo>/keys/. The directory is created on first use.
func keysDir() string { return filepath.Join(flagRepoPath, "keys") }

// newZoneKeygenCmd implements `zonegit zone-keygen [zone]`.
//
// Generates a fresh Ed25519 KSK + ZSK and writes them under
// <repo>/keys/<zone>.{ksk,zsk}.{key,pub}. Without an argument, it uses
// the active zone.
func newZoneKeygenCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "zone-keygen [zone]",
		Short:   "Generate a DNSSEC keypair (KSK + ZSK, Ed25519) for a zone",
		Args:    cobra.MaximumNArgs(1),
		Example: "  zonegit zone-keygen foo.com.",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			z := r.ActiveZone()
			if len(args) == 1 {
				z = args[0]
			}
			if z == "" {
				return fmt.Errorf("zone-keygen: no zone (active zone is empty; pass one explicitly)")
			}
			if dnssec.HasKeys(keysDir(), z) {
				return fmt.Errorf("zone-keygen: keys already exist for %s (delete them manually to regenerate)", z)
			}
			zk, err := dnssec.Generate()
			if err != nil {
				return err
			}
			if err := zk.WriteToDir(keysDir(), z); err != nil {
				return err
			}
			fmt.Printf("generated KSK + ZSK for %s in %s\n", z, keysDir())
			return nil
		},
	}
}

// newSignZoneCmd implements `zonegit sign-zone [--dry-run]`.
//
// Default behaviour (no --dry-run): loads the zone's KSK + ZSK from
// <repo>/keys/ and emits real RRSIGs that resolvers will validate.
// Falls back to placeholder mode automatically if no keys are present
// and --dry-run is set.
//
// The signing itself lives in pkg/zonesign; this is just flag parsing,
// key loading, and the commit.
func newSignZoneCmd() *cobra.Command {
	var dryRun bool
	var ttl uint32
	var validityDays uint32
	cmd := &cobra.Command{
		Use:   "sign-zone",
		Short: "Stage DNSSEC records (DNSKEY + RRSIG over every RRset + NSEC chain) on the active branch",
		Long: `Stage a DNSSEC-signed view of the active zone.

Without --dry-run: loads the zone's KSK and ZSK from <repo>/keys/ and
emits real RRSIGs that resolvers will validate end-to-end. Run
zone-keygen first.

With --dry-run: emits placeholder signatures (zero crypto). Useful for
demos and tests that don't want to roll keys.`,
		Example: "  zonegit zone-keygen foo.com.\n  zonegit sign-zone -m 'add DNSSEC'",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()

			zname := r.ActiveZone()
			if zname == "" {
				return fmt.Errorf("sign-zone: no active zone")
			}

			var keys *dnssec.ZoneKeys
			if !dryRun {
				if !dnssec.HasKeys(keysDir(), zname) {
					return fmt.Errorf("sign-zone: no DNSSEC keys for %s in %s. Run `zonegit zone-keygen %s` first, or pass --dry-run for unsigned placeholders", zname, keysDir(), zname)
				}
				keys, err = dnssec.LoadFromDir(keysDir(), zname)
				if err != nil {
					return fmt.Errorf("sign-zone: load keys: %w", err)
				}
			}

			opts := zonesign.Options{TTL: ttl, ValidityDays: validityDays}
			if err := zonesign.SignZone(ctx, r, keys, opts); err != nil {
				return err
			}
			msg := "DNSSEC signed"
			if dryRun {
				msg = "DNSSEC scaffold (dry-run, unsigned)"
			}
			h, err := r.Commit(ctx, authorIdentity(), msg)
			if err != nil {
				return err
			}
			printCommitLine(r, h, msg)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "emit placeholder signatures instead of real ones (no keys required)")
	cmd.Flags().Uint32Var(&ttl, "ttl", 300, "TTL for DNSKEY / NSEC records")
	cmd.Flags().Uint32Var(&validityDays, "validity-days", 30, "RRSIG validity window in days")
	return cmd
}
