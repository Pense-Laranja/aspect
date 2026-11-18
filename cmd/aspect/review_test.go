package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Pense-Laranja/aspect/internal/review"
)

func TestParseReadArgs(t *testing.T) {
	cases := []struct {
		sub      string
		args     []string
		runs     []string
		severity string
		err      bool
	}{
		{"findings", []string{"run-1", "-severity", "high"}, []string{"run-1"}, "high", false},
		{"findings", []string{"-severity", "high", "run-1"}, []string{"run-1"}, "high", false},
		{"findings", nil, nil, "", false},
		{"findings", []string{"a", "b"}, nil, "", true},
		{"findings", []string{"-severity", "hgih"}, nil, "", true},
		{"status", []string{"run-1", "-repo", "/r"}, []string{"run-1"}, "", false},
		{"compare", []string{"a", "b"}, []string{"a", "b"}, "", false},
		{"compare", []string{"only-one"}, nil, "", true},
		{"runs", []string{"-limit", "3"}, nil, "", false},
		{"runs", []string{"stray"}, nil, "", true},
	}
	for _, c := range cases {
		a, err := parseReadArgs(c.sub, c.args, ".")
		if (err != nil) != c.err {
			t.Errorf("%s %v: err = %v, want error %v", c.sub, c.args, err, c.err)
			continue
		}
		if c.err {
			continue
		}
		if !slices.Equal(a.runs, c.runs) || a.severity != c.severity {
			t.Errorf("%s %v: runs %v severity %q", c.sub, c.args, a.runs, a.severity)
		}
	}
	if a, _ := parseReadArgs("status", []string{"-repo", "/r"}, "."); a.repo != "/r" {
		t.Errorf("repo = %q", a.repo)
	}
	if a, _ := parseReadArgs("runs", []string{"-limit", "3"}, "."); a.limit != 3 {
		t.Errorf("limit = %d", a.limit)
	}
}

func TestOutcomeLineBannerIntegration(t *testing.T) {
	cases := []struct {
		outcome string
		usage   review.Usage
		reused  bool
		want    string
	}{
		{
			outcome: "PASS",
			usage:   review.Usage{CostUSD: 0.0595, PromptTokens: 4500, CompletionTokens: 1200, CachedTokens: 2100, TotalTokens: 7800},
			reused:  false,
			want:    "Aspect gate outcome: PASS — Cost: $0.0595 (4.5k prompt, 1.2k completion, 2.1k cached [75% discount])",
		},
		{
			outcome: "PASS",
			usage:   review.Usage{CostUSD: 0.0595, PromptTokens: 4500, CompletionTokens: 1200, CachedTokens: 2100, TotalTokens: 7800},
			reused:  true,
			want:    "Aspect gate outcome: PASS — Cost: $0.0000 (reused)",
		},
		{
			outcome: "PASS",
			usage:   review.Usage{Engine: review.DefaultAntigravityEngine},
			reused:  false,
			want:    "Aspect gate outcome: PASS — Engine: antigravity (flash)",
		},
	}

	for _, tc := range cases {
		got := review.FormatOutcomeLine(tc.outcome, tc.usage, tc.reused)
		if got != tc.want {
			t.Errorf("FormatOutcomeLine() = %q, want %q", got, tc.want)
		}
	}
}

func setupReviewCLIRepo(t *testing.T, gatesYAML string) string {
	t.Helper()
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("ASPECT_BIN", "")
	t.Setenv("ASPECT_HOME", "")
	t.Setenv("ASPECT_ALLOW_REPO_EXEC", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	// Shadow `gh` so CLI tests never attempt external GitHub calls.
	binDir := t.TempDir()
	ghStub := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghStub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit("init", "-q", "-b", "main")
	runGit("remote", "add", "origin", "https://github.com/example/repo.git")
	if gatesYAML == "" {
		gatesYAML = "gates:\n  - agent: security-reviewer\n    paths: [\"**\"]\n    mode: gate\n  - agent: red-team\n    paths: [\"**\"]\n    mode: gate\n"
	}
	write(".claude/review/gates.yaml", gatesYAML)
	for _, a := range []string{"security-reviewer", "red-team", "architect"} {
		write(".claude/agents/"+a+".md", "---\nname: "+a+"\n---\n"+a+"\n")
	}
	write(".gitignore", ".review/\n")
	write("a.go", "package a\n")
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "base")
	runGit("checkout", "-q", "-b", "feature")
	write("a.go", "package a\n\nfunc A() int { return 1 }\n")
	runGit("commit", "-q", "-am", "feature")
	return repo
}

