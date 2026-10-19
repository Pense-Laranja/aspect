// Package agents holds the roles in the Aspect pipeline. Each agent is a pure
// function of its inputs and an llm.Client: it renders a prompt, requests a
// structured answer, and returns typed data. Agents never write files or run
// commands; the pipeline does that, so every side effect is traceable.
package agents

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/eideroliveira/aspect/internal/lang"
	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

// Task is the part of the input every agent shares.
type Task struct {
	Spec *spec.Spec
	// Tier the module belongs to (the implicit tier for single-tier specs).
	Tier   *spec.Tier
	Module *spec.Module
	Lang   *lang.Profile
}

func (t Task) system() spec.System { return t.Spec.System }

// tier returns the task's tier, resolving it from the module when unset.
func (t Task) tier() *spec.Tier {
	if t.Tier != nil {
		return t.Tier
	}
	if tier := t.Spec.TierOf(t.Module.Name); tier != nil {
		return tier
	}
	return t.Spec.EffectiveTiers()[0]
}

func (t Task) modulePath() string { return t.tier().ModulePath }

// importPath is where the module's dependants import it from: the tier's
// module path joined with the module's directory.
func (t Task) importPath() string {
	switch t.Module.Path {
	case "":
		return t.modulePath() + "/" + t.Module.Name
	case ".":
		return t.modulePath()
	}
	return t.modulePath() + "/" + t.Module.Path
}

// renderYAML gives the model the spec fragment exactly as the author wrote it.
// YAML is more compact than JSON and matches the file the user is editing, so
// the model's comments about the spec line up with what the user sees.
func renderYAML(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<render error: %v>", err)
	}
	return string(b)
}

