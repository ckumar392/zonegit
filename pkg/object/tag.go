package object

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ckumar392/zonegit/pkg/store"
)

// Tag is an immutable named pointer with an optional message and signature.
//
// Format mirrors Commit's text-line scheme; see docs/OBJECT_MODEL.md
// section 7.
type Tag struct {
	Object     store.Hash
	Type       string // typically "commit"
	Tag        string // tag name, e.g. "v2026.04.25-prod"
	Tagger     Identity
	TaggerTime time.Time
	Signature  string // optional
	Message    string
}

const tagVersion = 1

// Encode renders t in canonical form and returns (hash, object).
func (t Tag) Encode() (store.Hash, store.Object) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "version %d\n", tagVersion)
	fmt.Fprintf(&buf, "object %s\n", t.Object.String())
	fmt.Fprintf(&buf, "type %s\n", t.Type)
	fmt.Fprintf(&buf, "tag %s\n", t.Tag)
	fmt.Fprintf(&buf, "tagger %s %s\n", t.Tagger, formatTime(t.TaggerTime))
	if t.Signature != "" {
		fmt.Fprintf(&buf, "signature %s\n", t.Signature)
	}
	buf.WriteByte('\n')
	buf.WriteString(t.Message)
	if t.Message != "" && !strings.HasSuffix(t.Message, "\n") {
		buf.WriteByte('\n')
	}
	return Encode(KindTag, buf.Bytes())
}

// DecodeTag parses a tag payload.
func DecodeTag(payload []byte) (Tag, error) {
	r := bufio.NewReader(bytes.NewReader(payload))
	var t Tag
	sawVersion, sawObject, sawType, sawTag, sawTagger := false, false, false, false, false

	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return Tag{}, errInvalid("tag: unexpected EOF before headers ended")
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			return Tag{}, errInvalid("tag: malformed header %q", line)
		}
		switch key {
		case "version":
			v, err := strconv.Atoi(value)
			if err != nil || v != tagVersion {
				return Tag{}, errInvalid("tag: unsupported version %q", value)
			}
			sawVersion = true
		case "object":
			h, err := store.ParseHash(value)
			if err != nil {
				return Tag{}, errInvalid("tag: object: %v", err)
			}
			t.Object = h
			sawObject = true
		case "type":
			t.Type = value
			sawType = true
		case "tag":
			t.Tag = value
			sawTag = true
		case "tagger":
			id, ts, err := parseIdentityLine(value)
			if err != nil {
				return Tag{}, errInvalid("tag: tagger: %v", err)
			}
			t.Tagger = id
			t.TaggerTime = ts
			sawTagger = true
		case "signature":
			t.Signature = value
		}
	}
	if !sawVersion || !sawObject || !sawType || !sawTag || !sawTagger {
		return Tag{}, errInvalid("tag: missing required header(s)")
	}
	var msgBuf bytes.Buffer
	if _, err := msgBuf.ReadFrom(r); err != nil {
		return Tag{}, errInvalid("tag: read message: %v", err)
	}
	t.Message = strings.TrimRight(msgBuf.String(), "\n")
	return t, nil
}
