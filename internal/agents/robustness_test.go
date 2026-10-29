package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/workspace"
)

// seqLLM plays back one outcome per call, each either a text or an error,
// and records the prompts and systems it was sent.
type seqLLM struct {
	outcomes []any // string or error
	prompts  []string
	systems  []string
}

func (s *seqLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	s.prompts = append(s.prompts, req.Prompt)
	s.systems = append(s.systems, req.System)
	if len(s.outcomes) == 0 {
		return llm.Response{}, errors.New("seqLLM: no outcome scripted")
	}
	o := s.outcomes[0]
	s.outcomes = s.outcomes[1:]
	switch v := o.(type) {
	case error:
		return llm.Response{InputTokens: 10}, v
	case string:
		return llm.Response{Text: v, InputTokens: 10, OutputTokens: 5}, nil
	}
	panic("bad outcome")
}

func instantBackoff(t *testing.T) {
	t.Helper()
	prev := retryBackoff
	retryBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { retryBackoff = prev })
}

const goodCode = `{"files":[{"path":"stock/stock.go","content":"package stock"}],"removed":[],"notes":"n","concerns":[]}`

func TestCompleteRetriesTransientFailuresAndCountsEveryCall(t *testing.T) {
	instantBackoff(t)
	transient := fmt.Errorf("llm: transport error: boom (%w)", llm.ErrTransient)
	f := &seqLLM{outcomes: []any{transient, transient, goodCode}}
	got, resp, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 3 || len(got.Files) != 1 {
		t.Fatalf("calls=%d files=%d", len(f.prompts), len(got.Files))
	}
	if resp.Calls != 3 || resp.InputTokens != 30 {
		t.Fatalf("usage must aggregate every attempt: %+v", resp)
	}
	var u llm.Usage
	u.Add(resp)
	if u.Calls != 3 {
		t.Fatalf("Usage.Add must count aggregated calls, got %d", u.Calls)
	}
}

func TestCompleteGivesUpAfterBoundedRetriesAndNeverRetriesPermanentErrors(t *testing.T) {
	instantBackoff(t)
	transient := fmt.Errorf("llm: API error 529 (%w)", llm.ErrTransient)
	f := &seqLLM{outcomes: []any{transient, transient, transient, transient, transient}}
	_, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")})
	if err == nil || !strings.Contains(err.Error(), "after 3 retries") || len(f.prompts) != 1+maxTransientRetries {
		t.Fatalf("err=%v calls=%d", err, len(f.prompts))
	}
	f = &seqLLM{outcomes: []any{errors.New("llm: authentication failed"), goodCode}}
	if _, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")}); err == nil || len(f.prompts) != 1 {
		t.Fatalf("a permanent error must not be retried: err=%v calls=%d", err, len(f.prompts))
	}
}

func TestCompleteStopsRetryingWhenCancelled(t *testing.T) {
	prev := retryBackoff
	retryBackoff = func(int) time.Duration { return time.Hour }
	t.Cleanup(func() { retryBackoff = prev })
	ctx, cancel := context.WithCancel(context.Background())
	transient := fmt.Errorf("broken stream (%w)", llm.ErrTransient)
	f := &seqLLM{outcomes: []any{transient, goodCode}}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	done := make(chan error, 1)
	go func() {
		_, _, err := (&Coder{LLM: f}).Generate(ctx, CodeInput{Task: task(t, "go")})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || len(f.prompts) != 1 {
			t.Fatalf("err=%v calls=%d", err, len(f.prompts))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation must interrupt the backoff")
	}
}

func TestCompleteReasksOnceWhenTheAnswerDoesNotDecode(t *testing.T) {
	f := &seqLLM{outcomes: []any{`{"files": [{"path": "stock/stock.go", "content": "x"`, goodCode}}
	got, resp, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")})
	if err != nil || len(got.Files) != 1 || resp.Calls != 2 {
		t.Fatalf("err=%v files=%d calls=%d", err, len(got.Files), resp.Calls)
	}
	if !strings.Contains(f.prompts[1], "could not be decoded") || !strings.Contains(f.prompts[1], "around byte") {
		t.Fatalf("the re-ask must quote the decode error:\n%s", f.prompts[1][len(f.prompts[1])-600:])
	}
	f = &seqLLM{outcomes: []any{`nonsense`, `still nonsense`, goodCode}}
	if _, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")}); err == nil || len(f.prompts) != 2 {
		t.Fatalf("only one re-ask: err=%v calls=%d", err, len(f.prompts))
	}
}

