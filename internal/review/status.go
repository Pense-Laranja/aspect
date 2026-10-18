package review

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// StatusSchema identifies a run's status.json.
const StatusSchema = "aspect-review/status/v1"

// Agent and run states, as status.json spells them.
const (
	statePending   = "pending"
	stateQueued    = "queued" // waiting for one of the run's model slots
	stateRunning   = "running"
	stateCompleted = "completed"
	stateFailed    = "failed"
)

// Run outcomes, worst last. ERROR (a gate never reported) and BLOCK both
// fail the gate; BLOCK ranks higher because it is a verdict.
const (
	OutcomePass       = "PASS"
	OutcomeOverridden = "OVERRIDDEN"
	OutcomeWarn       = "WARN"
	OutcomeError      = "ERROR"
	OutcomeBlock      = "BLOCK"
)

var outcomeRank = map[string]int{OutcomePass: 0, OutcomeOverridden: 0, OutcomeWarn: 1, OutcomeError: 2, OutcomeBlock: 3}

// worstOutcome combines outcomes, failing closed: an unknown one counts as
// ERROR, an empty one is skipped.
func worstOutcome(outcomes ...string) string {
	worst := OutcomePass
	for _, o := range outcomes {
		if o == "" {
			continue
		}
		o = strings.ToUpper(o)
		if _, ok := outcomeRank[o]; !ok {
			o = OutcomeError
		}
		if outcomeRank[o] > outcomeRank[worst] {
			worst = o
		}
	}
	return worst
}

// agentStatus is one planned agent's progress and result.
type agentStatus struct {
	name, mode, state string
	queuedAt          time.Time
	startedAt         time.Time
	finishedAt        time.Time
	engine            string
	partsTotal        int
	partsDone         int
	summary           string
	err               string
	files             []string
	findings          []finding
	blockers          int
	warnings          int
	usage             usage
}

// runStatus is a run's progress, kept in .review/<run-id>/status.json and
// rewritten at every change so a dashboard can follow it.
type runStatus struct {
	mu        sync.Mutex
	dir       string // .review/<run-id>
	runID     string
	baseRef   string
	headRef   string
	baseSHA   string
	headSHA   string
	branch    string
	diffHash  string
	pr, prURL string
	state     string
	outcome   string
	errMsg    string
	published string
	agents    map[string]*agentStatus
	order     []string
	startedAt time.Time
	now       func() time.Time
}

// computeOutcome is the outcome the agents' states support, failing closed:
// BLOCK when a gate agent reported a blocker; otherwise ERROR when a gate
// agent has no report (the rule `aspect gate check` applies to a missing
// report); otherwise PASS. Advisory agents decide only when there are no
// gate agents and none of them completed.
func (r *runStatus) computeOutcome() string {
	gates, anyCompleted := 0, false
	missing, blocked := false, false
	for _, a := range r.agents {
		if a.state == stateCompleted {
			anyCompleted = true
		}
		if a.mode != "gate" {
			continue
		}
		gates++
		if a.blockers > 0 {
			blocked = true
		}
		if a.state != stateCompleted {
			missing = true
		}
	}
	switch {
	case blocked:
		return OutcomeBlock
	case missing:
		return OutcomeError
	case gates == 0 && len(r.agents) > 0 && !anyCompleted:
		return OutcomeError
	}
	return OutcomePass
}

func (r *runStatus) gatesMissing() bool {
	for _, a := range r.agents {
		if a.mode == "gate" && a.state != stateCompleted {
			return true
		}
	}
	return false
}

func (r *runStatus) counts() (completed, findings, blockers, warnings int, u usage) {
	for _, name := range r.order {
		a := r.agents[name]
		if a.state == stateCompleted || a.state == stateFailed {
			completed++
		}
		findings += len(a.findings)
		blockers += a.blockers
		warnings += a.warnings
		u.add(a.usage)
	}
	return
}

func (r *runStatus) engine() string {
	var u usage
	for _, name := range r.order {
		u.add(usage{Engine: r.agents[name].engine})
	}
	if u.Engine == "" {
		return "gemini"
	}
	return u.Engine
}

// update changes the run under its lock and saves it.
func (r *runStatus) update(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
	r.save()
}

func (r *runStatus) agent(name string) *agentStatus {
	a, ok := r.agents[name]
	if !ok {
		a = &agentStatus{name: name, mode: "gate", state: statePending}
		r.agents[name] = a
		r.order = append(r.order, name)
	}
	return a
}

func (r *runStatus) queued(name string) {
	r.update(func() {
		a := r.agent(name)
		if a.state == statePending || a.state == stateQueued {
			a.state = stateQueued
			if a.queuedAt.IsZero() {
				a.queuedAt = r.now()
			}
		}
	})
}

