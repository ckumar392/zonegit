// Command coredns-with-zonegit is a custom CoreDNS binary that
// includes the zonegit plugin. It is otherwise identical to upstream
// CoreDNS.
//
// Build instructions live in plugin/coredns/zonegit/README.md.
package main

import (
	"github.com/coredns/coredns/core/dnsserver"

	// Pull in the standard CoreDNS plugin set so the resulting binary
	// behaves like upstream CoreDNS for every directive that isn't
	// `zonegit`.
	_ "github.com/coredns/coredns/core/plugin"

	"github.com/coredns/coredns/coremain"

	// Side-effect import: registers the `zonegit` plugin with CoreDNS's
	// directive table.
	_ "github.com/ckumar392/zonegit/plugin/coredns/zonegit"
)

func init() {
	// CoreDNS plugin order is governed by dnsserver.Directives. Inserting
	// "zonegit" at the front means it runs before standard plugins like
	// `file`, so it captures queries for the mounted zone unconditionally.
	dnsserver.Directives = append([]string{"zonegit"}, dnsserver.Directives...)
}

func main() {
	coremain.Run()
}
