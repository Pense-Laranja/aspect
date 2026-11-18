package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mutateManifest(t *testing.T, runDir string, fn func(m *RunManifest)) {
	t.Helper()
	p := filepath.Join(runDir, "manifest.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m RunManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	fn(&m)
	if err := writeJSON(p, &m); err != nil {
		t.Fatal(err)
	}
}

func TestFinalize_RejectsPathTraversalAndSymlinkEscapes(t *testing.T) {
	outsideRoot := t.TempDir()
	outsideFile := filepath.Join(outsideRoot, "outside.json")
	writeFile(t, outsideFile, clean)
	outsideMissing := filepath.Join(outsideRoot, "dangling-target.txt")

	cases := []struct {
		name  string
		setup func(t *testing.T, repo string, eng *Engine, prep *PreparedRun) FinalizeOptions
	}{
		{
			name: "Run option dot-dot segment",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				return FinalizeOptions{Repo: repo, Run: ".."}
			},
		},
		{
			name: "Run option hidden segment",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				return FinalizeOptions{Repo: repo, Run: ".hidden"}
			},
		},
		{
			name: "Run option parent traversal path",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				return FinalizeOptions{Repo: repo, Run: filepath.Join(repo, ".review", "..", "..")}
			},
		},
		{
			name: "Run option pointing to .review directory itself",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				return FinalizeOptions{Repo: repo, Run: filepath.Join(repo, ".review")}
			},
		},
		{
			name: "Run option pointing to another repository's run while Repo is set",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				otherRepo := testRepo(t)
				otherEng := newPrepareEngine(t, otherRepo)
				otherPrep, err := otherEng.Prepare(context.Background(), Options{Repo: otherRepo, Base: "main"})
				if err != nil {
					t.Fatal(err)
				}
				return FinalizeOptions{Repo: repo, Run: otherPrep.RunDir}
			},
		},
		{
			name: "Run option symlink inside .review escaping repository",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				otherRepo := testRepo(t)
				otherEng := newPrepareEngine(t, otherRepo)
				otherPrep, err := otherEng.Prepare(context.Background(), Options{Repo: otherRepo, Base: "main"})
				if err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(repo, ".review", "local-symlink")
				if err := os.Symlink(otherPrep.RunDir, link); err != nil {
					t.Fatal(err)
				}
				return FinalizeOptions{Repo: repo, Run: "local-symlink"}
			},
		},
		{
			name: "Run path symlink without Repo option",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				otherRepo := testRepo(t)
				otherEng := newPrepareEngine(t, otherRepo)
				otherPrep, err := otherEng.Prepare(context.Background(), Options{Repo: otherRepo, Base: "main"})
				if err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(repo, ".review", "local-symlink-path")
				if err := os.Symlink(otherPrep.RunDir, link); err != nil {
					t.Fatal(err)
				}
				return FinalizeOptions{Run: link}
			},
		},
		{
			name: "current_run.json traversal run_id",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				writeFile(t, filepath.Join(repo, ".review", "current_run.json"), `{"run_id": "../outside"}`)
				return FinalizeOptions{Repo: repo}
			},
		},
		{
			name: "current_run.json symlink run_id escaping .review",
			setup: func(t *testing.T, repo string, _ *Engine, _ *PreparedRun) FinalizeOptions {
				link := filepath.Join(repo, ".review", "local-escaped")
				if err := os.Symlink(outsideRoot, link); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(repo, ".review", "current_run.json"), `{"run_id": "local-escaped"}`)
				return FinalizeOptions{Repo: repo}
			},
		},
		{
			name: "manifest schema mismatch",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Schema = "wrong/v0" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "manifest run_id mismatch",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.RunID = "local-other" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "manifest Agent traversal",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Agents[0].Agent = "../evil" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "manifest Task Agent traversal",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Tasks[0].Agent = "../evil" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "manifest TaskID path separator",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Tasks[0].TaskID = "sub/task" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "PromptFile traversal",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Tasks[0].PromptFile = "../plan.json" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "OutputFile absolute path",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Tasks[0].OutputFile = outsideFile })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "OutputFile relative parent escape",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Tasks[0].OutputFile = "raw/../../outside.json" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "OutputFile backslash separator",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Tasks[0].OutputFile = `raw\..\outside.json` })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "ErrorFile parent traversal",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				mutateManifest(t, prep.RunDir, func(m *RunManifest) { m.Tasks[0].ErrorFile = "../../outside.err" })
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "OutputFile symlink escaping run directory",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				outLink := filepath.Join(prep.RunDir, filepath.FromSlash(prep.Tasks[0].OutputFile))
				_ = os.Remove(outLink)
				if err := os.Symlink(outsideFile, outLink); err != nil {
					t.Fatal(err)
				}
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "dangling symlink on agent report output path",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				reportLink := filepath.Join(prep.RunDir, prep.Tasks[0].Agent+".json")
				if err := os.Symlink(outsideMissing, reportLink); err != nil {
					t.Fatal(err)
				}
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
		{
			name: "directory symlink on raw/ escaping run directory",
			setup: func(t *testing.T, repo string, _ *Engine, prep *PreparedRun) FinalizeOptions {
				rawDir := filepath.Join(prep.RunDir, "raw")
				if err := os.RemoveAll(rawDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outsideRoot, rawDir); err != nil {
					t.Fatal(err)
				}
				return FinalizeOptions{Repo: repo, Run: prep.RunID}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			eng := newPrepareEngine(t, repo)
			prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			fopt := tc.setup(t, repo, eng, prep)
			if _, err := eng.Finalize(context.Background(), fopt); err == nil {
				t.Fatalf("expected Finalize to reject %s, got nil error", tc.name)
			}
			if exists(outsideMissing) {
				t.Fatalf("Finalize wrote through dangling symlink to %s", outsideMissing)
			}
		})
	}
}

