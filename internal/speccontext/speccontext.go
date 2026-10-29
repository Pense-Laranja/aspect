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
//
// A module may split its rules into topics, each binding only the files its
// patterns match (spec.Topic). Given the files a reader is about (a
// reviewer's share of a diff), the rendering shows the topics that cover
// them, after the module's own rules and brief in each rank, and names the
// others in a line, so a module whose rules outgrow the budget is shown the
// ones that bind the change rather than all of them cut short.
package speccontext

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/Pense-Laranja/aspect/internal/spec"
)

// CharsPerToken is the estimate the budget is counted in, and the review
// engine's (internal/review's charsPerToken is defined from it): code and
// diffs average 3.5-4 characters per token, and 3 errs toward fitting.
const CharsPerToken = 3

const (
	// omittedNoteReserve is kept for the closing "Left out ..." note, which
	// is shortened to fit it.
	omittedNoteReserve = 240
	// cutMarkerReserve is room for the "[cut to fit the size limit]" line.
	cutMarkerReserve = 60
	// minCutRoom: below this much room a part is left out rather than cut,
	// since a few words of it would say nothing.
	minCutRoom = 160
)

// Options shape a rendering. The zero value renders everything.
type Options struct {
	// Budget is the most tokens the rendering may take (estimated at
	// CharsPerToken); 0 is no limit.
	Budget int
	// OmitSystem leaves out the system's constraints, which bind every
	// module.
	OmitSystem bool
	// OmitBriefs leaves out the modules' briefs and their topics' briefs.
	OmitBriefs bool
	// Files are the files the reader is about, relative to the repository
	// root: of a module's topics only those they touch are shown
	// (spec.Spec.TopicsTouching), the others named.
	Files []string
	// Topics are explicit topic names (`<topic>` or `<module>:<topic>`) to
	// show alongside any topics touched by Files.
	Topics []string
	// AllTopics shows every topic whatever Files holds, for a reader about
	// a whole module.
	AllTopics bool
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
	kindTopic
	kindOperations
	kindScenarios
	kindBrief
	kindTopicBrief
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
	if !opt.OmitSystem && len(s.System.Constraints) > 0 {
		var b strings.Builder
		b.WriteString("## System constraints (they bind every module)\n\n")
		for _, c := range s.System.Constraints {
			fmt.Fprintf(&b, "- %s\n", oneLine(c))
		}
		parts = append(parts, part{module: -1, kind: kindSystem, label: "system constraints", text: b.String(), truncatable: true})
	}
	shown := make([][]*spec.Topic, len(mods))
	for i, m := range mods {
		var hidden []*spec.Topic
		if opt.AllTopics {
			for j := range m.Topics {
				shown[i] = append(shown[i], &m.Topics[j])
			}
		} else if len(opt.Topics) > 0 {
			touched, _ := s.TopicsTouching(m, opt.Files)
			touchedSet := make(map[string]bool, len(touched))
			for _, t := range touched {
				touchedSet[t.Name] = true
			}
			for _, want := range opt.Topics {
				if wantMod, wantTopic, ok := strings.Cut(want, ":"); ok {
					if wantMod == m.Name {
						touchedSet[wantTopic] = true
					}
				} else {
					touchedSet[want] = true
				}
			}
			for j := range m.Topics {
				t := &m.Topics[j]
				if touchedSet[t.Name] {
					shown[i] = append(shown[i], t)
				} else {
					hidden = append(hidden, t)
				}
			}
		} else {
			shown[i], hidden = s.TopicsTouching(m, opt.Files)
		}
		parts = append(parts, part{module: i, kind: kindCore, label: "the rules of " + m.Name, text: core(s, m, hidden), truncatable: true})
	}
	for i, m := range mods {
		for _, t := range shown[i] {
			if text := topicRules(m, t); text != "" {
				parts = append(parts, part{module: i, kind: kindTopic, label: "the rules of " + m.Name + " topic " + t.Name, text: text, truncatable: true})
			}
		}
	}
	appendTopicBriefs := func() {
		if opt.OmitBriefs {
			return
		}
		for i, m := range mods {
			for _, t := range shown[i] {
				if text := strings.TrimSpace(t.Brief.Text); text != "" {
					where := briefPath(s, t.Brief.Path)
					parts = append(parts, part{module: i, kind: kindTopicBrief, label: "brief of " + m.Name + " topic " + t.Name + " (" + where + ")",
						text: fmt.Sprintf("### Brief of topic %s (%s)\n\nThe owner's longer description of what the topic binds; binding where it is more specific than the rules above.\n\n%s\n", t.Name, where, text)})
				}
			}
		}
	}
	if len(opt.Topics) > 0 {
		appendTopicBriefs()
	}
	for i, m := range mods {
		if t := operations(m, opt.Files); t != "" {
			parts = append(parts, part{module: i, kind: kindOperations, label: "operations of " + m.Name, text: t, truncatable: true})
		}
		if t := scenarios(m, opt.Files); t != "" {
			parts = append(parts, part{module: i, kind: kindScenarios, label: "scenarios of " + m.Name, text: t, truncatable: true})
		}
	}
	if !opt.OmitBriefs {
		for i, m := range mods {
			if text := strings.TrimSpace(m.Brief.Text); text != "" {
				where := briefPath(s, m.Brief.Path)
				parts = append(parts, part{module: i, kind: kindBrief, label: "brief of " + m.Name + " (" + where + ")",
					text: fmt.Sprintf("### Brief (%s)\n\nThe owner's longer description of the module; binding where it is more specific than the rules above.\n\n%s\n", where, text)})
			}
		}
		if len(opt.Topics) == 0 {
			appendTopicBriefs()
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
		b.WriteString(omittedNote(omitted))
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
	room := budget*CharsPerToken - omittedNoteReserve
	dropped := map[int]bool{}
	for idx, p := range parts {
		switch {
		case p.kind != kindCore && p.kind != kindSystem && dropped[p.module]:
			omitted = append(omitted, p.label)
		case len(p.text)+1 <= room: // and the blank line after it
			kept = append(kept, p)
			room -= len(p.text) + 1
		case p.truncatable && room > minCutRoom:
			partRoom := room
			if p.kind == kindCore || p.kind == kindTopic {
				remRules := 0
				for _, q := range parts[idx:] {
					if (q.kind == kindCore || q.kind == kindTopic) && !dropped[q.module] {
						remRules++
					}
				}
				if remRules > 1 && room/remRules >= minCutRoom+cutMarkerReserve {
					partRoom = room / remRules
				}
			}
			cut := p.text[:runeStart(p.text, max(0, partRoom-cutMarkerReserve))]
			p.text = cut + "\n[cut to fit the size limit]\n"
			kept = append(kept, p)
			if partRoom < room {
				room = max(0, room-(len(p.text)+1))
			} else {
				room = 0
			}
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

// omittedNote names what was left out, within omittedNoteReserve: the
// names that fit, then how many more.
func omittedNote(omitted []string) string {
	const head, tail = "Left out to fit the size limit: ", ". Do not report something as unspecified only because it is not shown here.\n"
	names := strings.Join(omitted, "; ")
	if len(head)+len(names)+len(tail) > omittedNoteReserve {
		names = ""
		for i, o := range omitted {
			more := fmt.Sprintf("; and %d more", len(omitted)-i)
			if i > 0 && len(head)+len(names)+2+len(o)+len(more)+len(tail) > omittedNoteReserve {
				names += more
				break
			}
			if i > 0 {
				names += "; "
			}
			names += o
		}
	}
	return head + names + tail
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

// topicRules is what binds the files a topic covers.
func topicRules(m *spec.Module, t *spec.Topic) string {
	if len(t.Constraints) == 0 && len(t.Invariants) == 0 && t.Intent == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Topic %s (binds %s)\n", t.Name, strings.Join(t.Files, ", "))
	if t.Intent != "" {
		fmt.Fprintf(&b, "\n%s\n", oneLine(t.Intent))
	}
	if len(t.Constraints) > 0 {
		b.WriteString("\nConstraints:\n")
		for _, c := range t.Constraints {
			fmt.Fprintf(&b, "- %s\n", oneLine(c))
		}
	}
	if len(t.Invariants) > 0 {
		b.WriteString("\nInvariants (cite as " + m.Name + ".topics." + t.Name + ".invariants[i], or the id):\n")
		writeInvariants(&b, t.Invariants)
	}
	return b.String()
}

func writeInvariants(b *strings.Builder, invs []spec.Invariant) {
	for i, inv := range invs {
		id := inv.ID
		if id == "" {
			id = fmt.Sprintf("%d", i)
		}
		fmt.Fprintf(b, "- [%s] %s\n", id, oneLine(inv.Statement))
	}
}

// core is what binds a module: why it exists, the goals it answers for, its
// constraints and invariants, and the topics not shown (hidden), named so a
// reader knows those rules exist.
func core(s *spec.Spec, m *spec.Module, hidden []*spec.Topic) string {
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
		writeInvariants(&b, m.Invariants)
	}
	if len(hidden) > 0 {
		b.WriteString("\nTopics not shown, since none of the files here is theirs (their rules bind other files):\n")
		for _, t := range hidden {
			if t.Intent != "" {
				fmt.Fprintf(&b, "- %s: %s\n", t.Name, oneLine(t.Intent))
			} else {
				fmt.Fprintf(&b, "- %s\n", t.Name)
			}
		}
	}
	return b.String()
}

var stemStopWords = map[string]bool{
	"go": true, "test": true, "tests": true, "internal": true,
	"pkg": true, "cmd": true, "mod": true, "modules": true, "src": true,
}

func splitTokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 && unicode.IsUpper(r) {
			prev := cur[len(cur)-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func fileStems(files []string) []string {
	var stems []string
	seen := map[string]bool{}
	for _, f := range files {
		for _, raw := range splitTokens(f) {
			tok := strings.ToLower(raw)
			if len(tok) < 3 || stemStopWords[tok] || seen[tok] {
				continue
			}
			seen[tok] = true
			stems = append(stems, tok)
		}
	}
	return stems
}

func scoreText(stems []string, fields ...string) int {
	if len(stems) == 0 {
		return 0
	}
	joined := strings.ToLower(strings.Join(fields, " "))
	score := 0
	for _, stem := range stems {
		if strings.Contains(joined, stem) {
			score++
		}
	}
	return score
}

func operations(m *spec.Module, files []string) string {
	if len(m.Interface) == 0 {
		return ""
	}
	ops := m.Interface
	if stems := fileStems(files); len(stems) > 0 {
		type scoredOp struct {
			op    spec.Operation
			score int
		}
		scored := make([]scoredOp, len(ops))
		anyMatch := false
		for i, op := range ops {
			sc := scoreText(stems, op.Name, op.Signature, op.Intent, strings.Join(op.Pre, " "), strings.Join(op.Post, " "))
			if sc > 0 {
				anyMatch = true
			}
			scored[i] = scoredOp{op: op, score: sc}
		}
		if anyMatch {
			slices.SortStableFunc(scored, func(a, b scoredOp) int {
				return cmp.Compare(b.score, a.score)
			})
			ops = make([]spec.Operation, len(scored))
			for i, s := range scored {
				ops[i] = s.op
			}
		}
	}
	var b strings.Builder
	b.WriteString("### Operations\n\n")
	for _, op := range ops {
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

func scenarios(m *spec.Module, files []string) string {
	if len(m.Scenarios) == 0 {
		return ""
	}
	scs := m.Scenarios
	if stems := fileStems(files); len(stems) > 0 {
		type scoredSc struct {
			sc    spec.Scenario
			score int
		}
		scored := make([]scoredSc, len(scs))
		anyMatch := false
		for i, sc := range scs {
			s := scoreText(stems, sc.ID, sc.Given, sc.When, sc.Then)
			if s > 0 {
				anyMatch = true
			}
			scored[i] = scoredSc{sc: sc, score: s}
		}
		if anyMatch {
			slices.SortStableFunc(scored, func(a, b scoredSc) int {
				return cmp.Compare(b.score, a.score)
			})
			scs = make([]spec.Scenario, len(scored))
			for i, s := range scored {
				scs[i] = s.sc
			}
		}
	}
	var b strings.Builder
	b.WriteString("### Scenarios\n\n")
	for _, sc := range scs {
		fmt.Fprintf(&b, "- %s:", sc.ID)
		if sc.Given != "" {
			fmt.Fprintf(&b, " given %s;", oneLine(sc.Given))
		}
		fmt.Fprintf(&b, " when %s; then %s\n", oneLine(sc.When), oneLine(sc.Then))
	}
	return b.String()
}

// briefPath shows where a brief is (p, as the spec writes it): relative to
// the repository when the spec knows it, else as written.
func briefPath(s *spec.Spec, p string) string {
	if s.Root == "" || s.Dir == "" {
		return p
	}
	abs, err := filepath.Abs(filepath.Join(s.Dir, filepath.FromSlash(p)))
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(s.Root, abs)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// oneLine folds a YAML block scalar's line breaks so a rule stays one list
// item.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
