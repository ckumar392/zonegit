package main

import (
	"fmt"

	"github.com/miekg/dns"
	"github.com/spf13/cobra"

	"github.com/ckumar392/zonegit/pkg/dnssec"
)

// newDSCmd implements `zonegit ds [zone]`.
//
// Prints the DS (Delegation Signer) record for the zone's KSK, in
// zone-file format. The parent zone's operator pastes this into their
// own zone to complete the chain of trust:
//
//	foo.com.   IN  DS  <key_tag> <algorithm> 2 <SHA256 digest>
//
// Digest type 2 (SHA-256) is mandatory per RFC 4509; we don't expose
// the SHA-1 option (type 1) at all because it's been deprecated for
// new deployments since 2014.
func newDSCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ds [zone]",
		Short: "Print the DS record for the zone's KSK (paste into parent zone)",
		Args:  cobra.MaximumNArgs(1),
		Example: "  zonegit ds foo.com.",
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
				return fmt.Errorf("ds: no zone")
			}
			if !dnssec.HasKeys(keysDir(), z) {
				return fmt.Errorf("ds: no keys for %s in %s. Run `zonegit zone-keygen %s` first", z, keysDir(), z)
			}
			zk, err := dnssec.LoadFromDir(keysDir(), z)
			if err != nil {
				return err
			}
			ksk, _ := zk.DNSKEYs(z, 300)
			ds := ksk.ToDS(dns.SHA256)
			if ds == nil {
				return fmt.Errorf("ds: SHA-256 digest unsupported by miekg/dns (this should not happen)")
			}
			fmt.Println(ds.String())
			return nil
		},
	}
}
