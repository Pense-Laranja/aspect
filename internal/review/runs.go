package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/eideroliveira/aspect/internal/gate"
)

// The run readers behind `aspect review status|findings|compare|runs`. They
// read .review/*/status.json, which a run rewrites at every change, so
// status on a run in progress shows its agents live.

// RunSummary is a run as its status.json records it.
type RunSummary struct {
	RunID           string                  `json:"run_id"`
	State           string                  `json:"state"`
	Outcome         string                  `json:"outcome"`
	Verdict         string                  `json:"verdict"`
	Summary         string                  `json:"summary"`
	Base            string                  `json:"base"`
	Head            string                  `json:"head"`
	PR              string                  `json:"pr"`
	Engine          string                  `json:"engine"`
	TotalBlockers   int                     `json:"total_blockers"`
	TotalWarnings   int                     `json:"total_warnings"`
	TotalFindings   int                     `json:"total_findings"`
	CompletedAgents int                     `json:"completed_agents"`
	TotalAgents     int                     `json:"total_agents"`
	TotalCostUSD    float64                 `json:"total_cost_usd"`
	Published       string                  `json:"published_status"`
	Agents          map[string]AgentSummary `json:"agents"`
	Findings        []FindingSummary        `json:"findings"`
}

// AgentSummary is one agent of a run.
type AgentSummary struct {
	Name          string  `json:"name"`
	Mode          string  `json:"mode"`
	State         string  `json:"state"`
	Verdict       string  `json:"verdict"`
	Summary       string  `json:"summary"`
	FindingsCount int     `json:"findings_count"`
	BlockersCount int     `json:"blockers_count"`
	WarningsCount int     `json:"warnings_count"`
	Error         string  `json:"error"`
	PartsTotal    int     `json:"parts_total"`
	PartsDone     int     `json:"parts_done"`
	CostUSD       float64 `json:"cost_usd"`
}

// FindingSummary is one finding of a run, with the agent that reported it.
type FindingSummary struct {
	ID             string         `json:"id"`
	Severity       string         `json:"severity"`
	Category       string         `json:"category"`
	Title          string         `json:"title"`
	Recommendation string         `json:"recommendation"`
	Agent          string         `json:"agent"`
	Location       *gate.Location `json:"location"`
}

// LoadRun reads one run of the repository: the run named, else the one
// current_run.json names (while its directory exists), else the newest
// local run.
func LoadRun(repo, runID string) (*RunSummary, error) {
	reviewDir := filepath.Join(repo, ".review")
	if runID != "" {
		if filepath.Base(runID) != runID || strings.HasPrefix(runID, ".") {
			return nil, fmt.Errorf("%q is not a run id", runID)
		}
		return loadRunDir(filepath.Join(reviewDir, runID))
	}
	var cur struct {
		RunID string `json:"run_id"`
	}
	if b, err := os.ReadFile(filepath.Join(reviewDir, "current_run.json")); err == nil && json.Unmarshal(b, &cur) == nil &&
		cur.RunID != "" && filepath.Base(cur.RunID) == cur.RunID && !strings.HasPrefix(cur.RunID, ".") {
		r, err := loadRunDir(filepath.Join(reviewDir, cur.RunID))
		if !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
	}
	runs, err := LoadRuns(repo, 1)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("no review runs in %s", reviewDir)
	}
	return runs[0], nil
}

func loadRunDir(dir string) (*RunSummary, error) {
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		return nil, err
	}
	var r RunSummary
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s/status.json: %w", dir, err)
	}
	if r.RunID == "" {
		r.RunID = filepath.Base(dir)
	}
	return &r, nil
}

// LoadRuns lists the repository's local runs (.review/local-<UTC timestamp>,
// as Engine.Run names them), newest first by name, at most limit (0: all).
// A run whose status.json cannot be read is skipped, with a warning on
// stderr, so one broken run does not hide the others. Runs recorded under
// other ids are read only by LoadRun with their id.
func LoadRuns(repo string, limit int) ([]*RunSummary, error) {
	dirs, err := filepath.Glob(filepath.Join(repo, ".review", runDirPrefix+"*"))
	if err != nil {
		return nil, err
	}
	slices.Sort(dirs)
	slices.Reverse(dirs)
	var runs []*RunSummary
	for _, d := range dirs {
		r, err := loadRunDir(d)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "aspect review: skipping %s: %v\n", filepath.Base(d), err)
			}
			continue
		}
		runs = append(runs, r)
		if limit > 0 && len(runs) == limit {
			break
		}
	}
	return runs, nil
}

// ValidSeverity reports whether s is a severity a findings filter may name
// ("" is no filter).
func ValidSeverity(s string) bool {
	return s == "" || slices.Contains(gate.Severities, strings.ToLower(s))
}

