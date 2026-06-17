package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newMergeCmd() *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:   "merge <branch>",
		Short: "Integrate <branch> into the current branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			res, err := r.Merge(context.Background(), args[0], authorIdentity(), msg)
			if err != nil {
				return err
			}
			switch {
			case res.AlreadyUpToDate:
				fmt.Println("Already up to date.")
			case res.FastForward:
				fmt.Printf("Fast-forward to %s.\n", res.Commit.Short())
			case len(res.Conflicts) > 0:
				return printMergeConflicts(cmd.OutOrStderr(), "merge conflicts (no commit produced):", res)
			default:
				fmt.Printf("Merge made by 3-way; new commit %s.\n", res.Commit.Short())
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "merge commit message (default: 'Merge branch ...')")
	return cmd
}

func newRevertCmd() *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:   "revert <commit>",
		Short: "Create a new commit that undoes <commit>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			h, err := r.Revert(context.Background(), args[0], authorIdentity(), msg)
			if err != nil {
				return err
			}
			fmt.Printf("Reverted as %s\n", h.Short())
			return nil
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "commit message (default: 'Revert \"...\"')")
	return cmd
}

func newResetCmd() *cobra.Command {
	var hard bool
	cmd := &cobra.Command{
		Use:   "reset [--hard] <ref-ish>",
		Short: "Move the current branch tip to <ref-ish>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !hard {
				return fmt.Errorf("reset: only --hard is supported (zonegit has no working tree)")
			}
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			h, err := r.ResetHard(context.Background(), args[0], authorIdentity())
			if err != nil {
				return err
			}
			fmt.Printf("HEAD is now at %s\n", h.Short())
			return nil
		},
	}
	cmd.Flags().BoolVar(&hard, "hard", false, "reset the branch tip (required; --soft / --mixed are not meaningful in zonegit)")
	return cmd
}
