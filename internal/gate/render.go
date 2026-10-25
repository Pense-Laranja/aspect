package gate

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// CommentMarker starts every rendered comment so CI can find and update its
// own sticky comment instead of adding a new one per push.
const CommentMarker = "<!-- aspect-review-gates -->"

// RenderOptions controls what a rendered comment may disclose.
type RenderOptions struct {
	// Public redacts security findings at the config's public_redaction
	// severities to title, category and file, so a public PR does not
	// publish an exploit before it is fixed.
	Public bool
}

// Render writes the check result as Markdown for a PR comment or a terminal.
// Every string that came from a model is escaped: a report is untrusted
// input and must not be able to inject HTML or forge the marker.
func Render(c *Config, p *Plan, r *Result, opt RenderOptions) string {
	var b strings.Builder
	b.WriteString(CommentMarker + "\n")
	fmt.Fprintf(&b, "## Review gates: %s\n\n", headline(r.Outcome))
	fmt.Fprintf(&b, "`%s...%s`, %d file(s), %d line(s) changed.\n\n", short(p.Base), short(p.Head), p.ChangedFiles, p.ChangedLines)
	if p.Spec == NoSpec {
		fmt.Fprintf(&b, "No spec: `%s` does not exist at head, so the agents reviewed against the repository's own rules and docs. The spec keeper can draft one in author mode.\n\n", inline(c.Spec))
	}
	if p.SpecNote != "" {
		fmt.Fprintf(&b, "**Spec not fully shown:** %s\n\n", p.SpecNote)
	}
	if c := p.CarriedFrom; c != nil {
		fmt.Fprintf(&b, "**Verdict carried** from run `%s`, which judged `%s`: this head's diff against its merge base is the same, byte for byte, "+
			"and so are the agents' definitions and the spec they were shown; only the base moved. No agent ran on this head.\n\n",
			inline(c.Run), short(c.Head))
	}
	if len(r.Agents) == 0 {
		b.WriteString("No gate applies to this change.\n")
		return b.String()
	}
	b.WriteString("| Agent | Mode | Report | Blocking | Warnings |\n|---|---|---|---|---|\n")
	for _, a := range r.Agents {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d |\n", a.Agent, a.Mode, a.Status, len(a.Blocking), len(a.Warnings))
	}
	for _, a := range r.Agents {
		fmt.Fprintf(&b, "\n### %s\n\n", a.Agent)
		if a.Status != StatusOK {
			fmt.Fprintf(&b, "Report %s: %s\n", a.Status, inline(a.Error))
			continue
		}
		b.WriteString(inline(a.Summary) + "\n")
		for _, group := range []struct {
			name string
			fs   []Finding
		}{{"Blocking", a.Blocking}, {"Warnings", a.Warnings}} {
			if len(group.fs) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n**%s**\n\n", group.name)
			for _, f := range group.fs {
				renderFinding(&b, c, f, opt)
			}
		}
		if len(a.Handoffs) > 0 {
			b.WriteString("\n**Handoffs**\n\n")
			for _, h := range a.Handoffs {
				fmt.Fprintf(&b, "- to %s: %s\n", inline(h.To), inline(h.Reason))
			}
		}
		if len(a.ADRs) > 0 {
			b.WriteString("\n**Proposed ADRs**\n\n")
			for _, d := range a.ADRs {
				fmt.Fprintf(&b, "- `%s`: %s\n", inline(d.Slug), inline(d.Title))
			}
		}
	}
	if r.Outcome == Overridden {
		b.WriteString("\nA maintainer overrode the blocking findings with the `review-gates/override` label.\n")
	}
	if hasSpecDrift(r) && strings.HasPrefix(path.Clean(p.Spec), SpecUpdateDir) {
		fmt.Fprintf(&b, "\nThe spec and the code disagree. Adding the `%s` label runs spec-keeper in author mode and proposes its spec edits as a PR into this branch.\n", UpdateSpecLabel)
	}
	return b.String()
}

// UpdateSpecLabel is the PR label that runs spec-keeper in author mode on
// the PR (.github/workflows/spec-update.yml).
const UpdateSpecLabel = "review-gates/update-spec"

// SpecUpdateDir is the only directory the spec-update workflow lets
// spec-keeper write, so the label is offered only for a spec inside it: a
// root aspect.yaml, or a spec gates.yaml puts elsewhere, cannot be updated
// that way.
const SpecUpdateDir = "_aspect/"

// hasSpecDrift reports whether spec-keeper found drift a spec edit could
// close: a spec-drift finding that blocks or warns.
func hasSpecDrift(r *Result) bool {
	for _, a := range r.Agents {
		if a.Agent != "spec-keeper" {
			continue
		}
		for _, f := range slices.Concat(a.Blocking, a.Warnings) {
			if f.Category == "spec-drift" {
				return true
			}
		}
	}
	return false
}

func renderFinding(b *strings.Builder, c *Config, f Finding, opt RenderOptions) {
	loc := ""
	if l := f.Location; l != nil {
		loc = fmt.Sprintf(" `%s`", inline(l.File))
		if l.Line > 0 {
			loc = fmt.Sprintf(" `%s:%d`", inline(l.File), l.Line)
		}
	}
	fmt.Fprintf(b, "- **%s** %s · %s%s: %s", f.Severity, inline(f.ID), f.Category, loc, inline(f.Title))
	if opt.Public && f.Category == "security" && slices.Contains(c.PublicRedaction, f.Severity) {
		b.WriteString(" _(details withheld on a public PR; run `/review` locally)_\n")
		return
	}
	fmt.Fprintf(b, " (confidence %.2f)\n", f.Confidence)
	var details []string
	for _, kv := range [][2]string{{"Spec", f.SpecRef}, {"Evidence", f.Evidence}, {"Reproduction", f.Reproduction}, {"Recommendation", f.Recommendation}} {
		if strings.TrimSpace(kv[1]) != "" {
			details = append(details, fmt.Sprintf("  - %s: %s", kv[0], inline(kv[1])))
		}
	}
	if len(details) > 0 {
		b.WriteString(strings.Join(details, "\n") + "\n")
	}
}

func headline(outcome string) string {
	switch outcome {
	case Pass:
		return "pass"
	case Warn:
		return "pass with warnings"
	case Block:
		return "blocked"
	case Overridden:
		return "blocked, overridden by a maintainer"
	}
	return outcome
}

// inline flattens model text to one escaped line.
func inline(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "|", "\\|").Replace(s)
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
