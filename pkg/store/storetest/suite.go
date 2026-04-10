package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/ckumar392/dnsdb/pkg/store"
)

type Factory func(t *testing.T) store.Storage

func Run(t *testing.T, f Factory) {
	t.Helper()
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"PutGetObject", testPutGetObject},
		{"GetObjectNotFound", testGetObjectNotFound},
		{"HasObject", testHasObject},
		{"PutObjectIdempotent", testPutObjectIdempotent},
		{"IterObjects", testIterObjects},
		{"IterObjectsEmpty", testIterObjectsEmpty},
		{"CASRefCreateAndUpdate", testCASRefCreateAndUpdate},
		{"CASRefConflict", testCASRefConflict},
		{"CASRefZeroNextRejected", testCASRefZeroNextRejected},
		{"DeleteRef", testDeleteRef},
		{"DeleteRefNotFound", testDeleteRefNotFound},
		{"DeleteRefConflict", testDeleteRefConflict},
		{"ListRefs", testListRefs},
		{"ListRefsEmpty", testListRefsEmpty},
		{"Reflog", testReflog},
		{"ReflogEmpty", testReflogEmpty},
		{"Close", testClose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, f)
		})
	}
}

func h(data string) store.Hash {
	return sha256.Sum256([]byte(data))
}

func obj(kind, payload string) store.Object {
	return store.Object{Kind: kind, Payload: []byte(payload)}
}

func ctx() context.Context { return context.Background() }

func testPutGetObject(t *testing.T, f Factory) {
	s := f(t)
	hash := h("blob1")
	o := obj("blob", "hello world")
	if err := s.PutObject(ctx(), hash, o); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got, err := s.GetObject(ctx(), hash)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got.Kind != o.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, o.Kind)
	}
	if string(got.Payload) != string(o.Payload) {
		t.Errorf("Payload = %q, want %q", got.Payload, o.Payload)
	}
}

