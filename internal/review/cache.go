package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/eideroliveira/aspect/internal/gate"
	"github.com/eideroliveira/aspect/internal/gitx"
)

// reusableOutcomes are the outcomes of a run that finished with a verdict.
var reusableOutcomes = []string{OutcomePass, OutcomeWarn, OutcomeBlock, OutcomeOverridden}

// findCachedRun is the latest finished run of the same diff whose every
// planned agent reported, or "". The run current_run.json names is tried
// first, then the local-* runs newest first.
//
// A run of the same head is reused as it is. Failing that, carried is true
// and dir is a run whose verdict may be carried to plan's head: it judged an
// earlier head of the branch (an ancestor of this one), its diff against its
// merge base hashes the same, context lines included, and its reviewers were
// given what this head's would be (inputsHash: gates.yaml, the agents'
// definitions, the spec each is shown). That is a branch that only merged
// its base in, cleanly: the change, and every question asked about it, are
// the ones already answered. A run from before inputs hashes were recorded
// is never carried.
func (e *Engine) findCachedRun(ctx context.Context, repo string, plan *gate.Plan, diff, diffHash, inputsHash string) (dir string, carried bool) {
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

	carry := ""
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
		if !complete {
			continue
		}
		prevHash := ""
		if b, err := os.ReadFile(filepath.Join(dir, "diff.hash")); err == nil {
			prevHash = strings.TrimSpace(string(b))
		}
		if prevHash == "" {
			prevHash = status.DiffHash
		}
		if prev.Head != plan.Head {
			if carry == "" && prevHash == diffHash && prev.Head != "" &&
				readTrimmed(filepath.Join(dir, inputsHashFile)) == inputsHash && isAncestor(ctx, repo, prev.Head, plan.Head) {
				carry = dir
			}
			continue
		}
		if prevHash != "" {
			if prevHash == diffHash {
				return dir, false
			}
			continue
		}
		// A run from before diff hashes were recorded.
		if prev.Base == plan.Base {
			_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)
			return dir, false
		}
		if prev.Base != "" {
			if d, err := gitx.Run(ctx, repo, "diff", prev.Base+"..."+prev.Head); err == nil && strings.TrimSpace(d) == diff {
				_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)
				return dir, false
			}
		}
	}
	return carry, carry != ""
}

// isAncestor reports whether commit a is an ancestor of b (or b itself).
func isAncestor(ctx context.Context, repo, a, b string) bool {
	_, err := gitx.Run(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// inputsHashFile records, in each run, what its reviewers were given apart
// from the diff (inputsHash).
const inputsHashFile = "inputs.hash"

// inputsHash is what a plan's reviewers are given apart from the diff and
// the pull request's description: gates.yaml, and for each planned agent its
// mode, files and modules, its definition, and the spec it is shown, with
// the note saying what of the spec was not. A run whose inputs hash and diff
// hash are this head's asked the model the same questions about the same
// change.
func (e *Engine) inputsHash(repo string, plan *gate.Plan, specOf func(gate.Planned) string) string {
	h := sha256.New()
	field := func(s string) {
		fmt.Fprintf(h, "%d:%s\n", len(s), s)
	}
	gates, _ := os.ReadFile(e.gatesPath(repo))
	field(string(gates))
	field(plan.Spec)
	field(plan.SpecNote)
	for _, a := range plan.Agents {
		def, _ := e.agentDefinition(repo, a.Agent)
		field(a.Agent)
		field(a.Mode)
		field(strings.Join(a.Files, "\x00"))
		field(strings.Join(a.Modules, "\x00"))
		field(def)
		field(specOf(a))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// carry makes a run of plan's head out of from, a finished run of an earlier
// head whose verdict findCachedRun found may be carried: the agents' reports
// and usage as they were, still naming the range they reviewed, plan.json
// naming this head and the run it carries (gate.Plan.CarriedFrom, which
// gate.Check accepts those reports under and the rendered result shows), and from's
// status with this head's run id, refs and time, not yet published. The new
// run is then reused like any other, so it is what publish.sh posts on this
// head: the record names the head it is for, and the run it came from.
func (e *Engine) carry(repo, from string, plan *gate.Plan, diffHash, inputsHash string) (string, error) {
	var prev struct {
		Base string `json:"base"`
		Head string `json:"head"`
	}
	pb, err := os.ReadFile(filepath.Join(from, "plan.json"))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(pb, &prev); err != nil {
		return "", err
	}
	runID, dir, err := e.newRunDir(repo)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return "", err
	}
	// What the run records about itself is written anew below; comment.md
	// and rendered.md name the head they judged, and are rendered again.
	skip := []string{"plan.json", "status.json", "diff.hash", inputsHashFile, "comment.md", "rendered.md"}
	for _, ent := range entries {
		if !ent.Type().IsRegular() || slices.Contains(skip, ent.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, ent.Name()))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, ent.Name()), b, 0o644); err != nil {
			return "", err
		}
	}
	p := *plan
	p.CarriedFrom = &gate.Carried{Run: filepath.Base(from), Base: prev.Base, Head: prev.Head}
	if err := writeJSON(filepath.Join(dir, "plan.json"), &p); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)
	_ = os.WriteFile(filepath.Join(dir, inputsHashFile), []byte(inputsHash), 0o644)

	status := NewObject()
	if b, err := os.ReadFile(filepath.Join(from, "status.json")); err == nil {
		if v, err := parseValue(b); err == nil {
			if o, ok := v.(*Object); ok {
				status = o
			}
		}
	}
	now := e.now().UTC().Format(time.RFC3339)
	status.Set("run_id", runID)
	status.Set("base", plan.Base)
	status.Set("head", plan.Head)
	status.Set("diff_hash", diffHash)
	status.Set("started_at", now)
	status.Set("carried_from", filepath.Base(from))
	status.Set("carried_from_head", prev.Head)
	status.Set("pr", nil)
	status.Set("pr_url", nil)
	status.Set("published_status", nil)
	for _, f := range records(status, "findings") {
		f.Set("run_id", runID)
	}
	if err := writeJSON(filepath.Join(dir, "status.json"), status); err != nil {
		return "", err
	}
	return dir, nil
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
	carriedFrom := ""
	if runPlan.CarriedFrom != nil {
		carriedFrom = runPlan.CarriedFrom.Run
	}
	return &Result{
		RunID: runID, RunDir: dir, Cached: true, CarriedFrom: carriedFrom,
		PR: firstNonEmpty(pr.Number, asText(status.Get("pr"))), PRURL: firstNonEmpty(pr.URL, asText(status.Get("pr_url"))),
		Plan: runPlan, Outcome: outcome, Rendered: rendered, Published: published,
	}, nil
}
