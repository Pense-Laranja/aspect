# Aspect

Agents that turn a formal specification into code, tests, and a verdict on
whether the result achieves the spec's stated goals. Go module
`github.com/Pense-Laranja/aspect`.

## Layout

- `internal/spec` — spec format (YAML), loader (includes via `file`/`dir` on the node tree, briefs, dependencies), splitter (the inverse, one file per module), semantic validator; issues carry the file:line they come from. Pure Go, no LLM. Single-tier specs are an implicit tier (`EffectiveTiers`); never branch on the spec's shape elsewhere.
- `internal/plan` — deterministic build order from the module dependency graph.
- `internal/lang` — language profiles (go, swift): layout, manifest, toolchain steps, import guard, prompt rules. Agents and pipeline never branch on language.
- `internal/llm` — the only package that calls a model provider: Anthropic (the pipeline's agents) and Gemini (the review engine). Agents use the `llm.Client` interface.
- `internal/agents` — Coder, Tester, Validator (build) and Describer, Synthesizer (import). Pure functions of their input plus a client; never touch disk.
- `internal/analyze` — deterministic Go inventory (parser only, no type checking).
- `internal/importer` — import orchestration, fragment cache, deterministic assembly, YAML writer.
- `internal/drift` — re-validate existing code against a spec: presence, orphans, tests, verdicts vs baseline.
- `internal/workspace` — writes proposed files (path-guarded) and runs the profile's toolchain steps.
- `internal/pipeline` — orchestration and the report.
- `internal/gate` — review gates: which agents apply to a diff, report validation, pass/warn/block, PR comment. Pure Go, no model, no git.
- `internal/gitx` — git for the commands that read a repository: the gate plan of a change, shared by `aspect gate plan` and `aspect review`.
- `internal/speccontext` — what the spec says about some modules, as budgeted Markdown: the text `aspect review` puts in each reviewer's prompt and `aspect context` prints.
- `internal/review` — the review engine behind `aspect review`: runs the planned agents on Gemini, normalizes their reports, records the run in `.review/<run-id>/`, and gates with `internal/gate`. It replaced CodeReviewer (Python); keep its run files, `CODE_REVIEWER_*` variables and exit codes, which repositories' scripts read.
- `cmd/aspect` — CLI: `validate`, `expand`, `split`, `plan`, `run`, `drift`, `paths`, `briefs`, `context`, `inventory`, `import`, `gate`, `review`, and `mcp`, the review engine served over MCP (stdio) under the tool names the Python CodeReviewer's server used.
- `.claude/agents`, `.claude/review`, `.claude/commands` — the review agents, their shared protocol, report schema and `gates.yaml`, and the `/review` and `/implement` commands. See `docs/design.md`.
- `.agents/` — Antigravity multi-agent suite: `aspect_developer` orchestrator, `aspect_designer`, `aspect_coder`, and validation review subagents with GitHub PR management.
- `examples/` — reference specs; CI validates them.
- `docs/` — architecture and spec format.

## Rules

- Write all engineering text in English.
- Agents propose, the workspace applies. Do not add filesystem or exec calls to `internal/agents`.
- The Validator must never share a prompt or output with the Coder or Tester; separation is the point.
- Every agent output is structured JSON with a schema in the agent's file; keep schemas `additionalProperties: false`.
- Tests must run without an API key. Use the fake clients in `*_test.go` (an `httptest` server for a provider's HTTP); never call the network in tests.
- Stage files explicitly (`git add <path>`), never `git add .` or `-A`.
- Never squash-merge PRs.
- `.claude/review/report.schema.json` and `internal/gate/report.go` describe the same document; change both together (a test checks they agree).
- Every yaml tag on spec structs has a matching json tag; the Synthesizer's output is decoded with encoding/json and snake_case keys would otherwise be dropped silently.

## Verify

```
gofmt -l . && go vet ./... && go test -race ./...
go run ./cmd/aspect validate examples/inventory/aspect.yaml
go run ./cmd/aspect plan examples/inventory/aspect.yaml
```
