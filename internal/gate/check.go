package gate

import (
	"fmt"
	"slices"
)

// Outcomes of a check, from best to worst. Overridden means the gate would
// have blocked but a maintainer overrode it.
const (
	Pass       = "pass"
	Warn       = "warn"
	Block      = "block"
	Overridden = "overridden"
)

// Report statuses.
const (
	StatusOK      = "ok"
	StatusMissing = "missing"
	StatusInvalid = "invalid"
)

// Result is the outcome of a check across every planned agent.
type Result struct {
	Outcome string        `json:"outcome"`
	Agents  []AgentResult `json:"agents"`
}

// AgentResult is one planned agent's report and what it contributes.
type AgentResult struct {
	Agent    string    `json:"agent"`
	Mode     string    `json:"mode"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Blocking []Finding `json:"blocking"`
	Warnings []Finding `json:"warnings"`
	Info     []Finding `json:"info"`
	Handoffs []Handoff `json:"handoffs"`
	ADRs     []ADR     `json:"adrs,omitempty"`
}

// Input is one agent's report as found on disk: the bytes, or the error
// reading them. A planned agent absent from the map has no report.
type Input struct {
	Data []byte
	Err  error
}

// Check validates the report of every planned agent and applies the
// thresholds. It blocks when a gate-mode agent's report is missing or
// invalid, or when any gate-mode finding meets the block threshold with
// enough confidence. Advisory agents only ever warn.
func Check(c *Config, p *Plan, reports map[string]Input, override bool) *Result {
	res := &Result{Outcome: Pass, Agents: []AgentResult{}}
	worst := Pass
	raise := func(o string) {
		if rank(o) > rank(worst) {
			worst = o
		}
	}
	for _, pl := range p.Agents {
		ar := AgentResult{Agent: pl.Agent, Mode: pl.Mode, Blocking: []Finding{}, Warnings: []Finding{}, Info: []Finding{}, Handoffs: []Handoff{}}
		failed := func(status, msg string) {
			ar.Status, ar.Error = status, msg
			if pl.Mode == "gate" {
				raise(Block)
			} else {
				raise(Warn)
			}
		}
		in, ok := reports[pl.Agent]
		switch {
		case !ok:
			failed(StatusMissing, "no report")
		case in.Err != nil:
			failed(StatusMissing, in.Err.Error())
		default:
			r, err := DecodeReport(in.Data)
			switch {
			case err != nil:
				failed(StatusInvalid, err.Error())
			case r.Agent != pl.Agent:
				failed(StatusInvalid, fmt.Sprintf("report is from agent %q", r.Agent))
			case r.Mode != pl.Mode:
				failed(StatusInvalid, fmt.Sprintf("report mode %q, planned %q", r.Mode, pl.Mode))
			case !SameRev(r.Base, p.Base) || !SameRev(r.Head, p.Head):
				failed(StatusInvalid, fmt.Sprintf("report reviews %s...%s, planned %s...%s", r.Base, r.Head, p.Base, p.Head))
			default:
				ar.Status, ar.Summary, ar.Handoffs, ar.ADRs = StatusOK, r.Summary, append(ar.Handoffs, r.Handoffs...), r.ADRs
				for _, f := range r.Findings {
					switch classify(c, f) {
					case Block:
						if pl.Mode == "gate" {
							ar.Blocking = append(ar.Blocking, f)
							raise(Block)
						} else {
							ar.Warnings = append(ar.Warnings, f)
							raise(Warn)
						}
					case Warn:
						ar.Warnings = append(ar.Warnings, f)
						raise(Warn)
					default:
						ar.Info = append(ar.Info, f)
					}
				}
				sortFindings(ar.Blocking)
				sortFindings(ar.Warnings)
			}
		}
		res.Agents = append(res.Agents, ar)
	}
	if worst == Block && override {
		worst = Overridden
	}
	res.Outcome = worst
	return res
}

// Failed reports whether the outcome should fail the check.
func (r *Result) Failed() bool { return r.Outcome == Block }

// classify maps one finding to block, warn or info under the thresholds.
// A finding below the confidence floor never blocks; it warns instead.
func classify(c *Config, f Finding) string {
	t := c.Thresholds
	switch {
	case slices.Contains(t.Block, f.Severity) && f.Confidence >= t.MinConfidenceToBlock:
		return Block
	case slices.Contains(t.Block, f.Severity), slices.Contains(t.Warn, f.Severity):
		return Warn
	default:
		return Pass
	}
}

func rank(outcome string) int {
	switch outcome {
	case Warn:
		return 1
	case Block:
		return 2
	}
	return 0
}

func sortFindings(fs []Finding) {
	slices.SortStableFunc(fs, func(a, b Finding) int {
		return slices.Index(Severities, a.Severity) - slices.Index(Severities, b.Severity)
	})
}
