package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Pense-Laranja/aspect/internal/gate"
	"github.com/Pense-Laranja/aspect/internal/spec"
)

// ManifestSchema identifies a prepared run's manifest.json.
const ManifestSchema = "aspect-review/manifest/v1"

// DefaultAntigravityEngine is the default engine label recorded by Prepare and
// Finalize when reviewers execute via Antigravity Flash workflows.
const DefaultAntigravityEngine = "antigravity (flash)"

// DefaultAntigravityPromptBudget bounds each prepared reviewer task's prompt in
// tokens (55k tokens * 3 chars/token = 165 KB) so user_prompt stays safely
// below Antigravity's 191,956-byte run_workflow USER_INPUT ceiling.
const DefaultAntigravityPromptBudget = 55_000

const antigravitySpecHeadroomTokens = 15_000

// PromptTask describes one stateless reviewer invocation (the full diff or one
// budgeted part of a diff).
type PromptTask struct {
	TaskID     string `json:"task_id"`
	Agent      string `json:"agent"`
	Mode       string `json:"mode"`
	Part       int    `json:"part"`
	TotalParts int    `json:"total_parts"`
	PromptFile string `json:"prompt_file"`
	OutputFile string `json:"output_file"`
	ErrorFile  string `json:"error_file"`
}

// PromptPayload is written to <run-dir>/<prompt_file> so an external runner can
// dispatch each reviewer task without re-computing git diffs or spec contexts.
type PromptPayload struct {
	TaskID       string         `json:"task_id"`
	Agent        string         `json:"agent"`
	Mode         string         `json:"mode"`
	Base         string         `json:"base"`
	Head         string         `json:"head"`
	Part         int            `json:"part"`
	TotalParts   int            `json:"total_parts"`
	SystemPrompt string         `json:"system_prompt"`
	UserPrompt   string         `json:"user_prompt"`
	ReportSchema map[string]any `json:"report_schema"`
}

// ManifestAgentState records how Prepare handled each planned agent.
type ManifestAgentState struct {
	Agent       string        `json:"agent"`
	Mode        string        `json:"mode"`
	Files       []string      `json:"files,omitempty"`
	Status      string        `json:"status"` // "pending" | "carried" | "empty" | "skipped" | "missing_def" | "diff_error"
	Parts       int           `json:"parts,omitempty"`
	Summary     string        `json:"summary,omitempty"`
	Error       string        `json:"error,omitempty"`
	CarriedFrom *gate.Carried `json:"carried_from,omitempty"`
}

// RunManifest is persisted to .review/<run-id>/manifest.json by Prepare and
// consumed by Finalize.
type RunManifest struct {
	Schema     string               `json:"schema"`
	RunID      string               `json:"run_id"`
	Repo       string               `json:"repo"`
	BaseRef    string               `json:"base_ref"`
	HeadRef    string               `json:"head_ref"`
	Base       string               `json:"base"`
	Head       string               `json:"head"`
	Branch     string               `json:"branch,omitempty"`
	DiffHash   string               `json:"diff_hash"`
	PR         string               `json:"pr,omitempty"`
	PRURL      string               `json:"pr_url,omitempty"`
	Engine     string               `json:"engine"`
	StartedAt  time.Time            `json:"started_at"`
	NotCurrent bool                 `json:"not_current,omitempty"`
	CarryError string               `json:"carry_error,omitempty"`
	Agents     []ManifestAgentState `json:"agents"`
	Tasks      []PromptTask         `json:"tasks"`
}

// PreparedRun is returned by Engine.Prepare.
type PreparedRun struct {
	RunID       string       `json:"run_id"`
	RunDir      string       `json:"run_dir"`
	Status      string       `json:"status"` // "pending" | "reused" | "completed"
	Engine      string       `json:"engine"`
	PR          string       `json:"pr,omitempty"`
	PRURL       string       `json:"pr_url,omitempty"`
	CarriedFrom string       `json:"carried_from,omitempty"`
	CarryError  string       `json:"carry_error,omitempty"`
	Plan        *gate.Plan   `json:"plan,omitempty"`
	Tasks       []PromptTask `json:"tasks"`
	Result      *Result      `json:"result,omitempty"`
}

// FinalizeOptions configures Engine.Finalize.
type FinalizeOptions struct {
	Repo    string // repository root (optional when Run is a path inside <repo>/.review/<run-id>)
	Run     string // run ID or run directory path (optional; defaults to current_run.json in Repo)
	PR      string // pull request number override
	Publish bool   // run .claude/review/publish.sh when finalized
}

