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

	"github.com/Pense-Laranja/aspect/internal/gate"
	"github.com/Pense-Laranja/aspect/internal/gitx"
)

// reusableOutcomes are the outcomes of a run that finished with a verdict.
var reusableOutcomes = []string{OutcomePass, OutcomeWarn, OutcomeBlock, OutcomeOverridden}

func candidateRunDirs(repo string) []string {
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
	return candidates
}

// findCachedRun is the latest finished run whose every planned agent
// reported and whose verdict holds for this head, or "". The run
// current_run.json names is tried first, then the local-* runs newest first.
//
// A run of the same head and diff is reused as it is. Failing that, carried is true
// and dir is a run whose verdict may be carried to plan's head: it judged an
// earlier head of the branch (an ancestor of this one), its diff against its
// merge base hashes the same, context lines included, and its reviewers were
// given what this head's would be (inputsHash: gates.yaml, the agents'
// definitions, the spec each is shown). That is a branch that only merged
// its base in, cleanly: the change, and every question asked about it, are
// the ones already answered. A run from before inputs hashes were recorded
// is never carried.
func (e *Engine) findCachedRun(ctx context.Context, repo string, plan *gate.Plan, diff, diffHash, inputsHash string) (dir string, carried bool) {
	candidates := candidateRunDirs(repo)

	carryFrom := ""
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
			if carryFrom == "" && carriable(ctx, repo, dir, prev.Head, plan.Head, prevHash, diffHash, inputsHash) {
				carryFrom = dir
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
	return carryFrom, carryFrom != ""
}

// carriable says whether the finished run in dir, which judged prevHead, may
// carry its verdict to head: the same diff (prevHash), the same reviewer
// inputs (its inputs.hash; a run without one never carries), and a head that
// descends from the one it judged.
func carriable(ctx context.Context, repo, dir, prevHead, head, prevHash, diffHash, inputsHash string) bool {
	prevRunHash, _, _ := readInputsHashFile(filepath.Join(dir, inputsHashFile))
	return prevHead != "" && prevHash != "" && prevHash == diffHash &&
		prevRunHash == inputsHash &&
		isAncestor(ctx, repo, prevHead, head)
}

// isAncestor reports whether commit a is an ancestor of b (or b itself).
// Any failure of git reads as "no": a verdict is never carried on a doubt.
func isAncestor(ctx context.Context, repo, a, b string) bool {
	_, err := gitx.Run(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// inputsHashFile records, in each run, what its reviewers were given apart
// from the diff (inputsHash), followed by optional per-agent input and
// scoped-diff hashes (`agent:<name>=<hash>` and `diff:<name>=<hash>`).
const inputsHashFile = "inputs.hash"

// readInputsHashFile parses inputs.hash. Line 1 is the run-wide inputsHash;
// optional subsequent lines record `agent:<name>=<hash>` and `diff:<name>=<hash>`.
func readInputsHashFile(path string) (runHash string, agentInputs map[string]string, agentDiffs map[string]string) {
	agentInputs = map[string]string{}
	agentDiffs = map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", agentInputs, agentDiffs
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 0 {
		runHash = strings.TrimSpace(lines[0])
	}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "agent:"):
			rest := strings.TrimPrefix(line, "agent:")
			if name, hash, ok := strings.Cut(rest, "="); ok && name != "" && hash != "" {
				agentInputs[name] = hash
			}
		case strings.HasPrefix(line, "diff:"):
			rest := strings.TrimPrefix(line, "diff:")
			if name, hash, ok := strings.Cut(rest, "="); ok && name != "" && hash != "" {
				agentDiffs[name] = hash
			}
		}
	}
	return runHash, agentInputs, agentDiffs
}

// inputsHash is what a plan's reviewers are given apart from the diff and
// the pull request's description: gates.yaml, and for each planned agent its
// mode, files and modules, its definition, and the spec it is shown, with
// the note saying what of the spec was not. A run whose inputs hash and diff
// hash are this head's asked the model the same questions about the same
// change. It names runAgent's inputs rather than hashing the prompt runAgent
// builds, which splits the diff into parts and adds the pull request's
// description: a new input to the prompt belongs here too.
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