func TestFinalize_EmptyErrorFileAndStaleArtifactCleanupOnReFinalize(t *testing.T) {
	t.Run("whitespace error file with valid output passes while empty error file without output fails", func(t *testing.T) {
		repo := testRepo(t)
		eng := newPrepareEngine(t, repo)
		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}

		// security-reviewer has whitespace-only .err AND valid .json -> succeeds.
		// red-team has 0-byte .err AND missing .json -> fails with empty error file message!
		for _, task := range prep.Tasks {
			errPath := filepath.Join(prep.RunDir, filepath.FromSlash(task.ErrorFile))
			outPath := filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile))
			if task.Agent == "security-reviewer" {
				writeFile(t, errPath, "   \n\t  ")
				writeFile(t, outPath, clean)
			} else {
				writeFile(t, errPath, "")
			}
		}

		res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlock {
			t.Fatalf("Outcome = %q, want BLOCK", res.Outcome)
		}
		if state := agentState(t, prep.RunDir, "security-reviewer"); state != stateCompleted {
			t.Errorf("security-reviewer state = %q, want completed", state)
		}
		if state := agentState(t, prep.RunDir, "red-team"); state != stateFailed {
			t.Errorf("red-team state = %q, want failed", state)
		}
		errTxt, err := os.ReadFile(filepath.Join(prep.RunDir, "red-team.error.txt"))
		if err != nil || !strings.Contains(string(errTxt), "empty error file") {
			t.Errorf("red-team.error.txt = %q, err = %v", errTxt, err)
		}
	})

	t.Run("re-finalizing transitions cleanly between PASS and BLOCK without stale reports or error files", func(t *testing.T) {
		repo := testRepo(t)
		eng := newPrepareEngine(t, repo)
		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}

		// 1. Both pass -> <agent>.json exists, <agent>.error.txt absent.
		writeTaskOutputs(t, prep, map[string]string{
			"security-reviewer": clean,
			"red-team":          clean,
		})
		res1, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
		if err != nil || res1.Outcome != OutcomePass {
			t.Fatalf("res1 = %+v, err = %v", res1, err)
		}

		// 2. Inject failure on red-team and re-finalize -> stale red-team.json MUST be removed!
		var redTask PromptTask
		for _, task := range prep.Tasks {
			if task.Agent == "red-team" {
				redTask = task
			}
		}
		writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(redTask.ErrorFile)), "late failure\n")

		res2, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
		if err != nil || res2.Outcome != OutcomeBlock {
			t.Fatalf("res2 = %+v, err = %v", res2, err)
		}
		if exists(filepath.Join(prep.RunDir, "red-team.json")) {
			t.Errorf("stale red-team.json was not removed on failed re-finalize")
		}
		if !exists(filepath.Join(prep.RunDir, "red-team.error.txt")) {
			t.Errorf("red-team.error.txt was not written")
		}

		// 3. Remove error file and re-finalize -> stale red-team.error.txt MUST be removed and outcome returns to PASS!
		if err := os.Remove(filepath.Join(prep.RunDir, filepath.FromSlash(redTask.ErrorFile))); err != nil {
			t.Fatal(err)
		}
		res3, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
		if err != nil || res3.Outcome != OutcomePass {
			t.Fatalf("res3 = %+v, err = %v", res3, err)
		}
		if exists(filepath.Join(prep.RunDir, "red-team.error.txt")) {
			t.Errorf("stale red-team.error.txt was not removed on passing re-finalize")
		}
		if !exists(filepath.Join(prep.RunDir, "red-team.json")) {
			t.Errorf("red-team.json was not restored")
		}
	})
}

