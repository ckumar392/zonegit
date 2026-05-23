package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDaemonConfig_EmptyPath(t *testing.T) {
	c, err := loadDaemonConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultBranch != "" {
		t.Errorf("empty config should have empty default branch, got %q", c.DefaultBranch)
	}
}

func TestLoadDaemonConfig_TopLevelDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(`
default_branch: prod
canary: canary:10
canary_salt: rollout-7
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadDaemonConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultBranch != "prod" || c.DefaultCanary != "canary:10" || c.DefaultSalt != "rollout-7" {
		t.Errorf("got %+v", c)
	}
}

func TestRuleFor_OverlaysOnDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(`
default_branch: prod
canary: canary:10
canary_salt: default-salt
zones:
  foo.com.:
    branch: staging
    canary: canary:50
  bar.com.:
    at: HEAD~3
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadDaemonConfig(p)
	if err != nil {
		t.Fatal(err)
	}

	foo := c.ruleFor("foo.com.")
	if foo.Branch != "staging" || foo.Canary != "canary:50" || foo.Salt != "default-salt" {
		t.Errorf("foo.com. rule = %+v", foo)
	}

	// bar.com. only overrides 'at'; everything else falls through.
	bar := c.ruleFor("bar.com.")
	if bar.Branch != "prod" || bar.Canary != "canary:10" || bar.At != "HEAD~3" {
		t.Errorf("bar.com. rule = %+v", bar)
	}

	// A zone absent from the map still gets the top-level defaults.
	other := c.ruleFor("baz.com.")
	if other.Branch != "prod" || other.Canary != "canary:10" {
		t.Errorf("baz.com. rule (defaults) = %+v", other)
	}
}

func TestRuleFor_CanonicalizesZoneKey(t *testing.T) {
	// Keys in YAML may be missing the trailing dot or use mixed case.
	// ruleFor should still find them.
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(`
default_branch: main
zones:
  "Foo.Com":
    branch: special
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := loadDaemonConfig(p)
	if got := c.ruleFor("foo.com."); got.Branch != "special" {
		t.Errorf("canonicalisation failed: got %+v", got)
	}
}
