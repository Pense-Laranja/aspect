package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// placeholders are the stand-ins an agent file may leave in its example
// report; each is replaced by a valid value before the example is decoded.
var placeholders = strings.NewReplacer(
	`"critical|high|medium|low|info"`, `"medium"`,
	"<base-sha>", "c84528a",
	"<head-sha>", "9f3e1b2",
)

// TestAgentExampleReports decodes the example report in every agent file
// with the same functions `aspect gate check` uses. The model copies the
// example it is shown, so an example the validator rejects makes the agent
// write invalid reports, and an invalid report from a gate agent blocks the
// whole gate.
func TestAgentExampleReports(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", ".claude", "agents", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no agent files found: %v", err)
	}
	checked := 0
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".md")
		t.Run(name, func(t *testing.T) {
			text, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			block, err := LastJSONBlock(string(text))
			if err != nil {
				t.Skip("no example report: this agent writes no findings")
			}
			checked++
			r, err := DecodeReport([]byte(placeholders.Replace(string(block))))
			if err != nil {
				t.Fatalf("example report is invalid: %v", err)
			}
			if r.Agent != name {
				t.Errorf("example report names agent %q, want %q", r.Agent, name)
			}
			if len(r.Findings) == 0 {
				t.Error("example report shows no finding, so it does not show the finding shape")
			}
			for _, f := range r.Findings {
				if want := name + "/"; !strings.HasPrefix(f.ID, want) {
					t.Errorf("finding id %q should start with %q", f.ID, want)
				}
			}
		})
	}
	if checked == 0 {
		t.Error("no agent file has an example report; the extraction is broken")
	}
}

func TestAgentPromptsAreSelfContainedAndScoped(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", ".claude", "agents", "*.md"))
	if err != nil {
		t.Fatalf("glob agent files: %v", err)
	}
	if len(files) < 19 {
		t.Fatalf("expected at least 19 agent files, got %d", len(files))
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".md")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			text := string(b)
			if strings.Contains(text, "Read .claude/review/PROTOCOL.md") || strings.Contains(text, "Read `.claude/review/PROTOCOL.md`") {
				t.Errorf("%s still instructs reading .claude/review/PROTOCOL.md", name)
			}
			if strings.Contains(text, "go test ./...") {
				t.Errorf("%s contains unscoped 'go test ./...'", name)
			}
			if !strings.Contains(text, "files:") {
				t.Errorf("%s does not document 'files:' input for scoped diff execution", name)
			}
			if !strings.Contains(text, "aspect-review/v1") {
				t.Errorf("%s does not document 'aspect-review/v1' schema", name)
			}
		})
	}
}