// agentInputHash is what one planned agent is given apart from its diff and
// the pull request's description: the thresholds and gate for that agent,
// the plan's spec and spec note, the agent's mode, modules, definition, and
// rendered spec context. Note: a.Files is intentionally excluded from
// agentInputHash so Tier 2 can compare non-file inputs across runs where
// the branch touched an additional file; Tier 1 checks slices.Equal(a.Files, prevAgent.Files)
// and agentDiffHash separately.
func (e *Engine) agentInputHash(repo string, cfg *gate.Config, plan *gate.Plan, a gate.Planned, specContext string) string {
	h := sha256.New()
	field := func(s string) {
		fmt.Fprintf(h, "%d:%s\n", len(s), s)
	}
	tb, _ := json.Marshal(cfg.Thresholds)
	field(string(tb))
	if g, ok := cfg.GateFor(a.Agent); ok {
		gb, _ := json.Marshal(g)
		field(string(gb))
	}
	field(plan.Spec)
	field(plan.SpecNote)
	def, _ := e.agentDefinition(repo, a.Agent)
	field(a.Agent)
	field(a.Mode)
	field(strings.Join(a.Modules, "\x00"))
	field(def)
	field(specContext)
	return hex.EncodeToString(h.Sum(nil))
}

// agentDiffHash hashes the exact diff that runAgent sends to a.
func (e *Engine) agentDiffHash(ctx context.Context, repo string, plan *gate.Plan, a gate.Planned, fullDiff string) (string, error) {
	agentDiff := fullDiff
	if len(a.Files) > 0 {
		args := append([]string{"--literal-pathspecs", "diff", plan.Base + "..." + plan.Head, "--"}, a.Files...)
		d, err := gitx.Run(ctx, repo, args...)
		if err != nil {
			return "", err
		}
		agentDiff = strings.TrimSpace(d)
	}
	sum := sha256.Sum256([]byte(agentDiff))
	return hex.EncodeToString(sum[:]), nil
}

// buildInputsHashFile formats inputs.hash with the run-wide inputsHash on line 1
// followed by per-agent input and diff hashes. For carried agents, the diff hash
// of the origin commit they actually reviewed is preserved.
func (e *Engine) buildInputsHashFile(ctx context.Context, repo string, cfg *gate.Config, plan *gate.Plan, fullDiff, inputsHash string, specOf func(gate.Planned) string, carried map[string]carriedAgent) string {
	var b strings.Builder
	b.WriteString(inputsHash)
	b.WriteByte('\n')
	for _, a := range plan.Agents {
		fmt.Fprintf(&b, "agent:%s=%s\n", a.Agent, e.agentInputHash(repo, cfg, plan, a, specOf(a)))
		if ca, ok := carried[a.Agent]; ok && ca.diffHash != "" {
			fmt.Fprintf(&b, "diff:%s=%s\n", a.Agent, ca.diffHash)
		} else if dh, err := e.agentDiffHash(ctx, repo, plan, a, fullDiff); err == nil {
			fmt.Fprintf(&b, "diff:%s=%s\n", a.Agent, dh)
		}
	}
	return b.String()
}

// carriedAgent holds a report and origin carried for a single agent when
// re-running review on a descendant commit.
type carriedAgent struct {
	report    *Report
	rawReport []byte
	origin    *gate.Carried
	diffHash  string
}

func isSubset(sub, super []string) bool {
	for _, f := range sub {
		if !slices.Contains(super, f) {
			return false
		}
	}
	return true
}

