// Command dnsdbd is the dnsdb server: a DNS responder that resolves
// queries against a chosen branch's tree, plus (in v4+) a gRPC control
// plane.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "dnsdbd: not yet implemented (v0 in progress)")
	os.Exit(1)
}
