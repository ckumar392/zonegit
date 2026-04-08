package object

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ckumar392/dnsdb/pkg/store"
)

// Identity is a name/email pair used for authors, committers, taggers.
type Identity struct {
	Name  string
	Email string
}

// String renders as "Name <email>". Empty fields are tolerated by the
// parser but discouraged.
func (id Identity) String() string {
	if id.Email == "" {
		return id.Name
	}
	return fmt.Sprintf("%s <%s>", id.Name, id.Email)
}

// Commit is the user-facing form of a commit object.
//
// On-disk format is line-oriented text per docs/OBJECT_MODEL.md section 6:
//
//	version 1
//	tree <hex>
//	parent <hex>           (zero or more)
//	author Name <email> <unix> <±HHMM>
//	committer Name <email> <unix> <±HHMM>
//	[selector <expr>]      (optional, reserved for v2 canary)
//	[signature ed25519 <base64>] (optional, populated in v3)
//
//	<message>
//
// Header order is fixed where it matters (tree first; parents in order;
// author then committer). A blank line separates headers from message.
type Commit struct {
	Tree       store.Hash
	Parents    []store.Hash
	Author     Identity
	Committer  Identity
	AuthorTime time.Time
	CommitTime time.Time
	Selector   string // optional, for canary commits (v2+)
	Signature  string // optional, populated in v3
	Message    string
}

const commitVersion = 1

// Encode renders c in canonical text form and returns (hash, object).
func (c Commit) Encode() (store.Hash, store.Object) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "version %d\n", commitVersion)
	fmt.Fprintf(&buf, "tree %s\n", c.Tree.String())
	for _, p := range c.Parents {
		fmt.Fprintf(&buf, "parent %s\n", p.String())
	}
	fmt.Fprintf(&buf, "author %s %s\n", c.Author, formatTime(c.AuthorTime))
	fmt.Fprintf(&buf, "committer %s %s\n", c.Committer, formatTime(c.CommitTime))
	if c.Selector != "" {
		fmt.Fprintf(&buf, "selector %s\n", c.Selector)
	}
	if c.Signature != "" {
		fmt.Fprintf(&buf, "signature %s\n", c.Signature)
	}
	buf.WriteByte('\n')
	buf.WriteString(c.Message)
	// Trailing newline normalization: ensure exactly one if message is non-empty
	// and does not already end with one. Empty message: no trailing newline.
	if c.Message != "" && !strings.HasSuffix(c.Message, "\n") {
		buf.WriteByte('\n')
	}
	return Encode(KindCommit, buf.Bytes())
}

// DecodeCommit parses a commit payload.
func DecodeCommit(payload []byte) (Commit, error) {
	r := bufio.NewReader(bytes.NewReader(payload))
	var c Commit
	sawVersion, sawTree, sawAuthor, sawCommitter := false, false, false, false

	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return Commit{}, errInvalid("commit: unexpected EOF before headers ended")
		}
		// Strip exactly one trailing \n if present.
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break // end of headers
		}

		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return Commit{}, errInvalid("commit: malformed header %q", line)
		}
		switch key {
		case "version":
			v, err := strconv.Atoi(value)
			if err != nil || v != commitVersion {
				return Commit{}, errInvalid("commit: unsupported version %q", value)
			}
			sawVersion = true
		case "tree":
			h, err := store.ParseHash(value)
			if err != nil {
				return Commit{}, errInvalid("commit: tree: %v", err)
			}
			c.Tree = h
			sawTree = true
		case "parent":
			h, err := store.ParseHash(value)
			if err != nil {
				return Commit{}, errInvalid("commit: parent: %v", err)
			}
			c.Parents = append(c.Parents, h)
		case "author":
			id, t, err := parseIdentityLine(value)
			if err != nil {
				return Commit{}, errInvalid("commit: author: %v", err)
			}
			c.Author = id
			c.AuthorTime = t
			sawAuthor = true
		case "committer":
			id, t, err := parseIdentityLine(value)
			if err != nil {
				return Commit{}, errInvalid("commit: committer: %v", err)
			}
			c.Committer = id
			c.CommitTime = t
			sawCommitter = true
		case "selector":
			c.Selector = value
		case "signature":
			c.Signature = value
		default:
			// Unknown headers are ignored for forward-compatibility, per
			// the design note in OBJECT_MODEL.md section 6.
		}
	}
	if !sawVersion || !sawTree || !sawAuthor || !sawCommitter {
		return Commit{}, errInvalid("commit: missing required header(s)")
	}

	// Remainder is the message.
	var msgBuf bytes.Buffer
	if _, err := msgBuf.ReadFrom(r); err != nil {
		return Commit{}, errInvalid("commit: read message: %v", err)
	}
	c.Message = strings.TrimRight(msgBuf.String(), "\n")
	return c, nil
}

// formatTime renders a time as "<unix> <±HHMM>" matching the on-disk
// format. The offset is taken from t's location.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "0 +0000"
	}
	_, offsetSec := t.Zone()
	sign := '+'
	if offsetSec < 0 {
		sign = '-'
		offsetSec = -offsetSec
	}
	hh := offsetSec / 3600
	mm := (offsetSec % 3600) / 60
	return fmt.Sprintf("%d %c%02d%02d", t.Unix(), sign, hh, mm)
}

// parseIdentityLine parses "Name <email> <unix> <±HHMM>".
func parseIdentityLine(s string) (Identity, time.Time, error) {
	// Find the last "<" / ">" pair that delimits the email.
	lt := strings.LastIndexByte(s, '<')
	gt := strings.LastIndexByte(s, '>')
	if lt < 0 || gt < lt {
		return Identity{}, time.Time{}, fmt.Errorf("missing <email> in %q", s)
	}
	name := strings.TrimSpace(s[:lt])
	email := s[lt+1 : gt]
	rest := strings.TrimSpace(s[gt+1:])

	parts := strings.Fields(rest)
	if len(parts) != 2 {
		return Identity{}, time.Time{}, fmt.Errorf("expected <unix> <±HHMM> after email in %q", s)
	}
	unix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Identity{}, time.Time{}, fmt.Errorf("bad unix time %q: %w", parts[0], err)
	}
	offsetMinutes, err := parseTZOffset(parts[1])
	if err != nil {
		return Identity{}, time.Time{}, err
	}
	loc := time.FixedZone(parts[1], offsetMinutes*60)
	return Identity{Name: name, Email: email}, time.Unix(unix, 0).In(loc), nil
}

func parseTZOffset(s string) (int, error) {
	if len(s) != 5 || (s[0] != '+' && s[0] != '-') {
		return 0, fmt.Errorf("bad tz offset %q (want ±HHMM)", s)
	}
	hh, err := strconv.Atoi(s[1:3])
	if err != nil {
		return 0, fmt.Errorf("bad tz hours %q: %w", s, err)
	}
	mm, err := strconv.Atoi(s[3:5])
	if err != nil {
		return 0, fmt.Errorf("bad tz minutes %q: %w", s, err)
	}
	off := hh*60 + mm
	if s[0] == '-' {
		off = -off
	}
	return off, nil
}