// findCarriedAgents inspects the latest completed ancestor run on the same
// branch and determines which planned agents in plan can have their earlier
// verdict carried forward without calling the LLM.
//
// Two tiers of agent carry are supported when head descends from the
// earlier run's head (and from each carried agent's origin head):
//
//  1. Unchanged Scoped Diff (Tier 1): The agent's non-file inputs
//     (agentInputHash), matched file list (a.Files), and scoped diff
//     (git diff base...head -- a.Files) are byte-for-byte identical to the
//     earlier run's, and the agent had no blocking findings.
//
//  2. Comment-Resolution / Delta Skipping (Tier 2): Between the agent's
//     origin head and the new head (git diff origin.Head..plan.Head):
//     a) None of the files modified since origin.Head match the agent's gate
//     paths (Tier 2a), OR
//     b) The previous run had freshly-reported review findings (blockers or
//     warnings), this agent passed cleanly with 0 blockers and 0 warnings,
//     no new file was added to a.Files, and the delta changes since
//     origin.Head do not satisfy the agent's gate When condition (or the
//     gate has no When condition) (Tier 2b).
//
// An agent is never carried if:
//   - The earlier run had no inputs.hash (pre-inputs-hash legacy run).
//   - Any agent in the earlier run failed or had an invalid/missing report.
//   - The agent's report in the earlier run had blocking findings.
//   - Another agent in the earlier run handed off to this agent.
//   - The agent's definition, mode, gate config, thresholds, or touched
//     module spec context changed (agentInputHash mismatch).
func (e *Engine) findCarriedAgents(ctx context.Context, repo string, cfg *gate.Config, plan *gate.Plan, diff, inputsHash string, specOf func(gate.Planned) string) map[string]carriedAgent {
	for _, dir := range candidateRunDirs(repo) {
		var status struct {
			State   string `json:"state"`
			Outcome string `json:"outcome"`
			Agents  map[string]struct {
				State string `json:"state"`
			} `json:"agents"`
		}
		var prevPlan gate.Plan
		sb, err1 := os.ReadFile(filepath.Join(dir, "status.json"))
		pb, err2 := os.ReadFile(filepath.Join(dir, "plan.json"))
		if err1 != nil || err2 != nil || json.Unmarshal(sb, &status) != nil || json.Unmarshal(pb, &prevPlan) != nil {
			continue
		}
		if status.State != stateCompleted || !slices.Contains(reusableOutcomes, strings.ToUpper(status.Outcome)) {
			continue
		}
		if prevPlan.Head == "" || len(prevPlan.Agents) == 0 {
			continue
		}
		prevRunHash, prevAgentInputs, prevAgentDiffs := readInputsHashFile(filepath.Join(dir, inputsHashFile))
		if prevRunHash == "" {
			continue
		}
		if !isAncestor(ctx, repo, prevPlan.Head, plan.Head) {
			continue
		}
		// Verify all planned agents in prevPlan completed and have readable reports.
		prevInputs := map[string]gate.Input{}
		complete := true
		for _, pa := range prevPlan.Agents {
			if status.Agents[pa.Agent].State != stateCompleted {
				complete = false
				break
			}
			rb, err := os.ReadFile(filepath.Join(dir, pa.Agent+".json"))
			if err != nil {
				complete = false
				break
			}
			prevInputs[pa.Agent] = gate.Input{Data: rb}
		}
		if !complete {
			continue
		}
		prevCheck := gate.Check(cfg, &prevPlan, prevInputs, false)
		prevResults := map[string]gate.AgentResult{}
		handoffTargets := map[string]bool{}
		prevRunHadBlockers := false
		prevRunHadWarnings := false
		allValid := true
		for _, ar := range prevCheck.Agents {
			if ar.Status != gate.StatusOK {
				allValid = false
				break
			}
			prevResults[ar.Agent] = ar
			prevA, _ := prevPlan.Agent(ar.Agent)
			wasCarried := prevPlan.CarriedFrom != nil || prevA.CarriedFrom != nil
			if !wasCarried {
				if len(ar.Blocking) > 0 {
					prevRunHadBlockers = true
				}
				if len(ar.Warnings) > 0 {
					prevRunHadWarnings = true
				}
			}
			for _, h := range ar.Handoffs {
				if h.To != "" {
					handoffTargets[h.To] = true
				}
			}
		}
		if !allValid {
			continue
		}

		// We found the latest valid ancestor run on this branch. Evaluate each
		// agent in the current plan against it.
		out := map[string]carriedAgent{}
		runID := filepath.Base(dir)

		// Cache gitx.Changes(ctx, repo, originHead, plan.Head) per originHead.
		deltaCache := map[string][]gate.Change{}
		deltaErrCache := map[string]error{}
		changesSince := func(originHead string) ([]gate.Change, error) {
			if ch, ok := deltaCache[originHead]; ok || deltaErrCache[originHead] != nil {
				return ch, deltaErrCache[originHead]
			}
			ch, err := gitx.Changes(ctx, repo, originHead, plan.Head)
			deltaCache[originHead] = ch
			deltaErrCache[originHead] = err
			return ch, err
		}

		for _, a := range plan.Agents {
			prevAgent, ok := prevPlan.Agent(a.Agent)
			if !ok || prevAgent.Mode != a.Mode {
				continue
			}
			prevAR, ok := prevResults[a.Agent]
			if !ok || prevAR.Status != gate.StatusOK {
				continue
			}
			// Never carry an agent that had blocking findings: the new commit
			// may exist specifically to fix those blockers.
			if len(prevAR.Blocking) > 0 {
				continue
			}
			// Never carry an agent that another agent handed off to.
			if handoffTargets[a.Agent] {
				continue
			}
			rawBytes := prevInputs[a.Agent].Data
			var rep Report
			if err := json.Unmarshal(rawBytes, &rep); err != nil {
				continue
			}
			// Determine the origin run & range that actually produced this report.
			origin := prevAgent.CarriedFrom
			if origin == nil {
				origin = prevPlan.CarriedFrom
			}
			if origin == nil {
				origin = &gate.Carried{Run: runID, Base: prevPlan.Base, Head: prevPlan.Head}
			}
			if origin.Base == "" || origin.Head == "" || !gate.SameRev(rep.Base, origin.Base) || !gate.SameRev(rep.Head, origin.Head) {
				continue
			}
			if !isAncestor(ctx, repo, origin.Head, plan.Head) {
				continue
			}

			// Verify the agent's non-file inputs (definition, mode, gate config,
			// thresholds, touched module spec context) have not changed.
			curInputHash := e.agentInputHash(repo, cfg, plan, a, specOf(a))
			prevInputHash := prevAgentInputs[a.Agent]
			if prevInputHash == "" {
				// Fallback for single-line inputs.hash when the whole plan's
				// inputsHash is identical.
				if prevRunHash != inputsHash {
					continue
				}
			} else if curInputHash != prevInputHash {
				continue
			}

			// Compute current scoped diff hash for Tier 1.
			curDiffHash, err := e.agentDiffHash(ctx, repo, plan, a, diff)
			if err != nil {
				continue
			}
			prevDiffHash := prevAgentDiffs[a.Agent]
			if prevDiffHash == "" && len(prevAgent.Files) > 0 {
				if d, err := gitx.Run(ctx, repo, append([]string{"--literal-pathspecs", "diff", origin.Base + "..." + origin.Head, "--"}, prevAgent.Files...)...); err == nil {
					sum := sha256.Sum256([]byte(strings.TrimSpace(d)))
					prevDiffHash = hex.EncodeToString(sum[:])
				}
			}

			// Tier 1: Unchanged Scoped Diff.
			// The agent's matched file list and scoped diff are byte-for-byte
			// identical to the previous run's.
			carry := slices.Equal(a.Files, prevAgent.Files) && curDiffHash != "" && curDiffHash == prevDiffHash

			// Tier 2: Delta / Comment-Resolution Skipping.
			if !carry && origin.Head != plan.Head {
				deltaChanges, err := changesSince(origin.Head)
				if g, ok := cfg.GateFor(a.Agent); ok && err == nil {
					touchesGate := false
					for _, ch := range deltaChanges {
						if g.MatchesPath(ch.Path) {
							touchesGate = true
							break
						}
					}
					if !touchesGate {
						// Tier 2a: The commits since origin.Head did not touch
						// any file matching this agent's gate paths.
						carry = true
					} else if (prevRunHadBlockers || prevRunHadWarnings) &&
						len(prevAR.Blocking) == 0 && len(prevAR.Warnings) == 0 &&
						isSubset(a.Files, prevAgent.Files) {
						// Tier 2b: The previous run had review findings being
						// addressed, this agent already passed cleanly on
						// origin.Head (0 blockers and 0 warnings), no brand-new
						// file entered this agent's scope, and the delta commit
						// does not trigger a When threshold/touch condition on
						// this gate.
						if !g.MatchesWhen(deltaChanges) {
							carry = true
						}
					}
				}
			}

			if carry {
				out[a.Agent] = carriedAgent{
					report:    &rep,
					rawReport: rawBytes,
					origin:    origin,
					diffHash:  prevDiffHash,
				}
			}
		}
		return out
	}
	return nil
}

