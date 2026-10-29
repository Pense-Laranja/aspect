// Command aspect drives the Aspect pipeline: validate a specification, show
// the build plan, or run the agents and produce code, tests and a report.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Pense-Laranja/aspect/internal/analyze"
	"github.com/Pense-Laranja/aspect/internal/drift"
	"github.com/Pense-Laranja/aspect/internal/gate"
	"github.com/Pense-Laranja/aspect/internal/importer"
	"github.com/Pense-Laranja/aspect/internal/llm"
	"github.com/Pense-Laranja/aspect/internal/pipeline"
	"github.com/Pense-Laranja/aspect/internal/plan"
	"github.com/Pense-Laranja/aspect/internal/spec"
)

const usage = `aspect - build software from a formal specification with a team of agents

Usage:
  aspect validate <spec.yaml>          check the spec and report every issue
  aspect expand   <spec.yaml>          print the spec with every include resolved
  aspect split    <spec.yaml> [-n]     break a single-file spec into one file per module and interface
  aspect plan     <spec.yaml>          show the module build order and what each step needs
  aspect run      <spec.yaml> [flags]  generate code and tests, run them, validate goals
  aspect drift    <spec.yaml> [flags]  re-validate existing code against the spec without regenerating
  aspect paths    <spec.yaml> [flags]  record each imported module's package directory (path:) in the spec
  aspect briefs   <spec.yaml> [-w]     read each module's brief from its package's ASPECT.md in place
                                       instead of an import copy beside the spec
  aspect context  <spec.yaml> [flags] <path|module>...
                                       what the spec says about the modules owning these files: the
                                       system's constraints, each module's rules, operations,
                                       scenarios and brief (what aspect review shows its reviewers)
  aspect inventory <dir> [flags]       deterministic inventory of an existing Go codebase (no model calls)
  aspect import   <dir> [flags]        recover a spec from an existing Go codebase
  aspect check-invariants <dir> [flags] deterministic verification of code and architectural invariants
  aspect gate plan [flags]             which review agents apply to a change (JSON)
  aspect gate check <run-dir> [flags]  validate the agents' reports; exit 1 when the gates block
  aspect gate render <run-dir> [flags] the review result as a Markdown PR comment
  aspect gate extract -o FILE [flags] [input]  save the report an agent printed (claude -p output)
  aspect review [flags]                run the review agents on Gemini and gate the change;
                                       exit 1 when the gates block, 2 when there is no verdict
  aspect review status [run-id]        a run's agents and outcome (the current run; live while it runs)
  aspect review findings [run-id] [-severity S] [-agent A]   a run's findings, most severe first
  aspect review compare <run-a> <run-b>                     two runs side by side, agent by agent
  aspect review runs [-limit N]        the repository's runs, newest first

Split flags:
  -n                list the files split would write, and their sizes, without writing

Run flags:
  -out DIR          output directory (default ./out)
                    Go specs produce a Go module; Swift specs a SwiftPM package
  -model ID         model id (default $ASPECT_MODEL or claude-opus-5)
  -effort LEVEL     low|medium|high|xhigh|max (default high)
  -max-repairs N    coder repair rounds per module (default 3)
  -parallel N       independent modules generated at once (default 1)
  -fallbacks=false  disable server-side refusal fallbacks

Drift flags:
  -out DIR          where the code lives, as given to run -out (default ./out): out/<system>[/<tier>]
  -repo DIR         check a repository in place instead (the code the spec describes, e.g. -repo .):
                    a single-tier spec's code is DIR, a tier's DIR/<tier>; nothing there is written
  -report DIR       where drift.json and DRIFT.md go (default out/<system>, or
                    <user cache dir>/aspect/drift/<system> with -repo)
  -baseline FILE    a previous report.json or drift.json to compare goal verdicts against
                    (default out/<system>/report.json when it exists)
  -no-llm           presence, orphans and tests only; no verdicts
  -no-fail          exit 0 even when drift is found
  -model, -effort, -fallbacks as for run

Paths flags (for specs imported before modules recorded their directory):
  -repo DIR         the repository the spec was imported from (default .)
  -w                write path: into the spec files; without it, print what would be written

Context flags (before the paths):
  -repo DIR         the repository the paths are in (default: the spec's, the nearest .git above it)
  -budget N         print at most about N tokens, rules first (default 0: everything)
  -system=false     leave out the system's constraints
  -briefs=false     leave out the modules' briefs
  A directory stands for the files directly in it. Exit 1 when no argument names a module.

Inventory and import flags:
  -include a,b      only these package directories (prefix match)
  -exclude a,b      skip these package directories (external, vendor, testdata are always skipped)

Import flags:
  -o FILE           where to write the spec (default _aspect/aspect.yaml; briefs go beside it)
  -split            write one file per module, interface and entity owner (see aspect split)
  -language LANG    language of the new system, or of its frontend tier (default: source)
  -topology T       monolith | api_backend | cloud_service
                    monolith:      one tier connecting to its own database (default when
                                   the language does not change)
                    api_backend:   a backend tier serving an API (mirrors the source when
                                   it keeps the source language) plus a frontend tier in
                                   -language consuming it (default when the language changes)
                    cloud_service: one app tier in -language over hosted services; the
                                   database and server interfaces are external
  -backend-language L  language of the api_backend backend tier (default: source)
  -hint TEXT        guidance for the target ("an iOS app for members; admin stays on the web")
  -name NAME        system name (default: last element of the module path)
  -module-path P    target module path
  -cache DIR        fragment cache so re-runs skip described packages (default .aspect-cache/<name>)
  -concurrency N    packages described in parallel (default 4)
  -model, -effort, -fallbacks as for run

Gate flags:
  -config FILE      gates.yaml (default .claude/review/gates.yaml)
  plan:   -base REF (default origin/main; the diff starts at its merge base with head)
          -head REF (default HEAD)  -o FILE (also write the plan, e.g. <run-dir>/plan.json)
  check, render: -override (a maintainer overrode the gate)  -public (redact security details)
  check:  -json (print the result as JSON)
  extract: -agent A -mode M -base SHA -head SHA (the run's; fix what cannot change the verdict, such
          as an invented category, and refuse what could; a refused report is kept as FILE.rejected)
  A run directory holds plan.json and one <agent>.json report per planned agent.

Review flags:
  -repo DIR         repository to review (default $CODE_REVIEWER_DEFAULT_REPO or .)
  -base REF, -head REF as for gate plan
  -pr N             pull request number (default: the branch's open pull request)
  -publish          publish the result with the repository's .claude/review/publish.sh
  -force            review again even if a run of the same diff exists
  -o FILE           also write the Markdown result to FILE
  Each reviewer is shown the system's constraints and the rules of the modules its files
  belong to (see aspect context), within CODE_REVIEWER_SPEC_BUDGET tokens (30000; 0: none).
  The run is .review/<run-id>/: plan.json, the reports, status.json. Gemini credentials
  come from GEMINI_API_KEY or GOOGLE_API_KEY; GEMINI_MODEL and the CODE_REVIEWER_*
  variables tune it (see internal/review/config.go). ASPECT_HOME supplies the agents
  and gates.yaml a repository lacks.

MCP server (aspect mcp):
  Serves the review engine on stdin/stdout for an MCP client (the Claude desktop app, Claude
  Code, Antigravity): review_with_aspect_gates, run_single_aspect_agent, get_review_status,
  get_review_findings, compare_review_runs, list_review_runs, check_review_engine_status.
  Configured by the same environment as aspect review. It replaced the Python CodeReviewer's
  server, with the same tool names and arguments.

Credentials come from ANTHROPIC_API_KEY or an "ant auth login" profile.
`

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "review" {
		exit(runReview(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "mcp" {
		exit(runMCP(os.Args[2:]))
	}
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, path := os.Args[1], os.Args[2]
	var err error
	switch cmd {
	case "validate":
		err = runValidate(path)
	case "expand":
		var b []byte
		b, err = spec.Expand(path)
		if err == nil {
			os.Stdout.Write(b)
		}
	case "split":
		err = runSplit(path, os.Args[3:])
	case "plan":
		err = runPlan(path)
	case "run":
		err = runRun(path, os.Args[3:])
	case "drift":
		err = runDrift(path, os.Args[3:])
	case "paths":
		err = runPaths(path, os.Args[3:])
	case "briefs":
		err = runBriefs(path, os.Args[3:])
	case "context":
		err = runContext(path, os.Args[3:])
	case "inventory":
		err = runInventory(path, os.Args[3:])
	case "import":
		err = runImport(path, os.Args[3:])
	case "check-invariants":
		err = runCheckInvariants(path, os.Args[3:])
	case "gate":
		err = runGate(path, os.Args[3:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	exit(err)
}

// exit ends the command: status 0 on success, the status an exitCode
// carries, else 1.
func exit(err error) {
	if err == nil {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "aspect:", err)
	var ec exitCode
	if errors.As(err, &ec) {
		os.Exit(ec.code)
	}
	os.Exit(1)
}

func loadValidated(path string) (*spec.Spec, spec.Issues, error) {
	s, err := spec.Load(path)
	if err != nil {
		return nil, nil, err
	}
	issues := spec.Validate(s)
	for _, i := range issues {
		fmt.Fprintln(os.Stderr, i)
	}
	if issues.HasErrors() {
		return s, issues, fmt.Errorf("spec has errors")
	}
	return s, issues, nil
}

func runValidate(path string) error {
	s, issues, err := loadValidated(path)
	if err != nil {
		return err
	}
	var langs []string
	for _, t := range s.EffectiveTiers() {
		langs = append(langs, t.Language)
	}
	deps := ""
	if n := len(s.Deps); n > 0 {
		deps = fmt.Sprintf(", %d dependenc%s", n, map[bool]string{true: "y", false: "ies"}[n == 1])
	}
	fmt.Printf("ok: %s (%s, %s, %d modules, %d goals%s, %d warnings)\n", s.System.Name, s.EffectiveTopology(), strings.Join(langs, "+"), len(s.AllModules()), len(s.System.Goals), deps, len(issues))
	return nil
}

func runSplit(path string, args []string) error {
	fs := flag.NewFlagSet("split", flag.ContinueOnError)
	dryRun := fs.Bool("n", false, "list the files without writing them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	before, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	l, err := splitFile(path, *dryRun)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	for _, p := range l.Paths() {
		fmt.Printf("%6d lines  %s\n", lineCount(l.Files[p]), filepath.Join(dir, filepath.FromSlash(p)))
	}
	verb := "split"
	if *dryRun {
		verb = "would split"
	}
	fmt.Printf("%s %s (%d lines) into %d files; the entry point keeps %d lines\n", verb, path, lineCount(before), len(l.Files), lineCount(l.Files[l.Root]))
	return nil
}

// splitFile splits the single-file spec at path in place.
func splitFile(path string, dryRun bool) (*spec.Layout, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	l, err := spec.Split(data, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := l.CheckTarget(dir); err != nil {
		return nil, err
	}
	if dryRun {
		return l, nil
	}
	return l, l.Write(dir)
}

func lineCount(b []byte) int {
	return strings.Count(string(b), "\n")
}

func runPlan(path string) error {
	s, _, err := loadValidated(path)
	if err != nil {
		return err
	}
	p, err := plan.Build(s)
	if err != nil {
		return err
	}
	fmt.Printf("system %s (%s): %d modules in build order\n", p.System, p.Topology, len(p.Steps()))
	i := 0
	for _, tp := range p.Tiers {
		if tp.Name != "" {
			fmt.Printf("\n## tier %s (%s) -> %s\n", tp.Name, tp.Language, pipeline.TierDir("out", s.System.Name, tp.Name))
		}
		fmt.Println()
		for _, st := range tp.Steps {
			i++
			fmt.Printf("%d. %s\n", i, st.Module)
			if len(st.DependsOn) > 0 {
				fmt.Printf("   depends on: %v\n", st.DependsOn)
			}
			fmt.Printf("   coder     -> implement %d operation(s)\n", len(s.Module(st.Module).Interface))
			if len(st.Entities) > 0 {
				fmt.Printf("   owns entities: %v\n", st.Entities)
			}
			if len(st.Surfaces) > 0 {
				fmt.Printf("   implements: %v\n", st.Surfaces)
			}
			if len(st.Consumes) > 0 {
				fmt.Printf("   consumes: %v\n", st.Consumes)
			}
			fmt.Printf("   tester    -> %d scenario(s), %d invariant(s)\n", st.Scenarios, st.Invariants)
			fmt.Printf("   validator -> verdict on goals %v\n", st.Goals)
		}
	}
	return nil
}

func runRun(path string, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	out := fs.String("out", "out", "output directory")
	model := fs.String("model", envOr("ASPECT_MODEL", llm.DefaultModel), "model id")
	effort := fs.String("effort", "high", "effort level")
	maxRepairs := fs.Int("max-repairs", 3, "coder repair rounds per module")
	parallel := fs.Int("parallel", 1, "independent modules generated at once")
	fallbacks := fs.Bool("fallbacks", true, "server-side refusal fallbacks")
	if err := fs.Parse(args); err != nil {
		return err
	}

	s, _, err := loadValidated(path)
	if err != nil {
		return err
	}
	p, err := plan.Build(s)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := llm.NewAnthropic(llm.WithModel(*model), llm.WithEffort(*effort), llm.WithFallbacks(*fallbacks))
	runner := pipeline.New(client, pipeline.Options{
		OutDir: *out, MaxRepairs: *maxRepairs, Parallel: *parallel, Log: os.Stderr, Model: *model,
	})
	rep, runErr := runner.Run(ctx, s, p)
	if rep != nil {
		if err := rep.Write(*out); err != nil {
			return err
		}
		fmt.Printf("\nreport: %s\n", filepath.Join(*out, s.System.Name, "REPORT.md"))
		for _, g := range rep.Goals {
			fmt.Printf("  %-4s %-13s %s\n", g.ID, g.Status, g.Statement)
		}
	}
	return runErr
}

func runDrift(path string, args []string) error {
	fs := flag.NewFlagSet("drift", flag.ContinueOnError)
	out := fs.String("out", "out", "directory holding the generated code")
	repo := fs.String("repo", "", "check this repository in place instead of -out")
	reportDir := fs.String("report", "", "where drift.json and DRIFT.md go")
	baseline := fs.String("baseline", "", "previous report.json or drift.json")
	noLLM := fs.Bool("no-llm", false, "skip verdicts")
	noFail := fs.Bool("no-fail", false, "exit 0 even when drift is found")
	model := fs.String("model", envOr("ASPECT_MODEL", llm.DefaultModel), "model id")
	effort := fs.String("effort", "high", "effort level")
	fallbacks := fs.Bool("fallbacks", true, "server-side refusal fallbacks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo != "" && flagSet(fs, "out") {
		return fmt.Errorf("drift: -out and -repo name two different places for the code; pass one")
	}
	s, _, err := loadValidated(path)
	if err != nil {
		return err
	}
	p, err := plan.Build(s)
	if err != nil {
		return err
	}
	if *baseline == "" && *repo == "" {
		if candidate := filepath.Join(*out, s.System.Name, "report.json"); fileExists(candidate) {
			*baseline = candidate
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var client llm.Client
	if !*noLLM {
		client = llm.NewAnthropic(llm.WithModel(*model), llm.WithEffort(*effort), llm.WithFallbacks(*fallbacks))
	}
	rep, err := drift.Run(ctx, client, s, p, drift.Options{OutDir: *out, Repo: *repo, Baseline: *baseline, NoLLM: *noLLM, Log: os.Stderr, Model: *model})
	if err != nil {
		return err
	}
	dir := *reportDir
	switch {
	case dir != "":
	case *repo != "":
		// Never leave files in the repository being checked. The user's
		// cache directory, unlike a shared temp directory, is not writable
		// by other users who could plant a symlink there.
		if dir, err = driftReportDir(s.System.Name); err != nil {
			return err
		}
	default:
		dir = filepath.Join(*out, s.System.Name)
	}
	if err := rep.Write(dir); err != nil {
		return err
	}
	fmt.Printf("drift report: %s\n", filepath.Join(dir, "DRIFT.md"))
	for _, g := range rep.Goals {
		fmt.Printf("  %-4s %-13s %-9s %s\n", g.ID, g.Status, g.Change, g.Statement)
	}
	if m := rep.Missing(); len(m) > 0 {
		fmt.Printf("  missing modules: %s\n", strings.Join(m, ", "))
		if s.System.Source != nil && !anyModulePath(s) {
			fmt.Printf("  (an imported spec without module paths: record them with `aspect paths %s -repo <dir> -w`)\n", path)
		}
	}
	if len(rep.Orphans) > 0 {
		fmt.Printf("  unclaimed code: %s\n", strings.Join(rep.Orphans, ", "))
	}
	if f := rep.Failing(); len(f) > 0 {
		fmt.Printf("  failing tests: %s\n", strings.Join(f, ", "))
	}
	if rep.Drifted() && !*noFail {
		return fmt.Errorf("drift detected")
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runInventory(dir string, args []string) error {
	fs := flag.NewFlagSet("inventory", flag.ContinueOnError)
	include := fs.String("include", "", "package directories to include")
	exclude := fs.String("exclude", "", "package directories to exclude")
	asJSON := fs.Bool("json", false, "print the inventory as JSON")
	pkg := fs.String("package", "", "render one package as the Describer would see it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	inv, err := analyze.Go(dir, analyze.Options{Include: splitList(*include), Exclude: splitList(*exclude)})
	if err != nil {
		return err
	}
	switch {
	case *pkg != "":
		p := inv.Package(*pkg)
		if p == nil {
			return fmt.Errorf("no package at %q", *pkg)
		}
		fmt.Print(p.Render(0))
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(inv)
	default:
		inv.Summary(os.Stdout)
	}
	return nil
}

func runImport(dir string, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	out := fs.String("o", gate.DefaultSpec, "output spec file")
	include := fs.String("include", "", "package directories to include")
	exclude := fs.String("exclude", "", "package directories to exclude")
	language := fs.String("language", "", "target language (default: the source language)")
	topology := fs.String("topology", "", "monolith | api_backend | cloud_service")
	backendLanguage := fs.String("backend-language", "", "backend tier language for api_backend")
	hint := fs.String("hint", "", "guidance for the target platform")
	name := fs.String("name", "", "system name")
	modulePath := fs.String("module-path", "", "target module path")
	cache := fs.String("cache", "", "fragment cache directory")
	concurrency := fs.Int("concurrency", 4, "packages described in parallel")
	model := fs.String("model", envOr("ASPECT_MODEL", llm.DefaultModel), "model id")
	effort := fs.String("effort", "high", "effort level")
	fallbacks := fs.Bool("fallbacks", true, "server-side refusal fallbacks")
	split := fs.Bool("split", false, "one file per module, interface and entity owner")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cache == "" {
		base := *name
		if base == "" {
			base = filepath.Base(mustAbs(dir))
		}
		*cache = filepath.Join(".aspect-cache", base)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	client := llm.NewAnthropic(llm.WithModel(*model), llm.WithEffort(*effort), llm.WithFallbacks(*fallbacks))
	res, err := importer.Run(ctx, client, importer.Options{
		Root: dir, Include: splitList(*include), Exclude: splitList(*exclude),
		Target: *language, TargetHint: *hint, Name: *name, ModulePath: *modulePath,
		Topology: spec.Topology(*topology), BackendLanguage: *backendLanguage,
		Repository: gitRemote(dir), Commit: gitHead(dir),
		CacheDir: *cache, Concurrency: *concurrency, Log: os.Stderr,
	})
	if res != nil {
		fmt.Fprintf(os.Stderr, "LLM calls: %d (in %d / out %d / cached %d tokens)\n", res.Usage.Calls, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.CacheReadTokens)
	}
	if err != nil {
		return err
	}
	if err := importer.Write(*out, res.Spec, res.Warnings, res.Briefs); err != nil {
		return err
	}
	if len(res.Briefs) > 0 {
		fmt.Fprintf(os.Stderr, "wrote %d brief(s) under %s\n", len(res.Briefs), filepath.Join(filepath.Dir(*out), "briefs"))
	}
	issues := res.Issues
	if *split {
		l, err := splitFile(*out, false)
		if err != nil {
			return fmt.Errorf("%s was written as one file; split failed: %w", *out, err)
		}
		fmt.Fprintf(os.Stderr, "split %s into %d files under %s\n", *out, len(l.Files), strings.Join(l.Dirs(), ", "))
		// Validate the split spec so issues point at fragment files.
		if s, err := spec.Load(*out); err == nil {
			issues = spec.Validate(s)
		}
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	for _, i := range issues {
		fmt.Fprintln(os.Stderr, i)
	}
	fmt.Printf("wrote %s: %s (%s, %d tier(s), %d modules, %d goals, %d entities, %d interfaces)\n", *out, res.Spec.System.Name, res.Spec.EffectiveTopology(),
		len(res.Spec.EffectiveTiers()), len(res.Spec.AllModules()), len(res.Spec.System.Goals), entityCount(res.Spec), len(res.Spec.System.Interfaces))
	if issues.HasErrors() {
		return fmt.Errorf("the recovered spec has validation errors; fix them in %s and run `aspect validate`", *out)
	}
	return nil
}

func entityCount(s *spec.Spec) int {
	n := 0
	for _, db := range s.Databases() {
		n += len(db.Entities)
	}
	return n
}

func mustAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func gitRemote(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitHead(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runCheckInvariants(dir string, args []string) error {
	fs := flag.NewFlagSet("check-invariants", flag.ContinueOnError)
	specPath := fs.String("spec", "", "spec file (default _aspect/aspect.yaml if present)")
	asJSON := fs.Bool("json", false, "print violations as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var sp *spec.Spec
	spPath := *specPath
	if spPath == "" {
		for _, cand := range []string{filepath.Join(dir, "_aspect", "aspect.yaml"), filepath.Join(dir, "aspect.yaml")} {
			if fileExists(cand) {
				spPath = cand
				break
			}
		}
	}
	if spPath != "" && fileExists(spPath) {
		var err error
		sp, _, err = loadValidated(spPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not load spec %s: %v\n", spPath, err)
		}
	}

	inv, err := analyze.Go(dir, analyze.Options{})
	if err != nil {
		return err
	}

	var allViolations []analyze.InvariantViolation
	if sp != nil {
		depViolations := analyze.VerifyModuleDependencies(inv, sp)
		allViolations = append(allViolations, depViolations...)
	}

	codeViolations, err := analyze.CheckCodeInvariants(dir, nil)
	if err != nil {
		return err
	}
	allViolations = append(allViolations, codeViolations...)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(allViolations)
	}

	if len(allViolations) == 0 {
		fmt.Printf("ok: %d package(s) verified, 0 invariant violations\n", len(inv.Packages))
		return nil
	}

	fmt.Printf("%d invariant violation(s) found:\n", len(allViolations))
	for _, v := range allViolations {
		fmt.Printf("  %s\n", v)
	}
	return fmt.Errorf("%d invariant violation(s) detected", len(allViolations))
}

// driftReportDir is where `drift -repo` writes its report by default:
// <user cache dir>/aspect/drift/<system>.
func driftReportDir(system string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("drift: no user cache directory for the report (%v); pass -report", err)
	}
	return filepath.Join(cache, "aspect", "drift", system), nil
}

// flagSet reports whether name was given on the command line, as opposed
// to holding its default.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func anyModulePath(s *spec.Spec) bool {
	for _, m := range s.AllModules() {
		if m.Path != "" {
			return true
		}
	}
	return false
}

// runPaths recovers the package directory of every module of an imported
// spec that lacks one, and with -w writes it into the spec files.
func runPaths(path string, args []string) error {
	fs := flag.NewFlagSet("paths", flag.ContinueOnError)
	repo := fs.String("repo", ".", "the repository the spec was imported from")
	write := fs.Bool("w", false, "write path: into the spec files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := spec.Load(path)
	if err != nil {
		return err
	}
	paths, problems, err := importer.RecoverPaths(s, *repo)
	if err != nil {
		return err
	}
	for _, name := range sortedNames(paths) {
		fmt.Printf("%-40s path: %s\n", name, paths[name])
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "aspect:", p)
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d module(s) could not be placed; nothing written", len(problems))
	}
	if len(paths) == 0 {
		fmt.Println("every module already has a path")
		return nil
	}
	if !*write {
		fmt.Printf("%d module(s) would get a path; rerun with -w to write them\n", len(paths))
		return nil
	}
	files, err := spec.SetModulePaths(path, paths)
	if err != nil {
		return fmt.Errorf("%w (files already rewritten: %s)", err, listOrNone(files))
	}
	if _, _, err := loadValidated(path); err != nil {
		return fmt.Errorf("after writing paths into %s: %w", listOrNone(files), err)
	}
	fmt.Printf("wrote %d path(s) into %d file(s)\n", len(paths), len(files))
	return nil
}

func listOrNone(files []string) string {
	if len(files) == 0 {
		return "none"
	}
	return strings.Join(files, ", ")
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
