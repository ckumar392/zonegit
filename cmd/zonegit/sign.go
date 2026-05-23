package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/sign"
)

// newKeygenCmd implements `zonegit keygen <pubpath> <privpath>`.
func newKeygenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keygen <pubpath> <privpath>",
		Short: "Generate a new Ed25519 keypair for commit signing",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := sign.GenerateKeypair(args[0], args[1]); err != nil {
				return err
			}
			fmt.Printf("wrote public key to  %s\nwrote private key to %s\n", args[0], args[1])
			return nil
		},
	}
}

// newSignCommitCmd re-encodes a commit with an Ed25519 signature header
// and rewrites the branch ref to point at the new (signed) commit.
//
// Note: this changes the commit hash, since the signature header is part
// of the canonical bytes. The reflog records the rewrite.
func newSignCommitCmd() *cobra.Command {
	var keyPath string
	cmd := &cobra.Command{
		Use:   "sign-commit [refish]",
		Short: "Re-encode a commit with an Ed25519 signature and move the branch tip",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyPath == "" {
				return fmt.Errorf("--key is required")
			}
			priv, err := sign.LoadPrivateKey(keyPath)
			if err != nil {
				return err
			}
			refish := "HEAD"
			if len(args) == 1 {
				refish = args[0]
			}

			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			h, err := r.Resolve(ctx, refish)
			if err != nil {
				return err
			}
			obj, err := r.Storage().GetObject(ctx, h)
			if err != nil {
				return err
			}
			c, err := object.DecodeCommit(obj.Payload)
			if err != nil {
				return err
			}
			signed, newH, newObj, err := sign.SignCommit(c, priv)
			if err != nil {
				return err
			}
			if err := r.Storage().PutObject(ctx, newH, newObj); err != nil {
				return err
			}

			// If refish was a branch tip, move it. Otherwise just print the
			// new hash; the user can promote it manually.
			branch, headHash, err := r.Refs().ReadHEAD(ctx)
			if err == nil && headHash == h {
				name := strings.TrimPrefix(branch, refs.BranchPrefix)
				if err := r.Refs().UpdateBranch(ctx, name, h, newH); err != nil {
					return fmt.Errorf("sign-commit: move branch: %w", err)
				}
				_ = r.Refs().AppendReflog(ctx, branch, h, newH, signed.Author.String(), "sign", "ed25519 sign")
			}
			fmt.Printf("signed %s -> %s\n", h.Short(), newH.Short())
			return nil
		},
	}
	cmd.Flags().StringVar(&keyPath, "key", "", "path to Ed25519 private key (base64)")
	return cmd
}

// newVerifyCmd verifies the signature on a single commit (or on the
// first-parent chain when --chain is set).
func newVerifyCmd() *cobra.Command {
	var keyPath string
	var chain bool
	cmd := &cobra.Command{
		Use:   "verify [refish]",
		Short: "Verify an Ed25519 signature on a commit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyPath == "" {
				return fmt.Errorf("--key is required")
			}
			pub, err := sign.LoadPublicKey(keyPath)
			if err != nil {
				return err
			}
			refish := "HEAD"
			if len(args) == 1 {
				refish = args[0]
			}
			r, err := openRepo()
			if err != nil {
				return err
			}
			defer r.Close()
			ctx := context.Background()
			h, err := r.Resolve(ctx, refish)
			if err != nil {
				return err
			}
			for {
				obj, err := r.Storage().GetObject(ctx, h)
				if err != nil {
					return err
				}
				c, err := object.DecodeCommit(obj.Payload)
				if err != nil {
					return err
				}
				switch err := sign.VerifyCommit(c, pub); err {
				case nil:
					fmt.Printf("OK     %s  %s\n", h.Short(), firstLine(c.Message))
				default:
					fmt.Printf("FAIL   %s  %s  (%v)\n", h.Short(), firstLine(c.Message), err)
					if !chain {
						return err
					}
				}
				if !chain || len(c.Parents) == 0 {
					return nil
				}
				h = c.Parents[0]
			}
		},
	}
	cmd.Flags().StringVar(&keyPath, "key", "", "path to Ed25519 public key (base64)")
	cmd.Flags().BoolVar(&chain, "chain", false, "verify the entire first-parent chain back to the root")
	return cmd
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
