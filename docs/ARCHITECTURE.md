# Architecture

## Why separate agents

One model asked to "write this module and its tests" grades its own homework:
the tests it writes mirror the code it wrote, and passing them proves little.
Aspect splits the work by *what each agent is allowed to know and do*:

- The **Coder** sees the spec and its dependencies' source. It cannot write
  tests.
- The **Tester** sees the spec and the implementation, but is instructed to
  derive tests from scenarios and invariants and to let the spec win when the
  two disagree. It cannot change implementation files.
- The **Validator** sees everything, including the real test run and the
  Tester's coverage claims, and is told it did not write any of it. Its output
  is a verdict per goal with evidence that must name files and identifiers.

The pipeline enforces these boundaries mechanically, not just by prompt: each
agent's file list is filtered to its module directory and file kind before
anything reaches disk (`agents.keepModuleFiles`), and the workspace refuses
paths that escape the root or touch `go.mod`.

## Data flow

```
spec.Load ─▶ spec.Validate ─▶ plan.Build ─▶ pipeline.Run
                                              │
                        for each module in dependency order:
                                              │
        ┌──── Coder.Generate(spec, deps) ──────┤
        │            │                        │
        │     ws.WriteFiles                   │
        │            │                        │
        │     Tester.Generate(spec, code) ────┤
        │            │                        │
        │     ws.WriteFiles                   │
        │            │                        │
        │  ┌──▶ ws.Test ── fail ──▶ Coder.Generate(feedback) ─┐
        │  │         │                                        │
        │  └─────────┼────────────────────────────────────────┘  (≤ max-repairs)
        │          pass
        │            │
        │     Validator.Judge(spec, code, tests, result, coverage)
        │            │
        └──── ModuleReport
                                              │
                                        Report.Write ─▶ report.json, REPORT.md
```

Modules are generated in topological order so each Coder call sees the real
source of its dependencies. Ties are broken alphabetically to keep runs
comparable.

Within a tier, the plan is cut into waves: a module joins the first wave
after all its dependencies' waves, so modules in one wave are independent.
With `-parallel N`, up to N modules of a wave run at once. Only the model
calls overlap; manifest syncs and toolchain runs in a workspace are
serialised behind a lock, because two `go mod tidy` or `swift build`
invocations in one tree corrupt each other. The Swift manifest declares
only targets whose directories exist, so a module still being written
cannot break another module's build. The report keeps plan order.

## Verification methods

A goal declares how it can be checked, and the Validator applies a different
standard to each:

| `verify` | Achieved when |
|---|---|
| `test` | a test genuinely exercising the goal exists and the real run passed |
| `invariant` | a property-style test exists **and** the Validator's reading of the code finds no path (errors, concurrency, overflow) that breaks it |
| `review` | the Validator can cite code that supports the claim; no test can prove "the API is idiomatic" |

A failed test run caps every `test` and `invariant` goal below `achieved`,
whatever the model would otherwise say; the pipeline states the real outcome
in the Validator's prompt and the report shows the last run.

## The system pass

Module verdicts answer "does this module do its part?". Once every tier is
built, a separate System Validator answers "do the parts add up?". It sees
the whole spec, every module's test outcome and verdict, and the
implementation of every module that provides or consumes a surface (other
modules are included while a size budget allows). It returns one verdict
per system goal considering all accountable modules together, an intent
judgement for the system as a whole, and an integration finding per
provider/consumer pairing: routes, methods, shapes, errors and auth
compared on both sides, across tiers and languages.

A goal's final status is the weakest of the module roll-up and the system
verdict, so a system that composes badly cannot hide behind modules that
each pass.

## Silence never means success

Two rules make the report conservative:

1. `reconcileGoals` guarantees one verdict per accountable goal. A goal the
   Validator forgot becomes `unverifiable` with zero confidence.
2. `summarise` rolls a goal up across modules as its *weakest* verdict, and a
   module that errored out marks every goal it owns `unverifiable`.

## Model usage

- One provider package, `internal/llm`, behind a two-method interface. Agents
  and the pipeline are tested with fakes; the Anthropic client has no tests
  because it would need the network.
- Every call is single-turn and stateless. The system prompt carries a cache
  breakpoint, so the role prompt is read from cache across the run; the
  volatile spec and code come after it.
- Responses are constrained with structured outputs (a JSON schema per agent),
  streamed, and accumulated. `max_tokens` is high because agents emit whole
  files.
- Server-side refusal fallbacks are on by default so a classifier refusal on
  one call is re-served by a fallback model instead of aborting the run.
- Effort defaults to `high`; `xhigh` is worth trying for large modules.

## Tiers

A multi-tier spec is a set of single-tier specs joined by interfaces. The
pipeline builds tiers in dependency order, each in its own workspace with its
own language profile, manifest and import allowlist. Modules see the whole
system in outline (every tier, every interface with its provider) but only
their own tier's stack, and the full contract of the surfaces they implement
or consume. A consumed surface is rendered with a note that it is served
elsewhere: the module must implement a client against the contract, with
the base address from configuration.