func (r *runStatus) planned(name, engine string, parts int) {
	r.update(func() {
		a := r.agent(name)
		a.engine, a.partsTotal, a.partsDone = engine, parts, 0
	})
}

// started: a request of the agent started. Called again for later parts;
// the agent's clock keeps its first start.
func (r *runStatus) started(name string) {
	r.update(func() {
		a := r.agent(name)
		if a.startedAt.IsZero() {
			a.startedAt = r.now()
		}
		a.state = stateRunning
	})
}

// waiting: a started agent waits for a slot again for its next part.
func (r *runStatus) waiting(name string) {
	r.update(func() {
		a := r.agent(name)
		if a.state == stateRunning {
			a.state = stateQueued
			a.queuedAt = r.now()
		}
	})
}

func (r *runStatus) partDone(name string, done, total int) {
	r.update(func() {
		a := r.agent(name)
		a.partsDone, a.partsTotal = done, total
	})
}

func (r *runStatus) completed(name string, rep *Report, u usage) {
	r.update(func() {
		a := r.agent(name)
		a.state = stateCompleted
		a.finishedAt = r.now()
		a.summary = rep.Summary
		a.findings = rep.Findings
		a.blockers, a.warnings = rep.blockersAndWarnings()
		a.usage = u
		if u.Engine != "" {
			a.engine = u.Engine
		}
	})
}

func (r *runStatus) failed(name, msg string) {
	r.update(func() {
		a := r.agent(name)
		a.state = stateFailed
		a.finishedAt = r.now()
		a.err = msg
	})
}

// finish records the run as ended. The outcome is the worse of the one
// given (aspect's verdict) and what the agents support, so a gate agent
// without a report turns a PASS into ERROR.
func (r *runStatus) finish(outcome, published string) string {
	r.update(func() {
		r.state = stateCompleted
		completed, _, _, _, _ := r.counts()
		if (len(r.agents) > 0 && completed == 0) || r.gatesMissing() {
			r.outcome = worstOutcome(OutcomeError, outcome, r.computeOutcome())
		} else {
			r.outcome = worstOutcome(outcome, r.computeOutcome())
		}
		r.published = published
	})
	return r.outcome
}

// fail records a run that stopped before it finished: never PASS.
func (r *runStatus) fail(msg string) {
	r.update(func() {
		now := r.now()
		for _, a := range r.agents {
			if a.state == stateRunning || a.state == stateQueued {
				a.state, a.finishedAt = stateFailed, now
				if a.err == "" {
					a.err = msg
				}
			}
		}
		r.state = stateFailed
		r.errMsg = msg
		r.outcome = worstOutcome(OutcomeError, r.outcome, r.computeOutcome())
	})
}

func roundTo(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}