// reviewerReportJSONSchema returns a closed inline JSON schema compatible with
// Antigravity's run_workflow agent(..., schema=...) contract (no $ref, $defs,
// definitions, if/then/else, or allOf/anyOf/oneOf, and additionalProperties:
// false on every object).
func reviewerReportJSONSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"summary", "findings", "handoffs"},
		"properties": map[string]any{
			"summary": map[string]any{
				"type": "string",
			},
			"findings": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required": []string{
						"id",
						"severity",
						"category",
						"title",
						"location",
						"evidence",
						"recommendation",
						"confidence",
					},
					"properties": map[string]any{
						"id": map[string]any{
							"type": "string",
						},
						"severity": map[string]any{
							"type": "string",
							"enum": slices.Clone(gate.Severities),
						},
						"category": map[string]any{
							"type": "string",
							"enum": slices.Clone(gate.Categories),
						},
						"title": map[string]any{
							"type": "string",
						},
						"location": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"required":             []string{"file"},
							"properties": map[string]any{
								"file": map[string]any{
									"type": "string",
								},
								"line": map[string]any{
									"type": "integer",
								},
								"end_line": map[string]any{
									"type": "integer",
								},
							},
						},
						"spec_ref": map[string]any{
							"type": "string",
						},
						"evidence": map[string]any{
							"type": "string",
						},
						"reproduction": map[string]any{
							"type": "string",
						},
						"recommendation": map[string]any{
							"type": "string",
						},
						"confidence": map[string]any{
							"type": "number",
						},
					},
				},
			},
			"handoffs": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"to", "reason"},
					"properties": map[string]any{
						"to": map[string]any{
							"type": "string",
						},
						"reason": map[string]any{
							"type": "string",
						},
					},
				},
			},
			"adrs": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"slug", "title", "body"},
					"properties": map[string]any{
						"slug": map[string]any{
							"type": "string",
						},
						"title": map[string]any{
							"type": "string",
						},
						"finding": map[string]any{
							"type": "string",
						},
						"body": map[string]any{
							"type": "string",
						},
					},
				},
			},
		},
	}
}

func (e *Engine) preparePromptBudget() int {
	budget := e.Config.AntigravityPromptBudget
	if budget <= 0 {
		budget = DefaultAntigravityPromptBudget
	}
	if e.Config.PromptBudget > 0 && e.Config.PromptBudget < budget {
		budget = e.Config.PromptBudget
	}
	return budget
}

