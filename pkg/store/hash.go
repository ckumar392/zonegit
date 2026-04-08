package store

import (
	"encoding/hex"
	"fmt"
)

// ZeroHash is the all-zeros sentinel meaning "no object" / "create-if-absent".
var ZeroHash Hash

// IsZero reports whether h is the zero hash.
func (h Hash) IsZero() bool {
	return h == ZeroHash
}

// String returns the lowercase hex encoding of the full hash.
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// Short returns the 12-character hex prefix used for display.
func (h Hash) Short() string {
	return hex.EncodeToString(h[:6])
}

// ParseHash decodes a hex-encoded hash (full length only — short hashes
// are resolved at a higher layer where prefix matching against an index
// is possible).
func ParseHash(s string) (Hash, error) {
	var h Hash
	if len(s) != 2*HashSize {
		return h, fmt.Errorf("hash must be %d hex chars, got %d: %w", 2*HashSize, len(s), ErrInvalidObject)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("decode hash %q: %w", s, ErrInvalidObject)
	}
	copy(h[:], b)
	return h, nil
}
