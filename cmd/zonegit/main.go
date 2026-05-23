package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	"github.com/spf13/cobra"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/repo"
)

// currentBranch returns the name of the branch HEAD points at. Used for
// the "[<branch> <hash>] message" commit summary line so the demo output
// matches the branch the user is actually on.
func currentBranch(r *repo.Repo) string {
	_, branch, _, err := r.Refs().ReadHEAD(context.Background())
	if err != nil {
		return "(unknown)"
	}
	return branch
}

// Globals populated from --repo / ZONEGIT_REPO.
var (
	flagRepoPath string
	flagZone     string
)

// Build-time metadata, populated via -ldflags by goreleaser.
// Defaults make `go build` / `go install` still produce something useful.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	root := &cobra.Command{
		Use:           "zonegit",
		Short:         "Versioned authoritative DNS state store",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
	}
	root.PersistentFlags().StringVar(&flagRepoPath, "repo", envDefault("ZONEGIT_REPO", "./.zonegit"), "path to zonegit repository (Badger dir)")
	root.PersistentFlags().StringVar(&flagZone, "zone", envDefault("ZONEGIT_ZONE", ""), "zone name (required for some commands)")

	root.AddCommand(
		newInitCmd(),
		newImportCmd(),
		newSetCmd(),
		newDeleteCmd(),
		newLogCmd(),
		newDiffCmd(),
		newBlameCmd(),
		newShowCmd(),
		newStatusCmd(),
		newBranchCmd(),
		newCheckoutCmd(),
		newCatObjectCmd(),
		newMergeCmd(),
		newRevertCmd(),
		newResetCmd(),
		newProposeCmd(),
		newApproveCmd(),
		newReviewCmd(),
		newKeygenCmd(),
		newSignCommitCmd(),
		newVerifyCmd(),
		newZoneCmd(),
		newSignZoneCmd(),
		newZoneKeygenCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func openRepo() (*repo.Repo, error) {
	r, err := repo.Open(repo.Options{Path: flagRepoPath})
	if err != nil {
		return nil, fmt.Errorf("open repo %s: %w", flagRepoPath, err)
	}
	// --zone, when given, switches HEAD to that zone's current branch (or
	// the default branch if no branch is in HEAD yet). The switch is
	// transient for the duration of the process — the next CLI invocation
	// sees whatever HEAD landed at.
	if flagZone != "" {
		ctx := context.Background()
		registered, err := r.Refs().IsZoneRegistered(ctx, flagZone)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		if !registered {
			_ = r.Close()
			return nil, fmt.Errorf("--zone %q is not registered; use `zonegit zone add %s` first", flagZone, flagZone)
		}
		// Pick the current branch in the target zone if HEAD already names one,
		// otherwise fall back to "main".
		branch := repo.DefaultBranch
		branches, _ := r.Refs().ListBranches(ctx, flagZone)
		if len(branches) > 0 {
			// Prefer "main" if present, else the first listed branch.
			for _, b := range branches {
				if b == repo.DefaultBranch {
					branch = b
					break
				}
			}
			if branch == repo.DefaultBranch {
				// confirm "main" actually exists; otherwise pick first
				found := false
				for _, b := range branches {
					if b == repo.DefaultBranch {
						found = true
						break
					}
				}
				if !found {
					branch = branches[0]
				}
			}
		}
		if err := r.SwitchZone(ctx, flagZone, branch); err != nil {
			_ = r.Close()
			return nil, err
		}
	}
	return r, nil
}

func authorIdentity() object.Identity {
	name := os.Getenv("ZONEGIT_AUTHOR_NAME")
	email := os.Getenv("ZONEGIT_AUTHOR_EMAIL")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		} else {
			name = "anonymous"
		}
	}
	if email == "" {
		host, _ := os.Hostname()
		email = name + "@" + host
	}
	return object.Identity{Name: name, Email: email}
}

