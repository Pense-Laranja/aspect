package spec

import (
	"fmt"
	"path"
	"path/filepath"
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

// TopicCovers reports whether one of m's topics covers file.
func (m *Module) TopicCovers(file string) bool {
	for j := range m.Topics {
		if m.Topics[j].Covers(file) {
			return true
		}
	}
	return false
}

// TopicBrief is t's brief relative to the repository root, slash separated
// (as written when the spec has no root), or "" when t has none.
func (s *Spec) TopicBrief(t *Topic) string {
	if t.Brief.Path == "" {
		return ""
	}
	if s.Root == "" || s.Dir == "" {
		return t.Brief.Path
	}
	return s.repoRel(filepath.Join(s.Dir, filepath.FromSlash(t.Brief.Path)))
}

// TopicFragment is the YAML fragment t of m is written in, relative to the
// repository root, or "" when t is written in the root spec file or the spec
// has no root.
func (s *Spec) TopicFragment(m *Module, t *Topic) string {
	if s.Root == "" {
		return ""
	}
	f := s.Sources.TopicFile(m.Name, t.Name)
	if f == "" {
		f = s.Sources.ModuleFile(m.Name)
	}
	if f == "" {
		return ""
	}
	if r := s.repoRel(f); r != s.repoRel(s.Path) {
		return r
	}
	return ""
}

// TopicsTouching splits m's topics into those files touch and the rest, in
// order. files are relative to the repository root, as a diff names them. A
// topic is touched directly by a file its patterns match, by its own brief,
// or by a dedicated YAML fragment that defines the topic outside the module's
// own fragment. When no topic of m is touched directly, a change to the
// module's YAML fragment touches all of its topics (so a spec-only edit still
// shows the module's topics), whereas a change that already touches specific
// topic files or briefs alongside the module's fragment shows only the
// directly touched topics instead of flooding the prompt with every topic.
// The root spec file touches none, since it holds the system and, in a
// single-file spec, every module.
func (s *Spec) TopicsTouching(m *Module, files []string) (touched, rest []*Topic) {
	modFrag := ""
	if f := s.Sources.ModuleFile(m.Name); f != "" && s.Root != "" {
		if r := s.repoRel(f); r != s.repoRel(s.Path) {
			modFrag = r
		}
	}
	direct := make([]bool, len(m.Topics))
	fragHit := make([]bool, len(m.Topics))
	anyDirect := false
	for j := range m.Topics {
		t := &m.Topics[j]
		brief := s.TopicBrief(t)
		topicFrag := s.TopicFragment(m, t)
		for _, f := range files {
			if t.Covers(f) || (brief != "" && f == brief) || (topicFrag != "" && topicFrag != modFrag && f == topicFrag) {
				direct[j] = true
				anyDirect = true
				break
			}
			if (topicFrag != "" && f == topicFrag) || (modFrag != "" && f == modFrag) {
				fragHit[j] = true
			}
		}
	}
	for j := range m.Topics {
		t := &m.Topics[j]
		if direct[j] || (!anyDirect && fragHit[j]) {
			touched = append(touched, t)
		} else {
			rest = append(rest, t)
		}
	}
	return touched, rest
}

// repoRel is file (absolute, or relative to the working directory) relative
// to the repository root, slash separated, or "" when it is not inside it.
func (s *Spec) repoRel(file string) string {
	if s.Root == "" || file == "" {
		return ""
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(file)
	if err != nil || !Inside(root, abs) {
		return ""
	}
	r, err := filepath.Rel(root, abs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(r)
}
