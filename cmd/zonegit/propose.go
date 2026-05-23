package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newProposeCmd implements `zonegit propose <name> [--from main]`.
//
// This is a thin convenience over `branch + checkout`: it creates a
// proposal branch off a starting point and switches HEAD onto it so the
// subsequent `set` / `delete` / `import` commands land on the proposal.
//
// The verb exists because branches alone do not communicate intent. An
// operator reviewing a change wants to see "proposal X" before approval,
// and that vocabulary lands better with an SME audience than "branch X".
func newProposeCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "propose <name>",
		Short: "Create a proposal branch off <from> and switch HEAD onto it",
		Long: `Create a proposal branch and check it out.

A proposal is just a branch. The verb exists to make change-management
flows read naturally: 'propose api-failover', stage edits, 'approve
api-failover'. Under the hood this is 'branch + checkout'.`,
		Args:    cobra.ExactArgs(1),
		Example: "  zonegit propose api-failover --from main",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()

			start, err := r.Resolve(ctx, from)
			if err != nil {
				return fmt.Errorf("propose: resolve --from %q: %w", from, err)
			}
			zoneName, _, _, err := r.Head(ctx)
			if err != nil {
				return fmt.Errorf("propose: read HEAD: %w", err)
			}
			if err := r.Refs().CreateBranch(ctx, zoneName, name, start); err != nil {
				return fmt.Errorf("propose: create %s: %w", name, err)
			}
			if err := r.SwitchZone(ctx, zoneName, name); err != nil {
				return fmt.Errorf("propose: checkout %s: %w", name, err)
			}
			fmt.Printf("proposal %q created from %s (HEAD now on %s/%s)\n", name, start.Short(), zoneName, name)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "main", "starting point for the proposal")
	return cmd
}

// newApproveCmd implements `zonegit approve <proposal> [--into main]`.
//
// Equivalent to: checkout main, merge proposal. The summary output is
// shaped for change-management consumption (commit hash + brief
// description of what landed).
func newApproveCmd() *cobra.Command {
	var into string
	var msg string
	cmd := &cobra.Command{
		Use:   "approve <proposal>",
		Short: "Merge <proposal> into <into> and switch HEAD onto <into>",
		Long: `Approve a proposal by merging it into the target branch.

This is 'checkout <into>; merge <proposal>'. On conflicts the merge
aborts and the proposal stays open for further edits.`,
		Args:    cobra.ExactArgs(1),
		Example: "  zonegit approve api-failover --into main",
		RunE: func(cmd *cobra.Command, args []string) error {
			proposal := args[0]
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()

			zoneName, _, _, err := r.Head(ctx)
			if err != nil {
				return fmt.Errorf("approve: read HEAD: %w", err)
			}
			if err := r.SwitchZone(ctx, zoneName, into); err != nil {
				return fmt.Errorf("approve: checkout %s: %w", into, err)
			}
			res, err := r.Merge(ctx, proposal, authorIdentity(), msg)
			if err != nil {
				return err
			}
			switch {
			case res.AlreadyUpToDate:
				fmt.Printf("Proposal %q is already up to date with %s.\n", proposal, into)
			case res.FastForward:
				fmt.Printf("Approved %q: fast-forward to %s on %s.\n", proposal, res.Commit.Short(), into)
			case len(res.Conflicts) > 0:
				fmt.Fprintln(cmd.OutOrStderr(), "conflicts (proposal remains open):")
				for _, c := range res.Conflicts {
					fmt.Fprintf(cmd.OutOrStderr(), "  %s\n", c)
				}
				return fmt.Errorf("%d conflict(s)", len(res.Conflicts))
			default:
				fmt.Printf("Approved %q: merge commit %s on %s.\n", proposal, res.Commit.Short(), into)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&into, "into", "main", "branch to merge the proposal into")
	cmd.Flags().StringVarP(&msg, "message", "m", "", "approval/merge commit message")
	return cmd
}

// newReviewCmd implements `zonegit review <proposal>`, an alias for
// `diff <into>..<proposal>` shaped for change reviewers.
func newReviewCmd() *cobra.Command {
	var into string
	cmd := &cobra.Command{
		Use:   "review <proposal>",
		Short: "Show what <proposal> would change against <into>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			proposal := args[0]
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			changes, err := r.Diff(ctx, into, proposal)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Printf("proposal %q has no changes vs %s\n", proposal, into)
				return nil
			}
			fmt.Printf("proposal %q vs %s — %d change(s):\n", proposal, into, len(changes))
			for _, c := range changes {
				sym := "?"
				switch c.Op.String() {
				case "added":
					sym = "+"
				case "removed":
					sym = "-"
				case "modified":
					sym = "~"
				}
				fmt.Printf("  %s %s %s\n", sym, strings.TrimSuffix(c.FQDN(), "."), c.RRType)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&into, "into", "main", "branch to compare the proposal against")
	return cmd
}
