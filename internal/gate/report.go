package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// ReportSchema identifies a review report; it is the "schema" field of every
// report and matches .claude/review/report.schema.json.
const ReportSchema = "aspect-review/v1"

// Report is what one agent writes at the end of a run. Agents never state a
// verdict; Check computes it.
type Report struct {
	Schema   string    `json:"schema"`
	Agent    string    `json:"agent"`
	Mode     string    `json:"mode"`
	Base     string    `json:"base"`
	Head     string    `json:"head"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	Handoffs []Handoff `json:"handoffs"`
	Changes  []Written `json:"changes"`
	ADRs     []ADR     `json:"adrs,omitempty"`
}

// Finding is one problem or observation, located in the head version.
type Finding struct {
	ID             string    `json:"id"`
	Severity       string    `json:"severity"`
	Category       string    `json:"category"`
	Title          string    `json:"title"`
	Location       *Location `json:"location"`
	SpecRef        string    `json:"spec_ref"`
	Evidence       string    `json:"evidence"`
	Reproduction   string    `json:"reproduction"`
	Recommendation string    `json:"recommendation"`
	Confidence     float64   `json:"confidence"`
}

// Location is a file and 1-based line range in the head version. Line is
// omitted for a finding about a whole file, such as one that is missing.
type Location struct {
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
}

// Handoff asks the orchestrator to run another agent with this report.
type Handoff struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Written is a file an authoring agent changed.
type Written struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// ADR is an architecture decision record an agent proposes. The
// orchestrator writes an accepted one to docs/adr/NNNN-<slug>.md.
type ADR struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Finding string `json:"finding,omitempty"`
	Body    string `json:"body"`
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// DecodeReport parses a report strictly: unknown fields, missing required
// fields, trailing data and out-of-range values are all errors. A report
// that does not decode is treated as missing, never as a pass.
func DecodeReport(b []byte) (*Report, error) {
	if err := requireKeys(b); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after the report")
	}
	return &r, r.Validate()
}

// requireKeys checks that every required key is present, since
// encoding/json cannot tell a missing field from a zero value.
func requireKeys(b []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return err
	}
	if err := hasKeys("report", top, "schema", "agent", "mode", "base", "head", "summary", "findings", "handoffs", "changes"); err != nil {
		return err
	}
	var findings []map[string]json.RawMessage
	if err := json.Unmarshal(top["findings"], &findings); err != nil {
		return fmt.Errorf("findings: %w", err)
	}
	for i, f := range findings {
		if raw, ok := f["location"]; ok {
			var loc map[string]json.RawMessage
			if json.Unmarshal(raw, &loc) == nil {
				if line, ok := loc["line"]; ok && string(line) == "0" {
					return fmt.Errorf("findings[%d]: location line is 1-based; omit it for a whole-file finding", i)
				}
			}
		}
		if err := hasKeys(fmt.Sprintf("findings[%d]", i), f, "id", "severity", "category", "title", "evidence", "recommendation", "confidence"); err != nil {
			return err
		}
	}
	return nil
}

func hasKeys(where string, m map[string]json.RawMessage, keys ...string) error {
	var missing []string
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: missing %s", where, strings.Join(missing, ", "))
	}
	return nil
}

// Validate checks the field rules of docs/design.md §2.5 that the type
// system cannot express.
func (r *Report) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if r.Schema != ReportSchema {
		bad("schema must be %q, got %q", ReportSchema, r.Schema)
	}
	if r.Agent == "" {
		bad("agent is required")
	}
	if !slices.Contains(Modes, r.Mode) {
		bad("mode must be one of %v, got %q", Modes, r.Mode)
	}
	if strings.TrimSpace(r.Summary) == "" {
		bad("summary is required")
	}
	if r.Mode != "author" && len(r.Changes) > 0 {
		bad("a %s report must not list changes", r.Mode)
	}
	ids := map[string]bool{}
	for i, f := range r.Findings {
		at := fmt.Sprintf("findings[%d]", i)
		switch {
		case f.ID == "":
			bad("%s: id is required", at)
		case ids[f.ID]:
			bad("%s: duplicate id %q", at, f.ID)
		}
		ids[f.ID] = true
		if !slices.Contains(Severities, f.Severity) {
			bad("%s: severity must be one of %v, got %q", at, Severities, f.Severity)
		}
		if !slices.Contains(Categories, f.Category) {
			bad("%s: category must be one of %v, got %q", at, Categories, f.Category)
		}
		if strings.TrimSpace(f.Title) == "" {
			bad("%s: title is required", at)
		}
		if f.Confidence < 0 || f.Confidence > 1 {
			bad("%s: confidence must be in [0, 1], got %v", at, f.Confidence)
		}
		if f.Severity != "info" && f.Location == nil {
			bad("%s: a %s finding needs a location", at, f.Severity)
		}
		if l := f.Location; l != nil {
			if l.File == "" || l.Line < 0 {
				bad("%s: location needs a file, and a line only if it is 1-based", at)
			}
			if l.EndLine != 0 && l.Line == 0 {
				bad("%s: end_line needs a line", at)
			}
			if l.EndLine != 0 && l.EndLine < l.Line {
				bad("%s: end_line %d is before line %d", at, l.EndLine, l.Line)
			}
		}
	}
	for i, h := range r.Handoffs {
		if h.To == "" || h.To == r.Agent {
			bad("handoffs[%d]: to must name another agent", i)
		}
	}
	for i, w := range r.Changes {
		if w.File == "" {
			bad("changes[%d]: file is required", i)
		}
	}
	for i, a := range r.ADRs {
		if !slugRE.MatchString(a.Slug) {
			bad("adrs[%d]: slug must be kebab-case, got %q", i, a.Slug)
		}
		if strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Body) == "" {
			bad("adrs[%d]: title and body are required", i)
		}
		if a.Finding != "" && !ids[a.Finding] {
			bad("adrs[%d]: finding %q is not in this report", i, a.Finding)
		}
	}
	return errors.Join(errs...)
}

// SameRev reports whether two revisions name the same commit, allowing one
// to be an abbreviation of the other. Empty matches anything, so a plan
// without revisions does not reject reports.
func SameRev(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a) || a == b
}

// LastJSONBlock returns the body of the last fenced ```json block in an
// agent's final message. Agents print their report there as well as writing
// it to disk; CI uses this when the file is missing. The bytes are returned
// as written, never repaired: a malformed block must fail DecodeReport.
func LastJSONBlock(text string) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var last []string
	found := false
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```json" {
			continue
		}
		var body []string
		closed := false
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				closed, i = true, j
				break
			}
			body = append(body, lines[j])
		}
		if !closed {
			break
		}
		last, found = body, true
	}
	if !found {
		return nil, fmt.Errorf("no fenced json block in the agent's output")
	}
	return []byte(strings.Join(last, "\n") + "\n"), nil
}

// AgentText returns the final message from a headless agent run: the
// "result" field of Claude Code's --output-format json, the "response" field
// of Gemini CLI's, or the input itself when it is plain text.
func AgentText(raw []byte) string {
	var wrapped struct {
		Result   *string `json:"result"`
		Response *string `json:"response"`
	}
	if json.Unmarshal(raw, &wrapped) == nil {
		switch {
		case wrapped.Result != nil:
			return *wrapped.Result
		case wrapped.Response != nil:
			return *wrapped.Response
		}
	}
	return string(raw)
}