func TestFinalizeRejectsDotReviewSymlinkEscape(t *testing.T) {
	otherRepo := testRepo(t)
	otherEng := newPrepareEngine(t, otherRepo)
	otherPrep, err := otherEng.Prepare(context.Background(), Options{Repo: otherRepo, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeTaskOutputs(t, otherPrep, map[string]string{
		"security-reviewer": clean,
		"red-team":          clean,
	})

	repo := testRepo(t)
	eng := newPrepareEngine(t, repo)
	// Replace <repo>/.review with a symlink pointing to <otherRepo>/.review.
	reviewLink := filepath.Join(repo, ".review")
	_ = os.RemoveAll(reviewLink)
	if err := os.Symlink(filepath.Join(otherRepo, ".review"), reviewLink); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		opt  FinalizeOptions
	}{
		{"Case 1: empty Run via current_run.json", FinalizeOptions{Repo: repo}},
		{"Case 2: bare RunID", FinalizeOptions{Repo: repo, Run: otherPrep.RunID}},
		{"Case 3a: Run dir path with Repo", FinalizeOptions{Repo: repo, Run: filepath.Join(repo, ".review", otherPrep.RunID)}},
		{"Case 3b: Run dir path without Repo", FinalizeOptions{Run: filepath.Join(repo, ".review", otherPrep.RunID)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := eng.Finalize(context.Background(), tc.opt); err == nil {
				t.Fatalf("expected Finalize to reject symlinked .review directory in %s", tc.name)
			}
		})
	}
}

func TestPrepareAndRunRejectUnsafeAgentName(t *testing.T) {
	for _, bad := range []string{"../evil", "sub/agent", ".hidden"} {
		t.Run(bad, func(t *testing.T) {
			repo := testRepo(t)
			writeFile(t, filepath.Join(repo, ".claude/review/gates.yaml"),
				"gates:\n  - agent: \""+bad+"\"\n    paths: [\"**\"]\n    mode: gate\n")
			eng := newPrepareEngine(t, repo)
			if _, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"}); err == nil {
				t.Errorf("Prepare accepted unsafe agent name %q", bad)
			}
			te := newTestEngine(t, repo, nil)
			if _, err := te.Run(context.Background(), Options{Repo: repo, Base: "main"}); err == nil {
				t.Errorf("Run accepted unsafe agent name %q", bad)
			}
		})
	}
}

func TestFinalize_RejectsTamperedManifestTasksAndModes(t *testing.T) {
	hardErrorCases := []struct {
		name   string
		mutate func(m *RunManifest)
	}{
		{
			name: "invalid agent mode in manifest",
			mutate: func(m *RunManifest) {
				m.Agents[0].Mode = "root"
			},
		},
		{
			name: "missing planned agent from manifest",
			mutate: func(m *RunManifest) {
				m.Agents = m.Agents[1:]
			},
		},
		{
			name: "task part less than 1",
			mutate: func(m *RunManifest) {
				m.Tasks[0].Part = 0
			},
		},
		{
			name: "task part exceeds total_parts",
			mutate: func(m *RunManifest) {
				m.Tasks[0].Part = 3
				m.Tasks[0].TotalParts = 2
			},
		},
		{
			name: "duplicate task part for same agent",
			mutate: func(m *RunManifest) {
				dup := m.Tasks[0]
				dup.TaskID = dup.Agent + "-dup"
				m.Tasks = append(m.Tasks, dup)
			},
		},
	}
	for _, tc := range hardErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			eng := newPrepareEngine(t, repo)
			prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
			if err != nil {
				t.Fatal(err)
			}
			mutateManifest(t, prep.RunDir, tc.mutate)
			if _, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID}); err == nil {
				t.Fatalf("expected Finalize error for %s, got nil", tc.name)
			}
		})
	}

	failClosedAgentCases := []struct {
		name  string
		setup func(t *testing.T, prep *PreparedRun)
	}{
		{
			name: "pending agent with Parts < 1 fails closed",
			setup: func(t *testing.T, prep *PreparedRun) {
				writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean, "red-team": clean})
				mutateManifest(t, prep.RunDir, func(m *RunManifest) {
					m.Agents[0].Parts = 0
				})
			},
		},
		{
			name: "pending agent with missing part in tasks fails closed",
			setup: func(t *testing.T, prep *PreparedRun) {
				writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean, "red-team": clean})
				mutateManifest(t, prep.RunDir, func(m *RunManifest) {
					m.Agents[0].Parts = 2
				})
			},
		},
		{
			name: "unknown manifest agent status fails closed",
			setup: func(t *testing.T, prep *PreparedRun) {
				writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean, "red-team": clean})
				mutateManifest(t, prep.RunDir, func(m *RunManifest) {
					m.Agents[0].Status = "teleported"
				})
			},
		},
		{
			name: "carried agent with corrupted report JSON fails closed",
			setup: func(t *testing.T, prep *PreparedRun) {
				writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean, "red-team": clean})
				writeFile(t, filepath.Join(prep.RunDir, "security-reviewer.json"), "{corrupt")
				mutateManifest(t, prep.RunDir, func(m *RunManifest) {
					m.Agents[0].Status = "carried"
				})
			},
		},
		{
			name: "carried agent with missing report file fails closed",
			setup: func(t *testing.T, prep *PreparedRun) {
				writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean, "red-team": clean})
				_ = os.Remove(filepath.Join(prep.RunDir, "security-reviewer.json"))
				mutateManifest(t, prep.RunDir, func(m *RunManifest) {
					m.Agents[0].Status = "carried"
				})
			},
		},
	}
	for _, tc := range failClosedAgentCases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testRepo(t)
			eng := newPrepareEngine(t, repo)
			prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, prep)
			res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
			if err != nil {
				t.Fatalf("Finalize returned error: %v", err)
			}
			if res.Outcome != OutcomeBlock {
				t.Fatalf("%s: Outcome = %q, want BLOCK", tc.name, res.Outcome)
			}
			if exists(filepath.Join(prep.RunDir, "security-reviewer.json")) {
				t.Errorf("%s: security-reviewer.json should have been removed", tc.name)
			}
		})
	}
}

