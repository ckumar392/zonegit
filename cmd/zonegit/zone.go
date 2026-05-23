package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

// newZoneCmd is the parent for `zonegit zone <subcmd>`. Subcommands let
// the user register additional zones in a repo and list what's there.
func newZoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "zone",
		Short: "Manage zones registered in this repo (add, list, switch)",
	}
	cmd.AddCommand(newZoneAddCmd(), newZoneListCmd(), newZoneSwitchCmd())
	return cmd
}

// newZoneAddCmd implements `zonegit zone add <zone>`.
//
// Unlike `init`, `add` never moves HEAD — the new zone gets registered
// and starts with no branches. The user adds the first commit to it
// via `--zone <name>` on subsequent ops, or by `zone switch <name>`.
func newZoneAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "add <zone>",
		Short:   "Register a new zone in this repo (does not move HEAD)",
		Args:    cobra.ExactArgs(1),
		Example: "  zonegit zone add bar.com.",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			if err := r.AddZone(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Printf("registered zone %s\n", args[0])
			return nil
		},
	}
}

// newZoneListCmd implements `zonegit zone list`. The current active zone
// (as derived from HEAD) is marked with a "*".
func newZoneListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all zones registered in this repo",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			zones, err := r.Zones(ctx)
			if err != nil {
				return err
			}
			active := r.ActiveZone()
			for _, z := range zones {
				mark := "  "
				if z == active {
					mark = "* "
				}
				fmt.Printf("%s%s\n", mark, z)
			}
			return nil
		},
	}
}

// newZoneSwitchCmd implements `zonegit zone switch <zone> [branch]`. Moves
// HEAD to the given zone's branch (default "main"). The branch must
// already exist (or be created via `branch <name>` first).
func newZoneSwitchCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "switch <zone> [branch]",
		Short:   "Switch HEAD to a different zone (and optionally a branch within it)",
		Args:    cobra.RangeArgs(1, 2),
		Example: "  zonegit zone switch bar.com. main",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			branch := "main"
			if len(args) == 2 {
				branch = args[1]
			}
			return r.SwitchZone(context.Background(), args[0], branch)
		},
	}
}
