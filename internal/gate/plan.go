package gate

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/eideroliveira/aspect/internal/spec"
)

// PlanSchema identifies a plan document.
const PlanSchema = "aspect-gate-plan/v1"

// NoSpec is the plan's spec when no spec exists at head (see
// Config.ResolveSpec). Agents receive it in the change block and do not look for or report
// the missing spec; the rendered result states it once.
const NoSpec = "none"

// Change is one file a diff touched, with its line counts. Binary files
// count zero lines.
type Change struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// ModuleContext carries the spec metadata for one module touched by the changes:
// its intent, goals, owned entities, and invariants that must be preserved.
type ModuleContext struct {
	Name       string           `json:"name"`
	Intent     string           `json:"intent,omitempty"`
	Goals      []string         `json:"goals,omitempty"`
	Entities   []string         `json:"entities,omitempty"`
	Invariants []spec.Invariant `json:"invariants,omitempty"`
	Files      []string         `json:"files,omitempty"`
}

// Plan is the list of agents that apply to a change. It is written next to
// the reports so Check knows which reports must exist.
type Plan struct {
	Schema       string          `json:"schema"`
	Spec         string          `json:"spec"`
	Base         string          `json:"base"`
	Head         string          `json:"head"`
	ChangedFiles int             `json:"changed_files"`
	ChangedLines int             `json:"changed_lines"`
	Agents       []Planned       `json:"agents"`
	Modules      []ModuleContext `json:"modules,omitempty"`
}

// Planned is one agent the orchestrator must run.
type Planned struct {
	Agent   string   `json:"agent"`
	Mode    string   `json:"mode"`
	Reason  string   `json:"reason"`
	Files   []string `json:"files"`
	Modules []string `json:"modules,omitempty"`
}

// BuildPlan decides which gates apply to the changes, in the order
// gates.yaml lists them. A gate applies when at least one changed file
// matches its paths and its When condition, if any, holds. spec is the
// entry point Config.ResolveSpec chose, or NoSpec.
func BuildPlan(c *Config, base, head string, changes []Change, specPath string) *Plan {
	p := &Plan{Schema: PlanSchema, Spec: specPath, Base: base, Head: head, ChangedFiles: len(changes), Agents: []Planned{}}
	for _, ch := range changes {
		p.ChangedLines += ch.Added + ch.Deleted
	}
	for _, g := range c.Gates {
		var files []string
		lines := 0
		for _, ch := range changes {
			if matchAny(g.Paths, ch.Path) {
				files = append(files, ch.Path)
				lines += ch.Added + ch.Deleted
			}
		}
		if len(files) == 0 {
			continue
		}
		reason := fmt.Sprintf("%d changed file(s) match its paths", len(files))
		if w := g.When; w != nil {
			var touched []string
			for _, f := range files {
				if matchAny(w.OrTouches, f) {
					touched = append(touched, f)
				}
			}
			switch {
			case len(touched) > 0:
				reason = "touches " + strings.Join(touched, ", ")
			case w.MinChangedLines > 0 && lines >= w.MinChangedLines:
				reason = fmt.Sprintf("%d changed lines in matching files (threshold %d)", lines, w.MinChangedLines)
			default:
				continue
			}
		}
		p.Agents = append(p.Agents, Planned{Agent: g.Agent, Mode: g.Mode, Reason: reason, Files: files})
	}

	if specPath != NoSpec && specPath != "" {
		if sp, err := spec.Load(specPath); err == nil && sp != nil {
			for _, m := range sp.AllModules() {
				var modFiles []string
				for _, ch := range changes {
					if matchModule(m, ch.Path) {
						modFiles = append(modFiles, ch.Path)
					}
				}
				if len(modFiles) > 0 {
					p.Modules = append(p.Modules, ModuleContext{
						Name:       m.Name,
						Intent:     m.Intent,
						Goals:      m.Goals,
						Entities:   m.Entities,
						Invariants: m.Invariants,
						Files:      modFiles,
					})
				}
			}
			for i := range p.Agents {
				var agentMods []string
				for _, mc := range p.Modules {
					if hasIntersection(mc.Files, p.Agents[i].Files) {
						agentMods = append(agentMods, mc.Name)
					}
				}
				p.Agents[i].Modules = agentMods
			}
		}
	}

	return p
}

func matchModule(m *spec.Module, filePath string) bool {
	dir1 := m.Name + "/"
	dir2 := strings.ReplaceAll(m.Name, "_", "/") + "/"
	base := path.Base(filePath)
	ext := path.Ext(filePath)
	stem := strings.TrimSuffix(base, ext)

	if strings.HasPrefix(filePath, dir1) || strings.HasPrefix(filePath, dir2) {
		return true
	}
	if stem == m.Name || stem == strings.ReplaceAll(m.Name, "_", "/") {
		return true
	}
	if filePath == "_aspect/modules/"+m.Name+".yaml" || filePath == "_aspect/briefs/"+m.Name+".md" {
		return true
	}
	return false
}

func hasIntersection(a, b []string) bool {
	set := make(map[string]bool, len(a))
	for _, x := range a {
		set[x] = true
	}
	for _, y := range b {
		if set[y] {
			return true
		}
	}
	return false
}

// Agent reports whether the plan includes the named agent.
func (p *Plan) Agent(name string) (Planned, bool) {
	i := slices.IndexFunc(p.Agents, func(a Planned) bool { return a.Agent == name })
	if i < 0 {
		return Planned{}, false
	}
	return p.Agents[i], true
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}

// Match reports whether a slash-separated path matches a glob pattern.
// Segments follow path.Match; a segment that is exactly "**" matches zero or
// more whole segments, so "**/*.go" matches "main.go" and "a/b/c.go", and
// ".github/**" matches everything under .github.
func Match(pattern, name string) bool {
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

// checkPattern rejects patterns path.Match cannot parse, so a typo in
// gates.yaml fails loudly instead of silently never matching.
func checkPattern(p string) error {
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