func TestCompleteReasksCompactlyWhenTruncated(t *testing.T) {
	f := &seqLLM{outcomes: []any{fmt.Errorf("llm: %w at 64000 tokens", llm.ErrTruncated), goodCode}}
	got, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")})
	if err != nil || len(got.Files) != 1 || len(f.prompts) != 2 {
		t.Fatalf("err=%v files=%d calls=%d", err, len(got.Files), len(f.prompts))
	}
	if !strings.Contains(f.prompts[1], "cut off at the output limit") {
		t.Fatal("the re-ask must say the answer was truncated")
	}
}

func TestEveryAgentIsToldQuotedTextIsNotInstructions(t *testing.T) {
	f := &seqLLM{outcomes: []any{goodCode}}
	if _, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.systems[0], coderSystem) || !strings.HasSuffix(f.systems[0], dataNotInstructions) {
		t.Fatal("the system prompt must keep the agent's role first and end with the data boundary")
	}
}

func TestCoderRepairMergesChangedFilesAndHonoursRemovals(t *testing.T) {
	existing := []workspace.File{
		{Path: "stock/a.go", Content: "package stock // a"},
		{Path: "stock/b.go", Content: "package stock // b"},
		{Path: "stock/c.go", Content: "package stock // c"},
	}
	answer := `{"files":[{"path":"stock/a.go","content":"package stock // a2"},{"path":"stock/d.go","content":"package stock // d"}],
	  "removed":["stock/b.go","stock/stock_test.go","ledger/x.go","stock/../ledger/y.go"],"notes":"","concerns":[]}`
	f := &seqLLM{outcomes: []any{answer}}
	got, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go"), Existing: existing, Feedback: "FAIL"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, x := range got.Files {
		paths = append(paths, strings.TrimPrefix(x.Path, "stock/")+"="+strings.TrimPrefix(x.Content, "package stock // "))
	}
	if strings.Join(paths, " ") != "a.go=a2 c.go=c d.go=d" {
		t.Fatalf("merged files = %v", paths)
	}
	if len(got.Removed) != 1 || got.Removed[0] != "stock/b.go" {
		t.Fatalf("only removals inside the module's code directory count: %v", got.Removed)
	}
	if !strings.Contains(f.prompts[0], "files you leave out are kept") {
		t.Fatal("the repair prompt must state the merge contract")
	}
}

