# Aspect

**Aspect** is a system of agents that builds software from a formal
specification and then checks whether what it built achieves the goals the
specification states.

Most code-generation tools stop at "it compiles and the tests pass". Aspect
treats that as the midpoint. The specification declares the *intent* of the
system and of every module, plus the *goals* the finished system must reach.
Separate agents write the code, write the tests from the spec (not from the
code), run them for real, and finally judge, goal by goal and with cited
evidence, whether the implementation honours the intent.

```
spec.yaml ──▶ Planner ──▶ ┌─────────┐   ┌─────────┐   ┌───────────┐
                          │  Coder  │──▶│ Tester  │──▶│ go test   │──┐
                          └─────────┘   └─────────┘   └───────────┘  │ fail: repair
                               ▲                                     │
                               └─────────────────────────────────────┘
                                                        pass │
                                                             ▼
                                                      ┌───────────┐
                                                      │ Validator │──▶ REPORT.md
                                                      └───────────┘
```

## What "formal specification" means here

A spec is a YAML document with three layers:

| Layer | Declares | Consumed by |
|---|---|---|
| **System** | name, intent, goals (each with a verification method), global constraints | every agent |
| **Module** | intent, owned goals, dependencies, interface (signatures with pre/post conditions), invariants, Given/When/Then scenarios | Coder, Tester, Validator |
| **Goal** | one outcome with `verify: test`, `invariant` or `review` | Validator's verdict |
| **Stack** | per-language frameworks (e.g. qor5 for a Go admin), ORM, import allowlist, usage guidance | Coder, Tester, Validator; the pipeline enforces the allowlist |
| **Database** | engine, migration policy, test database, entities with fields and relations | modules that own entities |
| **Interfaces** | http, web, cli or grpc surfaces bound to entities and a framework | modules that implement surfaces |

A spec can be **split across files** with `file` and `dir` includes
(`aspect split` turns a single file into one file per module and interface,
and validation issues name the file and line), and
can **depend on other Aspect systems**: `system.dependencies` names their
specs, and modules consume their surfaces as `identity/api.login` with the
contract checked against the other system's real spec. See
[examples/modular_shop](examples/modular_shop/aspect.yaml), which depends on
[examples/identity](examples/identity/aspect.yaml).

A module (or a tier, or the system) may point at a **brief**: a sidecar
Markdown file with the longer story, domain rules and seams that would
crowd the YAML. Every agent working on that module reads it whole.

Goals are the contract. Every goal must be owned by at least one module or
scenario, every entity by exactly one module, every surface implemented by
exactly one module, or validation refuses the spec: an orphan could never be
judged. The language reference is [docs/SPEC.md](docs/SPEC.md); see
[examples/inventory/aspect.yaml](examples/inventory/aspect.yaml) for a
minimal spec and
[examples/warehouse_admin/aspect.yaml](examples/warehouse_admin/aspect.yaml)
for one with a Postgres data model, a qor5 admin and an HTTP API, and
[examples/monolith_minimal/aspect.yaml](examples/monolith_minimal/aspect.yaml)
for the smallest spec that runs.

Specs are language-agnostic. `system.language` picks a profile (`go` or
`swift` today) that decides layout, manifest and toolchain, so the same spec
can be built as a Go service and as a SwiftPM package for iOS.

A spec can also be split into **tiers** with a `topology`: `monolith` (one
tier on its own database), `api_backend` (a backend tier serving an API and
a web or mobile frontend tier consuming it, each in its own language), or
`cloud_service` (one app tier over hosted services). Tiers talk only through
interfaces that one tier provides and others consume; each is generated into
its own workspace in dependency order. See
[examples/shop_two_tier/aspect.yaml](examples/shop_two_tier/aspect.yaml).

## The agents

| Agent | Input | Output | Cannot |
|---|---|---|---|
| **Planner** | validated spec | deterministic build order (topological sort by dependency) | call a model; it is plain Go |
| **Coder** | system + module spec, source of dependencies | complete files for the module, notes, concerns | write tests, write outside its module |
| **Tester** | module spec, implementation (for identifiers only) | `_test.go` files, scenario→test mapping | modify the implementation |
| **Validator** | spec, code, tests, the *real* test result, coverage claims | verdict per goal with evidence and confidence, intent alignment | be the agent that wrote the code |