// Prepare plans a review run, reuses cached/carried verdicts where valid, and
// materializes prompt bundles for any remaining agents without calling an LLM.
func (e *Engine) Prepare(ctx context.Context, opt Options) (*PreparedRun, error) {
	budget := e.preparePromptBudget()
	eff := e
	if maxSpec := max(0, budget-antigravitySpecHeadroomTokens); e.Config.SpecBudget > maxSpec {
		copied := *e
		copied.Config.SpecBudget = maxSpec
		eff = &copied
	}

	engineLabel := firstNonEmpty(opt.EngineLabel, DefaultAntigravityEngine)
	ri, err := eff.initRun(ctx, opt, engineLabel)
	if err != nil {
		return nil, err
	}
	if ri.reused != nil {
		return &PreparedRun{
			RunID:       ri.reused.RunID,
			RunDir:      ri.reused.RunDir,
			Status:      "reused",
			Engine:      engineLabel,
			PR:          ri.reused.PR,
			PRURL:       ri.reused.PRURL,
			CarriedFrom: ri.reused.CarriedFrom,
			CarryError:  ri.reused.CarryError,
			Plan:        ri.reused.Plan,
			Tasks:       []PromptTask{},
			Result:      ri.reused,
		}, nil
	}

	shared := sharedPRContext(ri.pr)
	schema := reviewerReportJSONSchema()
	manifestAgents := make([]ManifestAgentState, 0, len(ri.plan.Agents))
	tasks := []PromptTask{}

	recordFailure := func(a gate.Planned, status, msg string) {
		_ = os.Remove(filepath.Join(ri.dir, a.Agent+".json"))
		_ = os.WriteFile(filepath.Join(ri.dir, a.Agent+".error.txt"), []byte(msg+"\n"), 0o644)
		ri.st.failed(a.Agent, msg)
		manifestAgents = append(manifestAgents, ManifestAgentState{
			Agent:  a.Agent,
			Mode:   a.Mode,
			Files:  a.Files,
			Status: status,
			Error:  msg,
		})
	}
	recordEmpty := func(a gate.Planned, status, summary string) {
		rep := emptyReport(a.Agent, a.Mode, ri.plan.Base, ri.plan.Head, summary)
		if err := writeJSON(filepath.Join(ri.dir, a.Agent+".json"), rep); err != nil {
			recordFailure(a, "diff_error", "writing the report: "+err.Error())
			return
		}
		ri.st.completed(a.Agent, rep, usage{})
		manifestAgents = append(manifestAgents, ManifestAgentState{
			Agent:   a.Agent,
			Mode:    a.Mode,
			Files:   a.Files,
			Status:  status,
			Summary: rep.Summary,
		})
	}

	dirsCreated := false
	for _, a := range ri.plan.Agents {
		if err := ctx.Err(); err != nil {
			ri.st.fail("review prepare stopped: cancelled")
			return nil, err
		}
		if !isSafeSegment(a.Agent) {
			err := fmt.Errorf("unsafe agent name %q in gate plan", a.Agent)
			ri.st.fail(err.Error())
			return nil, err
		}
		if ca, ok := ri.carriedAgents[a.Agent]; ok {
			manifestAgents = append(manifestAgents, ManifestAgentState{
				Agent:       a.Agent,
				Mode:        a.Mode,
				Files:       a.Files,
				Status:      "carried",
				Summary:     ca.report.Summary,
				CarriedFrom: ca.origin,
			})
			continue
		}
		agentDiff, err := eff.scopedDiff(ctx, ri.repo, ri.plan, a, ri.diff)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				ri.st.fail("review prepare stopped: cancelled")
				return nil, cerr
			}
			recordFailure(a, "diff_error", err.Error())
			continue
		}
		if agentDiff == "" {
			recordEmpty(a, "empty", "No changes matching this agent's scoped paths.")
			continue
		}
		def, ok := eff.agentDefinition(ri.repo, a.Agent)
		if !ok {
			if a.Mode == "gate" {
				recordFailure(a, "missing_def", fmt.Sprintf("agent definition %s.md not found", a.Agent))
				continue
			}
			recordEmpty(a, "skipped", fmt.Sprintf("Agent %s definition missing; skipped.", a.Agent))
			continue
		}

		req := buildAgentRequest(a, ri.plan, agentDiff, shared, ri.specOf(a), def)
		s, parts := planParts(req, budget)
		ri.st.planned(a.Agent, engineLabel, len(parts))
		ri.st.queued(a.Agent)

		if !dirsCreated {
			if err := os.MkdirAll(filepath.Join(ri.dir, "prompts"), 0o755); err != nil {
				ri.st.fail(err.Error())
				return nil, err
			}
			if err := os.MkdirAll(filepath.Join(ri.dir, "raw"), 0o755); err != nil {
				ri.st.fail(err.Error())
				return nil, err
			}
			dirsCreated = true
		}

		for i, p := range parts {
			taskID := a.Agent
			if len(parts) > 1 {
				taskID = fmt.Sprintf("%s.part-%d", a.Agent, i+1)
			}
			promptRel := "prompts/" + taskID + ".json"
			outRel := "raw/" + taskID + ".json"
			errRel := "raw/" + taskID + ".err"

			payload := PromptPayload{
				TaskID:       taskID,
				Agent:        a.Agent,
				Mode:         a.Mode,
				Base:         ri.plan.Base,
				Head:         ri.plan.Head,
				Part:         i + 1,
				TotalParts:   len(parts),
				SystemPrompt: sharedSystemPrompt,
				UserPrompt:   userPrompt(req, s, p),
				ReportSchema: schema,
			}
			if err := writeJSON(filepath.Join(ri.dir, filepath.FromSlash(promptRel)), &payload); err != nil {
				ri.st.fail(err.Error())
				return nil, err
			}
			tasks = append(tasks, PromptTask{
				TaskID:     taskID,
				Agent:      a.Agent,
				Mode:       a.Mode,
				Part:       i + 1,
				TotalParts: len(parts),
				PromptFile: promptRel,
				OutputFile: outRel,
				ErrorFile:  errRel,
			})
		}
		manifestAgents = append(manifestAgents, ManifestAgentState{
			Agent:  a.Agent,
			Mode:   a.Mode,
			Files:  a.Files,
			Status: "pending",
			Parts:  len(parts),
		})
	}
	if err := ctx.Err(); err != nil {
		ri.st.fail("review prepare stopped: cancelled")
		return nil, err
	}

	manifest := RunManifest{
		Schema:     ManifestSchema,
		RunID:      ri.runID,
		Repo:       ri.repo,
		BaseRef:    ri.baseRef,
		HeadRef:    ri.headRef,
		Base:       ri.plan.Base,
		Head:       ri.plan.Head,
		Branch:     ri.branch,
		DiffHash:   ri.diffHash,
		PR:         ri.pr.Number,
		PRURL:      ri.pr.URL,
		Engine:     engineLabel,
		StartedAt:  ri.st.startedAt,
		NotCurrent: ri.notCurrent,
		CarryError: ri.carryErr,
		Agents:     manifestAgents,
		Tasks:      tasks,
	}
	if err := writeJSON(filepath.Join(ri.dir, "manifest.json"), &manifest); err != nil {
		ri.st.fail(err.Error())
		return nil, err
	}

	if len(tasks) == 0 {
		allCarried := len(ri.plan.Agents) > 0 && len(ri.carriedAgents) == len(ri.plan.Agents)
		res := eff.finishRun(ctx, ri.repo, ri.dir, ri.cfg, ri.plan, ri.pr, ri.carryErr, opt.Publish, ri.notCurrent, ri.st, allCarried, true)
		return &PreparedRun{
			RunID:      ri.runID,
			RunDir:     ri.dir,
			Status:     "completed",
			Engine:     engineLabel,
			PR:         res.PR,
			PRURL:      res.PRURL,
			CarryError: ri.carryErr,
			Plan:       ri.plan,
			Tasks:      []PromptTask{},
			Result:     res,
		}, nil
	}

	return &PreparedRun{
		RunID:      ri.runID,
		RunDir:     ri.dir,
		Status:     "pending",
		Engine:     engineLabel,
		PR:         ri.pr.Number,
		PRURL:      ri.pr.URL,
		CarryError: ri.carryErr,
		Plan:       ri.plan,
		Tasks:      tasks,
	}, nil
}

