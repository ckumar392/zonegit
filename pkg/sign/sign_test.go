package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ckumar392/zonegit/pkg/object"
	"github.com/ckumar392/zonegit/pkg/store"
)

func newTestCommit() object.Commit {
	return object.Commit{
		Tree:       store.Hash{1, 2, 3},
		Parents:    []store.Hash{{4, 5, 6}},
		Author:     object.Identity{Name: "Tester", Email: "t@example.com"},
		Committer:  object.Identity{Name: "Tester", Email: "t@example.com"},
		AuthorTime: time.Unix(1700000000, 0).UTC(),
		CommitTime: time.Unix(1700000000, 0).UTC(),
		Message:    "test commit",
	}
}

func TestSignAndVerify_RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCommit()
	signed, _, _, err := SignCommit(c, priv)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Signature == "" {
		t.Fatal("signature was not set")
	}
	if err := VerifyCommit(signed, pub); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerify_RejectsWrongKey(t *testing.T) {
	_, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	signed, _, _, _ := SignCommit(newTestCommit(), priv1)
	if err := VerifyCommit(signed, pub2); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("expected ErrSignatureMismatch, got %v", err)
	}
}

func TestVerify_RejectsTamper(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signed, _, _, _ := SignCommit(newTestCommit(), priv)
	signed.Message = "tampered"
	if err := VerifyCommit(signed, pub); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("expected ErrSignatureMismatch, got %v", err)
	}
}

func TestVerify_RejectsUnsigned(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifyCommit(newTestCommit(), pub); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("expected ErrUnsigned, got %v", err)
	}
}

func TestKeypairFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pubP := filepath.Join(dir, "pub")
	privP := filepath.Join(dir, "priv")
	if err := GenerateKeypair(pubP, privP); err != nil {
		t.Fatal(err)
	}
	pub, err := LoadPublicKey(pubP)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := LoadPrivateKey(privP)
	if err != nil {
		t.Fatal(err)
	}
	signed, _, _, _ := SignCommit(newTestCommit(), priv)
	if err := VerifyCommit(signed, pub); err != nil {
		t.Fatal(err)
	}
}