func TestParseFinalizeArgs_OrderIndependentAndBoundsChecked(t *testing.T) {
	cases := []struct {
		args    []string
		wantRun string
		wantRep string
		wantOut string
		wantPub bool
		wantErr bool
	}{
		{[]string{"run-1", "-repo", "/r", "-publish", "-o", "out.md"}, "run-1", "/r", "out.md", true, false},
		{[]string{"-repo", "/r", "run-1"}, "run-1", "/r", "", false, false},
		{[]string{"-repo", "/r"}, "", "/r", "", false, false},
		{nil, "", "", "", false, false},
		{[]string{"run-1", "run-2"}, "", "", "", false, true},
		{[]string{"-unknown-flag"}, "", "", "", false, true},
	}
	for _, tc := range cases {
		opt, out, err := parseFinalizeArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseFinalizeArgs(%v) err = %v, wantErr %v", tc.args, err, tc.wantErr)
			continue
		}
		if tc.wantErr {
			continue
		}
		if opt.Run != tc.wantRun || opt.Repo != tc.wantRep || out != tc.wantOut || opt.Publish != tc.wantPub {
			t.Errorf("parseFinalizeArgs(%v) = (%+v, %q)", tc.args, opt, out)
		}
	}
}

func TestReviewPrepareAndFinalizeCLI_ExitCodesJSONAndFlagsWithoutAPIKey(t *testing.T) {
	t.Run("prepare -json, finalize PASS (exit 0) with -o, and reused human prepare", func(t *testing.T) {
		repo := setupReviewCLIRepo(t, "")

		// 1. `aspect review` without subcommand still requires GEMINI_API_KEY.
		if err := runReview([]string{"-repo", repo, "-base", "main", "-agent", "red-team"}); err == nil ||
			!strings.Contains(err.Error(), "no Gemini API key") {
			t.Fatalf("expected missing Gemini API key error, got %v", err)
		}
		// Reject unexpected positional argument on prepare.
		if err := runReview([]string{"prepare", "extra-arg"}); err == nil {
			t.Fatalf("expected error on extra positional argument to review prepare")
		}

		// 2. `aspect review prepare -json` succeeds without GEMINI_API_KEY.
		prepOut, err := captureStdout(t, func() error {
			return runReview([]string{"prepare", "-repo", repo, "-base", "main", "-json"})
		})
		if err != nil {
			t.Fatalf("review prepare -json error = %v", err)
		}
		var prep review.PreparedRun
		if err := json.Unmarshal([]byte(prepOut), &prep); err != nil {
			t.Fatalf("unmarshaling prepare -json output: %v\n%s", err, prepOut)
		}
		if prep.Status != "pending" || len(prep.Tasks) != 2 {
			t.Fatalf("prep = %+v", prep)
		}
		for _, task := range prep.Tasks {
			p := filepath.Join(prep.RunDir, filepath.FromSlash(task.OutputFile))
			if err := os.WriteFile(p, []byte(`{"summary":"ok","findings":[],"handoffs":[]}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		// 3. `aspect review finalize <run-dir> -o <md>` exits 0 and omits fabricated $0.0000.
		mdPath := filepath.Join(t.TempDir(), "gate.md")
		finOut, err := captureStdout(t, func() error {
			return runReview([]string{"finalize", prep.RunDir, "-o", mdPath})
		})
		if err != nil {
			t.Fatalf("review finalize PASS error = %v", err)
		}
		if !strings.Contains(finOut, "Aspect gate outcome: PASS — Engine: antigravity (flash)") {
			t.Errorf("finalize stdout missing Antigravity outcome banner:\n%s", finOut)
		}
		if strings.Contains(finOut, "$0.0000") {
			t.Errorf("finalize stdout fabricated $0.0000 on fresh Antigravity run:\n%s", finOut)
		}
		mdBytes, err := os.ReadFile(mdPath)
		if err != nil || !strings.Contains(string(mdBytes), "## Review gates: pass") {
			t.Errorf("-o markdown file = %q, err = %v", mdBytes, err)
		}

		// 4. Subsequent human-mode `prepare` reuses the finished run.
		reusedOut, err := captureStdout(t, func() error {
			return runReview([]string{"prepare", "-repo", repo, "-base", "main"})
		})
		if err != nil {
			t.Fatalf("reused prepare error = %v", err)
		}
		if !strings.Contains(reusedOut, "Diff unchanged since run "+prep.RunID) {
			t.Errorf("reused prepare stdout missing reuse notice:\n%s", reusedOut)
		}

		// 5. Human-mode `prepare -agent red-team -engine custom-flash` prints human summary and creates isolated agent-* run.
		agentOut, err := captureStdout(t, func() error {
			return runReview([]string{"prepare", "-repo", repo, "-base", "main", "-agent", "red-team", "-engine", "custom-flash"})
		})
		if err != nil {
			t.Fatalf("prepare -agent error = %v", err)
		}
		if !strings.Contains(agentOut, "Prepared run agent-") {
			t.Errorf("prepare -agent stdout = %q", agentOut)
		}

		// 6. Invalid flag on prepare and missing run on finalize return errors.
		if err := runReview([]string{"prepare", "-unknown-flag"}); err == nil {
			t.Errorf("expected error for unknown flag on review prepare")
		}
		if err := runReview([]string{"finalize", "-repo", repo, "local-does-not-exist"}); err == nil {
			t.Errorf("expected error for nonexistent run on review finalize")
		}
	})

	t.Run("finalize returns exitCode 1 on BLOCK, reused BLOCK prepare returns exitCode 1, and ERROR returns exitCode 2", func(t *testing.T) {
		// BLOCK case: blocking finding -> finalize exits 1, and subsequent human `prepare` reusing BLOCK also exits 1!
		repoBlock := setupReviewCLIRepo(t, "")
		prepBlockOut, err := captureStdout(t, func() error {
			return runReview([]string{"prepare", "-repo", repoBlock, "-base", "main", "-json"})
		})
		if err != nil {
			t.Fatal(err)
		}
		var prepBlock review.PreparedRun
		if err := json.Unmarshal([]byte(prepBlockOut), &prepBlock); err != nil {
			t.Fatal(err)
		}
		for _, task := range prepBlock.Tasks {
			p := filepath.Join(prepBlock.RunDir, filepath.FromSlash(task.OutputFile))
			content := `{"summary":"ok","findings":[],"handoffs":[]}`
			if task.Agent == "security-reviewer" {
				content = `{"summary":"blocker","findings":[{"id":"s1","severity":"high","category":"security","title":"Blocker","location":{"file":"a.go","line":3},"evidence":"e","recommendation":"r","confidence":0.95}],"handoffs":[]}`
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, blockErr := captureStdout(t, func() error {
			return runReview([]string{"finalize", "-repo", repoBlock})
		})
		var ec exitCode
		if !errors.As(blockErr, &ec) || ec.code != 1 {
			t.Fatalf("BLOCK finalize err = %v, want exitCode{code: 1}", blockErr)
		}
		_, reusedBlockErr := captureStdout(t, func() error {
			return runReview([]string{"prepare", "-repo", repoBlock, "-base", "main"})
		})
		if !errors.As(reusedBlockErr, &ec) || ec.code != 1 {
			t.Fatalf("reused BLOCK prepare err = %v, want exitCode{code: 1}", reusedBlockErr)
		}

		// ERROR case: advisory-only plan where all agents fail -> exit code 2.
		repoErr := setupReviewCLIRepo(t, "gates:\n  - agent: architect\n    paths: [\"**\"]\n    mode: advisory\n")
		prepOut, err := captureStdout(t, func() error {
			return runReview([]string{"prepare", "-repo", repoErr, "-base", "main", "-json"})
		})
		if err != nil {
			t.Fatal(err)
		}
		var prep review.PreparedRun
		if err := json.Unmarshal([]byte(prepOut), &prep); err != nil {
			t.Fatal(err)
		}
		errFile := filepath.Join(prep.RunDir, filepath.FromSlash(prep.Tasks[0].ErrorFile))
		if err := os.WriteFile(errFile, []byte("advisory crashed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, errOutcome := captureStdout(t, func() error {
			return runReview([]string{"finalize", "-repo", repoErr, prep.RunID})
		})
		if !errors.As(errOutcome, &ec) || ec.code != 2 {
			t.Fatalf("ERROR finalize err = %v, want exitCode{code: 2}", errOutcome)
		}
	})
}