func TestFinalize_ValidatesNonPendingReportsAndRedactsSecretsInErrorFiles(t *testing.T) {
	t.Run("tampered empty or carried report on disk is rejected", func(t *testing.T) {
		for _, status := range []string{"empty", "skipped", "carried"} {
			t.Run(status, func(t *testing.T) {
				repo := testRepo(t)
				eng := newPrepareEngine(t, repo)
				prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
				if err != nil {
					t.Fatal(err)
				}
				writeTaskOutputs(t, prep, map[string]string{"security-reviewer": clean, "red-team": clean})
				// Write a report claiming to be from another agent / wrong revision.
				tampered := `{
					"schema": "aspect-review/v1",
					"agent": "wrong-agent",
					"mode": "gate",
					"base": "000000000000",
					"head": "111111111111",
					"summary": "forged",
					"findings": [],
					"handoffs": [],
					"changes": []
				}`
				writeFile(t, filepath.Join(prep.RunDir, "security-reviewer.json"), tampered)
				mutateManifest(t, prep.RunDir, func(m *RunManifest) {
					m.Agents[0].Status = status
				})

				res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
				if err != nil {
					t.Fatal(err)
				}
				if res.Outcome != OutcomeBlock {
					t.Fatalf("tampered %s report must fail closed with BLOCK, got %q", status, res.Outcome)
				}
				if exists(filepath.Join(prep.RunDir, "security-reviewer.json")) {
					t.Errorf("tampered %s report was not deleted", status)
				}
			})
		}
	})

	t.Run("redacts credentials and caps oversized .err content", func(t *testing.T) {
		repo := testRepo(t)
		eng := newPrepareEngine(t, repo)
		prep, err := eng.Prepare(context.Background(), Options{Repo: repo, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		rawErr := "HTTP 401 Authorization: Bearer ya29.SuperSecretToken123 " +
			"x-goog-api-key: MySecretGoogKey " +
			"https://example.com?key=QuerySecret999&foo=1 " +
			"raw=AIzaSyDa1234567890abcdefghijklmnop " +
			strings.Repeat("padding-", 600)
		for _, task := range prep.Tasks {
			if task.Agent == "security-reviewer" {
				writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.ErrorFile)), rawErr)
			} else {
				writeFile(t, filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile)), clean)
			}
		}

		res, err := eng.Finalize(context.Background(), FinalizeOptions{Repo: repo, Run: prep.RunID})
		if err != nil || res.Outcome != OutcomeBlock {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		savedErr, err := os.ReadFile(filepath.Join(prep.RunDir, "security-reviewer.error.txt"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(savedErr)
		for _, leaked := range []string{"SuperSecretToken123", "MySecretGoogKey", "QuerySecret999", "SyDa1234567890abcdefghijklmnop"} {
			if strings.Contains(text, leaked) {
				t.Errorf("unredacted secret %q leaked into error.txt:\n%s", leaked, text)
			}
		}
		if !strings.Contains(text, "[REDACTED]") {
			t.Errorf("expected [REDACTED] marker in error.txt:\n%s", text)
		}
		if len(text) > 2200 {
			t.Errorf("error.txt was not capped (~2KB), got %d bytes", len(text))
		}
	})
}