The pipeline (not the agents) writes files and runs tools, so every side
effect passes through one path-guarded boundary. When tests fail, the Coder
receives the failing output and repairs the implementation; the tests stay
fixed because they are the spec's executable form. If the Coder believes a
test contradicts the spec, it says so in `concerns` and the report surfaces
it rather than silently bending the code to the test. On a repair round the
Coder returns only the files it changed plus an explicit `removed` list, sees a
summary of the earlier rounds, and the loop stops early when a repair leaves
the failure unchanged. Every agent call retries transient failures, re-asks
once when an answer is truncated or does not decode, and the Validator's
rules that can be checked mechanically (a failed run achieves nothing,
coverage claims must name real tests) are enforced in Go, not only in the
prompt.

## Install and run

```sh
go install github.com/eideroliveira/aspect/cmd/aspect@latest

aspect validate examples/inventory/aspect.yaml   # every issue in one pass
aspect expand   examples/modular_shop/aspect.yaml # a split spec, assembled
aspect split    _aspect/aspect.yaml -n           # one file per module, interface and entity owner
aspect plan     examples/inventory/aspect.yaml   # build order and per-step work
aspect run      examples/inventory/aspect.yaml -out ./out
```

`run` needs credentials: `ANTHROPIC_API_KEY` or an `ant auth login` profile.
It writes the generated Go module to `out/<system>/`, plus `report.json` and a
human-readable `REPORT.md` with the goal table.

Flags: `-model` (default `claude-opus-5`, or `$ASPECT_MODEL`), `-effort`
(`low`…`max`), `-max-repairs` (default 3), `-parallel N` to generate up to
N independent modules of a tier at once, `-fallbacks=false` to disable
server-side refusal fallbacks.

## Checking for drift

Specs and code both change. `aspect drift` re-validates the code on disk
against the spec without regenerating anything:

```sh
aspect drift shop.yaml -out ./out              # after editing the spec or the code
aspect drift shop.yaml -out ./out -no-llm      # presence, orphans and tests only
```

It reports modules the spec names that have no code, code directories no
module claims, modules whose tests fail, and, with a model, a fresh verdict
per goal compared against the last `report.json` (or a `-baseline` you
name): same, improved, regressed. It writes `DRIFT.md` and `drift.json`
next to the code and exits non-zero when anything needs attention, so it
can run in CI.

## Importing an existing system

Aspect can recover a spec from an existing Go codebase, and re-express it
for another platform:

```sh
aspect inventory ~/src/shop -exclude external            # deterministic, no model calls
aspect import    ~/src/shop -o shop.yaml                  # same language: one module per package
aspect import    ~/src/shop -o shop-ios.yaml -language swift \
                 -hint "an iOS app for members; the admin stays on the web"
aspect run shop-ios.yaml -out ./out                       # backend under out/shop/backend, app under out/shop/mobile
```

The `-topology` flag chooses the shape of the new system. `monolith` keeps
one tier on its own database (the default when the language does not
change). `api_backend` is the default when the language changes, because a
mobile app cannot open the database: the backend tier mirrors the existing
packages plus whatever API module the frontend needs, and the frontend tier
is designed for the target language. `cloud_service` generates only the
app tier over hosted services (Supabase, Firebase, an existing API) declared
as external providers.

Import runs in three stages. The **inventory** is deterministic: packages,
exported API, persistent structs (from ORM tags), routes, tests and the
dependency graph, from the parser alone. The **Describer** reads one package
at a time and writes its fragment: intent, candidate goals, entities,
surfaces, language-neutral operations, invariants and scenarios derived from
the tests. The **Synthesizer** consolidates fragments into system goals, the
stack and the interfaces; in retarget mode (target language differs) it also
designs the module list for the target, turning web pages into app screens
and keeping the backend API as an interface the app consumes.

