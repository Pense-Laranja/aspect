package gate

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Pense-Laranja/aspect/internal/lang"
	"github.com/Pense-Laranja/aspect/internal/spec"
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
// its intent, goals, owned entities, the invariants that must be preserved
// and the module's own constraints, and where its brief is.
type ModuleContext struct {
	Name        string           `json:"name"`
	Intent      string           `json:"intent,omitempty"`
	Goals       []string         `json:"goals,omitempty"`
	Entities    []string         `json:"entities,omitempty"`
	Invariants  []spec.Invariant `json:"invariants,omitempty"`
	Constraints []string         `json:"constraints,omitempty"`
	// Brief is the module's brief, relative to the repository.
	Brief string `json:"brief,omitempty"`
	// Topics are the module's topics the changes touch (a changed file
	// matches one of a topic's patterns, or is the topic's brief or the
	// module's fragment), with their rules: they bind the changed files as
	// the module's own do.
	Topics []TopicContext `json:"topics,omitempty"`
	Files  []string       `json:"files,omitempty"`
}

// TopicContext is one touched topic of a module (spec.Topic) and the rules
// it holds.
type TopicContext struct {
	Name        string           `json:"name"`
	Files       []string         `json:"files"`
	Constraints []string         `json:"constraints,omitempty"`
	Invariants  []spec.Invariant `json:"invariants,omitempty"`
	// Brief is the topic's brief, relative to the repository.
	Brief string `json:"brief,omitempty"`
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
	// SpecNote says why the reviewers were shown less of the spec than it
	// holds (it did not load, or names files not committed at head); the
	// rendered result repeats it, so a review that did not see the rules is
	// never taken for one that did. Set by the review engine.
	SpecNote string `json:"spec_note,omitempty"`
	// CarriedFrom names the run whose verdict this plan carries: one that
	// judged an earlier head of the same branch, whose diff against its merge
	// base is byte for byte this one's and whose reviewers were given the
	// same definitions and spec. No agent ran on this head; the rendered
	// result says so. Set by the review engine.
	CarriedFrom *Carried `json:"carried_from,omitempty"`
}

// Carried is the run a plan's verdict was carried from, and the range it
// judged. Its reports are carried as they were, so they name that range.
type Carried struct {
	Run  string `json:"run"`
	Base string `json:"base"`
	Head string `json:"head"`
}

