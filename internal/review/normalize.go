package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/eideroliveira/aspect/internal/gate"
)

// fencedJSON is a ```json fenced block holding one object.
var fencedJSON = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")

// extractJSON reads the JSON object a model answered with: the whole text,
// else a fenced block, else the span from the first "{" to the last "}".
func extractJSON(text string) (*Object, error) {
	if v, err := parseValue([]byte(text)); err == nil {
		if o, ok := v.(*Object); ok {
			return o, nil
		}
	}
	if m := fencedJSON.FindStringSubmatch(text); m != nil {
		if v, err := parseValue([]byte(m[1])); err == nil {
			if o, ok := v.(*Object); ok {
				return o, nil
			}
		}
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		v, err := parseValue([]byte(text[start : end+1]))
		if err != nil {
			return nil, fmt.Errorf("failed to parse the JSON in the model's output: %v; output was: %s", err, clip(text, 500))
		}
		if o, ok := v.(*Object); ok {
			return o, nil
		}
	}
	return nil, fmt.Errorf("no JSON object in the model's output: %s", clip(text, 500))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ErrInvalidReport means a model's report cannot be made valid without
// inventing content.
var ErrInvalidReport = errors.New("invalid report")

// categoryAliases are categories models use that aspect does not, mapped to
// the closest one.
var categoryAliases = map[string]string{
	"architecture": "design", "architectural": "design", "maintainability": "design",
	"style": "readability", "formatting": "readability", "naming": "readability",
	"bug": "correctness", "logic": "correctness", "reliability": "correctness", "error-handling": "correctness",
	"testing": "test-gap", "tests": "test-gap", "test": "test-gap", "test-coverage": "test-gap", "coverage": "test-gap",
	"documentation": "docs", "doc": "docs", "comments": "docs",
	"spec": "spec-drift", "specification": "spec-drift",
	"logging": "observability", "monitoring": "observability", "metrics": "observability", "tracing": "observability",
	"localization": "i18n", "l10n": "i18n", "internationalization": "i18n", "translation": "i18n",
	"api": "contract", "interface": "contract", "compatibility": "contract",
	"database": "data", "schema": "data", "migration": "data", "sql": "data",
	"perf": "performance", "vulnerability": "security", "auth": "security",
}

// severityAliases are severities models use that aspect does not, mapped to
// the one they mean. Anything else is not guessed: a severity decides the
// verdict, so an unknown one makes the report invalid (see Normalize).
var severityAliases = map[string]string{
	"blocker": "high", "major": "high", "error": "high", "severe": "critical", "fatal": "critical",
	"warning": "medium", "warn": "medium", "moderate": "medium",
	"minor": "low",
	"nit":   "info", "nitpick": "info", "suggestion": "info", "note": "info", "trivial": "info", "informational": "info",
}

// severity is the aspect severity a model's value names, and whether it
// names one.
func severity(raw any) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(asText(raw)))
	if slices.Contains(gate.Severities, s) {
		return s, true
	}
	alias, ok := severityAliases[s]
	return alias, ok
}

// vagueCategories name no harm: the finding takes its agent's concern.
var vagueCategories = map[string]bool{
	"": true, "quality": true, "code-quality": true, "general": true, "other": true, "misc": true,
	"best-practice": true, "best-practices": true, "improvement": true,
}

// agentCategories are each agent's own concern, for vague or unknown
// categories, in the order they are tried. Agents whose name contains a
// category (security-reviewer, i18n-reviewer, ...) need no entry.
var agentCategories = []struct{ key, category string }{
	{"test-adequacy", "test-gap"}, {"api-contract", "contract"}, {"database", "data"},
	{"spec-keeper", "spec-drift"}, {"red-team", "security"}, {"architect", "design"},
	{"docs-writer", "docs"}, {"quality", "correctness"}, {"adversarial", "correctness"},
}

var (
	adrSlugRE  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	locationRE = regexp.MustCompile(`^(.+?):(\d+)(?:-(\d+))?$`)
)

// agentCategory is the category an agent's findings default to
// (test-adequacy-reviewer: test-gap).
func agentCategory(agent string) string {
	for _, ac := range agentCategories {
		if strings.Contains(agent, ac.key) {
			return ac.category
		}
	}
	for _, c := range gate.Categories {
		if strings.Contains(agent, c) {
			return c
		}
	}
	return "correctness"
}

func category(raw any, agent string) string {
	c := strings.NewReplacer("_", "-", " ", "-").Replace(strings.ToLower(asText(raw)))
	if slices.Contains(gate.Categories, c) {
		return c
	}
	if alias, ok := categoryAliases[c]; ok && !vagueCategories[c] {
		return alias
	}
	return agentCategory(agent)
}

