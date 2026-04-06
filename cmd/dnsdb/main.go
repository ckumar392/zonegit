// Command dnsdb is the dnsdb command-line interface.
//
// See docs/ROADMAP.md for the v0 subcommand set.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "dnsdb: subcommand required")
		fmt.Fprintln(os.Stderr, "usage: dnsdb <command> [args...]")
		os.Exit(2)
	}
	// Real subcommand dispatch lands when pkg/repo is wired up.
	fmt.Fprintln(os.Stderr, "dnsdb: not yet implemented (v0 in progress)")
	os.Exit(1)
}