// reviews says whether a report of base...head belongs to the plan: it
// reviews the plan's range, or, for a carried plan, the range of the run it
// was carried from (whose diff is the plan's, byte for byte).
func (p *Plan) reviews(base, head string) bool {
	if SameRev(base, p.Base) && SameRev(head, p.Head) {
		return true
	}
	c := p.CarriedFrom
	if c == nil {
		return false
	}
	// SameRev takes an empty revision for any revision; a carried range must
	// be named in full on both sides, or a report naming none would pass.
	named := c.Base != "" && c.Head != "" && base != "" && head != ""
	return named && SameRev(base, c.Base) && SameRev(head, c.Head)
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
// matches its paths and its When condition, if any, holds. specPath is the
// entry point Config.ResolveSpec chose, or NoSpec, relative to repo (the
// repository the changes' paths are relative to; "" is the working
// directory). The spec is read from repo's working tree.
func BuildPlan(c *Config, base, head string, changes []Change, repo, specPath string) *Plan {
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
		if sp, err := spec.Load(filepath.Join(repo, filepath.FromSlash(specPath))); err == nil && sp != nil {
			p.Modules = moduleContexts(sp, repo, changes)
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

// moduleContexts lists the modules the changes touch, in spec order. A file
// belongs to a module when the module owns it as code (see lang.ModuleOf),
// when one of the module's topics covers it, or when it is the module's
// brief, a topic's brief or the fragment the module is written in: a change
// to the rules a module is judged by is a change to the module. The
// root spec file is no module's: it holds the system and, in a single-file
// spec, every module, so any edit to it would touch them all.
func moduleContexts(sp *spec.Spec, repo string, changes []Change) []ModuleContext {
	repoAbs, _ := filepath.Abs(repo)
	rel := func(file string) string {
		if file == "" {
			return ""
		}
		abs, err := filepath.Abs(file)
		if err != nil {
			return ""
		}
		if !spec.Inside(repoAbs, abs) {
			return ""
		}
		r, _ := filepath.Rel(repoAbs, abs)
		return filepath.ToSlash(r)
	}
	owner := map[string]string{}
	for _, ch := range changes {
		if _, m := lang.ModuleOf(sp, ch.Path); m != nil {
			owner[ch.Path] = m.Name
		}
	}
	var out []ModuleContext
	for _, m := range sp.AllModules() {
		brief := ""
		if m.Brief.Path != "" {
			brief = rel(filepath.Join(sp.Dir, filepath.FromSlash(m.Brief.Path)))
		}
		specFile := rel(sp.Sources.ModuleFile(m.Name))
		if specFile == rel(sp.Path) {
			specFile = ""
		}
		topicBriefs := map[string]bool{}
		for j := range m.Topics {
			if b := sp.TopicBrief(&m.Topics[j]); b != "" {
				topicBriefs[b] = true
			}
			if tf := sp.TopicFragment(m, &m.Topics[j]); tf != "" {
				topicBriefs[tf] = true
			}
		}
		var files []string
		var topics []TopicContext
		for _, ch := range changes {
			switch {
			case owner[ch.Path] == m.Name,
				ch.Path == brief || ch.Path == specFile || topicBriefs[ch.Path],
				owner[ch.Path] == "" && !m.Placed() && legacyMatch(m, ch.Path),
				m.TopicCovers(ch.Path):
				files = append(files, ch.Path)
			}
		}
		if len(files) == 0 {
			continue
		}
		touched, _ := sp.TopicsTouching(m, files)
		for _, t := range touched {
			topics = append(topics, TopicContext{Name: t.Name, Files: t.Files, Constraints: t.Constraints, Invariants: t.Invariants, Brief: sp.TopicBrief(t)})
		}
		out = append(out, ModuleContext{
			Name:        m.Name,
			Intent:      m.Intent,
			Goals:       m.Goals,
			Entities:    m.Entities,
			Invariants:  m.Invariants,
			Constraints: m.Constraints,
			Brief:       brief,
			Topics:      topics,
			Files:       files,
		})
	}
	return out
}

// legacyMatch guesses a file's module for a spec imported before modules
// recorded their directory (path:), where a package directory had become a
// module name: modules/api is the module modules_api. `aspect paths` records
// the real directory; until then this keeps such specs' plans populated. A
// file another module owns is never given to a module by guess.
func legacyMatch(m *spec.Module, filePath string) bool {
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

func hasPositivePattern(patterns []string) bool {
	for _, p := range patterns {
		if p != "" && !strings.HasPrefix(p, "!") {
			return true
		}
	}
	return false
}

func matchAny(patterns []string, name string) bool {
	matched := false
	for _, p := range patterns {
		if neg, ok := strings.CutPrefix(p, "!"); ok {
			if Match(neg, name) {
				return false
			}
			continue
		}
		if Match(p, name) {
			matched = true
		}
	}
	return matched
}

// Match reports whether a slash-separated path matches a glob pattern
// (spec.MatchPath: path.Match per segment, "**" for any number of them).
func Match(pattern, name string) bool { return spec.MatchPath(pattern, name) }

// checkPattern rejects patterns path.Match cannot parse, so a typo in
// gates.yaml fails loudly instead of silently never matching.
func checkPattern(p string) error {
	if neg, ok := strings.CutPrefix(p, "!"); ok {
		if neg == "" {
			return fmt.Errorf("empty negated pattern %q", p)
		}
		return spec.CheckPathPattern(neg)
	}
	return spec.CheckPathPattern(p)
}
