package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/eideroliveira/aspect/internal/gate"
	"github.com/eideroliveira/aspect/internal/gitx"
	"github.com/eideroliveira/aspect/internal/review"
)

// runGate dispatches `aspect gate plan|check|render`.
func runGate(sub string, args []string) error {
	switch sub {
	case "plan":
		return runGatePlan(args)
	case "check":
		return runGateCheck(args, false)
	case "render":
		return runGateCheck(args, true)
	case "extract":
		return runGateExtract(args)
	}
	return fmt.Errorf("unknown gate subcommand %q (want plan, check, render or extract)", sub)
}

// runGateExtract saves the report an agent printed as the last fenced json
// block of its final message. Input is the --output-format json result of
// Claude Code or Gemini CLI, or plain text. Given the run's agent, mode,
// base and head, it saves the report through review.NormalizeJSON instead:
// what cannot change the verdict is fixed (a category the model invented,
// such as error-handling, maps to the nearest one rather than making the
// report invalid and blocking a gate that found nothing), and anything
// that could, such as an unknown severity, refuses the report. A refused
// report is kept beside -o as <out>.rejected for whoever runs the command,
// never printed, since CI logs of a public pull request are public and a
// security agent's report names the weakness.
func runGateExtract(args []string) error {
	fs := flag.NewFlagSet("gate extract", flag.ContinueOnError)
	out := fs.String("o", "", "where to write the report (required)")
	agent := fs.String("agent", "", "the agent that wrote the report; with -mode, -base and -head, normalize it")
	mode := fs.String("mode", "", "the agent's mode (gate, advisory, author)")
	base := fs.String("base", "", "the run's base commit")
	head := fs.String("head", "", "the run's head commit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("gate extract: -o is required")
	}
	flags := []string{*agent, *mode, *base, *head}
	normalize := slices.ContainsFunc(flags, func(s string) bool { return s != "" })
	if normalize && slices.Contains(flags, "") {
		return fmt.Errorf("gate extract: -agent, -mode, -base and -head go together")
	}
	if normalize && !slices.Contains(gate.Modes, *mode) {
		return fmt.Errorf("gate extract: -mode must be one of %v, got %q", gate.Modes, *mode)
	}
	in := os.Stdin
	if fs.NArg() > 0 {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	raw, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	text := gate.AgentText(raw)
	b, err := gate.LastJSONBlock(text)
	if err != nil {
		return err
	}
	if normalize {
		normalized, err := review.NormalizeJSON(b, review.AgentRun{Agent: *agent, Mode: *mode, Base: *base, Head: *head})
		if err != nil {
			return refuseReport(*agent, *out, b, err)
		}
		b = normalized
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(*out, b, 0o644)
}

// refuseReport keeps a report NormalizeJSON refused, and why, in
// <out>.rejected, and returns an error that names neither: the reason can
// quote the model's text (a finding's title), and this error is printed to a
// CI log that is public on a public pull request. An error that is the
// run's, not the report's, is returned as it is.
func refuseReport(agent, out string, report []byte, cause error) error {
	if !errors.Is(cause, review.ErrInvalidReport) {
		return fmt.Errorf("gate extract: %w", cause)
	}
	rejected := out + ".rejected"
	body := append([]byte("refused: "+cause.Error()+"\n\n"), report...)
	err := os.MkdirAll(filepath.Dir(out), 0o755)
	if err == nil {
		err = os.WriteFile(rejected, body, 0o644)
	}
	if err != nil {
		return fmt.Errorf("gate extract: %s's report was refused and not written to %s; keeping it in %s failed too: %v", agent, out, rejected, err)
	}
	return fmt.Errorf("gate extract: %s's report was refused and not written to %s; the report and the reason are in %s", agent, out, rejected)
}

func runGatePlan(args []string) error {
	fs := flag.NewFlagSet("gate plan", flag.ContinueOnError)
	config := fs.String("config", gate.DefaultConfigPath, "gates.yaml")
	base := fs.String("base", "origin/main", "base ref; the diff starts at its merge base with head")
	head := fs.String("head", "HEAD", "head ref")
	out := fs.String("o", "", "also write the plan to this file (e.g. .review/<run-id>/plan.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := gate.LoadConfig(*config)
	if err != nil {
		return err
	}
	p, err := gitx.Plan(context.Background(), "", c, *base, *head)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
	}
	_, err = os.Stdout.Write(b)
	return err
}

// runGateCheck validates the reports in a run directory against its
// plan.json. check prints a summary and fails on block; render prints the
// Markdown comment and always succeeds once the inputs load.
func runGateCheck(args []string, render bool) error {
	name := "gate check"
	if render {
		name = "gate render"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	config := fs.String("config", gate.DefaultConfigPath, "gates.yaml")
	override := fs.Bool("override", false, "a maintainer overrode the gate (the review-gates/override label)")
	public := fs.Bool("public", false, "redact security findings at public_redaction severities")
	asJSON := fs.Bool("json", false, "check: print the result as JSON")
	dir, err := parseWithPositional(fs, args)
	if err != nil {
		return err
	}
	c, err := gate.LoadConfig(*config)
	if err != nil {
		return err
	}
	pb, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		return fmt.Errorf("%w (write it with aspect gate plan -o %s/plan.json)", err, dir)
	}
	var p gate.Plan
	if err := json.Unmarshal(pb, &p); err != nil {
		return fmt.Errorf("plan.json: %w", err)
	}
	if p.Schema != gate.PlanSchema {
		return fmt.Errorf("plan.json: schema %q, want %q", p.Schema, gate.PlanSchema)
	}
	reports := map[string]gate.Input{}
	for _, a := range p.Agents {
		path := filepath.Join(dir, a.Agent+".json")
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		reports[a.Agent] = gate.Input{Data: b, Err: err}
	}
	res := gate.Check(c, &p, reports, *override)
	if render {
		fmt.Print(gate.Render(c, &p, res, gate.RenderOptions{Public: *public}))
		return nil
	}
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, a := range res.Agents {
			line := fmt.Sprintf("%-22s %-8s %-8s blocking=%d warnings=%d", a.Agent, a.Mode, a.Status, len(a.Blocking), len(a.Warnings))
			if a.Error != "" {
				line += "  " + a.Error
			}
			fmt.Println(line)
		}
		fmt.Println("outcome:", res.Outcome)
	}
	if res.Failed() {
		return fmt.Errorf("review gates blocked")
	}
	return nil
}

// parseWithPositional parses flags given before or after the single
// positional argument, so `check DIR -override` and `check -override DIR`
// both work.
func parseWithPositional(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		return "", fmt.Errorf("%s: missing run directory", fs.Name())
	}
	pos := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("%s: unexpected arguments %v", fs.Name(), fs.Args())
	}
	return pos, nil
}