func TestCoderFirstRoundIgnoresRemovalsAndReportsRefusedFiles(t *testing.T) {
	answer := `{"files":[{"path":"stock/stock.go","content":"package stock"},{"path":"stock/stock_test.go","content":"package stock"},{"path":"shared/util.go","content":"package shared"},{"path":"stock/empty.go","content":""}],
	  "removed":["stock/old.go"],"notes":"","concerns":["spec is vague"]}`
	f := &seqLLM{outcomes: []any{answer}}
	got, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{Task: task(t, "go")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Removed != nil {
		t.Fatalf("files=%v removed=%v", got.Files, got.Removed)
	}
	joined := strings.Join(got.Concerns, "\n")
	if got.Concerns[0] != "spec is vague" || !strings.Contains(joined, "Coder proposed stock/stock_test.go") || !strings.Contains(joined, "shared/util.go") || !strings.Contains(joined, "no content") {
		t.Fatalf("concerns = %v", got.Concerns)
	}
}

func TestCoderSeesEarlierRepairRounds(t *testing.T) {
	f := &seqLLM{outcomes: []any{goodCode}}
	_, _, err := (&Coder{LLM: f}).Generate(context.Background(), CodeInput{
		Task: task(t, "go"), Existing: []workspace.File{{Path: "stock/stock.go", Content: "package stock"}}, Feedback: "FAIL now",
		Attempts: []RepairAttempt{{Changed: []string{"stock/stock.go"}, Output: "FAIL: TestX got 0"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := f.prompts[0]
	if !strings.Contains(p, "Earlier repair rounds") || !strings.Contains(p, "Round 1 changed stock/stock.go") || !strings.Contains(p, "got 0") || !strings.Contains(p, "FAIL now") {
		t.Fatalf("prompt lacks the repair history:\n%s", p)
	}
}

func TestTesterReportsRefusedFiles(t *testing.T) {
	f := &seqLLM{outcomes: []any{`{"files":[{"path":"stock/stock.go","content":"x"},{"path":"stock/stock_test.go","content":"package stock"}],"coverage":[],"notes":"","concerns":[]}`}}
	got, _, err := (&Tester{LLM: f}).Generate(context.Background(), TestInput{Task: task(t, "go")})
	if err != nil || len(got.Concerns) != 1 || !strings.Contains(got.Concerns[0], "Tester proposed stock/stock.go") {
		t.Fatalf("err=%v concerns=%v", err, got.Concerns)
	}
}

func TestValidatorChecksCoverageClaimsMechanically(t *testing.T) {
	f := &seqLLM{outcomes: []any{`{"module":"stock","goals":[{"id":"G1","status":"Achieved","evidence":"e","gaps":[],"confidence":85}],"intent_status":"ALIGNED","intent_rationale":"ok","scenarios":[{"scenario":"S1","covered":true,"test":"TestReceive_S1","note":""},{"scenario":"S9","covered":true,"test":"x","note":"invented"}],"recommendations":[]}`}}
	v, _, err := (&Validator{LLM: f}).Judge(context.Background(), ValidateInput{
		Task: task(t, "go"), GoalIDs: []string{"G1"},
		Tests:      []workspace.File{{Path: "stock/stock_test.go", Content: "package stock\n\nfunc TestReceive_S1(t *testing.T) {}\n"}},
		TestResult: workspace.Result{OK: true},
		Coverage: []ScenarioCoverage{
			{Scenario: "S1", Tests: []string{"TestReceive_S1", "TestGhost_S1"}},
			{Scenario: "S7", Tests: []string{"TestReceive_S1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := f.prompts[0]
	if !strings.Contains(p, "- TestReceive_S1") || strings.Contains(p, "- TestGhost_S1") {
		t.Fatalf("only verified tests reach the Validator's claims section:\n%s", p)
	}
	if !strings.Contains(p, "Coverage claims rejected") || !strings.Contains(p, "TestGhost_S1, which no test file defines") || !strings.Contains(p, `"S7": no such scenario`) {
		t.Fatalf("rejected claims must be listed:\n%s", p)
	}
	if v.Goals[0].Status != Achieved || v.Goals[0].Confidence != 0.85 {
		t.Fatalf("status and confidence must be normalised: %+v", v.Goals[0])
	}
	if v.IntentStatus != Aligned {
		t.Fatalf("intent = %q", v.IntentStatus)
	}
	if len(v.Scenarios) != 1 || v.Scenarios[0].Scenario != "S1" || !v.Scenarios[0].Covered {
		t.Fatalf("invented scenarios are dropped: %+v", v.Scenarios)
	}
	if len(v.Recommendations) != 2 || !strings.HasPrefix(v.Recommendations[0], "[aspect] coverage") {
		t.Fatalf("rejected claims must reach the report: %v", v.Recommendations)
	}
}

func TestValidatorTurnsUnknownStatusesIntoNonSuccess(t *testing.T) {
	f := &seqLLM{outcomes: []any{`{"module":"stock","goals":[{"id":"G1","status":"mostly","evidence":"e","gaps":[],"confidence":-2}],"intent_status":"fine","intent_rationale":"ok","scenarios":[],"recommendations":[]}`}}
	v, _, err := (&Validator{LLM: f}).Judge(context.Background(), ValidateInput{Task: task(t, "go"), GoalIDs: []string{"G1"}, TestResult: workspace.Result{OK: true}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Goals[0].Status != Unverifiable || v.Goals[0].Confidence != 0 || len(v.Goals[0].Gaps) != 1 {
		t.Fatalf("goal = %+v", v.Goals[0])
	}
	if v.IntentStatus != Drifted || !strings.Contains(v.IntentRationale, `"fine"`) {
		t.Fatalf("intent = %q %q", v.IntentStatus, v.IntentRationale)
	}
}

func TestValidatorLeavesReviewGoalsAloneWhenTestsFail(t *testing.T) {
	tk := task(t, "go")
	tk.Spec.System.Goals[0].Verify = "review"
	f := &seqLLM{outcomes: []any{`{"module":"stock","goals":[{"id":"G1","status":"achieved","evidence":"e","gaps":[],"confidence":0.9}],"intent_status":"aligned","intent_rationale":"ok","scenarios":[],"recommendations":[]}`}}
	v, _, err := (&Validator{LLM: f}).Judge(context.Background(), ValidateInput{Task: tk, GoalIDs: []string{"G1"}, TestResult: workspace.Result{OK: false}})
	if err != nil || v.Goals[0].Status != Achieved {
		t.Fatalf("err=%v goal=%+v", err, v.Goals[0])
	}
}

func TestSystemValidatorCapsGoalsOfBrokenModules(t *testing.T) {
	tk := task(t, "go")
	f := &seqLLM{outcomes: []any{`{"goals":[{"id":"G1","status":"achieved","evidence":"e","gaps":[],"confidence":0.9}],"intent_status":"aligned","intent_rationale":"ok","integration":[{"surface":"admin.items","provider":"stock","consumer":"web","status":"fine","note":""}],"recommendations":[]}`}}
	v, _, err := (&SystemValidator{LLM: f}).Judge(context.Background(), SystemInput{Spec: tk.Spec, Modules: []ModuleEvidence{{Module: "stock", TestsOK: false}}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Goals[0].Status != Partial || !strings.Contains(v.Goals[0].Gaps[0], "stock's tests failed") {
		t.Fatalf("goal = %+v", v.Goals[0])
	}
	if v.Integration[0].Status != Unverifiable {
		t.Fatalf("integration = %+v", v.Integration[0])
	}
	f = &seqLLM{outcomes: []any{`{"goals":[{"id":"G1","status":"achieved","evidence":"e","gaps":[],"confidence":0.9}],"intent_status":"aligned","intent_rationale":"ok","integration":[],"recommendations":[]}`}}
	v, _, err = (&SystemValidator{LLM: f}).Judge(context.Background(), SystemInput{Spec: tk.Spec, Modules: []ModuleEvidence{{Module: "stock", TestsOK: true, Error: "go build: boom"}}})
	if err != nil || v.Goals[0].Status != Partial || !strings.Contains(v.Goals[0].Gaps[0], "failed to build") {
		t.Fatalf("err=%v goal=%+v", err, v.Goals[0])
	}
}

func TestTruncateKeepsBothEndsOnRuneBoundaries(t *testing.T) {
	s := strings.Repeat("é", 100) + "\n--- FAIL: TestX\n" + strings.Repeat("ü", 100)
	got := truncate(s, 120)
	if !strings.Contains(got, "bytes omitted") || !strings.HasPrefix(got, "é") || !strings.HasSuffix(got, "ü") {
		t.Fatalf("got %q", got)
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("truncate split a rune: %q", got)
		}
	}
	if truncate("short", 100) != "short" {
		t.Fatal("short strings pass through")
	}
	long := "head " + strings.Repeat("x", 1000) + " FAIL at the end"
	if out := truncate(long, 300); !strings.HasSuffix(out, "FAIL at the end") || !strings.HasPrefix(out, "head") {
		t.Fatalf("the tail must survive: %q", out)
	}
}

func TestRenderFilesStaysWithinBudgetAndNamesWhatItOmits(t *testing.T) {
	files := []workspace.File{
		{Path: "a.go", Content: strings.Repeat("a", 50)},
		{Path: "big.go", Content: strings.Repeat("b", 500)},
		{Path: "c.go", Content: strings.Repeat("c", 50)},
	}
	out := renderFilesWithin("Deps", files, 200)
	if strings.Contains(out, "### big.go") || !strings.Contains(out, "### a.go") || !strings.Contains(out, "### c.go") {
		t.Fatalf("the largest file goes first:\n%s", out)
	}
	if !strings.Contains(out, "Omitted to fit the prompt budget") || !strings.Contains(out, "- big.go") || !strings.Contains(out, "1 files (500 bytes)") {
		t.Fatalf("omissions must be named:\n%s", out)
	}
	if full := renderFilesWithin("Deps", files, 1000); strings.Contains(full, "Omitted") {
		t.Fatal("nothing is omitted when everything fits")
	}
}