func confidence(raw any) float64 {
	if raw == nil || raw == "" {
		return 0.9
	}
	c, ok := asFloat(raw)
	if !ok || math.IsNaN(c) {
		switch strings.ToLower(asText(raw)) {
		case "high":
			return 0.9
		case "medium":
			return 0.6
		case "low":
			return 0.3
		}
		return 0.5
	}
	if c > 1 && c <= 100 { // a percentage
		c /= 100
	}
	return min(max(c, 0), 1)
}

// location is aspect's Location from what the model gave: a location
// object, a "path:line[-end]" string, or file/line keys on the finding.
func location(f *Object) *gate.Location {
	var loc *Object
	switch v := f.Get("location").(type) {
	case *Object:
		loc = v
	case string:
		loc = NewObject()
		if m := locationRE.FindStringSubmatch(strings.TrimSpace(v)); m != nil {
			loc.Set("file", m[1])
			loc.Set("line", m[2])
			if m[3] != "" {
				loc.Set("end_line", m[3])
			}
		} else {
			loc.Set("file", v)
		}
	default:
		loc = NewObject()
		file := f.Get("file")
		if !truthy(file) {
			file = f.Get("path")
		}
		loc.Set("file", file)
		loc.Set("line", f.Get("line"))
		loc.Set("end_line", f.Get("end_line"))
	}
	fileV := loc.Get("file")
	if !truthy(fileV) {
		fileV = loc.Get("path")
	}
	file := asText(fileV)
	if file == "" {
		return nil
	}
	out := &gate.Location{File: file}
	if line := asInt(loc.Get("line")); line > 0 { // 1-based; omitted for a whole-file finding
		out.Line = line
		if end := asInt(loc.Get("end_line")); end > line {
			out.EndLine = end
		}
	}
	return out
}

// finding is one finding as written to <agent>.json: aspect's Finding with
// the optional fields left out when empty.
type finding struct {
	ID             string         `json:"id"`
	Severity       string         `json:"severity"`
	Category       string         `json:"category"`
	Title          string         `json:"title"`
	Evidence       string         `json:"evidence"`
	Recommendation string         `json:"recommendation"`
	Confidence     float64        `json:"confidence"`
	SpecRef        string         `json:"spec_ref,omitempty"`
	Reproduction   string         `json:"reproduction,omitempty"`
	Location       *gate.Location `json:"location,omitempty"`
}

// Report is an agent's report as written to <agent>.json, in the
// aspect-review/v1 shape `aspect gate check` decodes.
type Report struct {
	Schema   string         `json:"schema"`
	Agent    string         `json:"agent"`
	Mode     string         `json:"mode"`
	Base     string         `json:"base"`
	Head     string         `json:"head"`
	Summary  string         `json:"summary"`
	Findings []finding      `json:"findings"`
	Handoffs []gate.Handoff `json:"handoffs"`
	Changes  []gate.Written `json:"changes"`
	ADRs     []gate.ADR     `json:"adrs,omitempty"`
}

// emptyReport is a report with no findings: an agent with nothing to review.
func emptyReport(agent, mode, base, head, summary string) *Report {
	return &Report{
		Schema: gate.ReportSchema, Agent: agent, Mode: mode, Base: base, Head: head, Summary: summary,
		Findings: []finding{}, Handoffs: []gate.Handoff{}, Changes: []gate.Written{},
	}
}

// blockersAndWarnings counts the report's findings the way the run status
// does: critical and high block, medium and low warn.
func (r *Report) blockersAndWarnings() (blockers, warnings int) {
	for _, f := range r.Findings {
		switch f.Severity {
		case "critical", "high":
			blockers++
		case "medium", "low":
			warnings++
		}
	}
	return blockers, warnings
}

