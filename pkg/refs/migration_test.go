package refs_test

import (
	"testing"

	"github.com/ckumar392/zonegit/pkg/refs"
	"github.com/ckumar392/zonegit/pkg/store"
	"github.com/ckumar392/zonegit/pkg/store/memstore"
)

// TestMigrateLegacyV03 fabricates the v0.3 single-zone layout by hand and
// confirms the migration converts it to v0.4 in place.
func TestMigrateLegacyV03(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	c1 := seedCommit(t, s)
	c2 := seedCommit(t, s, c1)

	// Hand-build a v0.3 layout:
	//   refs/zonegit/zone        = length-prefix "foo.com."
	//   refs/heads/main          = c2
	//   refs/heads/canary        = c1
	//   refs/tags/v1             = c1
	//   HEAD                     = length-prefix "refs/heads/main"
	mustWriteLegacySlot(t, s, refs.LegacyZoneNameRef, "foo.com.")
	mustWriteSlot(t, s, "refs/heads/main", c2)
	mustWriteSlot(t, s, "refs/heads/canary", c1)
	mustWriteSlot(t, s, "refs/tags/v1", c1)
	mustWriteLegacySlot(t, s, refs.HeadRef, "refs/heads/main")

	// Pre-migration sanity: IsLegacyV03 should fire.
	legacy, zone, err := db.IsLegacyV03(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !legacy || zone != "foo.com." {
		t.Fatalf("pre-migrate: legacy=%v zone=%q", legacy, zone)
	}

	// Migrate.
	migrated, gotZone, err := db.MigrateLegacyV03(ctx())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !migrated || gotZone != "foo.com." {
		t.Fatalf("Migrate: migrated=%v zone=%q", migrated, gotZone)
	}

	// Post-migration: legacy marker gone.
	legacy, _, _ = db.IsLegacyV03(ctx())
	if legacy {
		t.Fatal("legacy marker still present after migration")
	}

	// New zone registered.
	zones, _ := db.ListZones(ctx())
	if len(zones) != 1 || zones[0] != "foo.com." {
		t.Fatalf("zones = %v", zones)
	}

	// Branches moved to new paths with original hashes.
	if got, err := db.GetBranch(ctx(), "foo.com.", "main"); err != nil || got != c2 {
		t.Fatalf("main: got=%s err=%v want=%s", got.Short(), err, c2.Short())
	}
	if got, err := db.GetBranch(ctx(), "foo.com.", "canary"); err != nil || got != c1 {
		t.Fatalf("canary: got=%s err=%v want=%s", got.Short(), err, c1.Short())
	}

	// Legacy branch refs are gone.
	if _, ok, _ := s.GetRef(ctx(), "refs/heads/main"); ok {
		t.Error("legacy refs/heads/main was not deleted")
	}

	// Tags moved.
	if got, err := db.GetTag(ctx(), "foo.com.", "v1"); err != nil || got != c1 {
		t.Fatalf("tag: got=%s err=%v", got.Short(), err)
	}

	// HEAD now points at refs/heads/foo.com./main via object-backed symref.
	gotZone2, gotBranch, gotCommit, err := db.ReadHEAD(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if gotZone2 != "foo.com." || gotBranch != "main" || gotCommit != c2 {
		t.Fatalf("HEAD = (%q, %q, %s), want (foo.com., main, %s)", gotZone2, gotBranch, gotCommit.Short(), c2.Short())
	}
}

// TestObjectBackedHEADLongTarget proves the v0.5 fix removes the 31-byte
// HEAD-target limit (long zone name + long branch name combined).
func TestObjectBackedHEADLongTarget(t *testing.T) {
	s := memstore.New()
	db := refs.New(s)
	// "really-long-subzone.example.co.uk." (33) + "/feature-very-long" (18) = 51 chars
	// plus "refs/heads/" (11) prefix = 62 chars total — well past the legacy 31-byte slot.
	zone := "really-long-subzone.example.co.uk."
	branch := "feature-very-long"

	if err := db.SetHEAD(ctx(), zone, branch); err != nil {
		t.Fatalf("SetHEAD: %v", err)
	}
	gotZone, gotBranch, _, err := db.ReadHEAD(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if gotZone != zone || gotBranch != branch {
		t.Fatalf("HEAD roundtrip: (%q, %q), want (%q, %q)", gotZone, gotBranch, zone, branch)
	}
}

// --- helpers ---

func mustWriteSlot(t *testing.T, s store.Storage, ref string, h store.Hash) {
	t.Helper()
	if err := s.CASRef(ctx(), ref, store.ZeroHash, h); err != nil {
		t.Fatal(err)
	}
}

func mustWriteLegacySlot(t *testing.T, s store.Storage, ref, payload string) {
	t.Helper()
	var h store.Hash
	if len(payload) > store.HashSize-1 {
		t.Fatalf("test fixture: legacy payload %q too long", payload)
	}
	h[0] = byte(len(payload))
	copy(h[1:], payload)
	if err := s.CASRef(ctx(), ref, store.ZeroHash, h); err != nil {
		t.Fatal(err)
	}
}
