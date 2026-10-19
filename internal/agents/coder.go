package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/eideroliveira/aspect/internal/lang"
	"github.com/eideroliveira/aspect/internal/llm"
	"github.com/eideroliveira/aspect/internal/spec"
	"github.com/eideroliveira/aspect/internal/workspace"
)

// CodeInput is everything the Coder may know about one module.
type CodeInput struct {
	Task
	// Dependencies is the source of modules this one depends on, so the Coder
	// codes against real signatures instead of guessing.
	Dependencies []workspace.File
	// Existing is the module's current source, set on repair rounds.
	Existing []workspace.File
	// Tests is the module's current tests, set on repair rounds. The Coder may
	// read them but not change them.
	Tests []workspace.File
	// Feedback is the failing build/test output on repair rounds.
	Feedback string
	// Attempts are the earlier repair rounds of this module, oldest first,
	// so the Coder does not retry a change that already failed.
	Attempts []RepairAttempt
}

// RepairAttempt is one earlier repair round: what the Coder changed and how
// the run failed afterwards.
type RepairAttempt struct {
	Changed []string
	Output  string
}

// CodeOutput is the Coder's proposal.
type CodeOutput struct {
	// Files is the complete implementation of the module. On a repair round
	// the model may answer with only the files it changed; Generate merges
	// them over the existing ones so Files is always the whole module.
	Files []workspace.File `json:"files"`
	// Removed names existing files the module no longer needs. Only paths
	// inside the module's code directory are honoured.
	Removed []string `json:"removed"`
	// Notes explain design decisions in one or two paragraphs.
	Notes string `json:"notes"`
	// Concerns list places where the Coder believes the spec or a test is
	// wrong or contradictory. They are surfaced in the report; the Coder does
	// not silently "fix" the spec.
	Concerns []string `json:"concerns"`
}

// Coder writes the implementation of one module.
type Coder struct {
	LLM llm.Client
}

const coderSystem = `You are the Coder agent in Aspect, a pipeline that builds software from a formal specification.

You implement exactly one module at a time. The specification is the source of truth: implement the stated interface with the stated intent, honour every invariant and constraint, and make each pre/post condition true. Where the spec is silent, choose the simplest design that keeps the module's intent obvious to a reader.

When the module owns database entities, it defines their persistent types, their schema or migrations, and every write path; other modules read through this module's interface. When the module implements interface surfaces (HTTP endpoints, web pages, commands), it wires them exactly as specified, using the framework the spec names and following the framework guidance.

Interface signatures in the spec are notation, not code: when they are written in another language's syntax or in the neutral form Name(arg: type) -> result | error, translate them idiomatically for the target language, keeping names and semantics.

Rules:
- Produce complete, compilable files. Never elide code with comments like "rest unchanged".
- Only write implementation files inside the module's code directory. Never write tests; the Tester agent owns those.
- Use only the libraries the stack section allows. The pipeline checks imports mechanically.
- On a repair round you receive failing build or test output. Fix the implementation. If you are convinced a test contradicts the spec, keep the implementation faithful to the spec and say so in concerns.
- On a repair round, return the files you changed or added, each complete; files you do not return are kept exactly as they are. Name files to delete in removed. Do not repeat a change an earlier round already tried.
- Answer with JSON matching the schema you were given.`

var codeSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"files", "removed", "notes", "concerns"},
	"properties": map[string]any{
		"files":    filesSchema(),
		"removed":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Paths of existing files to delete (repair rounds only)"},
		"notes":    map[string]any{"type": "string"},
		"concerns": stringList(),
	},
}