// --- subcommands ---

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init <zone>",
		Short: "Initialize a new zonegit repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			if err := r.Init(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Printf("Initialized empty zonegit repository in %s for zone %q\n", flagRepoPath, args[0])
			return nil
		},
	}
}

func newImportCmd() *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:   "import <zonefile>",
		Short: "Import every RRset from a zonefile and commit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			if r.ActiveZone() == "" {
				return fmt.Errorf("zone not set; pass --zone or run 'zonegit init' first")
			}
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			ctx := context.Background()
			n, err := r.Import(ctx, f)
			if err != nil {
				return err
			}
			if msg == "" {
				msg = fmt.Sprintf("import %s (%d RRsets)", args[0], n)
			}
			h, err := r.Commit(ctx, authorIdentity(), msg)
			if err != nil {
				return err
			}
			fmt.Printf("[%s %s] %s (%d RRsets)\n", currentBranch(r), h.Short(), msg, n)
			return nil
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "commit message (default: 'import <file>')")
	return cmd
}

func newSetCmd() *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:     "set <name> <type> <ttl> <rdata...>",
		Short:   "Set an RRset and commit",
		Args:    cobra.MinimumNArgs(4),
		Example: "  zonegit set api.foo.com. A 300 1.2.3.4 5.6.7.8 -m 'bump api'",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			name, rrtype := args[0], strings.ToUpper(args[1])
			ttl, err := strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("ttl: %w", err)
			}
			var rrs []dns.RR
			for _, rdata := range args[3:] {
				rrText := fmt.Sprintf("%s %d IN %s %s", name, ttl, rrtype, rdata)
				rr, err := dns.NewRR(rrText)
				if err != nil {
					return fmt.Errorf("parse %q: %w", rrText, err)
				}
				rrs = append(rrs, rr)
			}
			ctx := context.Background()
			if err := r.Set(ctx, rrs); err != nil {
				return err
			}
			if msg == "" {
				msg = fmt.Sprintf("set %s %s", name, rrtype)
			}
			h, err := r.Commit(ctx, authorIdentity(), msg)
			if err != nil {
				return err
			}
			fmt.Printf("[%s %s] %s\n", currentBranch(r), h.Short(), msg)
			return nil
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "commit message")
	return cmd
}

func newDeleteCmd() *cobra.Command {
	var msg string
	cmd := &cobra.Command{
		Use:   "delete <name> <type>",
		Short: "Delete an RRset and commit",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			fqdn := stripZone(args[0], r.ActiveZone())
			r.Delete(fqdn, args[1])
			if msg == "" {
				msg = fmt.Sprintf("delete %s %s", args[0], strings.ToUpper(args[1]))
			}
			h, err := r.Commit(context.Background(), authorIdentity(), msg)
			if err != nil {
				return err
			}
			fmt.Printf("[%s %s] %s\n", currentBranch(r), h.Short(), msg)
			return nil
		},
	}
	cmd.Flags().StringVarP(&msg, "message", "m", "", "commit message")
	return cmd
}

func newLogCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "log [refish]",
		Short: "Show commit history",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			refish := "HEAD"
			if len(args) == 1 {
				refish = args[0]
			}
			entries, err := r.Log(context.Background(), refish, n)
			if err != nil {
				return err
			}
			for _, e := range entries {
				fmt.Printf("commit %s\n", e.Hash.String())
				fmt.Printf("Author: %s\n", e.Commit.Author)
				fmt.Printf("Date:   %s\n\n", e.Commit.CommitTime.Format("Mon Jan 02 15:04:05 2006 -0700"))
				for _, line := range strings.Split(strings.TrimRight(e.Commit.Message, "\n"), "\n") {
					fmt.Printf("    %s\n", line)
				}
				fmt.Println()
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "max", "n", 0, "limit number of commits (0 = all)")
	return cmd
}

func newDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <from> <to>",
		Short: "Show RRset changes between two refs",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			changes, err := r.Diff(context.Background(), args[0], args[1])
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Println("(no changes)")
				return nil
			}
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
				fmt.Printf("%s %s %s\n", sym, c.FQDN(), c.RRType)
			}
			return nil
		},
	}
}

func newBlameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "blame <name> <type>",
		Short: "Show the commit that introduced the current value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			fqdn := stripZone(args[0], r.ActiveZone())
			info, err := r.Blame(context.Background(), fqdn, args[1])
			if err != nil {
				return err
			}
			if !info.Found {
				fmt.Printf("%s %s: not found at HEAD\n", args[0], args[1])
				return nil
			}
			fmt.Printf("%s\t%s\t(%s)\t%s\n",
				info.Commit.Short(),
				info.Author,
				info.Blob.Short(),
				strings.SplitN(info.Message, "\n", 2)[0],
			)
			return nil
		},
	}
}

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name> <type> [refish]",
		Short: "Print the current RRset (zonefile format)",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			var commit [32]byte
			if len(args) == 3 {
				h, err := r.Resolve(ctx, args[2])
				if err != nil {
					return err
				}
				commit = h
			}
			fqdn := stripZone(args[0], r.ActiveZone())
			rs, err := r.Lookup(ctx, commit, fqdn, args[1])
			if err != nil {
				return err
			}
			for _, rr := range rs.RRs {
				fmt.Println(rr.String())
			}
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show repository state",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			zoneName, branch, head, err := r.Head(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("repo:   %s\n", flagRepoPath)
			fmt.Printf("zone:   %s\n", zoneName)
			fmt.Printf("branch: %s\n", branch)
			if head.IsZero() {
				fmt.Println("HEAD:   (empty)")
			} else {
				fmt.Printf("HEAD:   %s\n", head.Short())
			}
			return nil
		},
	}
}

func newBranchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branch [name [start-point]]",
		Short: "List, create, or delete branches",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			zoneName, curName, _, err := r.Head(ctx)
			if err != nil {
				return fmt.Errorf("branch: read HEAD: %w", err)
			}
			switch len(args) {
			case 0:
				names, err := r.Refs().ListBranches(ctx, zoneName)
				if err != nil {
					return err
				}
				for _, n := range names {
					mark := "  "
					if n == curName {
						mark = "* "
					}
					fmt.Printf("%s%s\n", mark, n)
				}
			case 1, 2:
				start := "HEAD"
				if len(args) == 2 {
					start = args[1]
				}
				h, err := r.Resolve(ctx, start)
				if err != nil {
					return err
				}
				return r.Refs().CreateBranch(ctx, zoneName, args[0], h)
			}
			return nil
		},
	}
	return cmd
}

func newCheckoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "checkout <branch>",
		Short: "Switch HEAD to another branch (within the active zone)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			zoneName, _, _, err := r.Head(ctx)
			if err != nil {
				return fmt.Errorf("checkout: read HEAD: %w", err)
			}
			return r.SwitchZone(ctx, zoneName, args[0])
		},
	}
}

func newCatObjectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cat-object <hash>",
		Short: "Print raw object bytes (debugging)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			h, err := r.Resolve(context.Background(), args[0])
			if err != nil {
				return err
			}
			obj, err := r.Storage().GetObject(context.Background(), h)
			if err != nil {
				return err
			}
			fmt.Printf("kind: %s\nsize: %d\n---\n", obj.Kind, len(obj.Payload))
			os.Stdout.Write(obj.Payload)
			return nil
		},
	}
}

// stripZone turns "api.foo.com." into "api" given zone "foo.com.".
func stripZone(name, zone string) string {
	name = strings.ToLower(name)
	zone = strings.ToLower(zone)
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	if !strings.HasSuffix(zone, ".") && zone != "" {
		zone += "."
	}
	if zone == "" {
		return strings.TrimSuffix(name, ".")
	}
	if name == zone {
		return "@"
	}
	if strings.HasSuffix(name, "."+zone) {
		return strings.TrimSuffix(name, "."+zone)
	}
	return strings.TrimSuffix(name, ".")
}
