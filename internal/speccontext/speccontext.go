// Package speccontext renders what a spec says about some of its modules as
// Markdown, for a reader who has not opened the spec: a reviewer judging a
// change to them, or a coding assistant about to edit them. It is the one
// rendering `aspect review` puts in a reviewer's prompt and `aspect context`
// prints, so a person can see exactly what the reviewers were told.
//
// A spec can hold far more than a prompt should carry (a module's brief may
// run to tens of thousands of words), so the rendering fits a budget, in
// order of what binds most: the system's constraints, then each module's
// intent, goals, constraints and invariants, then its operations and
// scenarios, then its brief. What does not fit is named at the end, never
// dropped silently.
package speccontext

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eideroliveira/aspect/internal/spec"
)

// CharsPerToken is the estimate the budget is counted in, the same one the
// review engine splits diffs by.
const CharsPerToken = 4

// Options shape a rendering.
type Options struct {
	// Budget is the most tokens the rendering may take (estimated at
	// CharsPerToken); 0 is no limit.
	Budget int
	// System adds the system's constraints, which bind every module.
	System bool
	// Briefs adds each module's brief.
	Briefs bool
}

// part is one piece the budget decides on.
type part struct {
	module int // index into the modules rendered; -1 for the system
	kind   kind
	label  string // names the part in the "left out" note
	text   string
	// truncatable parts are cut to fit rather than left out: a module's
	// rules are worth more cut than missing.
	truncatable bool
}

type kind int

const (
	kindSystem kind = iota
	kindCore
	kindOperations
	kindScenarios
	kindBrief
)

