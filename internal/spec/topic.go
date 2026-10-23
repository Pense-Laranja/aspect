package spec

import (
	"fmt"
	"path"
	"strings"
)

// Topic is a part of a module's rules that binds only some files: the
// constraints, invariants and brief that apply where its file patterns
// match. A module whose rules outgrow one reviewer's prompt splits them
// into topics, and `aspect review` shows a reviewer the module's own rules
// plus the topics whose files the change touches, instead of every rule cut
// to fit. The pipeline's agents, which build the whole module, see every
// topic.
type Topic struct {
	// Name identifies the topic within its module: lower snake_case, unique
	// in the module.
	Name string `yaml:"name" json:"name"`
	// Intent says in a line what the topic's rules are about.
	Intent string `yaml:"intent,omitempty" json:"intent,omitempty"`
	// Files are the glob patterns of the files the topic's rules bind,
	// relative to the repository root (the paths a diff names), in the
	// gates' syntax (MatchPath). They may name files outside the module's
	// own directory: a change to such a file is a change to the module.
	Files       []string    `yaml:"files" json:"files"`
	Constraints []string    `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	Invariants  []Invariant `yaml:"invariants,omitempty" json:"invariants,omitempty"`
	Brief       `yaml:",inline" json:",inline"`
}

// Covers reports whether file, a slash-separated path relative to the
// repository root, matches one of the topic's patterns.
func (t *Topic) Covers(file string) bool {
	for _, p := range t.Files {
		if MatchPath(p, file) {
			return true
		}
	}
	return false
}

// CoversAny reports whether the topic covers any of files.
func (t *Topic) CoversAny(files []string) bool {
	for _, f := range files {
		if t.Covers(f) {
			return true
		}
	}
	return false
}

// MatchPath reports whether a slash-separated path matches a glob pattern.
// Segments follow path.Match; a segment that is exactly "**" matches zero or
// more whole segments, so "**/*.go" matches "main.go" and "a/b/c.go", and
// ".github/**" matches everything under .github.
func MatchPath(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// CheckPathPattern rejects patterns path.Match cannot parse, so a typo fails
// loudly instead of silently never matching.
func CheckPathPattern(p string) error {
	if p == "" {
		return fmt.Errorf("empty path pattern")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("bad path pattern %q: %w", p, err)
		}
	}
	return nil
}