Single-tier specs are handled through the same code path: the spec exposes
an implicit tier with an empty name, so nothing branches on the spec's
shape. Ownership rules become tier-aware (entities belong to the database's
tier, surfaces to the provider's tier, `depends_on` never crosses tiers),
and the topology field is validated against the shape it claims.

## Language profiles

`internal/lang` is the only place that knows a language's layout, manifest,
toolchain commands and idioms. A profile supplies:

- `CodeDir` / `TestDir` / `IsTestFile`, used to filter agent proposals and to
  collect dependency sources;
- `Init` / `Sync`, which write the manifest (`go.mod` once; `Package.swift`
  regenerated from the spec after every module so SwiftPM sees exactly the
  targets on disk);
- `Steps`, the build-and-test commands the workspace runs;
- `CheckImports`, the mechanical allowlist (Go parses import declarations;
  Swift relies on the manifest, which only declares packages from the stack);
- `CoderRules` / `TesterRules`, appended to the agents' system prompts.

The agents and the pipeline never branch on the language name.

## Stack, database and interfaces in prompts

The system-level context every agent sees carries the stack for the target
language, the database in outline (engine, test strategy, entity names and
intents) and the interfaces in outline. The module section then carries the
full definitions of the entities the module owns and the surfaces it
implements, plus the guidance of the frameworks those surfaces use. Keeping
detail with the owner keeps prompts small and makes drift visible: a module
that touches an entity it does not own is contradicting what it was shown.

## Drift

`aspect drift` runs the judging half of the pipeline over code that already
exists, generated or not. For every tier it opens the workspace, reads the
tree, and for every planned module finds its implementation and tests by
the profile's layout, runs the real toolchain, and asks the Validator for a
verdict with the same prompt the build used. The System Validator then
judges the whole. Three things fall out that a build cannot tell you:

- **missing** modules: named in the spec, no code on disk (their goals
  become `not_achieved`);
- **orphans**: source directories no module claims, the usual sign that
  the spec fell behind the code (directories the import excluded, and
  what the toolchain ignores, such as `testdata`, hidden directories and
  nested Go modules, are not code);
- **regressions**: goal statuses compared with a baseline `report.json` or
  `drift.json`, so a change to either side shows up as same, improved or
  regressed.

Every file goes to exactly one module: the one whose directory holds it,
the deepest when several do. A module placed by `path` (what `aspect
import` writes) owns its one package directory, so `modules/api` never
takes `modules/apikeys/` or `modules/api/v2/`, and the root module owns
only the root's files. `-repo DIR` points drift at a repository instead of
`aspect run` output; it then writes nothing there (no manifest, no `go mod
tidy`), and a symlinked `-out` entry is followed.

`-no-llm` keeps the deterministic part only, which is enough for a CI gate
on presence and tests.

## Importing an existing system

```
analyze.Go ─▶ inventory (deterministic)
                 │
                 ├─▶ Describer × packages ─▶ fragments (cached as JSON)
                 │
                 └─▶ Synthesizer (fragments + summary [+ target]) ─▶ synthesis
                                                                       │
                          importer.Assemble (deterministic) ◀──────────┘
                                   │
                          spec.Validate ─▶ aspect.yaml + warnings
```

Two modes share the same agents:

- **Mirror** (target language = source): one module per package, dependency
  edges from the import graph, entities from the fragments (owner = the
  package that defines the struct), surfaces merged into interfaces by
  canonical name (`api`, `web`, `admin`, `cli`, `grpc`). The Synthesizer
  contributes only the system level: intent, goals mapped to modules, stack,
  interface metadata.
- **Retarget** (monolith in another language): the Synthesizer designs the
  module list for the target platform from the fragments. The assembler
  sanitises identifiers and validates; it does not redesign.
- **Tiered** (`api_backend`, `cloud_service`): for `api_backend` with the
  backend in the source language, the backend tier is mirrored from the
  fragments and the Synthesizer proposes only the modules to add (typically
  the API module the frontend needs) plus the frontend tier's design; web
  and admin surfaces become `app` or `web` screens provided by the frontend,
  and modules that call the backend list its surfaces under `consumes`. For
  `cloud_service` the Synthesizer designs a single app tier and declares
  everything server-side as external providers.

Assembly's last pass moves any surface a module "implements" but does not
provide into `consumes`, so a model that blurs the line still yields a
valid spec.

The inventory is the only language-specific part of the importer. Adding a
source language means adding an analyser that produces the same `Package`
shape.

## Trust boundaries

Generated code is executed (`go test -race`) on the machine running Aspect.
Treat `aspect run` like running any code you have not read: use a throwaway
directory or container for specs you do not control. The workspace bounds
each run with a timeout but does not sandbox it.
