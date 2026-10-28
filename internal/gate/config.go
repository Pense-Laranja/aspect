// Package gate is the deterministic half of the review gates: it decides
// which review agents apply to a change, validates the reports they write,
// and turns their findings into a pass, warn or block outcome. Models judge;
// this package counts. It never calls a model, git or the network: the CLI
// hands it the changed files and the report bytes.
package gate

import (
	"bytes"
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is where gates.yaml lives in a repository.
const DefaultConfigPath = ".claude/review/gates.yaml"

// DefaultSpec is where a repository keeps its Aspect spec: the entry point
// inside _aspect/, next to the files it includes and its briefs, so the spec
// is one directory rather than files scattered at the root.
const DefaultSpec = "_aspect/aspect.yaml"

// LegacySpec is the root entry point used before _aspect/. When gates.yaml
// names no spec and DefaultSpec is absent, the plan falls back to it.
const LegacySpec = "aspect.yaml"

// Severities in order of consequence, most severe first.
var Severities = []string{"critical", "high", "medium", "low", "info"}

// Categories a finding may carry; they name the harm, not the agent.
var Categories = []string{"correctness", "spec-drift", "security", "performance", "design", "test-gap", "docs", "data", "readability", "consistency", "observability", "contract", "i18n"}

// Modes an agent can run in.
var Modes = []string{"gate", "advisory", "author"}

// Config is gates.yaml: which agents gate which changes, and how findings
// become an outcome.
type Config struct {
	Spec            string     `yaml:"spec" json:"spec"`
	Gates           []Gate     `yaml:"gates" json:"gates"`
	Thresholds      Thresholds `yaml:"thresholds" json:"thresholds"`
	PublicRedaction []string   `yaml:"public_redaction" json:"public_redaction"`

	// specDefaulted records that gates.yaml named no spec, so ResolveSpec
	// may fall back to LegacySpec.
	specDefaulted bool
}

// Gate binds an agent to the paths whose changes it reviews.
type Gate struct {
	Agent string   `yaml:"agent" json:"agent"`
	Paths []string `yaml:"paths" json:"paths"`
	Mode  string   `yaml:"mode" json:"mode"`
	When  *When    `yaml:"when,omitempty" json:"when,omitempty"`
}

// When narrows a gate beyond its paths. The gate applies when the matching
// changes reach MinChangedLines, or when any changed file matches OrTouches.
type When struct {
	MinChangedLines int      `yaml:"min_changed_lines" json:"min_changed_lines"`
	OrTouches       []string `yaml:"or_touches" json:"or_touches"`
}

// Thresholds turn severities into outcomes.
type Thresholds struct {
	Block                []string `yaml:"block" json:"block"`
	Warn                 []string `yaml:"warn" json:"warn"`
	MinConfidenceToBlock float64  `yaml:"min_confidence_to_block" json:"min_confidence_to_block"`
}

// LoadConfig reads and validates a gates.yaml file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// ParseConfig decodes gates.yaml strictly (unknown keys are errors) and
// validates it.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if c.Spec == "" {
		c.Spec, c.specDefaulted = DefaultSpec, true
	}
	if c.Thresholds.Block == nil && c.Thresholds.Warn == nil {
		c.Thresholds.Block = []string{"critical", "high"}
		c.Thresholds.Warn = []string{"medium", "low"}
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	if len(c.Gates) == 0 {
		return fmt.Errorf("no gates")
	}
	seen := map[string]bool{}
	for i, g := range c.Gates {
		switch {
		case g.Agent == "":
			return fmt.Errorf("gates[%d]: agent is required", i)
		case seen[g.Agent]:
			return fmt.Errorf("gates[%d]: agent %q listed twice", i, g.Agent)
		case len(g.Paths) == 0:
			return fmt.Errorf("gates[%d] (%s): paths is required", i, g.Agent)
		case g.Mode != "gate" && g.Mode != "advisory":
			return fmt.Errorf("gates[%d] (%s): mode must be gate or advisory, got %q", i, g.Agent, g.Mode)
		}
		seen[g.Agent] = true
		for _, p := range append(slices.Clone(g.Paths), whenTouches(g.When)...) {
			if err := checkPattern(p); err != nil {
				return fmt.Errorf("gates[%d] (%s): %w", i, g.Agent, err)
			}
		}
		if !hasPositivePattern(g.Paths) {
			return fmt.Errorf("gates[%d] (%s): paths must contain at least one positive pattern", i, g.Agent)
		}
		if g.When != nil && len(g.When.OrTouches) > 0 && !hasPositivePattern(g.When.OrTouches) {
			return fmt.Errorf("gates[%d] (%s): when.or_touches must contain at least one positive pattern", i, g.Agent)
		}
	}
	for _, s := range append(append(slices.Clone(c.Thresholds.Block), c.Thresholds.Warn...), c.PublicRedaction...) {
		if !slices.Contains(Severities, s) {
			return fmt.Errorf("unknown severity %q", s)
		}
	}
	for _, s := range c.Thresholds.Block {
		if slices.Contains(c.Thresholds.Warn, s) {
			return fmt.Errorf("severity %q is in both block and warn", s)
		}
	}
	if m := c.Thresholds.MinConfidenceToBlock; m < 0 || m > 1 {
		return fmt.Errorf("min_confidence_to_block must be in [0, 1], got %v", m)
	}
	return nil
}

func whenTouches(w *When) []string {
	if w == nil {
		return nil
	}
	return w.OrTouches
}

// ResolveSpec returns the spec entry point the plan names: the configured
// spec when exists reports it present at head, LegacySpec when gates.yaml
// named none and only the root file exists, and NoSpec otherwise.
func (c *Config) ResolveSpec(exists func(path string) bool) string {
	if exists(c.Spec) {
		return c.Spec
	}
	if c.specDefaulted && exists(LegacySpec) {
		return LegacySpec
	}
	return NoSpec
}

// gateFor returns the configured gate for an agent.
func (c *Config) gateFor(agent string) (Gate, bool) {
	for _, g := range c.Gates {
		if g.Agent == agent {
			return g, true
		}
	}
	return Gate{}, false
}