// carry makes a run of plan's head out of from, a finished run of an earlier
// head whose verdict findCachedRun found may be carried: the agents' reports
// and usage as they were, still naming the range they reviewed, plan.json
// naming this head and the run it carries (gate.Plan.CarriedFrom, which
// gate.Check accepts those reports under and the rendered result shows), and from's
// status with this head's run id, refs and time, not yet published. The new
// run is then reused like any other, so it is what publish.sh posts on this
// head: the record names the head it is for, and the run it came from.
//
// A run that was itself carried passes on what it carried: its reports name
// the range of the run that reviewed, so the new plan names that run and
// range, however many merges of the base lie between. On an error the new
// run directory is removed; the caller then reviews afresh.
func (e *Engine) carry(repo, from string, plan *gate.Plan, diffHash, inputsHash string) (dir string, err error) {
	var prev gate.Plan
	pb, err := os.ReadFile(filepath.Join(from, "plan.json"))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(pb, &prev); err != nil {
		return "", err
	}
	origin := prev.CarriedFrom
	if origin == nil {
		origin = &gate.Carried{Run: filepath.Base(from), Base: prev.Base, Head: prev.Head}
	}
	runID, dir, err := e.newRunDir(repo, runDirPrefix)
	if err != nil {
		return "", err
	}
	// The directory made here, not the result: a `return "", err` has set
	// the result to "" by the time this runs.
	made := dir
	defer func() {
		if err != nil {
			_ = os.RemoveAll(made)
		}
	}()
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
	p.Agents = slices.Clone(plan.Agents)
	for i := range p.Agents {
		if prevA, ok := prev.Agent(p.Agents[i].Agent); ok && prevA.CarriedFrom != nil {
			p.Agents[i].CarriedFrom = prevA.CarriedFrom
		}
	}
	p.CarriedFrom = origin
	if err := writeJSON(filepath.Join(dir, "plan.json"), &p); err != nil {
		return "", err
	}
	// Without them the carried run is still a sound run of this head; it
	// only cannot be carried on or reused by hash.
	_ = os.WriteFile(filepath.Join(dir, "diff.hash"), []byte(diffHash), 0o644)
	inputsContent := inputsHash
	if prevRaw, err := os.ReadFile(filepath.Join(from, inputsHashFile)); err == nil {
		if _, rest, ok := strings.Cut(strings.TrimSpace(string(prevRaw)), "\n"); ok && rest != "" {
			inputsContent = inputsHash + "\n" + rest + "\n"
		}
	}
	_ = os.WriteFile(filepath.Join(dir, inputsHashFile), []byte(inputsContent), 0o644)

	status := readStatusObject(filepath.Join(from, "status.json"))
	now := e.now().UTC().Format(time.RFC3339)
	status.Set("run_id", runID)
	status.Set("base", plan.Base)
	status.Set("head", plan.Head)
	status.Set("diff_hash", diffHash)
	status.Set("started_at", now)
	// The same object as plan.json's carried_from.
	carried := NewObject()
	carried.Set("run", origin.Run)
	carried.Set("base", origin.Base)
	carried.Set("head", origin.Head)
	status.Set("carried_from", carried)
	status.Set("pr", nil)
	status.Set("pr_url", nil)
	status.Set("published_status", nil)
	for _, f := range records(status, "findings") {
		f.Set("run_id", runID)
	}
	// No model was paid for this run: what the reviews cost stays on the run
	// that made them, so a sum over runs counts it once.
	status.Set("total_cost_usd", 0)
	status.Set("total_tokens", 0)
	status.Set("usage", map[string]any{"total_cost_usd": 0, "prompt_tokens": 0, "cached_tokens": 0, "completion_tokens": 0, "total_tokens": 0})
	if agents, ok := status.Get("agents").(*Object); ok {
		for _, name := range agents.Keys() {
			if a, ok := agents.Get(name).(*Object); ok {
				a.Set("cost_usd", 0)
				a.Set("tokens", map[string]any{"prompt": 0, "cached": 0, "completion": 0, "total": 0})
				a.Set("duration_seconds", 0)
			}
		}
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
	status := readStatusObject(statusPath)
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
	carriedFrom := ""
	if runPlan.CarriedFrom != nil {
		carriedFrom = runPlan.CarriedFrom.Run
	}
	check, rendered := e.check(cfg, runPlan, dir, &gate.UsageSummary{
		Reused:      true,
		CarriedFrom: carriedFrom,
	})
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
	var runUsage Usage
	if summary, err := loadRunDir(dir); err == nil {
		runUsage = summary.Usage
		if runUsage.CostUSD == 0 && summary.TotalCostUSD > 0 {
			runUsage.CostUSD = summary.TotalCostUSD
		}
		if runUsage.TotalTokens == 0 && summary.TotalTokens > 0 {
			runUsage.TotalTokens = summary.TotalTokens
		}
	}
	return &Result{
		RunID: runID, RunDir: dir, Cached: true, CarriedFrom: carriedFrom,
		PR: firstNonEmpty(pr.Number, asText(status.Get("pr"))), PRURL: firstNonEmpty(pr.URL, asText(status.Get("pr_url"))),
		Plan: runPlan, Outcome: outcome, Rendered: rendered, Published: published,
		Usage: runUsage,
	}, nil
}

// readStatusObject is a run's status.json as an Object, keys in their order;
// an empty one when it cannot be read.
func readStatusObject(path string) *Object {
	if b, err := os.ReadFile(path); err == nil {
		if v, err := parseValue(b); err == nil {
			if o, ok := v.(*Object); ok {
				return o
			}
		}
	}
	return NewObject()
}
