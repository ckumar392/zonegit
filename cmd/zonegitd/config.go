package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ckumar392/zonegit/pkg/refs"
)

// daemonConfig is the YAML schema for --config. It lets each zone get its
// own branch / canary / time-travel pin so a single daemon can run
// production "main" on one tenant zone while simultaneously running
// "canary:20" on another — without restarting between rollouts.
//
// Top-level defaults apply to any zone that doesn't override; zones absent
// from the map are still served (with the defaults) since the daemon
// auto-discovers zones via the reconciler.
type daemonConfig struct {
	DefaultBranch string                    `yaml:"default_branch,omitempty"`
	DefaultAt     string                    `yaml:"at,omitempty"`
	DefaultCanary string                    `yaml:"canary,omitempty"`
	DefaultSalt   string                    `yaml:"canary_salt,omitempty"`
	Zones         map[string]zoneRuleConfig `yaml:"zones,omitempty"`
}

// zoneRuleConfig is the per-zone overlay. Empty fields fall through to
// the daemon-level defaults.
type zoneRuleConfig struct {
	Branch string `yaml:"branch,omitempty"`
	At     string `yaml:"at,omitempty"`     // refish; resolved at startup
	Canary string `yaml:"canary,omitempty"` // "branch:pct"
	Salt   string `yaml:"canary_salt,omitempty"`
}

// loadDaemonConfig reads and parses a YAML config file. Empty path
// returns an empty config (the daemon then falls back to CLI flags).
func loadDaemonConfig(path string) (*daemonConfig, error) {
	if path == "" {
		return &daemonConfig{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c daemonConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.DefaultBranch == "" {
		c.DefaultBranch = "main"
	}
	// Canonicalise zone keys (lowercase, trailing dot) so lookups match
	// the refs.CanonZone-canonicalised forms the daemon uses everywhere.
	canon := make(map[string]zoneRuleConfig, len(c.Zones))
	for k, v := range c.Zones {
		canon[refs.CanonZone(k)] = v
	}
	c.Zones = canon
	return &c, nil
}

// ruleFor returns the effective per-zone settings, layering zone
// overrides on top of the daemon-level defaults.
func (c *daemonConfig) ruleFor(zone string) zoneRuleConfig {
	zone = refs.CanonZone(zone)
	r := zoneRuleConfig{
		Branch: c.DefaultBranch,
		At:     c.DefaultAt,
		Canary: c.DefaultCanary,
		Salt:   c.DefaultSalt,
	}
	if z, ok := c.Zones[zone]; ok {
		if z.Branch != "" {
			r.Branch = z.Branch
		}
		if z.At != "" {
			r.At = z.At
		}
		if z.Canary != "" {
			r.Canary = z.Canary
		}
		if z.Salt != "" {
			r.Salt = z.Salt
		}
	}
	if r.Branch == "" {
		r.Branch = "main"
	}
	if r.Salt == "" {
		r.Salt = "zonegit"
	}
	return r
}

// String renders a compact one-line summary of a rule for logs / metrics.
func (r zoneRuleConfig) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "branch=%s", r.Branch)
	if r.Canary != "" {
		fmt.Fprintf(&b, " canary=%s", r.Canary)
	}
	if r.At != "" {
		fmt.Fprintf(&b, " at=%s", r.At)
	}
	return b.String()
}
