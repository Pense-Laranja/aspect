package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eideroliveira/aspect/internal/gate"
	"github.com/eideroliveira/aspect/internal/gitx"
)

// reusableOutcomes are the outcomes of a run that finished with a verdict.
var reusableOutcomes = []string{OutcomePass, OutcomeWarn, OutcomeBlock, OutcomeOverridden}

// findCachedRun is the latest finished run of the same diff whose every
// planned agent reported, or "". The run current_run.json names is tried
// first, then the local-* runs newest first.
func (e *Engine) findCachedRun(ctx context.Context, repo string, plan *gate.Plan, diff, diffHash string) string {
	reviewDir := filepath.Join(repo, ".review")
	var candidates []string
	if b, err := os.ReadFile(filepath.Join(reviewDir, "current_run.json")); err == nil {
		var cur struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal(b, &cur) == nil && cur.RunID != "" && filepath.Base(cur.RunID) == cur.RunID {
			if fi, err := os.Stat(filepath.Join(reviewDir, cur.RunID)); err == nil && fi.IsDir() {
				candidates = append(candidates, filepath.Join(reviewDir, cur.RunID))
			}
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(reviewDir, runDirPrefix+"*"))
	slices.Sort(dirs)
	slices.Reverse(dirs)
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && !slices.Contains(candidates, d) {
			candidates = append(candidates, d)
		}
	}

	for _, dir := range candidates {
		var status struct {
			State    string `json:"state"`
			Outcome  string `json:"outcome"`
			DiffHash string `json:"diff_hash"`
			Agents   map[string]struct {
				State string `json:"state"`
			} `json:"agents"`
		}
		var prev struct {
			Base string `json:"base"`
			Head string `json:"head"`
		}
		sb, err1 := os.ReadFile(filepath.Join(dir, "status.json"))
		pb, err2 := os.ReadFile(filepath.Join(dir, "plan.json"))
		if err1 != nil || err2 != nil || json.Unmarshal(sb, &status) != nil || json.Unmarshal(pb, &prev) != nil {
			continue
		}
		if status.State != stateCompleted || !slices.Contains(reusableOutcomes, strings.ToUpper(status.Outcome)) {
			continue
		}
		complete := true
		for _, a := range plan.Agents {
			if status.Agents[a.Agent].State != stateCompleted {
				complete = false
				break
			}
			if _, err := os.Stat(filepath.Join(dir, a.Agent+".json")); err != nil {
				complete = false
				break
			}
		}
		if !complete || prev.Head != plan.Head {
			continue
		}
		prevHash := ""
		if b, err := os.ReadFile(filepath.Join(dir, "diff.hash")); err == nil {
			prevHash = strings.TrimSpace(string(b))
		}
		if prevHash == "" {
			prevHash = status.DiffHash
		}
		if prevHash != "" {
			if prevHash == diffHash {
				return dir
			}
			continue
		}
		// A run from before diff hashes were recorded.
		if prev.Base == plan.Base {
			_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)
			return dir
		}
		if prev.Base != "" {
			if d, err := gitx.Run(ctx, repo, "diff", prev.Base+"..."+prev.Head); err == nil && strings.TrimSpace(d) == diff {
				_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)
				return dir
			}
		}
	}
	return ""
}

// reuse returns a finished run's result again, publishing it to the pull
// request when asked and not yet published there.
func (e *Engine) reuse(ctx context.Context, repo, dir string, cfg *gate.Config, plan *gate.Plan, pr PullRequest, publish bool) (*Result, error) {
	runID := filepath.Base(dir)
	statusPath := filepath.Join(dir, "status.json")
	status := NewObject()
	if b, err := os.ReadFile(statusPath); err == nil {
		if v, err := parseValue(b); err == nil {
			if o, ok := v.(*Object); ok {
				status = o
			}
		}
	}
	// Published again when the pull request is new to the run or the run was
	// never published. With no pull request found, publish.sh is still run
	// (it finds the branch's own) but nothing is recorded, so a later run
	// that finds one publishes too.
	published := asText(status.Get("published_status"))
	if publish && (pr.Number == "" || asText(status.Get("pr")) != pr.Number || published == "") {
		if _, err := os.Stat(filepath.Join(repo, ".claude", "review", "publish.sh")); err == nil {
			published = e.publish(ctx, repo, dir, pr.Number)
			if pr.Number != "" {
				status.Set("pr", pr.Number)
				status.Set("pr_url", nullable(pr.URL))
				status.Set("published_status", published)
				_ = writeJSON(statusPath, status)
			}
		}
	}

	// The plan the run was reviewed under, which may list other agents than
	// today's.
	runPlan := plan
	if b, err := os.ReadFile(filepath.Join(dir, "plan.json")); err == nil {
		var p gate.Plan
		if json.Unmarshal(b, &p) == nil {
			runPlan = &p
		}
	}
	check, rendered := e.check(cfg, runPlan, dir)
	for _, name := range []string{"comment.md", "rendered.md"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil && len(b) > 0 {
			rendered = string(b)
			break
		}
	}
	outcome := strings.ToUpper(asText(status.Get("outcome")))
	if outcome == "" {
		outcome = strings.ToUpper(check.Outcome)
	}
	if outcome == "" {
		outcome = OutcomePass
	}
	writeCurrentRun(filepath.Join(repo, ".review"), runID, stateCompleted, e.now())
	return &Result{
		RunID: runID, RunDir: dir, Cached: true,
		PR: firstNonEmpty(pr.Number, asText(status.Get("pr"))), PRURL: firstNonEmpty(pr.URL, asText(status.Get("pr_url"))),
		Plan: runPlan, Outcome: outcome, Rendered: rendered, Published: published,
	}, nil
}