// Normalize turns a model's report into one `aspect gate check` accepts,
// stamped with run.
//
// Wrong types are coerced and unknown fields dropped; nothing that changes
// the verdict is invented. Two things would, so they make the report an
// ErrInvalidReport and the agent fail instead: a severity that is none of
// gate.Severities nor a known alias (guessing info could turn a blocker into
// a pass), and a non-info finding without a location (which aspect
// requires; downgrading it to info would do the same).
func Normalize(raw *Object, run Run) (*Report, error) {
	agent, mode := run.Agent, run.Mode
	r := emptyReport(agent, mode, run.Base, run.Head, "")
	ids := map[string]bool{}
	list, _ := raw.Get("findings").([]any)
	for idx, e := range list {
		f, ok := e.(*Object)
		if !ok {
			continue
		}
		id := asText(f.Get("id"))
		if id == "" {
			id = agent + "-" + strconv.Itoa(idx+1)
		}
		for ids[id] {
			id += "-dup"
		}
		ids[id] = true
		sev, ok := severity(f.Get("severity"))
		if !ok {
			return nil, fmt.Errorf("%w: finding %q has severity %q, which is none of %v nor a known alias; guessing one could turn a blocker into a pass",
				ErrInvalidReport, id, asText(f.Get("severity")), gate.Severities)
		}
		cf := finding{
			ID:             id,
			Severity:       sev,
			Category:       category(f.Get("category"), agent),
			Title:          orText(f.Get("title"), "Code finding"),
			Evidence:       orText(f.Get("evidence"), "Observed during review"),
			Recommendation: orText(f.Get("recommendation"), "Review and address the issue"),
			Confidence:     confidence(f.Get("confidence")),
			SpecRef:        asText(f.Get("spec_ref")),
			Reproduction:   asText(f.Get("reproduction")),
			Location:       location(f),
		}
		if cf.Location == nil && sev != "info" {
			return nil, fmt.Errorf("%w: finding %q (%s: %q) has no file location; aspect rejects a non-info finding without one",
				ErrInvalidReport, id, sev, cf.Title)
		}
		r.Findings = append(r.Findings, cf)
	}
	for _, h := range records(raw, "handoffs") {
		if to := asText(h.Get("to")); to != "" && to != agent {
			r.Handoffs = append(r.Handoffs, gate.Handoff{To: to, Reason: asText(h.Get("reason"))})
		}
	}
	if mode == "author" {
		for _, c := range records(raw, "changes") {
			if file := asText(c.Get("file")); file != "" {
				r.Changes = append(r.Changes, gate.Written{File: file, Reason: asText(c.Get("reason"))})
			}
		}
	}
	r.Summary = orText(raw.Get("summary"), "Review completed.")
	for _, a := range records(raw, "adrs") {
		adr := gate.ADR{Slug: asText(a.Get("slug")), Title: asText(a.Get("title")), Body: asText(a.Get("body"))}
		if !adrSlugRE.MatchString(adr.Slug) || adr.Title == "" || adr.Body == "" {
			continue
		}
		if ref := asText(a.Get("finding")); ids[ref] {
			adr.Finding = ref
		}
		r.ADRs = append(r.ADRs, adr)
	}
	return r, nil
}

// Run names the review a report belongs to: the agent that wrote it, the
// mode it ran in, and the commits it reviewed.
type Run struct {
	Agent, Mode, Base, Head string
}

// NormalizeJSON normalizes a report that an agent printed as JSON text, as
// a Claude Code or Gemini CLI agent does in CI, and returns it as `aspect
// gate check` reads it, stamped with the run. It is stricter than
// Normalize, which turns a model's free answer into a report: the text it
// reads was meant to be a report already, so it fixes only what cannot
// change the verdict (an invented category, a known severity alias, a
// missing id or field the run supplies) and refuses everything else that
// gate check would have rejected:
//   - no findings list, or findings that are not objects (a truncated or
//     garbled report could hide a blocker behind an empty one);
//   - a confidence above 1 (Normalize would read 9 as 9%, below the block
//     threshold);
//   - another schema, agent, mode or revision than the run's (relabelling
//     would defeat gate check's stale-report rule); a report that leaves
//     them out, or abbreviates a commit, takes the run's;
//   - whatever Normalize refuses.
func NormalizeJSON(data []byte, run Run) ([]byte, error) {
	if run.Agent == "" || run.Base == "" || run.Head == "" || !slices.Contains(gate.Modes, run.Mode) {
		return nil, fmt.Errorf("normalize: the run needs an agent, a mode (one of %v), a base and a head, got %+v", gate.Modes, run)
	}
	v, err := parseValue(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidReport, err)
	}
	raw, ok := v.(*Object)
	if !ok {
		return nil, fmt.Errorf("%w: the report is not a JSON object", ErrInvalidReport)
	}
	list, ok := raw.Get("findings").([]any)
	if !ok {
		return nil, fmt.Errorf("%w: findings is missing or not a list", ErrInvalidReport)
	}
	for i, e := range list {
		f, ok := e.(*Object)
		if !ok {
			return nil, fmt.Errorf("%w: findings[%d] is not an object", ErrInvalidReport, i)
		}
		if c, isNumber := asFloat(f.Get("confidence")); isNumber && c > 1 {
			return nil, fmt.Errorf("%w: findings[%d] has confidence %v, outside [0, 1]", ErrInvalidReport, i, c)
		}
	}
	for _, field := range []struct{ key, want string }{{"schema", gate.ReportSchema}, {"agent", run.Agent}, {"mode", run.Mode}} {
		if got := asText(raw.Get(field.key)); got != "" && got != field.want {
			return nil, fmt.Errorf("%w: the report's %s is %q, not the run's %q", ErrInvalidReport, field.key, got, field.want)
		}
	}
	for _, field := range []struct{ key, want string }{{"base", run.Base}, {"head", run.Head}} {
		if got := asText(raw.Get(field.key)); got != "" && !gate.SameRev(got, field.want) {
			return nil, fmt.Errorf("%w: the report is for %s %s, not the run's %s: it reviewed another revision", ErrInvalidReport, field.key, got, field.want)
		}
	}
	r, err := Normalize(raw, run)
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func orText(v any, fallback string) string {
	if s := asText(v); s != "" {
		return s
	}
	return fallback
}