// renderContext is the system-level context every agent needs: intent,
// goals, constraints, the language stack, the data model in outline, and the
// interfaces in outline. Module-level detail is rendered by renderModule.
func renderContext(t Task) string {
	s := t.system()
	tier := t.tier()
	var b strings.Builder
	view := struct {
		Name        string        `yaml:"name"`
		Intent      string        `yaml:"intent"`
		Topology    spec.Topology `yaml:"topology"`
		Goals       []spec.Goal   `yaml:"goals"`
		Constraints []string      `yaml:"constraints,omitempty"`
	}{s.Name, s.Intent, t.Spec.EffectiveTopology(), s.Goals, s.Constraints}
	fmt.Fprintf(&b, "# System\n\n```yaml\n%s```\n\n", renderYAML(view))
	if text := strings.TrimSpace(s.Brief.Text); text != "" {
		fmt.Fprintf(&b, "## System brief (%s)\n\n%s\n\n", s.Brief.Path, text)
	}

	if len(s.Dependencies) > 0 {
		type depView struct {
			Name       string   `yaml:"name"`
			Intent     string   `yaml:"intent,omitempty"`
			Spec       string   `yaml:"spec"`
			Interfaces []string `yaml:"interfaces,omitempty"`
		}
		var views []depView
		for _, d := range s.Dependencies {
			v := depView{d.Name, d.Intent, d.Spec, nil}
			if dep, ok := t.Spec.Deps[d.Name]; ok {
				for _, i := range dep.System.Interfaces {
					v.Interfaces = append(v.Interfaces, fmt.Sprintf("%s (%s)", i.Name, i.Kind))
				}
			}
			views = append(views, v)
		}
		fmt.Fprintf(&b, "# System dependencies\n\nOther systems this one consumes interfaces of, referenced as <system>/<interface>.<surface>. They are built and run elsewhere; this system only calls them.\n\n```yaml\n%s```\n\n", renderYAML(views))
	}

	if len(t.Spec.Tiers) > 0 {
		type tierView struct {
			Name      string   `yaml:"name"`
			Intent    string   `yaml:"intent"`
			Language  string   `yaml:"language"`
			DependsOn []string `yaml:"depends_on,omitempty"`
			Modules   []string `yaml:"modules"`
		}
		var views []tierView
		for _, tr := range t.Spec.Tiers {
			v := tierView{tr.Name, tr.Intent, tr.Language, tr.DependsOn, nil}
			for _, m := range tr.Modules {
				v.Modules = append(v.Modules, m.Name)
			}
			views = append(views, v)
		}
		fmt.Fprintf(&b, "# Tiers (this module is in tier %q)\n\nTiers are separate deployables in their own languages; they talk only through interfaces.\n\n```yaml\n%s```\n\n", tier.Name, renderYAML(views))
	}
	fmt.Fprintf(&b, "# This tier\n\nLanguage: %s\nModule path: %s\n\n", t.Lang.DisplayName, tier.ModulePath)
	if text := strings.TrimSpace(tier.Brief.Text); text != "" {
		fmt.Fprintf(&b, "## Tier brief (%s)\n\n%s\n\n", tier.Brief.Path, text)
	}
	if ls := tier.LanguageStack(); ls != nil {
		fmt.Fprintf(&b, "# Stack (%s)\n\n```yaml\n%s```\n\n", t.Lang.DisplayName, renderYAML(ls))
	}
	renderDB := func(title string, db *spec.Database, owner string) {
		type outline struct {
			Name   string `yaml:"name"`
			Intent string `yaml:"intent,omitempty"`
		}
		var ents []outline
		for _, e := range db.Entities {
			ents = append(ents, outline{e.Name, e.Intent})
		}
		view := struct {
			Engine     string      `yaml:"engine"`
			Owner      string      `yaml:"owner,omitempty"`
			Migrations string      `yaml:"migrations"`
			Test       spec.DBTest `yaml:"test"`
			Entities   []outline   `yaml:"entities"`
		}{db.Engine, owner, db.Migrations, db.Test, ents}
		fmt.Fprintf(&b, "# %s (outline; owned entities are detailed below)\n\n```yaml\n%s```\n\n", title, renderYAML(view))
	}
	if db := s.Database; db != nil {
		owner := db.Tier
		if owner == spec.External {
			owner = "external (a hosted service; this spec generates no code for it)"
		}
		renderDB("System database", db, owner)
	}
	if tier.Database != nil {
		renderDB("Local database of tier "+tier.Name, tier.Database, tier.Name)
	}
	if len(s.Interfaces) > 0 {
		type outline struct {
			Name      string   `yaml:"name"`
			Kind      string   `yaml:"kind"`
			Intent    string   `yaml:"intent"`
			Provider  string   `yaml:"provider,omitempty"`
			Service   string   `yaml:"service,omitempty"`
			Framework string   `yaml:"framework,omitempty"`
			Surfaces  []string `yaml:"surfaces"`
		}
		var ifs []outline
		for _, i := range s.Interfaces {
			o := outline{i.Name, i.Kind, i.Intent, i.Provider, i.Service, i.Framework, nil}
			for _, sf := range i.Surfaces {
				o.Surfaces = append(o.Surfaces, sf.Name)
			}
			ifs = append(ifs, o)
		}
		fmt.Fprintf(&b, "# Interfaces (outline; surfaces this module implements or consumes are detailed below)\n\n```yaml\n%s```\n\n", renderYAML(ifs))
	}
	return b.String()
}