// agentNames is the run's agents in a stable order: gates first, then by
// name.
func (r *RunSummary) agentNames() []string {
	names := make([]string, 0, len(r.Agents))
	for n := range r.Agents {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b string) int {
		ga, gb := r.Agents[a].Mode == "gate", r.Agents[b].Mode == "gate"
		if ga != gb {
			if ga {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	return names
}

// PrintStatus writes a run's state and its agents'.
func PrintStatus(w io.Writer, r *RunSummary) {
	outcome := r.Outcome
	if outcome == "" {
		outcome = "-"
	}
	fmt.Fprintf(w, "Run %s: %s, outcome %s (%d/%d agents done)\n", r.RunID, r.State, outcome, r.CompletedAgents, r.TotalAgents)
	fmt.Fprintf(w, "%s..%s on %s", short(r.Base), short(r.Head), r.Engine)
	if r.PR != "" {
		fmt.Fprintf(w, ", PR #%s", r.PR)
	}
	if r.TotalCostUSD > 0 {
		fmt.Fprintf(w, ", $%.4f", r.TotalCostUSD)
	}
	fmt.Fprintf(w, "\n%s\n\n", r.Summary)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tMODE\tSTATE\tVERDICT\tBLOCKING\tWARNINGS\tFINDINGS\tPARTS\tERROR")
	for _, n := range r.agentNames() {
		a := r.Agents[n]
		parts := "-"
		if a.PartsTotal > 0 {
			parts = fmt.Sprintf("%d/%d", a.PartsDone, a.PartsTotal)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n", n, a.Mode, a.State, a.Verdict,
			a.BlockersCount, a.WarningsCount, a.FindingsCount, parts, oneLine(a.Error, 80))
	}
	tw.Flush()
	if r.Published != "" {
		fmt.Fprintf(w, "\nPublished: %s\n", oneLine(r.Published, 200))
	}
}

// PrintFindings writes a run's findings, most severe first, optionally only
// one severity or one agent's.
func PrintFindings(w io.Writer, r *RunSummary, severity, agent string) int {
	var fs []FindingSummary
	for _, f := range r.Findings {
		if (severity == "" || strings.EqualFold(f.Severity, severity)) && (agent == "" || f.Agent == agent) {
			fs = append(fs, f)
		}
	}
	slices.SortStableFunc(fs, func(a, b FindingSummary) int {
		return slices.Index(gate.Severities, a.Severity) - slices.Index(gate.Severities, b.Severity)
	})
	for _, f := range fs {
		loc := ""
		if f.Location != nil {
			loc = f.Location.File
			if f.Location.Line > 0 {
				loc += fmt.Sprintf(":%d", f.Location.Line)
			}
		}
		fmt.Fprintf(w, "%-8s %-22s %s\n         %s\n", strings.ToUpper(f.Severity), f.Agent, loc, f.Title)
		if f.Recommendation != "" {
			fmt.Fprintf(w, "         Fix: %s\n", oneLine(f.Recommendation, 300))
		}
	}
	return len(fs)
}

// PrintCompare writes two runs side by side, agent by agent.
func PrintCompare(w io.Writer, a, b *RunSummary) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "\tRUN A: %s\tRUN B: %s\n", a.RunID, b.RunID)
	fmt.Fprintf(tw, "Engine\t%s\t%s\n", a.Engine, b.Engine)
	fmt.Fprintf(tw, "Outcome\t%s\t%s\n", a.Outcome, b.Outcome)
	fmt.Fprintf(tw, "Findings\t%d\t%d\n", a.TotalFindings, b.TotalFindings)
	fmt.Fprintf(tw, "Blocking\t%d\t%d\n", a.TotalBlockers, b.TotalBlockers)
	fmt.Fprintf(tw, "Warnings\t%d\t%d\n", a.TotalWarnings, b.TotalWarnings)
	fmt.Fprintln(tw, "\t\t\t")
	fmt.Fprintln(tw, "AGENT\tRUN A\tRUN B\tDIFFERENCE")
	names := a.agentNames()
	for _, n := range b.agentNames() {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	// An agent a run did not plan shows "-", one that did not complete its
	// state: neither is a clean result, and neither has a difference.
	reported := func(x AgentSummary) bool { return x.State == "" || x.State == "completed" }
	cell := func(x AgentSummary, ok bool) string {
		switch {
		case !ok:
			return "-"
		case !reported(x):
			return x.State
		}
		return fmt.Sprintf("%d (%d blk)", x.FindingsCount, x.BlockersCount)
	}
	for _, n := range names {
		x, okA := a.Agents[n]
		y, okB := b.Agents[n]
		diff := ""
		if okA && okB && reported(x) && reported(y) {
			diff = fmt.Sprintf("%+d", y.FindingsCount-x.FindingsCount)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, cell(x, okA), cell(y, okB), diff)
	}
	tw.Flush()
}

// PrintRuns lists runs, one line each.
func PrintRuns(w io.Writer, runs []*RunSummary) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tPR\tSTATE\tOUTCOME\tBLOCKING\tFINDINGS\tENGINE")
	for _, r := range runs {
		pr := "-"
		if r.PR != "" {
			pr = "#" + r.PR
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n", r.RunID, pr, r.State, firstNonEmpty(r.Outcome, "-"), r.TotalBlockers, r.TotalFindings, r.Engine)
	}
	tw.Flush()
}

func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
