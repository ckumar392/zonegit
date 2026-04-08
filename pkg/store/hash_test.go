package store

import (
	"strings"
	"testing"
)

func TestHashStringAndParse(t *testing.T) {
	t.Parallel()
	var h Hash
	for i := range h {
		h[i] = byte(i)
	}
	s := h.String()
	if len(s) != 2*HashSize {
		t.Fatalf("hex length: got %d want %d", len(s), 2*HashSize)
	}
	if s != strings.ToLower(s) {
		t.Fatalf("hex must be lowercase: %q", s)
	}
	got, err := ParseHash(s)
	if err != nil {
		t.Fatalf("ParseHash: %v", err)
	}
	if got != h {
		t.Fatalf("round-trip mismatch")
	}
	if h.Short() != s[:12] {
		t.Fatalf("Short: got %q want %q", h.Short(), s[:12])
	}
}

func TestParseHashErrors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"deadbeef",              // too short
		strings.Repeat("z", 64), // not hex
	}
	for _, c := range cases {
		if _, err := ParseHash(c); err == nil {
			t.Fatalf("ParseHash(%q): expected error", c)
		}
	}
}

func TestZeroHash(t *testing.T) {
	t.Parallel()
	if !ZeroHash.IsZero() {
		t.Fatal("ZeroHash.IsZero must be true")
	}
	var h Hash
	h[0] = 1
	if h.IsZero() {
		t.Fatal("non-zero hash reported zero")
	}
}