// renderModule renders the module spec plus the full definitions of the
// entities it owns, the surfaces it implements, and the frameworks those
// surfaces use.
func renderModule(t Task, heading string) string {
	m := t.Module
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nModule: %s\nImport path: %s\nCode directory: %s\nTest directory: %s\n\n```yaml\n%s```\n\n",
		heading, m.Name, t.importPath(), lang.DirLabel(t.Lang.CodeDir(m)), lang.DirLabel(t.Lang.TestDir(m)), renderYAML(m))
	if text := strings.TrimSpace(m.Brief.Text); text != "" {
		fmt.Fprintf(&b, "## Module brief (%s)\n\nThe owner's own description of this module. It elaborates the spec above and is binding where it is more specific; where the two conflict, say so in concerns rather than choosing silently.\n\n%s\n\n", m.Brief.Path, text)
	}

	if len(m.Entities) > 0 {
		var ents []spec.Entity
		for _, name := range m.Entities {
			if e := t.Spec.Entity(name); e != nil {
				ents = append(ents, *e)
			}
		}
		fmt.Fprintf(&b, "## Entities owned by this module\n\nThis module defines these persistent types and is the only writer of them.\n\n```yaml\n%s```\n\n", renderYAML(ents))
	}
	type surfaceView struct {
		System    string       `yaml:"system,omitempty"`
		Interface string       `yaml:"interface"`
		Kind      string       `yaml:"kind"`
		Provider  string       `yaml:"provider,omitempty"`
		Service   string       `yaml:"service,omitempty"`
		Framework string       `yaml:"framework,omitempty"`
		Auth      string       `yaml:"interface_auth,omitempty"`
		Surface   spec.Surface `yaml:"surface"`
	}
	if refs, _ := t.Spec.ResolveSurfaces(m.Surfaces); len(refs) > 0 {
		var views []surfaceView
		frameworks := map[string]bool{}
		for _, r := range refs {
			views = append(views, surfaceView{"", r.Interface.Name, r.Interface.Kind, r.Interface.Provider, r.Interface.Service, r.Interface.Framework, r.Interface.Auth, *r.Surface})
			if r.Interface.Framework != "" {
				frameworks[r.Interface.Framework] = true
			}
		}
		fmt.Fprintf(&b, "## Surfaces this module implements\n\n```yaml\n%s```\n\n", renderYAML(views))
		if ls := t.tier().LanguageStack(); ls != nil {
			for _, f := range ls.Frameworks {
				if frameworks[f.Name] && f.Guidance != "" {
					fmt.Fprintf(&b, "### How to use %s here\n\n%s\n\n", f.Name, strings.TrimSpace(f.Guidance))
				}
			}
		}
	}
	if refs, _ := t.Spec.ResolveSurfaces(m.Consumes); len(refs) > 0 {
		var views []surfaceView
		for _, r := range refs {
			views = append(views, surfaceView{r.System, r.Interface.Name, r.Interface.Kind, r.Interface.Provider, r.Interface.Service, "", r.Interface.Auth, *r.Surface})
		}
		fmt.Fprintf(&b, "## Surfaces this module consumes\n\nThis module is a client of these surfaces. They are served by another tier, by another system (see system), or by an external service, never by code in this tier: implement a client against the contract below, take the base address from configuration, and represent every listed error.\n\n```yaml\n%s```\n\n", renderYAML(views))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n || n < 16 {
		return s
	}
	// Failing output puts the verdict at the end (FAIL lines, the last
	// panic), while the start says what was being compiled or run, so the
	// tail gets most of the budget and both ends are kept.
	head := s[:n/3]
	tail := s[len(s)-(n-n/3):]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return fmt.Sprintf("%s\n…(%d bytes omitted)…\n%s", head, len(s)-len(head)-len(tail), tail)
}

// sectionBudget bounds one rendered group of files. A module with many or
// large dependencies must not silently overflow the context; what is left
// out is named so the model knows its view is partial.
const sectionBudget = 300000

// renderFiles renders a titled group of files within sectionBudget, dropping
// the largest files first so the most files survive, and listing what it
// omitted.
func renderFiles(title string, files []workspace.File) string {
	return renderFilesWithin(title, files, sectionBudget)
}

func renderFilesWithin(title string, files []workspace.File, budget int) string {
	if len(files) == 0 {
		return ""
	}
	show, omitted := fitFiles(files, budget)
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", title)
	for _, f := range show {
		fmt.Fprintf(&b, "### %s\n```\n%s\n```\n\n", f.Path, strings.TrimRight(f.Content, "\n"))
	}
	if len(omitted) > 0 {
		size := 0
		var paths []string
		for _, f := range omitted {
			size += len(f.Content)
			paths = append(paths, f.Path)
		}
		fmt.Fprintf(&b, "### Omitted to fit the prompt budget\n\n%d files (%d bytes) are not shown. Their identifiers may still be referenced by the files above:\n%s\n\n", len(omitted), size, "- "+strings.Join(paths, "\n- "))
	}
	return b.String()
}

// fitFiles splits files into the ones that fit the budget, in their original
// order, and the ones that do not, largest first.
func fitFiles(files []workspace.File, budget int) (show, omitted []workspace.File) {
	total := 0
	for _, f := range files {
		total += len(f.Content)
	}
	if total <= budget {
		return files, nil
	}
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return len(files[idx[a]].Content) > len(files[idx[b]].Content) })
	drop := map[int]bool{}
	for _, i := range idx {
		if total <= budget {
			break
		}
		drop[i] = true
		total -= len(files[i].Content)
	}
	for i, f := range files {
		if drop[i] {
			omitted = append(omitted, f)
		} else {
			show = append(show, f)
		}
	}
	return show, omitted
}

// filesSchema is the JSON schema fragment for a list of files.
func filesSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"path", "content"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "Path relative to the project root, inside the module's directory"},
				"content": map[string]any{"type": "string", "description": "Complete file content"},
			},
		},
	}
}

func stringList() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}
