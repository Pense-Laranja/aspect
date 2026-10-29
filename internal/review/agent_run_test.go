package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A run of one agent alone is a record of its own: it reviews with that
// agent only, under agent-<time>, and is never what the gates' readers take
// for the change's run (current_run.json, the run list, the cache), so a
// single reviewer's verdict cannot pass for the gates'.
func TestAgentRunIsKeptApartFromTheGatesRuns(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, map[string]any{"security-reviewer": clean, "red-team": clean})

	var started string
	res := te.run(t, Options{Agent: "red-team", Started: func(id string) { started = id }})
	if !strings.HasPrefix(res.RunID, agentRunPrefix) {
		t.Fatalf("run id %q, want the %q prefix", res.RunID, agentRunPrefix)
	}
	if started != res.RunID {
		t.Errorf("Started got %q, want the run id %q", started, res.RunID)
	}
	if got := te.model.calls; len(got) != 1 || got[0] != "red-team" {
		t.Errorf("agents called %v, want red-team alone", got)
	}
	if res.Outcome != OutcomePass {
		t.Errorf("outcome %q, want PASS for a clean report", res.Outcome)
	}
	if _, err := os.Stat(filepath.Join(repo, ".review", "current_run.json")); !os.IsNotExist(err) {
		t.Errorf("an agent run wrote current_run.json (err %v)", err)
	}
	if runs, err := LoadRuns(repo, 0); err != nil || len(runs) != 0 {
		t.Errorf("LoadRuns = %d runs (err %v), want none: an agent run is not a gates run", len(runs), err)
	}
	if r, err := LoadRun(repo, res.RunID); err != nil || r.RunID != res.RunID {
		t.Errorf("LoadRun(%q) = %v, %v; an agent run is still read by its id", res.RunID, r, err)
	}
	var plan struct {
		Agents []struct {
			Agent string `json:"agent"`
		} `json:"agents"`
	}
	b, _ := os.ReadFile(filepath.Join(res.RunDir, "plan.json"))
	if json.Unmarshal(b, &plan) != nil || len(plan.Agents) != 1 || plan.Agents[0].Agent != "red-team" {
		t.Errorf("plan.json = %s, want red-team alone", b)
	}

	// The gates' run of the same diff reviews afresh, with every agent.
	full := te.run(t, Options{})
	if full.Cached || full.CarriedFrom != "" || !strings.HasPrefix(full.RunID, runDirPrefix) {
		t.Errorf("full run %q cached=%v carried=%q: an agent run was reused for the gates", full.RunID, full.Cached, full.CarriedFrom)
	}
	if n := te.model.callCount(); n != 3 {
		t.Errorf("%d model calls in all, want 1 + 2", n)
	}

	// And an agent run is never served from the gates' run either.
	again := te.run(t, Options{Agent: "security-reviewer"})
	if again.Cached || !strings.HasPrefix(again.RunID, agentRunPrefix) {
		t.Errorf("agent run %q cached=%v, want a run of its own", again.RunID, again.Cached)
	}
}

func TestAgentRunOfAnAgentOutsideThePlanIsRefused(t *testing.T) {
	repo := testRepo(t)
	te := newTestEngine(t, repo, nil)
	_, err := te.Run(t.Context(), Options{Repo: repo, Base: "main", Agent: "architect"})
	if err == nil || !strings.Contains(err.Error(), `"architect" is not in this change's gate plan`) || !strings.Contains(err.Error(), "red-team") {
		t.Fatalf("err = %v, want a refusal naming the planned agents", err)
	}
	if te.model.callCount() != 0 {
		t.Error("a refused agent run called the model")
	}
}
