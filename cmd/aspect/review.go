package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/eideroliveira/aspect/internal/review"
)

// exitCode is an error that ends the command with a given exit status.
type exitCode struct {
	code int
	msg  string
}

func (e exitCode) Error() string { return e.msg }

// runReview runs the review gates' agents on Gemini (internal/review) and
// exits 0 on pass or warn, 1 when the gates block, and 2 when there is no
// verdict (a gate agent has no report), which must not read as a pass.
func runReview(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status", "findings", "compare", "runs":
			return runReviewRead(args[0], args[1:])
		}
	}
	cfg := review.ConfigFromEnv()
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	repo := fs.String("repo", cfg.DefaultRepo, "repository to review")
	base := fs.String("base", "origin/main", "base ref; the diff starts at its merge base with head")
	head := fs.String("head", "HEAD", "head ref")
	pr := fs.String("pr", "", "pull request number (default: the branch's open pull request)")
	publish := fs.Bool("publish", false, "publish the result with .claude/review/publish.sh")
	force := fs.Bool("force", false, "review again even if a run of the same diff exists")
	out := fs.String("o", "", "also write the Markdown result to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("review: unexpected arguments %v", fs.Args())
	}
	if cfg.GeminiAPIKey == "" {
		return fmt.Errorf("review: no Gemini API key configured (set GEMINI_API_KEY or GOOGLE_API_KEY)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	eng := &review.Engine{Config: cfg, Client: cfg.Gemini()}
	fmt.Fprintf(os.Stderr, "aspect review: %s (%s...%s) on Gemini %s\n", *repo, *base, *head, cfg.GeminiModel)
	res, err := eng.Run(ctx, review.Options{Repo: *repo, Base: *base, Head: *head, PR: *pr, Publish: *publish, Force: *force})
	if err != nil {
		return err
	}
	if res.Cached {
		fmt.Printf("Diff unchanged since run %s: returning its results.\n", res.RunID)
	}
	fmt.Printf("Aspect gate outcome: %s\nRun ID: %s\n\n%s\n", res.Outcome, res.RunID, res.Rendered)
	if res.Published != "" {
		fmt.Printf("Publish status: %s\n", res.Published)
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.Rendered), 0o644); err != nil {
			return err
		}
	}
	switch res.Outcome {
	case review.OutcomePass, review.OutcomeWarn, review.OutcomeOverridden:
		return nil
	case review.OutcomeBlock:
		return exitCode{1, "review gates blocked"}
	}
	return exitCode{2, fmt.Sprintf("review gates gave no verdict (%s): a gate agent has no report", res.Outcome)}
}

// runReviewRead reads earlier runs: status (a run's agents, live while it
// runs), findings, compare (two runs side by side) and runs (the list).
func runReviewRead(sub string, args []string) error {
	cfg := review.ConfigFromEnv()
	fs := flag.NewFlagSet("review "+sub, flag.ContinueOnError)
	repo := fs.String("repo", cfg.DefaultRepo, "repository whose .review/ runs to read")
	severity := fs.String("severity", "", "findings: only this severity")
	agent := fs.String("agent", "", "findings: only this agent's")
	limit := fs.Int("limit", 20, "runs: how many, newest first")
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	switch sub {
	case "status", "findings":
		if len(pos) > 1 {
			return fmt.Errorf("review %s: one run id at most, got %v", sub, pos)
		}
		run := ""
		if len(pos) == 1 {
			run = pos[0]
		}
		r, err := review.LoadRun(*repo, run)
		if err != nil {
			return err
		}
		if sub == "status" {
			review.PrintStatus(os.Stdout, r)
			return nil
		}
		if review.PrintFindings(os.Stdout, r, *severity, *agent) == 0 {
			fmt.Printf("No findings in run %s match.\n", r.RunID)
		}
		return nil
	case "compare":
		if len(pos) != 2 {
			return fmt.Errorf("review compare: want two run ids, got %v", pos)
		}
		a, err := review.LoadRun(*repo, pos[0])
		if err != nil {
			return err
		}
		b, err := review.LoadRun(*repo, pos[1])
		if err != nil {
			return err
		}
		review.PrintCompare(os.Stdout, a, b)
		return nil
	}
	runs, err := review.LoadRuns(*repo, *limit)
	if err != nil {
		return err
	}
	review.PrintRuns(os.Stdout, runs)
	return nil
}
