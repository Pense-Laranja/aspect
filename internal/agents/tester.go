package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/Pense-Laranja/aspect/internal/lang"
	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/workspace"
)

// TestInput is what the Tester sees.
type TestInput struct {
	Task
	Code         []workspace.File
	Dependencies []workspace.File
	// Feedback is set when a previous proposal was rejected (for example for
	// importing a library the spec does not allow) and the Tester gets one
	// more attempt.
	Feedback string
	Existing []workspace.File
}

// ScenarioCoverage maps a spec scenario to the tests that exercise it.
type ScenarioCoverage struct {
	Scenario string   `json:"scenario"`
	Tests    []string `json:"tests"`
}

// TestOutput is the Tester's proposal.
type TestOutput struct {
	Files    []workspace.File   `json:"files"`
	Coverage []ScenarioCoverage `json:"coverage"`
	Notes    string             `json:"notes"`
	Concerns []string           `json:"concerns"`
}

// Tester writes tests from scenarios and invariants, not from the code. The
// code is shown only so tests compile against real names.
type Tester struct {
	LLM llm.Client
}

const testerSystem = `You are the Tester agent in Aspect, a pipeline that builds software from a formal specification.

Your tests are the executable form of the specification. Derive them from the scenarios, invariants, and pre/post conditions in the spec, not from what the implementation happens to do. The implementation is shown so your tests compile against its real identifiers; if it disagrees with the spec, the spec wins and the test should fail.

Rules:
- Every scenario gets at least one test whose name includes the scenario id (TestReserve_S2). Report the mapping in coverage.
- Every invariant gets a property-style test: drive the module through many operation sequences and assert the invariant after each step.
- Entities owned by the module get tests against the database configured for tests in the spec (an in-memory database when the test engine is sqlite; the DSN from the named environment variable when it is set). Never require a service the spec does not promise.
- Surfaces get tests that exercise them the way a client would: HTTP handlers through an in-process test server, commands through their entry point, pages through their rendered output or view model.
- Only write test files inside the module's test directory. Never modify implementation files.
- Use only the libraries the stack section allows. The pipeline checks imports mechanically.
- Answer with JSON matching the schema you were given.`

var testSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"files", "coverage", "notes", "concerns"},
	"properties": map[string]any{
		"files": filesSchema(),
		"coverage": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"scenario", "tests"},
				"properties": map[string]any{
					"scenario": map[string]any{"type": "string"},
					"tests":    stringList(),
				},
			},
		},
		"notes":    map[string]any{"type": "string"},
		"concerns": stringList(),
	},
}

// Generate writes the module's tests.
func (t *Tester) Generate(ctx context.Context, in TestInput) (TestOutput, llm.Response, error) {
	var b strings.Builder
	b.WriteString(renderContext(in.Task))
	b.WriteString(renderModule(in.Task, "Module under test"))
	b.WriteString(renderFiles("Dependencies", in.Dependencies))
	b.WriteString(renderFiles("Implementation (for identifiers only)", in.Code))
	if in.Feedback != "" {
		b.WriteString(renderFiles("Your previous tests (rejected)", in.Existing))
		fmt.Fprintf(&b, "## Why they were rejected\n\n```\n%s\n```\n\nRewrite the tests without the rejected dependencies. Return every test file.\n", truncate(in.Feedback, 8000))
	} else {
		b.WriteString("Write the tests that would convince a sceptical reviewer the module meets its spec.\n")
	}

	var out TestOutput
	system := testerSystem + "\n\n" + in.Lang.TesterRules
	resp, err := complete(ctx, t.LLM, system, b.String(), testSchema, &out)
	if err != nil {
		return out, resp, fmt.Errorf("tester: %w", err)
	}
	kept, dropped := in.Lang.SplitModuleFiles(in.Module, out.Files, true)
	out.Concerns = append(out.Concerns, droppedConcerns("Tester", dropped)...)
	out.Files = kept
	if len(out.Files) == 0 {
		return out, resp, fmt.Errorf("tester: returned no test files inside %s", lang.DirLabel(in.Lang.TestDir(in.Module)))
	}
	return out, resp, nil
}