// Finalize reads raw reviewer outputs produced for a prepared run, merges
// multi-part outputs, normalizes reports, runs gate.Check, writes status.json
// and rendered.md, and optionally publishes the result.
func (e *Engine) Finalize(ctx context.Context, opt FinalizeOptions) (*Result, error) {
	paths, err := resolveFinalizePaths(opt.Repo, opt.Run, e.Config.DefaultRepo)
	if err != nil {
		return nil, err
	}

	manifestPath, err := resolveSafeRunFile(paths.dirReal, "manifest.json")
	if err != nil {
		return nil, err
	}
	mb, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("finalize: reading manifest.json: %w", err)
	}
	var manifest RunManifest
	if err := json.Unmarshal(mb, &manifest); err != nil {
		return nil, fmt.Errorf("finalize: decoding manifest.json: %w", err)
	}
	if manifest.Schema != ManifestSchema {
		return nil, fmt.Errorf("finalize: unsupported manifest schema %q (want %q)", manifest.Schema, ManifestSchema)
	}
	if !isSafeSegment(manifest.RunID) || manifest.RunID != paths.runID {
		return nil, fmt.Errorf("finalize: manifest run_id %q does not match run directory %q", manifest.RunID, paths.runID)
	}

	planPath, err := resolveSafeRunFile(paths.dirReal, "plan.json")
	if err != nil {
		return nil, err
	}
	pb, err := os.ReadFile(planPath)
	if err != nil {
		return nil, fmt.Errorf("finalize: reading plan.json: %w", err)
	}
	var plan gate.Plan
	if err := json.Unmarshal(pb, &plan); err != nil {
		return nil, fmt.Errorf("finalize: decoding plan.json: %w", err)
	}
	if plan.Schema != gate.PlanSchema {
		return nil, fmt.Errorf("finalize: unsupported plan schema %q (want %q)", plan.Schema, gate.PlanSchema)
	}

	for _, a := range plan.Agents {
		if !isSafeSegment(a.Agent) {
			return nil, fmt.Errorf("finalize: unsafe agent name %q in plan.json", a.Agent)
		}
	}

	manifestByAgent := make(map[string]ManifestAgentState, len(manifest.Agents))
	for _, ms := range manifest.Agents {
		if !isSafeSegment(ms.Agent) {
			return nil, fmt.Errorf("finalize: unsafe agent name %q in manifest.json", ms.Agent)
		}
		if !slices.Contains(gate.Modes, ms.Mode) {
			return nil, fmt.Errorf("finalize: invalid mode %q for agent %q in manifest.json", ms.Mode, ms.Agent)
		}
		if _, err := resolveSafeRunFile(paths.dirReal, ms.Agent+".json"); err != nil {
			return nil, err
		}
		if _, err := resolveSafeRunFile(paths.dirReal, ms.Agent+".error.txt"); err != nil {
			return nil, err
		}
		manifestByAgent[ms.Agent] = ms
	}
	for _, a := range plan.Agents {
		if _, ok := manifestByAgent[a.Agent]; !ok {
			return nil, fmt.Errorf("finalize: planned agent %q missing from manifest.json", a.Agent)
		}
	}

	type validatedTask struct {
		PromptTask
		outPath string
		errPath string
	}
	tasksByAgent := map[string]map[int]validatedTask{}
	for _, t := range manifest.Tasks {
		if !isSafeSegment(t.Agent) {
			return nil, fmt.Errorf("finalize: unsafe task agent %q in manifest.json", t.Agent)
		}
		if !isSafeSegment(t.TaskID) {
			return nil, fmt.Errorf("finalize: unsafe task_id %q in manifest.json", t.TaskID)
		}
		if t.Part < 1 || t.TotalParts < 1 || t.Part > t.TotalParts {
			return nil, fmt.Errorf("finalize: invalid part %d/%d for task %q", t.Part, t.TotalParts, t.TaskID)
		}
		if _, err := resolveSafeRunFile(paths.dirReal, t.PromptFile); err != nil {
			return nil, fmt.Errorf("finalize: task %q prompt_file: %w", t.TaskID, err)
		}
		outPath, err := resolveSafeRunFile(paths.dirReal, t.OutputFile)
		if err != nil {
			return nil, fmt.Errorf("finalize: task %q output_file: %w", t.TaskID, err)
		}
		errPath, err := resolveSafeRunFile(paths.dirReal, t.ErrorFile)
		if err != nil {
			return nil, fmt.Errorf("finalize: task %q error_file: %w", t.TaskID, err)
		}
		if tasksByAgent[t.Agent] == nil {
			tasksByAgent[t.Agent] = map[int]validatedTask{}
		}
		if _, dup := tasksByAgent[t.Agent][t.Part]; dup {
			return nil, fmt.Errorf("finalize: duplicate part %d for agent %q", t.Part, t.Agent)
		}
		tasksByAgent[t.Agent][t.Part] = validatedTask{PromptTask: t, outPath: outPath, errPath: errPath}
	}

	statusPath, err := resolveSafeRunFile(paths.dirReal, "status.json")
	if err != nil {
		return nil, err
	}
	if _, err := resolveSafeRunFile(paths.dirReal, "rendered.md"); err != nil {
		return nil, err
	}

	cfg, err := gate.LoadConfig(e.gatesPath(paths.repoAbs))
	if err != nil {
		return nil, err
	}

	prevStatus := readStatusObject(statusPath)
	pr := PullRequest{Number: manifest.PR, URL: manifest.PRURL}
	if opt.PR != "" {
		pr = e.detectPR(ctx, paths.repoAbs, manifest.Branch, opt.PR)
	} else if pr.Number == "" && opt.Publish {
		pr = e.detectPR(ctx, paths.repoAbs, manifest.Branch, "")
	}

	alreadyPublished := ""
	if asText(prevStatus.Get("state")) == stateCompleted {
		prevPub := asText(prevStatus.Get("published_status"))
		prevPR := asText(prevStatus.Get("pr"))
		if prevPub != "" && pr.Number != "" && prevPR == pr.Number {
			alreadyPublished = prevPub
		}
	}

	startedAt := manifest.StartedAt
	if startedAt.IsZero() {
		startedAt = e.now()
	}
	engineLabel := firstNonEmpty(manifest.Engine, DefaultAntigravityEngine)
	st := &runStatus{
		dir:           paths.dirAbs,
		runID:         manifest.RunID,
		baseRef:       manifest.BaseRef,
		headRef:       manifest.HeadRef,
		baseSHA:       plan.Base,
		headSHA:       plan.Head,
		branch:        manifest.Branch,
		diffHash:      manifest.DiffHash,
		pr:            pr.Number,
		prURL:         pr.URL,
		state:         stateRunning,
		defaultEngine: engineLabel,
		published:     alreadyPublished,
		agents:        map[string]*agentStatus{},
		startedAt:     startedAt,
		now:           e.now,
		notCurrent:    manifest.NotCurrent,
	}

	carriedCount := 0
	for _, a := range plan.Agents {
		ms := manifestByAgent[a.Agent]
		st.update(func() {
			s := st.agent(a.Agent)
			s.mode, s.files = a.Mode, a.Files
		})

		reportPath := filepath.Join(paths.dirReal, a.Agent+".json")
		errFilePath := filepath.Join(paths.dirReal, a.Agent+".error.txt")
		failAgent := func(msg string) {
			_ = os.Remove(reportPath)
			_ = os.WriteFile(errFilePath, []byte(msg+"\n"), 0o644)
			st.failed(a.Agent, msg)
		}

		switch ms.Status {
		case "carried":
			rb, err := os.ReadFile(reportPath)
			if err != nil {
				failAgent("reading carried report: " + err.Error())
				continue
			}
			var rep Report
			if err := json.Unmarshal(rb, &rep); err != nil {
				failAgent("decoding carried report: " + err.Error())
				continue
			}
			carriedCount++
			st.update(func() {
				st.agent(a.Agent).applyCarried(&rep, ms.CarriedFrom, startedAt)
			})

		case "empty", "skipped":
			rb, err := os.ReadFile(reportPath)
			var rep Report
			if err != nil || json.Unmarshal(rb, &rep) != nil {
				fallback := emptyReport(a.Agent, a.Mode, plan.Base, plan.Head, firstNonEmpty(ms.Summary, "Review completed."))
				if werr := writeJSON(reportPath, fallback); werr != nil {
					failAgent("writing empty report: " + werr.Error())
					continue
				}
				rep = *fallback
			}
			_ = os.Remove(errFilePath)
			st.completed(a.Agent, &rep, usage{})

		case "missing_def", "diff_error":
			failAgent(firstNonEmpty(ms.Error, "agent failed during prepare"))

		case "pending":
			if ms.Parts < 1 {
				failAgent("invalid pending agent parts count in manifest")
				continue
			}
			st.planned(a.Agent, engineLabel, ms.Parts)
			partMap := tasksByAgent[a.Agent]
			if len(partMap) != ms.Parts {
				failAgent(fmt.Sprintf("incomplete task list in manifest: have %d, want %d", len(partMap), ms.Parts))
				continue
			}

			partResults := make([]*Object, 0, ms.Parts)
			partFailed := false
			for partIdx := 1; partIdx <= ms.Parts; partIdx++ {
				task, ok := partMap[partIdx]
				if !ok || task.TotalParts != ms.Parts {
					failAgent(fmt.Sprintf("missing or mismatched part %d/%d in manifest", partIdx, ms.Parts))
					partFailed = true
					break
				}
				errBytes, errReadErr := os.ReadFile(task.errPath)
				if errReadErr == nil {
					if errText := strings.TrimSpace(string(errBytes)); errText != "" {
						failAgent(fmt.Sprintf("task %s failed: %s", task.TaskID, errText))
						partFailed = true
						break
					}
				} else if !errors.Is(errReadErr, os.ErrNotExist) {
					failAgent(fmt.Sprintf("task %s: reading error file: %v", task.TaskID, errReadErr))
					partFailed = true
					break
				}

				outBytes, outReadErr := os.ReadFile(task.outPath)
				if outReadErr != nil {
					switch {
					case errReadErr == nil:
						failAgent(fmt.Sprintf("task %s: reviewer task failed with empty error file", task.TaskID))
					case errors.Is(outReadErr, os.ErrNotExist):
						failAgent(fmt.Sprintf("task %s: missing output file %s", task.TaskID, task.OutputFile))
					default:
						failAgent(fmt.Sprintf("task %s: reading output file %s: %v", task.TaskID, task.OutputFile, outReadErr))
					}
					partFailed = true
					break
				}

				obj, err := extractJSON(string(outBytes))
				if err != nil {
					failAgent(fmt.Sprintf("task %s: %v", task.TaskID, err))
					partFailed = true
					break
				}
				if !obj.Has("summary") && !obj.Has("findings") {
					failAgent(fmt.Sprintf("task %s: invalid report object (missing summary and findings)", task.TaskID))
					partFailed = true
					break
				}
				if obj.Has("findings") {
					if _, ok := obj.Get("findings").([]any); !ok {
						failAgent(fmt.Sprintf("task %s: invalid report object (findings must be an array)", task.TaskID))
						partFailed = true
						break
					}
				}
				partResults = append(partResults, obj)
			}
			if partFailed {
				continue
			}

			merged := mergeParts(partResults)
			rep, err := Normalize(merged, AgentRun{
				Agent: a.Agent,
				Mode:  a.Mode,
				Base:  plan.Base,
				Head:  plan.Head,
			})
			if err != nil {
				failAgent(err.Error())
				continue
			}
			if err := writeJSON(reportPath, rep); err != nil {
				failAgent("writing the report: " + err.Error())
				continue
			}
			_ = os.Remove(errFilePath)
			st.partDone(a.Agent, ms.Parts, ms.Parts)
			st.completed(a.Agent, rep, usage{Engine: engineLabel})

		default:
			failAgent(fmt.Sprintf("unknown manifest agent status %q", ms.Status))
		}
	}

	allCarried := len(plan.Agents) > 0 && carriedCount == len(plan.Agents)
	return e.finishRun(ctx, paths.repoAbs, paths.dirAbs, cfg, &plan, pr, manifest.CarryError, opt.Publish, manifest.NotCurrent, st, allCarried, true), nil
}