// document is status.json: the run's evaluation, verdicts, findings and
// publishing, without process-level telemetry.
func (r *runStatus) document() *Object {
	completed, nFindings, blockers, warnings, u := r.counts()
	total := len(r.agents)
	var outcome any
	var verdict, summary string
	switch r.state {
	case stateRunning:
		verdict = "Review in progress"
		summary = fmt.Sprintf("Review running (%d/%d agents completed)", completed, total)
	case stateFailed:
		o := r.outcome
		if o == "" || o == OutcomePass {
			o = OutcomeError
		}
		outcome = o
		verdict = "Review failed: incomplete run"
		summary = r.errMsg
		if summary == "" {
			summary = fmt.Sprintf("Review interrupted: %d of %d agents completed.", completed, total)
		}
	default:
		if (total > 0 && completed == 0) || r.gatesMissing() {
			o := OutcomeError
			if blockers > 0 {
				o = OutcomeBlock
			}
			outcome, verdict = o, "Review gates: incomplete"
			summary = fmt.Sprintf("Incomplete run: only %d of %d agents completed.", completed, total)
			break
		}
		o := r.outcome
		if o == "" {
			o = r.computeOutcome()
		}
		outcome = o
		switch o {
		case OutcomeBlock:
			verdict = "Review gates: blocked"
			summary = fmt.Sprintf("Review blocked: %d blocking issue(s) detected across %d agents.", blockers, completed)
		case OutcomeWarn:
			verdict = "Review gates: warning"
			summary = fmt.Sprintf("Review passed with warnings: %d warning(s) detected across %d agents.", warnings, completed)
		case OutcomePass:
			verdict = "Review gates: pass"
			summary = fmt.Sprintf("Review gates passed: all %d agents cleared with 0 blockers.", completed)
		default:
			verdict = "Review gates: " + strings.ToLower(o)
			summary = r.errMsg
			if summary == "" {
				summary = "Review outcome: " + o
			}
		}
	}

	agents := NewObject()
	var all []any
	for _, name := range r.order {
		a := r.agents[name]
		v := "pass"
		switch {
		case a.blockers > 0:
			v = "block"
		case a.warnings > 0:
			v = "warn"
		case a.state == statePending:
			v = "missing"
		case a.state != stateCompleted:
			v = a.state
		}
		e := NewObject()
		e.Set("name", a.name)
		e.Set("mode", a.mode)
		e.Set("state", a.state)
		e.Set("status", a.state)
		e.Set("verdict", v)
		e.Set("summary", a.summary)
		e.Set("findings_count", len(a.findings))
		e.Set("blockers_count", a.blockers)
		e.Set("warnings_count", a.warnings)
		e.Set("findings", nonNil(a.findings))
		e.Set("error", nullable(a.err))
		e.Set("engine", nullable(a.engine))
		e.Set("parts_total", a.partsTotal)
		e.Set("parts_done", a.partsDone)
		if !a.startedAt.IsZero() && !a.finishedAt.IsZero() {
			e.Set("duration_seconds", roundTo(a.finishedAt.Sub(a.startedAt).Seconds(), 1))
		}
		if a.usage.CostUSD > 0 || strings.Contains(strings.ToLower(a.engine), "gemini") {
			e.Set("cost_usd", a.usage.CostUSD)
			tok := NewObject()
			tok.Set("prompt", a.usage.PromptTokens)
			tok.Set("completion", a.usage.CompletionTokens)
			tok.Set("total", a.usage.TotalTokens)
			e.Set("tokens", tok)
		}
		agents.Set(name, e)
		for _, f := range a.findings {
			item := NewObject()
			item.Set("id", f.ID)
			item.Set("severity", f.Severity)
			item.Set("category", f.Category)
			item.Set("title", f.Title)
			item.Set("evidence", f.Evidence)
			item.Set("recommendation", f.Recommendation)
			item.Set("confidence", f.Confidence)
			if f.Location != nil {
				item.Set("location", f.Location)
			}
			item.Set("agent", name)
			item.Set("mode", a.mode)
			item.Set("run_id", r.runID)
			item.Set("engine", firstNonEmpty(a.engine, r.engine()))
			if f.Location != nil {
				item.Set("file", f.Location.File)
				item.Set("line", nullableInt(f.Location.Line))
				item.Set("end_line", nullableInt(f.Location.EndLine))
			}
			all = append(all, item)
		}
	}

	d := NewObject()
	d.Set("schema", StatusSchema)
	d.Set("run_id", r.runID)
	d.Set("state", r.state)
	d.Set("outcome", outcome)
	d.Set("verdict", verdict)
	d.Set("summary", summary)
	d.Set("base", firstNonEmpty(r.baseSHA, r.baseRef))
	d.Set("head", firstNonEmpty(r.headSHA, r.headRef))
	d.Set("diff_hash", nullable(r.diffHash))
	d.Set("pr", nullable(r.pr))
	d.Set("pr_url", nullable(r.prURL))
	d.Set("engine", r.engine())
	d.Set("branch", r.branch)
	d.Set("started_at", r.startedAt.UTC().Format(time.RFC3339))
	d.Set("total_blockers", blockers)
	d.Set("total_warnings", warnings)
	d.Set("total_findings", nFindings)
	d.Set("completed_agents", completed)
	d.Set("total_agents", total)
	d.Set("published_status", nullable(r.published))
	d.Set("agents", agents)
	if all == nil {
		all = []any{}
	}
	d.Set("findings", all)
	if u.CostUSD > 0 || strings.Contains(strings.ToLower(r.engine()), "gemini") {
		cost := roundTo(u.CostUSD, 6)
		d.Set("total_cost_usd", cost)
		d.Set("total_tokens", u.TotalTokens)
		us := NewObject()
		us.Set("total_cost_usd", cost)
		us.Set("prompt_tokens", u.PromptTokens)
		us.Set("completion_tokens", u.CompletionTokens)
		us.Set("total_tokens", u.TotalTokens)
		d.Set("usage", us)
	}
	return d
}

// save writes status.json and points .review/current_run.json at the run.
// A failed write is not fatal: the reports and the outcome do not depend
// on it.
func (r *runStatus) save() {
	if err := writeJSON(filepath.Join(r.dir, "status.json"), r.document()); err != nil {
		fmt.Fprintf(os.Stderr, "aspect review: writing %s/status.json: %v\n", r.dir, err)
	}
	writeCurrentRun(filepath.Dir(r.dir), r.runID, r.state, r.now())
}

func writeCurrentRun(reviewDir, runID, state string, now time.Time) {
	cur := NewObject()
	cur.Set("run_id", runID)
	cur.Set("state", state)
	cur.Set("updated_at", float64(now.UnixMilli())/1000)
	_ = writeJSON(filepath.Join(reviewDir, "current_run.json"), cur)
}

// writeJSON writes v indented, through a temporary file so a reader never
// sees half a document.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func nonNil(fs []finding) []finding {
	if fs == nil {
		return []finding{}
	}
	return fs
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
