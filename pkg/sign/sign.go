// Package sign provides Ed25519 sign/verify primitives for zonegit
// commits and tags.
//
// The commit object reserves a "signature" header (see pkg/object/commit.go).
// A signature is computed over the canonical commit bytes with the
// "signature" line stripped — exactly the way Git computes GPG signatures
// over commits. Re-encoding the commit with the new header lands an
// identical canonical payload byte-for-byte for any future verifier.
//
// Scope of v3 (per roadmap):
//   - file-backed Ed25519 keypairs (no KMS yet)
//   - sign single commits via the CLI
//   - verify a single commit or the first-parent chain to the root
//
// Out of scope here (later milestones): KMS integration, multi-sig,
// X.509 chains, server-side "refuse unsigned" policy.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
)

// GenerateKeypair generates a new Ed25519 keypair and writes the public
// and private keys (base64) to the given paths. The private file is
// chmod 0600.
func GenerateKeypair(pubPath, privPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("sign: generate: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		return err
	}
	return nil
}

// LoadPrivateKey reads a base64-encoded Ed25519 private key from path.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("sign: decode private key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sign: private key size %d, expected %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// LoadPublicKey reads a base64-encoded Ed25519 public key from path.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("sign: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("sign: public key size %d, expected %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// SignCommit returns a copy of c with Signature populated. The signature
// covers the canonical commit bytes with the "signature" header stripped
// (i.e. exactly the bytes that an unsigned re-encode would produce).
//
// The returned commit hashes differently from the unsigned version; the
// caller is responsible for writing the new commit object and moving the
// branch ref (analogous to `git commit --amend -S`).
func SignCommit(c object.Commit, priv ed25519.PrivateKey) (object.Commit, store.Hash, store.Object, error) {
	// Compute the unsigned canonical bytes.
	unsigned := c
	unsigned.Signature = ""
	_, unsignedObj := unsigned.Encode()
	sig := ed25519.Sign(priv, unsignedObj.Payload)
	c.Signature = "ed25519:" + base64.StdEncoding.EncodeToString(sig)
	h, obj := c.Encode()
	return c, h, obj, nil
}

// VerifyCommit reports whether c's Signature is a valid Ed25519 signature
// by pub over the unsigned canonical bytes of c.
//
// Returns:
//   - nil if the signature verifies
//   - ErrUnsigned if c.Signature is empty
//   - ErrBadSignatureFormat if the header does not parse
//   - ErrSignatureMismatch if the signature does not verify
func VerifyCommit(c object.Commit, pub ed25519.PublicKey) error {
	if c.Signature == "" {
		return ErrUnsigned
	}
	const prefix = "ed25519:"
	if !strings.HasPrefix(c.Signature, prefix) {
		return ErrBadSignatureFormat
	}
	sig, err := base64.StdEncoding.DecodeString(c.Signature[len(prefix):])
	if err != nil {
		return ErrBadSignatureFormat
	}
	unsigned := c
	unsigned.Signature = ""
	_, unsignedObj := unsigned.Encode()
	if !ed25519.Verify(pub, unsignedObj.Payload, sig) {
		return ErrSignatureMismatch
	}
	return nil
}

// Sentinel errors.
var (
	ErrUnsigned           = errors.New("commit has no signature")
	ErrBadSignatureFormat = errors.New("commit signature is not in ed25519:<base64> form")
	ErrSignatureMismatch  = errors.New("commit signature does not verify")
)