func testGetObjectNotFound(t *testing.T, f Factory) {
	s := f(t)
	_, err := s.GetObject(ctx(), h("nonexistent"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func testHasObject(t *testing.T, f Factory) {
	s := f(t)
	hash := h("has-test")
	ok, err := s.HasObject(ctx(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("HasObject returned true for absent hash")
	}
	if err := s.PutObject(ctx(), hash, obj("blob", "x")); err != nil {
		t.Fatal(err)
	}
	ok, err = s.HasObject(ctx(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("HasObject returned false after Put")
	}
}

func testPutObjectIdempotent(t *testing.T, f Factory) {
	s := f(t)
	hash := h("idem")
	o := obj("blob", "data")
	if err := s.PutObject(ctx(), hash, o); err != nil {
		t.Fatal(err)
	}
	if err := s.PutObject(ctx(), hash, o); err != nil {
		t.Fatalf("second PutObject: %v", err)
	}
}

func testIterObjects(t *testing.T, f Factory) {
	s := f(t)
	hashes := make(map[store.Hash]bool)
	for i := 0; i < 5; i++ {
		hash := h(fmt.Sprintf("iter-%d", i))
		hashes[hash] = true
		if err := s.PutObject(ctx(), hash, obj("blob", fmt.Sprintf("%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	err := s.IterObjects(ctx(), func(hash store.Hash, _ store.Object) error {
		if !hashes[hash] {
			t.Errorf("unexpected hash %s", hash.Short())
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 5 {
		t.Errorf("seen = %d, want 5", seen)
	}
}

func testIterObjectsEmpty(t *testing.T, f Factory) {
	s := f(t)
	seen := 0
	err := s.IterObjects(ctx(), func(_ store.Hash, _ store.Object) error {
		seen++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Errorf("seen = %d, want 0", seen)
	}
}

func testCASRefCreateAndUpdate(t *testing.T, f Factory) {
	s := f(t)
	var zero store.Hash
	h1 := h("ref-v1")
	h2 := h("ref-v2")
	if err := s.CASRef(ctx(), "refs/heads/main", zero, h1); err != nil {
		t.Fatalf("CASRef create: %v", err)
	}
	got, ok, err := s.GetRef(ctx(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != h1 {
		t.Fatalf("GetRef = (%s, %v), want (%s, true)", got.Short(), ok, h1.Short())
	}
	if err := s.CASRef(ctx(), "refs/heads/main", h1, h2); err != nil {
		t.Fatalf("CASRef update: %v", err)
	}
	got, _, _ = s.GetRef(ctx(), "refs/heads/main")
	if got != h2 {
		t.Fatalf("after update got %s, want %s", got.Short(), h2.Short())
	}
}

func testCASRefConflict(t *testing.T, f Factory) {
	s := f(t)
	var zero store.Hash
	h1 := h("ref-c1")
	h2 := h("ref-c2")
	wrong := h("wrong")
	_ = s.CASRef(ctx(), "refs/heads/main", zero, h1)
	err := s.CASRef(ctx(), "refs/heads/main", wrong, h2)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func testCASRefZeroNextRejected(t *testing.T, f Factory) {
	s := f(t)
	var zero store.Hash
	err := s.CASRef(ctx(), "refs/heads/main", zero, zero)
	if err == nil {
		t.Fatal("expected error for zero next hash")
	}
}

func testDeleteRef(t *testing.T, f Factory) {
	s := f(t)
	var zero store.Hash
	h1 := h("del-1")
	_ = s.CASRef(ctx(), "refs/heads/main", zero, h1)
	if err := s.DeleteRef(ctx(), "refs/heads/main", h1); err != nil {
		t.Fatalf("DeleteRef: %v", err)
	}
	_, ok, _ := s.GetRef(ctx(), "refs/heads/main")
	if ok {
		t.Error("ref still exists after delete")
	}
}

func testDeleteRefNotFound(t *testing.T, f Factory) {
	s := f(t)
	err := s.DeleteRef(ctx(), "refs/heads/nope", h("x"))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func testDeleteRefConflict(t *testing.T, f Factory) {
	s := f(t)
	var zero store.Hash
	h1 := h("del-c1")
	_ = s.CASRef(ctx(), "refs/heads/main", zero, h1)
	err := s.DeleteRef(ctx(), "refs/heads/main", h("wrong"))
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func testListRefs(t *testing.T, f Factory) {
	s := f(t)
	var zero store.Hash
	_ = s.CASRef(ctx(), "refs/heads/main", zero, h("l1"))
	_ = s.CASRef(ctx(), "refs/heads/dev", zero, h("l2"))
	_ = s.CASRef(ctx(), "refs/tags/v1", zero, h("l3"))
	refs, err := s.ListRefs(ctx(), "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("len = %d, want 2", len(refs))
	}
	if refs[0].Name != "refs/heads/dev" || refs[1].Name != "refs/heads/main" {
		t.Errorf("refs = %v", refs)
	}
}

func testListRefsEmpty(t *testing.T, f Factory) {
	s := f(t)
	refs, err := s.ListRefs(ctx(), "refs/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("len = %d, want 0", len(refs))
	}
}

func testReflog(t *testing.T, f Factory) {
	s := f(t)
	e1 := store.ReflogEntry{Old: h("old1"), New: h("new1"), Op: "commit", Message: "first"}
	e2 := store.ReflogEntry{Old: h("old2"), New: h("new2"), Op: "commit", Message: "second"}
	if err := s.AppendReflog(ctx(), "refs/heads/main", e1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendReflog(ctx(), "refs/heads/main", e2); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ReadReflog(ctx(), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Message != "first" || entries[1].Message != "second" {
		t.Error("reflog order wrong")
	}
}

func testReflogEmpty(t *testing.T, f Factory) {
	s := f(t)
	entries, err := s.ReadReflog(ctx(), "refs/heads/nope")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("len = %d, want 0", len(entries))
	}
}

func testClose(t *testing.T, f Factory) {
	s := f(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