// Generate produces or repairs the module's implementation.
func (c *Coder) Generate(ctx context.Context, in CodeInput) (CodeOutput, llm.Response, error) {
	var b strings.Builder
	b.WriteString(renderContext(in.Task))
	b.WriteString(renderModule(in.Task, "Module to implement"))
	b.WriteString(renderFiles("Dependencies (already implemented, import them)", in.Dependencies))
	if in.Feedback != "" {
		b.WriteString(renderFiles("Current implementation", in.Existing))
		b.WriteString(renderFiles("Current tests (read-only)", in.Tests))
		b.WriteString(renderAttempts(in.Attempts))
		fmt.Fprintf(&b, "## Failing output\n\n```\n%s\n```\n\n", truncate(in.Feedback, 12000))
		b.WriteString("Repair the implementation so the build and tests pass while staying faithful to the spec. Return each file you change or add in full; files you leave out are kept; list files to delete in removed.\n")
	} else {
		b.WriteString("Implement the module. Return every file it needs.\n")
	}

	var out CodeOutput
	system := coderSystem + "\n\n" + in.Lang.CoderRules
	resp, err := complete(ctx, c.LLM, system, b.String(), codeSchema, &out)
	if err != nil {
		return out, resp, fmt.Errorf("coder: %w", err)
	}
	kept, dropped := in.Lang.SplitModuleFiles(in.Module, out.Files, false)
	out.Concerns = append(out.Concerns, droppedConcerns("Coder", dropped)...)
	if in.Feedback != "" {
		out.Removed = keepRemovals(in.Lang, in.Module, out.Removed)
		kept = mergeFiles(in.Existing, kept, out.Removed)
	} else {
		out.Removed = nil
	}
	out.Files = kept
	if len(out.Files) == 0 {
		return out, resp, fmt.Errorf("coder: returned no implementation files inside %s", lang.DirLabel(in.Lang.CodeDir(in.Module)))
	}
	return out, resp, nil
}

// renderAttempts summarises earlier repair rounds so the model sees what was
// already tried and how it failed, instead of oscillating between two fixes.
func renderAttempts(attempts []RepairAttempt) string {
	if len(attempts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Earlier repair rounds\n\nEach round below was already tried on this module and still failed. Do not repeat them.\n\n")
	for i, a := range attempts {
		changed := "no files"
		if len(a.Changed) > 0 {
			changed = strings.Join(a.Changed, ", ")
		}
		fmt.Fprintf(&b, "### Round %d changed %s; the run then failed with\n\n```\n%s\n```\n\n", i+1, changed, truncate(strings.TrimSpace(a.Output), 2500))
	}
	return b.String()
}

// mergeFiles overlays a repair answer on the existing module: answered files
// replace or join the existing ones by path, removed paths are dropped, and
// everything else is kept as it was.
func mergeFiles(existing, answered []workspace.File, removed []string) []workspace.File {
	gone := map[string]bool{}
	for _, p := range removed {
		gone[p] = true
	}
	byPath := map[string]int{}
	var out []workspace.File
	for _, f := range existing {
		if gone[f.Path] {
			continue
		}
		byPath[f.Path] = len(out)
		out = append(out, f)
	}
	for _, f := range answered {
		if i, ok := byPath[f.Path]; ok {
			out[i] = f
			continue
		}
		byPath[f.Path] = len(out)
		out = append(out, f)
	}
	return out
}

// keepRemovals keeps the removal requests that name an implementation file
// of this module, so a repair cannot delete tests (which share the directory
// in Go) or another module's code. The profile's filter decides, exactly as
// it does for proposed files.
func keepRemovals(p *lang.Profile, module *spec.Module, paths []string) []string {
	files := make([]workspace.File, 0, len(paths))
	for _, path := range paths {
		files = append(files, workspace.File{Path: path, Content: "-"})
	}
	kept, _ := p.SplitModuleFiles(module, files, false)
	out := make([]string, 0, len(kept))
	for _, f := range kept {
		out = append(out, f.Path)
	}
	return out
}

// droppedConcerns turns refused files into report-visible concerns, so a
// model that tried to write a shared helper elsewhere, or a test, is seen
// rather than silently trimmed.
func droppedConcerns(agent string, dropped []lang.Dropped) []string {
	var out []string
	for _, d := range dropped {
		out = append(out, fmt.Sprintf("[aspect] %s proposed %s, refused: %s", agent, d.Path, d.Reason))
	}
	return out
}