type finalizePaths struct {
	repoAbs  string
	repoReal string
	dirAbs   string
	dirReal  string
	runID    string
}

func resolveFinalizePaths(repoOpt, runOpt, defaultRepo string) (*finalizePaths, error) {
	// Case 1: no Run specified -> resolve via <repo>/.review/current_run.json.
	if runOpt == "" {
		repoAbs, err := filepath.Abs(firstNonEmpty(repoOpt, defaultRepo, "."))
		if err != nil {
			return nil, err
		}
		repoReal, err := filepath.EvalSymlinks(repoAbs)
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving repository %q: %w", repoAbs, err)
		}
		if err := checkNoSymlinkEscape(repoReal, filepath.Join(repoReal, ".review")); err != nil {
			return nil, fmt.Errorf("finalize: .review directory: %w", err)
		}
		reviewReal, err := filepath.EvalSymlinks(filepath.Join(repoReal, ".review"))
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving .review directory: %w", err)
		}
		curPath := filepath.Join(reviewReal, "current_run.json")
		if err := checkNoSymlinkEscape(reviewReal, curPath); err != nil {
			return nil, fmt.Errorf("finalize: current_run.json: %w", err)
		}
		b, err := os.ReadFile(curPath)
		if err != nil {
			return nil, fmt.Errorf("finalize: reading current_run.json: %w", err)
		}
		var cur struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(b, &cur); err != nil {
			return nil, fmt.Errorf("finalize: decoding current_run.json: %w", err)
		}
		if !isSafeSegment(cur.RunID) {
			return nil, fmt.Errorf("finalize: invalid run_id %q in current_run.json", cur.RunID)
		}
		candReal := filepath.Join(reviewReal, cur.RunID)
		if err := checkNoSymlinkEscape(reviewReal, candReal); err != nil {
			return nil, fmt.Errorf("finalize: run %q: %w", cur.RunID, err)
		}
		dirReal, err := filepath.EvalSymlinks(candReal)
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving run %q: %w", cur.RunID, err)
		}
		fi, err := os.Stat(dirReal)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("finalize: run %q is not a directory", cur.RunID)
		}
		if filepath.Dir(dirReal) != reviewReal {
			return nil, fmt.Errorf("finalize: run %q escapes .review directory", cur.RunID)
		}
		return &finalizePaths{
			repoAbs:  repoAbs,
			repoReal: repoReal,
			dirAbs:   filepath.Join(repoAbs, ".review", cur.RunID),
			dirReal:  dirReal,
			runID:    cur.RunID,
		}, nil
	}

	// Case 2: bare run ID (no path separators).
	if !strings.ContainsAny(runOpt, `/\`) {
		if !isSafeSegment(runOpt) {
			return nil, fmt.Errorf("finalize: invalid run id %q", runOpt)
		}
		repoAbs, err := filepath.Abs(firstNonEmpty(repoOpt, defaultRepo, "."))
		if err != nil {
			return nil, err
		}
		repoReal, err := filepath.EvalSymlinks(repoAbs)
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving repository %q: %w", repoAbs, err)
		}
		if err := checkNoSymlinkEscape(repoReal, filepath.Join(repoReal, ".review")); err != nil {
			return nil, fmt.Errorf("finalize: .review directory: %w", err)
		}
		reviewReal, err := filepath.EvalSymlinks(filepath.Join(repoReal, ".review"))
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving .review directory: %w", err)
		}
		candReal := filepath.Join(reviewReal, runOpt)
		if err := checkNoSymlinkEscape(reviewReal, candReal); err != nil {
			return nil, fmt.Errorf("finalize: run %q: %w", runOpt, err)
		}
		dirReal, err := filepath.EvalSymlinks(candReal)
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving run %q: %w", runOpt, err)
		}
		fi, err := os.Stat(dirReal)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("finalize: run %q is not a directory", runOpt)
		}
		if filepath.Dir(dirReal) != reviewReal {
			return nil, fmt.Errorf("finalize: run %q escapes .review directory", runOpt)
		}
		return &finalizePaths{
			repoAbs:  repoAbs,
			repoReal: repoReal,
			dirAbs:   filepath.Join(repoAbs, ".review", runOpt),
			dirReal:  dirReal,
			runID:    runOpt,
		}, nil
	}

	// Case 3: relative or absolute directory path.
	dirAbs, err := filepath.Abs(runOpt)
	if err != nil {
		return nil, err
	}
	if !isSafeSegment(filepath.Base(dirAbs)) || filepath.Base(filepath.Dir(dirAbs)) != ".review" {
		return nil, fmt.Errorf("finalize: run directory %q must be inside a repository's .review/<run-id> directory", runOpt)
	}
	inferredRepoAbs := filepath.Dir(filepath.Dir(dirAbs))
	inferredRepoReal, err := filepath.EvalSymlinks(inferredRepoAbs)
	if err != nil {
		return nil, fmt.Errorf("finalize: resolving repository for %q: %w", runOpt, err)
	}
	if err := checkNoSymlinkEscape(inferredRepoReal, filepath.Join(inferredRepoReal, ".review")); err != nil {
		return nil, fmt.Errorf("finalize: .review directory: %w", err)
	}
	if lfi, lerr := os.Lstat(dirAbs); lerr != nil {
		return nil, fmt.Errorf("finalize: resolving run directory %q: %w", runOpt, lerr)
	} else if lfi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("finalize: run directory %q must not be a symlink", runOpt)
	}
	dirReal, err := filepath.EvalSymlinks(dirAbs)
	if err != nil {
		return nil, fmt.Errorf("finalize: resolving run directory %q: %w", runOpt, err)
	}
	fi, err := os.Stat(dirReal)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("finalize: %q is not a directory", runOpt)
	}
	runID := filepath.Base(dirReal)
	if !isSafeSegment(runID) || filepath.Base(filepath.Dir(dirReal)) != ".review" {
		return nil, fmt.Errorf("finalize: run directory %q must be inside a repository's .review/<run-id> directory", runOpt)
	}

	var repoAbs, repoReal string
	if repoOpt != "" {
		repoAbs, err = filepath.Abs(repoOpt)
		if err != nil {
			return nil, err
		}
		repoReal, err = filepath.EvalSymlinks(repoAbs)
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving repository %q: %w", repoOpt, err)
		}
		if err := checkNoSymlinkEscape(repoReal, filepath.Join(repoReal, ".review")); err != nil {
			return nil, fmt.Errorf("finalize: .review directory: %w", err)
		}
		reviewReal, err := filepath.EvalSymlinks(filepath.Join(repoReal, ".review"))
		if err != nil {
			return nil, fmt.Errorf("finalize: resolving .review directory: %w", err)
		}
		if filepath.Dir(dirReal) != reviewReal {
			return nil, fmt.Errorf("finalize: run directory %q is outside repository %q", runOpt, repoOpt)
		}
	} else {
		if filepath.Dir(filepath.Dir(dirReal)) != inferredRepoReal {
			return nil, fmt.Errorf("finalize: run directory %q escapes repository %q", runOpt, inferredRepoAbs)
		}
		repoReal = inferredRepoReal
		repoAbs = inferredRepoAbs
	}

	return &finalizePaths{
		repoAbs:  repoAbs,
		repoReal: repoReal,
		dirAbs:   filepath.Join(repoAbs, ".review", runID),
		dirReal:  dirReal,
		runID:    runID,
	}, nil
}

func isSafeSegment(s string) bool {
	return s != "" &&
		s != "." &&
		s != ".." &&
		!strings.HasPrefix(s, ".") &&
		!strings.ContainsAny(s, `/\`) &&
		filepath.Base(s) == s
}

func strictlyInside(dir, path string) bool {
	dirClean := filepath.Clean(dir)
	pathClean := filepath.Clean(path)
	return pathClean != dirClean && spec.Inside(dirClean, pathClean)
}

func resolveSafeRunFile(dirReal, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("empty file path")
	}
	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") || strings.Contains(relPath, `\`) {
		return "", fmt.Errorf("path %q must be a slash-separated path relative to the run directory", relPath)
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes run directory", relPath)
	}
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if !isSafeSegment(seg) {
			return "", fmt.Errorf("path %q contains unsafe segment %q", relPath, seg)
		}
	}
	joined := filepath.Join(dirReal, clean)
	if !strictlyInside(dirReal, joined) {
		return "", fmt.Errorf("path %q escapes run directory", relPath)
	}
	if err := checkNoSymlinkEscape(dirReal, joined); err != nil {
		return "", fmt.Errorf("path %q: %w", relPath, err)
	}
	return joined, nil
}

// checkNoSymlinkEscape walks every existing path prefix from baseReal down to
// target and rejects any symlink whose destination is dangling or resolves
// outside baseReal.
func checkNoSymlinkEscape(baseReal, target string) error {
	rel, err := filepath.Rel(baseReal, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("escapes %s", baseReal)
	}
	cur := baseReal
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return fmt.Errorf("dangling symlink %q", cur)
			}
			if !strictlyInside(baseReal, resolved) {
				return fmt.Errorf("symlink %q escapes %s", cur, baseReal)
			}
		}
	}
	return nil
}