// Render renders the named modules of s, in the order given; names the spec
// does not have are skipped. It returns "" when there is nothing to say.
func Render(s *spec.Spec, modules []string, opt Options) string {
	var mods []*spec.Module
	seen := map[string]bool{}
	for _, name := range modules {
		if m := s.Module(name); m != nil && !seen[name] {
			mods = append(mods, m)
			seen[name] = true
		}
	}
	var parts []part
	if opt.System && len(s.System.Constraints) > 0 {
		var b strings.Builder
		b.WriteString("## System constraints (they bind every module)\n\n")
		for _, c := range s.System.Constraints {
			fmt.Fprintf(&b, "- %s\n", oneLine(c))
		}
		parts = append(parts, part{module: -1, kind: kindSystem, label: "system constraints", text: b.String(), truncatable: true})
	}
	for i, m := range mods {
		parts = append(parts, part{module: i, kind: kindCore, label: "the rules of " + m.Name, text: core(s, m), truncatable: true})
	}
	for i, m := range mods {
		if t := operations(m); t != "" {
			parts = append(parts, part{module: i, kind: kindOperations, label: "operations of " + m.Name, text: t})
		}
		if t := scenarios(m); t != "" {
			parts = append(parts, part{module: i, kind: kindScenarios, label: "scenarios of " + m.Name, text: t})
		}
	}
	if opt.Briefs {
		for i, m := range mods {
			if text := strings.TrimSpace(m.Brief.Text); text != "" {
				where := briefPath(s, m)
				parts = append(parts, part{module: i, kind: kindBrief, label: "brief of " + m.Name + " (" + where + ")",
					text: fmt.Sprintf("### Brief (%s)\n\nThe owner's longer description of the module; binding where it is more specific than the rules above.\n\n%s\n", where, text)})
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}

	kept, omitted := fit(parts, opt.Budget)
	var b strings.Builder
	for _, p := range kept {
		if p.module == -1 {
			b.WriteString(p.text + "\n")
		}
	}
	for i := range mods {
		for _, p := range kept {
			if p.module == i {
				b.WriteString(p.text + "\n")
			}
		}
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "Left out to fit the size limit: %s. Do not report something as unspecified only because it is not shown here.\n", strings.Join(omitted, "; "))
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// fit keeps parts in order while they fit the budget; a truncatable part
// that does not fit is cut to the room left (when any is left), any other
// is left out, and so is everything of a module whose rules were left out:
// its scenarios mean little without them. The note on what was left out is
// counted against the budget too, roughly.
func fit(parts []part, budget int) (kept []part, omitted []string) {
	if budget <= 0 {
		return parts, nil
	}
	room := budget*CharsPerToken - 200 // the "left out" note
	dropped := map[int]bool{}
	for _, p := range parts {
		switch {
		case p.kind != kindCore && p.kind != kindSystem && dropped[p.module]:
			omitted = append(omitted, p.label)
		case len(p.text) <= room:
			kept = append(kept, p)
			room -= len(p.text)
		case p.truncatable && room > 160:
			cut := p.text[:runeStart(p.text, room-60)]
			p.text = cut + "\n[cut to fit the size limit]\n"
			kept = append(kept, p)
			room = 0
			omitted = append(omitted, "the rest of "+p.label)
		default:
			omitted = append(omitted, p.label)
			if p.kind == kindCore {
				dropped[p.module] = true
			}
		}
	}
	return kept, omitted
}

// runeStart backs n off to the start of a UTF-8 sequence in s.
func runeStart(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return n
}

// core is what binds a module: why it exists, the goals it answers for, and
// its constraints and invariants.
func core(s *spec.Spec, m *spec.Module) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Module %s", m.Name)
	if m.Path != "" {
		fmt.Fprintf(&b, " (%s)", m.Path)
	}
	fmt.Fprintf(&b, "\n\nIntent: %s\n", oneLine(m.Intent))
	if len(m.Goals) > 0 {
		b.WriteString("\nGoals it answers for:\n")
		for _, id := range m.Goals {
			if g := s.Goal(id); g != nil {
				fmt.Fprintf(&b, "- %s: %s\n", id, oneLine(g.Statement))
			} else {
				fmt.Fprintf(&b, "- %s\n", id)
			}
		}
	}
	if len(m.Entities) > 0 {
		fmt.Fprintf(&b, "\nEntities it owns (the only module that writes them): %s\n", strings.Join(m.Entities, ", "))
	}
	if len(m.Constraints) > 0 {
		b.WriteString("\nConstraints:\n")
		for _, c := range m.Constraints {
			fmt.Fprintf(&b, "- %s\n", oneLine(c))
		}
	}
	if len(m.Invariants) > 0 {
		b.WriteString("\nInvariants (cite as " + m.Name + ".invariants[i], or the id):\n")
		for i, inv := range m.Invariants {
			id := inv.ID
			if id == "" {
				id = fmt.Sprintf("%d", i)
			}
			fmt.Fprintf(&b, "- [%s] %s\n", id, oneLine(inv.Statement))
		}
	}
	return b.String()
}

func operations(m *spec.Module) string {
	if len(m.Interface) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Operations\n\n")
	for _, op := range m.Interface {
		fmt.Fprintf(&b, "- `%s`", op.Signature)
		if op.Intent != "" {
			fmt.Fprintf(&b, ": %s", oneLine(op.Intent))
		}
		b.WriteString("\n")
		for _, c := range op.Pre {
			fmt.Fprintf(&b, "  - pre: %s\n", oneLine(c))
		}
		for _, c := range op.Post {
			fmt.Fprintf(&b, "  - post: %s\n", oneLine(c))
		}
	}
	return b.String()
}

func scenarios(m *spec.Module) string {
	if len(m.Scenarios) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Scenarios\n\n")
	for _, sc := range m.Scenarios {
		fmt.Fprintf(&b, "- %s:", sc.ID)
		if sc.Given != "" {
			fmt.Fprintf(&b, " given %s;", oneLine(sc.Given))
		}
		fmt.Fprintf(&b, " when %s; then %s\n", oneLine(sc.When), oneLine(sc.Then))
	}
	return b.String()
}

// briefPath shows where a brief is: relative to the repository when the spec
// knows it, else as written.
func briefPath(s *spec.Spec, m *spec.Module) string {
	if s.Root == "" || s.Dir == "" {
		return m.Brief.Path
	}
	abs, err := filepath.Abs(filepath.Join(s.Dir, filepath.FromSlash(m.Brief.Path)))
	if err != nil {
		return m.Brief.Path
	}
	rel, err := filepath.Rel(s.Root, abs)
	if err != nil {
		return m.Brief.Path
	}
	return filepath.ToSlash(rel)
}

// oneLine folds a YAML block scalar's line breaks so a rule stays one list
// item.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