Markdown documents found in a package directory (README.md, CLAUDE.md,
design notes) become `briefs/<module>.md` next to the recovered spec and are
referenced as the module's brief, so the owner's own rules travel with the
spec. Assembly is deterministic again, records provenance under `system.source`,
and never fails: whatever it cannot reconcile becomes a warning at the top of
the YAML. Fragments are cached under `.aspect-cache/`, so an interrupted or
re-run import only pays for packages not yet described. Review the recovered
intents and goals before running the pipeline: they are the model's reading
of the code, not the owner's statement of it.

## Review gates

Aspect ships a comprehensive, multi-agent review system designed to continuously review and gate changes after the initial build. Copy `.claude/agents/`, `.claude/review/` and `.claude/commands/` into a repository and install `aspect`:

### Agent Personas

Aspect provides specialized review personas configured with grounded checklists, anti-hallucination guardrails, and reachability proofs:

| Agent | Default Mode | What it does |
|---|---|---|
| **`spec-keeper`** | gate, author | Keeps the Aspect spec in lockstep with the code; blocks drift, writes spec updates. |
| **`adversarial-reviewer`** | gate | Edge cases, race conditions, nil dereferences, concurrent deadlocks, data corruption. |
| **`security-reviewer`** | gate | AppSec auditing: OWASP Top 10, injection, auth/authz bypass, path traversal. |
| **`red-team`** | gate | Attacker mindset: weaponizable chains, state desynchronization, permission boundaries. |
| **`security-red-team`** | gate | System-level security, secrets, Dockerfile, dependency boundaries, CI integrity. |
| **`database-reviewer`** | gate | Schema migrations, dangerous locks (`CONCURRENTLY`), multi-step writes outside `tx`, N+1 queries. |
| **`test-adequacy-reviewer`** | gate | Missing regression tests for bug fixes, unexercised error branches, hollow/assertion-less mocks. |
| **`quality-reviewer`** | gate | Idiomatic code patterns, error wrapping (`%w`), resource cleanup (`defer`), goroutine leaks. |
| **`i18n-reviewer`** | gate | Hardcoded user-facing copy, translation catalogs, brand glossary compliance, translation pipeline integrity. |
| **`consistency-reviewer`** | advisory | Pattern uniformity, style coherence, architectural conventions across packages. |
| **`readability-reviewer`** | advisory | Cognitive complexity, deep nesting, naming clarity, self-documenting code. |
| **`design-reviewer`** | advisory | API contracts, single responsibility, leaky abstractions, tight coupling. |
| **`architect`** | advisory | Structural contracts, module decoupling, migration safety, cross-package boundaries. |
| **`performance-reviewer`** | advisory | Unbounded buffer reads (`io.ReadAll` without `LimitReader`), zero-timeout HTTP clients, slice retention leaks. |
| **`observability-reviewer`** | advisory | Production debuggability, PII/secret logging prevention, swallowed errors, trace context. |
| **`api-contract-reviewer`** | advisory | Breaking JSON tags (`json:"..."`), webhook idempotency/ACKs, backward-compatibility breaks. |
| **`docs-writer`** | author | Drafts and updates documentation, briefs, and API guides based on spec changes. |
| **`test-data-generator`** | author | Generates realistic, edge-case test fixtures and synthetic data. |
| **`videocast-script-writer`** | author | Generates changelog walkthrough scripts for video updates. |

The agents read the repository's spec from `_aspect/aspect.yaml`, with its includes and briefs beside it in `_aspect/` (a root `aspect.yaml` still works). A repository without one gets a first draft from `spec-keeper` in author mode.

Each agent outputs a structured JSON report conforming to the `aspect-review/v1` schema (`.claude/review/report.schema.json`). Findings carry severities (`critical`, `high`, `medium`, `low`, `info`) and categories (`correctness`, `spec-drift`, `security`, `performance`, `design`, `test-gap`, `docs`, `data`, `readability`, `consistency`, `observability`, `contract`, `i18n`).

### Execution & Gating Pipeline

