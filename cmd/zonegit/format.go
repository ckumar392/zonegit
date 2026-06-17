package main

import (
	"fmt"
	"io"

	"github.com/ckumar392/zonegit/pkg/history"
	"github.com/ckumar392/zonegit/pkg/repo"
	"github.com/ckumar392/zonegit/pkg/store"
)

// diffSymbol maps a change op to its one-character diff marker.
func diffSymbol(c history.Change) string {
	switch c.Op {
	case history.OpAdded:
		return "+"
	case history.OpRemoved:
		return "-"
	case history.OpModified:
		return "~"
	default:
		return "?"
	}
}

// printCommitLine prints the "[<branch> <hash>] <message>" summary shared by
// the commands that produce a single commit (set, delete, sign-zone).
func printCommitLine(r *repo.Repo, h store.Hash, msg string) {
	fmt.Printf("[%s %s] %s\n", currentBranch(r), h.Short(), msg)
}

// printMergeConflicts writes a merge/approve conflict report under header and
// returns the error to surface to the caller. The success-path messages stay
// with each command (merge and approve use different vocabulary on purpose).
func printMergeConflicts(w io.Writer, header string, res repo.MergeResult) error {
	fmt.Fprintln(w, header)
	for _, c := range res.Conflicts {
		fmt.Fprintf(w, "  %s\n", c)
	}
	return fmt.Errorf("%d conflict(s)", len(res.Conflicts))
}