`aspect gate` calculates the plan, evaluates reports, and enforces thresholds defined in `.claude/review/gates.yaml`:

```sh
aspect gate plan -base origin/main -o .review/run/plan.json  # determine which agents apply
aspect gate check  .review/run                               # exits non-zero if a gate blocks
aspect gate render .review/run                               # formats the markdown PR review comment
aspect gate extract -o .review/run/<agent>.json <transcript>  # extracts verbatim JSON report
```

### Review Engines & Providers

Aspect review gates can be executed via three environments:

1. **Gemini Review Engine (`aspect-code-reviewer` / `aspect-review`):**
   Runs via Google Gemini (`gemini-flash-latest` or configured model) with high throughput:
   - As an MCP server in **Claude Desktop** (exposing `review_with_aspect_gates` and `run_single_aspect_agent`).
   - Via CLI: `aspect-review gates --repo . --base origin/main [--publish]`.
   - As automated merge guards: `pre-merge-commit` git hook or `tools/gate-pr.sh`.
2. **Claude Code (Interactive CLI):**
   Runs interactively inside Claude Code via the `/review` command or `.claude/review/run.sh`.
3. **Gemini CLI / Headless CI:**
   Runs headlessly via `.claude/review/gemini-policy.toml` or CI workflow (`REVIEW_GATES_PROVIDER=gemini`).

When the spec keeper finds drift, the `review-gates/update-spec` label on the PR runs it in author mode (`.github/workflows/spec-update.yml`): its edits to `_aspect/` come back as a PR into the PR's branch, for a human to merge.

`/implement [spec ref]` closes the loop from the other side: the main Claude Code session builds what the spec states and the code lacks, with a test per operation contract, invariant and scenario, then runs `/review` and fixes blocking findings until `aspect gate check` passes.

## Status

Active development. What exists today:

- Spec format v1 with stack, database and interface sections; a validator
  that reports every problem at once (identifiers, references, dependency
  cycles, goal, entity and surface ownership).
- Language profiles for Go and Swift (SwiftPM, iOS + macOS platforms).
- A mechanical import allowlist for Go: generated code that imports anything
  outside the spec's stack is rejected and fed back to the Coder.
- Deterministic planner.
- Coder, Tester and Validator agents on the Anthropic API with structured
  JSON outputs, prompt caching, streaming, and refusal fallbacks.
- Workspace runner per language with a timeout.
- Repair loop and per-module, per-goal report, plus a system pass that judges
  goals across modules and checks every provider/consumer contract.
- Parallel generation of independent modules (`-parallel N`): model calls
  overlap, tool runs in a workspace stay serialised.
- `aspect drift`: re-validate existing code against a changed spec, with
  goal verdicts compared to a baseline.
- Tiers and topologies: monolith, api_backend (backend + frontend in
  different languages, each in its own workspace), cloud_service.
- `aspect inventory` and `aspect import`: recover a spec from a Go codebase,
  optionally split into tiers and re-expressed for Swift/iOS.
- Multi-agent review gate engine with 15+ specialized gate and advisory personas.
- Gemini review engine integration via Model Context Protocol (MCP) and CLI.
- Expanded review categories: readability, consistency, observability, contract, i18n.
- Tests run offline against fake clients, including end-to-end pipeline
  tests that exercise a real repair round through the Go toolchain and a
  real build through the Swift toolchain.

Planned next:

- Importers for other source languages (the Describer and Synthesizer are
  language-agnostic; only the inventory is Go-specific).
- A Swift import guard equivalent to the Go one.
- More target languages: a profile in `internal/lang` is all a language needs.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design rationale.

## Development

```sh
gofmt -l . && go vet ./... && go test -race ./...
```

No test touches the network. Contributions follow the conventions in
[CLAUDE.md](CLAUDE.md).

## License

MIT. See [LICENSE](LICENSE).

## Prompt 

This system was generated from this prompt:
```
I want to create a new project, preserved in github. This project is about aspect agents. I want to build a system of agents, 
which based on a formal specification of a system, will generate the code, tests and validate if the system and modules 
intents are achieving the stated goals.
```
