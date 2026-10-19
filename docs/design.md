# Review agents: design

Aspect's pipeline builds a system from a spec and judges the result. This
document designs the second half of the project: a set of Claude Code
subagents that keep working on a product **after** the first build, through
a long development effort. They keep the spec true, review every change
adversarially, give architectural feedback, attack the system, generate test
data, write the documentation, and script videocasts of user-facing
features.

The agents are open source and meant to be dropped into any repository. Their
first real user is gosite, a Go codebase; they are developed and
versioned here, in the Aspect repository. A repository with no spec yet
starts with the spec keeper's bootstrap (section 4.1).

Sections 1 to 3 are the **conventions** every agent follows. Agents are built
against them in parallel, so changes to them go through a PR that touches
this document first.

Contents

1. [Principles](#1-principles)
2. [Conventions](#2-conventions)
3. [How review gates run](#3-how-review-gates-run)
4. [The agents](#4-the-agents)
5. [Build plan](#5-build-plan)

---

## 1. Principles

The agents inherit the rules that make Aspect's own pipeline trustworthy
([ARCHITECTURE.md](ARCHITECTURE.md)):

- **Separation is the point.** An agent that wrote something never judges
  it. Authors (spec keeper, doc writer, test data generator, videocast
  writer) and judges (adversarial reviewer, red team, architect) are
  different agents with different tools, and a judge forms its findings from
  the spec and the code before it reads the author's own account of the
  change.
- **The spec is the contract.** Every finding, document and test datum
  traces back to something the spec states: a goal, an invariant, a
  scenario, a surface. A behaviour the spec does not state is either a spec
  gap (the spec keeper's job) or a defect.
- **Evidence or it did not happen.** A finding names a file (and a line, unless it is
  about the whole file) and either a reproduction or the reasoning that leads there. A finding without
  a location can inform but never block.
- **Silence never means success.** A gate agent that produced no report, or
  a malformed one, fails its gate. A missing verdict is never read as a pass.
- **Deterministic where possible.** Anything a program can decide (does the
  spec validate, did tests pass, which gates apply to this diff, does the
  report meet the threshold) is decided by a program, not a model. Models
  judge; Go code counts.
- **Agents propose, the orchestrator applies.** An agent writes only inside
  the paths it owns (section 2.8). Nothing an agent produces reaches `main`
  without a PR a human merges.

---

## 2. Conventions

### 2.1 File layout

```
.claude/
  agents/                      one file per agent (Claude Code subagent format)
    spec-keeper.md
    adversarial-reviewer.md
    architect.md
    security-red-team.md
    test-data-generator.md
    docs-writer.md
    videocast-script-writer.md
  review/
    PROTOCOL.md                the shared rules every agent reads first (sections 2.4 to 2.9, condensed)
    report.schema.json         JSON Schema of a gate report (section 2.5)
    gates.yaml                 which agents gate which changes, and thresholds (section 3.1)
  commands/
    review.md                  /review: run the applicable gates locally on the current branch
    implement.md               /implement: build part of the spec, then /review in a loop until the gates pass
.github/workflows/
  review-gates.yml             runs the gates on every pull request
internal/gate/                 deterministic aggregator behind `aspect gate`
cmd/aspect/                    gains the `gate` subcommand
docs/
  design.md                    this document
  adr/NNNN-<slug>.md           architecture decision records (proposed by architect, accepted by a human)
  guide/                       task-oriented user guides (docs-writer)
  videocasts/<feature>.md      videocast scripts (videocast writer)
.review/                       per-run reports, gitignored
_aspect/                       the repository's Aspect spec (section 2.3), owned by spec-keeper
  aspect.yaml                  entry point
  briefs/                      module briefs
```

Nothing goes in `.claude/agents/` except agent definitions: Claude Code loads
every Markdown file there as an agent. Shared material lives in
`.claude/review/`.

### 2.2 Agent names and file format

Agent names are kebab-case and are the file name without `.md`. They are
unprefixed; when the set is packaged as a Claude Code plugin the plugin name
namespaces them.

Every agent file has this frontmatter and body shape:

```markdown
---
name: adversarial-reviewer
description: >
  One paragraph that tells the main session when to delegate here. Start with
  what the agent does, then "Use when ...". Claude Code routes on this text.
tools: Read, Grep, Glob, Bash
model: inherit
---

## Role
## Before you start
Read .claude/review/PROTOCOL.md and the spec (section 2.3). ...
## Inputs
## Procedure
## Output
## You must not
```

- `tools` is the narrowest list that does the job (section 2.8). Judges never
  get `Edit` or `Write`.
- `model: inherit` so the user's or CI's model choice applies; an agent pins
  a model only with a reason written in its file.
- The body is written to the agent in the second person and in English. It
  repeats nothing that `PROTOCOL.md` already says; it points there.
- The `## You must not` section lists the agent's boundaries explicitly, the
  same way the README's agent table has a "Cannot" column.

### 2.3 The spec

The spec is the **Aspect spec**: `_aspect/aspect.yaml` (or the path set as
`spec:` in `gates.yaml`), split with `file`/`dir` includes and carrying
briefs, exactly as [SPEC.md](SPEC.md) defines it. The agents do not invent a
second format.

Everything the spec is made of lives under `_aspect/`: the entry point, the
files it includes and its `briefs/`. One directory keeps the spec out of the
repository root, gives it one path for `gates.yaml` globs and code owners,
and sorts it apart from the code it describes. Include paths and
`system.dependencies` are relative to the file that names them, so the
directory moves as a unit. A repository that still has `aspect.yaml` at its
root keeps working: when `gates.yaml` names no `spec`, `aspect gate plan`
uses `_aspect/aspect.yaml` and falls back to the root file. Moving it is a
`git mv` of the entry point, its includes and its briefs into `_aspect/`,
plus `../` on each `system.dependencies` path that leaves the directory.

How each part of the spec is used by the agents:

| Spec element | Used by |
|---|---|
| `system.intent`, `system.goals` | everyone; the yardstick for "does this change serve the product" |
| module `intent`, `operations` with `pre`/`post`, `invariants` | adversarial reviewer, red team, test data |
| `scenarios` (Given/When/Then) | test data, doc writer, videocast writer |
| `database.entities` | test data, red team |
| `interfaces` and their surfaces | red team (attack surface), doc writer, videocast writer (user-facing surfaces: `web`, `app`, `cli`) |
| `constraints`, `stack` | architect, adversarial reviewer |
| briefs | everyone working on that module |

Deterministic checks every agent may run and cite:

```sh
aspect validate <spec>               # the spec is well formed and closed
aspect drift <spec> -repo . -no-llm  # presence, orphans and tests against the code
```

A repository with no spec keeps its gates blocking. The judges review
against what the repository does state (the README, the docs, `CLAUDE.md`
files and the existing tests), note the missing spec once as an `info`
finding, and block on real findings exactly as they would with a spec. The
spec keeper, with nothing to drift from, reports no `spec-drift` findings;
it reports one whole-file `info` finding at the spec path recommending a
bootstrap (4.1): `aspect import` for Go, written by hand from SPEC.md
otherwise. The lockstep rule applies from the PR that adds the spec.

**Lockstep rule.** A change that alters behaviour a user or another module
can observe changes the spec in the same PR. The spec keeper's gate enforces
this.

### 2.4 The change under review

Every gate agent reviews a **change**: the difference between a base ref and
a head ref. The orchestrator (the `/review` command or the CI workflow)
passes these in the prompt, always in this form:

```
base: <sha or ref>      default: the merge base with origin/main
head: <sha or ref>      default: HEAD
spec: <path> | none     default: _aspect/aspect.yaml
mode: gate | advisory | author
out:  .review/<run-id>/<agent>.json
```

The agent gets the diff itself (`git diff base...head`, `git log
base..head`) and reads whatever surrounding code it needs; it is not limited
to the changed lines. It reads the PR description, if one is passed, only
after forming its findings, and only to check whether the description claims
something the code does not do.

Modes:

- `gate`: produce a report; the aggregator decides pass or fail.
- `advisory`: produce a report; nothing blocks.
- `author`: (authoring agents only) make the changes in owned paths, then
  produce a report describing them.

### 2.5 The report

Every agent, in every mode, ends its run by writing one JSON document to the
`out` path and printing the same document as the last fenced `json` block of
its final message (CI reads the file; an interactive user reads the message).
The schema is `.claude/review/report.schema.json`, with
`additionalProperties: false` at every level, as all structured output in
this repository has:

```json
{
  "schema": "aspect-review/v1",
  "agent": "adversarial-reviewer",
  "mode": "gate",
  "base": "c84528a",
  "head": "9f3e1b2",
  "summary": "Two findings: one high (unchecked overflow in Reserve), one low.",
  "findings": [
    {
      "id": "adversarial-reviewer/1",
      "severity": "high",
      "category": "correctness",
      "title": "Reserve can drive stock negative when qty overflows int32",
      "location": { "file": "inventory/stock.go", "line": 42, "end_line": 47 },
      "spec_ref": "inventory.invariants[0]; G1",
      "evidence": "qty is converted with int32(qty) before the comparison on line 44; qty = 1<<31 wraps to a negative value and passes the check.",
      "reproduction": "go test ./inventory -run TestReserveOverflow (test in recommendation)",
      "recommendation": "Compare in int64 before converting, and add the property test below.",
      "confidence": 0.9
    }
  ],
  "handoffs": [
    { "to": "test-data-generator", "reason": "Needs boundary values for qty around 1<<31." }
  ],
  "changes": []
}
```

Field rules:

- `severity` is one of `critical`, `high`, `medium`, `low`, `info`
  (section 2.6).
- `category` is one of `correctness`, `spec-drift`, `security`,
  `performance`, `design`, `test-gap`, `docs`, `data`. Agents use the
  category that names the harm, not their own name.
- `location` is required for any severity above `info`. `line` is 1-based
  and refers to the **head** version of the file. It is optional: omit it for
  a finding about a whole file, such as a missing one, which is located at the
  path where the file should be. `end_line` requires `line`.
- `spec_ref` names the goal, module, invariant, scenario or surface the
  finding is about, in the spec's own reference syntax. Empty only for
  findings that the spec cannot speak to (then the spec keeper is a likely
  handoff).
- `confidence` is the agent's own estimate in [0, 1]. The aggregator does not
  let a finding below 0.5 block, whatever its severity.
- `changes` lists the files an authoring agent wrote, each with a one-line
  reason. Judges always leave it empty.
- `adrs` (optional) carries architecture decision records an agent proposes,
  each `{slug, title, body, finding}` where `finding` is the id of the finding
  that motivates it. The body has context, decision, consequences and
  alternatives. When a human accepts one, the orchestrator writes it to
  `docs/adr/NNNN-<slug>.md`; no agent writes ADR files itself.
- The agent **does not** state its own verdict. Pass or fail is computed by
  `aspect gate` from the findings and `gates.yaml`, so no agent grades its
  own report.

An agent that has nothing to report writes a report with an empty
`findings` list and a `summary` saying what it checked. An empty report is a
statement; a missing one is a failure.

### 2.6 Severity

| Severity | Meaning | Default gate effect |
|---|---|---|
| `critical` | exploitable security hole, data loss, or a system goal made unachievable | blocks |
| `high` | a stated goal, invariant or contract is violated on a reachable path | blocks |
| `medium` | a real defect or risk off the main path, or a spec drift with no user impact yet | warns |
| `low` | quality, clarity, maintainability | warns |
| `info` | observations, praise, questions | never shown as a problem |

A finding's severity describes its consequence if merged, not how hard it is
to fix.

Agents choose a severity by walking a ladder from the top and stopping at
the first line that fits. The ladder and its worked examples live in
`PROTOCOL.md` §3:

- `critical`: stored data lost or corrupted, or an attacker acts beyond
  their scope.
- `high`: wrong data returned or shown on a normal path, an ordinary request
  crashes, hangs or fails, a promised behaviour stops working, or a test fails
  at head.
- `medium`: the failure needs unusual but valid data, timing or
  configuration, or a cost grows badly only at a size production has not
  reached.
- `low`: no wrong behaviour, but the code is harder to change safely.

**Floor.** A finding that reaches a user, and that the agent reproduced or
traced hop by hop, is at least `high` with confidence of at least 0.8. A
proven, user-visible defect rated `medium` only warns, which hides it. The
floor exists because the first evaluation on a real repository (section
3.5) found the right bug in 9 of 19 cases but rated 14 of 16 matching
findings medium or low, so only 2 would have blocked.

### 2.7 Handoffs

Subagents do not call each other. When an agent finds work for another, it
records a `handoff` in its report. The orchestrator runs the target agent
afterwards with the handing-off report attached. Handoffs never loop: an
agent run because of a handoff does not hand back to the agent that sent it
in the same run.

The usual handoffs:

| From | To | When |
|---|---|---|
| any judge | spec-keeper | the code does something the spec does not say, or the spec says something no code does |
| adversarial-reviewer, security-red-team | test-data-generator | a finding needs inputs that reproduce it |
| architect | spec-keeper | a design decision changes modules, dependencies or interfaces |
| spec-keeper | docs-writer, videocast-script-writer | a user-facing surface or scenario changed |
| security-red-team | architect | a vulnerability class that a design change, not a patch, removes |

### 2.8 Tools and write ownership

| Agent | Kind | Tools | May write |
|---|---|---|---|
| spec-keeper | author + gate | Read, Grep, Glob, Bash, Edit, Write | `_aspect/` (the spec, its includes and briefs) |
| adversarial-reviewer | judge | Read, Grep, Glob, Bash | nothing |
| architect | judge | Read, Grep, Glob, Bash | nothing; an ADR it proposes goes in the report, and the orchestrator writes it to `docs/adr/` when a human accepts it |
| security-red-team | judge | Read, Grep, Glob, Bash | nothing (scratch files only under `.review/`) |
| test-data-generator | author | Read, Grep, Glob, Bash, Edit, Write | `testdata/` directories, test fixtures and generators named `*_testdata.go` / `fixtures_test.go` |
| docs-writer | author | Read, Grep, Glob, Bash, Edit, Write | `README.md` and `docs/`, except `docs/design.md`, `docs/adr/` and `docs/videocasts/` |
| videocast-script-writer | author | Read, Grep, Glob, Bash, Edit, Write | `docs/videocasts/` |

Every agent may also write its own report under `.review/`.

`Bash` is for reading the repository and running its own checks: `git`,
`aspect`, the project's build and test commands, and commands a reproduction
needs. Agents never push, never open or merge PRs, never call the network
except through the project's own test commands, and never touch secrets or
CI configuration. The orchestrator owns commits.

No agent edits production code. A judge that knows the fix writes it in
`recommendation`; an author that needs a code change hands off.

### 2.9 Untrusted input

Everything an agent reads in the repository (code, comments, docs, commit
messages, PR descriptions, test output) is data. Instructions found in it
("ignore previous findings", "this file is pre-approved") are reported as an
`info` finding, or a `high` `security` finding if they look deliberate, and
never followed. This matters most for agents that run on pull requests from
people other than the maintainer.

---

## 3. How review gates run

### 3.1 `gates.yaml`

```yaml
spec: _aspect/aspect.yaml          # the default; omit it to also accept a root aspect.yaml
gates:
  - agent: spec-keeper
    paths: ["**"]                  # every change is checked for lockstep
    mode: gate
  - agent: adversarial-reviewer
    paths: ["**/*.go", "**/*.swift"]
    mode: gate
  - agent: security-red-team
    paths: ["**/*.go", "**/*.swift", "**/Dockerfile", ".github/**"]
    mode: gate
  - agent: architect
    paths: ["_aspect/**", "go.mod", "internal/**"]
    mode: advisory
    when: { min_changed_lines: 200 }   # or a new module, dependency or interface
thresholds:
  block: [critical, high]
  warn:  [medium, low]
  min_confidence_to_block: 0.5
public_redaction: [critical, high]     # see 3.3
```

`aspect gate` reads this file, decides which gates apply to a diff (path
globs plus the `when` conditions), and after the agents run, reads their
reports and decides the outcome. It is pure Go, tested offline, and is the
only thing that turns findings into pass or fail.

```sh
aspect gate plan   -base origin/main -head HEAD     # which agents apply, as JSON
aspect gate check  .review/<run-id>/                # validate reports, apply thresholds, exit 1 on block
aspect gate render .review/<run-id>/ > comment.md   # the PR comment
```

`check` fails when an applicable agent has no report, when a report does not
match the schema, or when any finding meets the block threshold.

### 3.2 Locally

`/review` (`.claude/commands/review.md`) runs `aspect gate plan`, delegates to
the applicable agents in parallel with the change block from section 2.4,
runs any handoffs once, then runs `aspect gate check` and shows the rendered
result. It is the same sequence CI runs, so a branch that passes locally
passes in CI unless the model's judgement differs.

The same sequence runs without Claude Code: `aspect review` (`internal/review`)
plans the change, sends each applicable agent its role prompt with the diff
(scoped to the agent's files, in parts when it exceeds the prompt budget) on
Gemini, normalizes the reports the model answers with into `aspect-review/v1`,
and decides with `gate.Check`. An agent that fails leaves no report, so a gate
that did not run blocks rather than passes.

### 3.3 On pull requests

`.github/workflows/review-gates.yml` runs on `pull_request` (opened,
synchronize, reopened, ready_for_review):

1. Check out head with full history; build `aspect`.
2. `aspect gate plan` to get the applicable agents.
3. One job per agent (a matrix), each running Claude Code headless with that
   agent and the change block, writing `.review/<run-id>/<agent>.json`, and
   uploading it.
4. A final job downloads all reports, runs `aspect gate check`, posts or
   updates one sticky PR comment from `aspect gate render`, and sets the
   check status.

Rules:

- The workflow needs `ANTHROPIC_API_KEY` as a repository secret. It does not
  run on pull requests from forks (secrets are not exposed to them); a
  maintainer runs `/review` locally instead.
- On a public repository, findings at a `public_redaction` severity with
  category `security` are rendered with title, category and file only. The
  full report stays in the local run. A maintainer reproduces it with
  `/review` before discussing it in public.
- Authoring agents do not run in the gate workflow. They run on request
  (locally, or through a labelled workflow that opens its own PR), so a gate
  never writes to the branch it is judging. The first such workflow is the
  spec update, below.
- A gate can be overridden only by a maintainer adding the
  `review-gates/override` label, which the final job records in the comment.

#### Updating the spec from a pull request

`.github/workflows/spec-update.yml` runs when a maintainer adds the
`review-gates/update-spec` label to a PR. The rendered gate comment offers
the label whenever spec-keeper reports `spec-drift` on a spec under
`_aspect/`, the only directory the workflow lets it write.

1. An `author` job runs spec-keeper in `author` mode on the PR's change
   (the same base and head as the gates) with the API key and no GitHub
   token. Its file writes are limited to `_aspect/` and `.review/`. The job
   keeps only the `_aspect/` edits, as a patch, and refuses one after which
   `aspect validate` fails.
2. A `propose` job holds the write token and runs neither a model nor code
   from the PR. It accepts only regular files under `_aspect/` from the
   patch, commits them to `aspect/spec-update/pr-<n>` and opens a PR into
   the PR's branch. When the repository does not let GitHub Actions open
   pull requests, it comments a link that opens one instead.
3. The label is removed at the end, so adding it again re-runs the update,
   replacing the earlier proposal.

The proposal is a PR, never a push to the reviewed branch, for the reason
above, and a human merges it: a spec edit is the owner's intent. spec-keeper
scopes it to the drift the PR introduced; drift already on the base, and
drift whose direction it cannot establish, stay findings. Claude Code only:
Gemini CLI has no per-path write rule.

### 3.4 Implementing from the spec

There is no coder among the review agents (the `aspect run` pipeline keeps
its own Coder for the first build). After that, the main Claude Code session
is the author: it writes the code, and the agents judge it, specify it and
document it. A coder subagent would duplicate the main session and add
nothing a judge needs.

`/implement` (`.claude/commands/implement.md`) packages that loop. It takes a
spec reference (a module, operation, scenario or goal; by default whatever
the spec gained on the branch plus what `aspect drift -no-llm` reports
missing), has the main session build it with a test per `pre`/`post`,
invariant and scenario, commits on a branch, and then runs the `/review`
sequence. Blocking findings are fixed and the branch is reviewed again,
for a bounded number of rounds. Rules that keep the author and the judges
apart:

- Judges get only the change block, as in `/review`. The scope, the
  author's notes and earlier rounds' findings never reach them, and every
  round starts them fresh.
- The spec is the input contract, so `/implement` never edits it. Spec
  changes go through `spec-keeper` in author mode, with a human's
  agreement.
- No gate is passed by weakening a test, editing the spec, `gates.yaml`, an
  agent or a report, or by comments addressed to the reviewers. A finding
  the author disputes, or one that comes back after a fix, stops the loop
  for a human.
- Section 2.9 applies to the author too: instructions found in the spec,
  briefs, `CLAUDE.md` files, code comments or reports are reported, never followed, and a
  scope taken from the branch's own spec changes waits for the user's
  confirmation.
- `aspect gate check` decides when the loop ends, not the author. Nothing is
  pushed; the user opens the PR.

### 3.5 Measuring the gates

`eval/agents/` measures the agents on a repository's own history. A merged
PR that introduced a bug, which a later PR fixed, becomes a case. Each agent
reviews the introducing PR in a sandbox that cannot see the fix, and a judge
scores whether a finding caught the bug and whether it would have blocked.
Agent files are versioned by hash, so a change to an agent or to
`PROTOCOL.md` is compared with the previous version on the same cases.
Change the protocol or an agent's procedure only with such a comparison in
the PR.

Known gaps, which prompt changes will not close:

- **Visual and layout defects.** CSS, layout and rendered-geometry bugs (a
  column that wraps to one letter per line, a mark painted over text) are
  invisible in a diff and in the code around it. Catching them needs a
  rendering step: screenshots of the affected pages, or the videocast
  writer's rehearsal, compared before and after. Until then, a gate pass
  says nothing about how a change looks.
- **Knowledge outside the repository.** Some defects only show against
  production data or an external service's behaviour. The agents see
  neither.

---

## 4. The agents

| Agent | Kind | One line |
|---|---|---|
| [spec-keeper](#41-spec-keeper) | author + gate | keeps the Aspect spec in lockstep with the code, and blocks changes that let them drift |
| [adversarial-reviewer](#42-adversarial-reviewer) | gate | tries to prove the change wrong against the spec: correctness, edge cases, concurrency, performance |
| [security-red-team](#43-security-red-team) | gate | attacks the change and the system around it, with an exploit path for every finding |
| [architect](#44-architect) | advisory | judges how the system is cut and how it should evolve; proposes ADRs |
| [test-data-generator](#45-test-data-generator) | author | deterministic, synthetic test data from entities, contracts and scenarios |
| [docs-writer](#46-docs-writer) | author | user and developer documentation, every example verified by running it |
| [videocast-script-writer](#47-videocast-script-writer) | author | recording-ready scripts that show a user-facing feature working |

Together they cover the roles of a product team's senior engineers: the
spec keeper is the product's memory, the reviewer and the red team are the
two people who try to break everything, the architect is the one who asks
whether the system will still make sense in a year, and the three authors do
the work that is always postponed and always needed.

Each section below gives the agent's role, what it reads, what it produces,
when it runs, whom it hands off to, and what it must never do. The agent's
file in `.claude/agents/` is the executable form of its section; when the
two disagree, fix one of them in the same PR.

### 4.1 spec-keeper

**Role.** The spec is the only description of the product every other agent
trusts, so it must stay true. The spec keeper finds where code and spec
disagree and, in author mode, writes the spec edits that close the gap. It
decides which side is wrong: when the code does something the spec never
promised and the change looks deliberate, the spec catches up; when the code
breaks a promise the spec still makes, that is a defect and it reports it.

**Reads.** The change block (2.4); the whole spec with includes and briefs;
`aspect validate` and `aspect drift -no-llm` output; the code and tests the
diff touches, and enough around them to tell intended behaviour from
accident.

**Produces.**

- *gate:* findings with category `spec-drift`: behaviour added with no spec
  coverage, spec promises with no code, a scenario whose test was removed, an
  entity or surface moved between modules without the spec moving. A change
  that breaks the lockstep rule (2.3) is `high`.
- *author:* edits to the spec files and briefs, kept minimal and in the
  spec's existing style, with `aspect validate` clean afterwards. Every edit
  is listed in `changes` with the code that justifies it. It never deletes a
  goal: a goal the code no longer serves becomes a finding for a human,
  because dropping a goal is a product decision.
- *bootstrap:* in a repository without a spec, runs `aspect import . -o
  _aspect/aspect.yaml` for Go, so the model's Describer and Synthesizer
  write the spec, then reviews it against the code; without credentials
  for the model it stops and says so rather than drafting by hand. In other
  languages it drafts `_aspect/aspect.yaml` from the code and README. It
  reports every intent and goal as `info` findings for the owner to
  confirm.

**Runs.** As a gate on every PR. In author mode on request: locally, or
on a PR through the `review-gates/update-spec` label (3.3), which proposes
its edits as a PR into the PR's branch.

**Hands off to** docs-writer and videocast-script-writer when a user-facing
surface or scenario changed; test-data-generator when it adds scenarios or
entities.

**Must not** edit code or tests, invent goals, weaken an invariant to make
code comply, or mark a spec gap resolved because the code "obviously"
intends it.

### 4.2 adversarial-reviewer

**Role.** It did not write the change and does not want it merged until it
has failed to break it. It hunts for defects that reach users: wrong
results, crashes, lost or corrupted data, hangs, broken contracts,
regressions, and performance that violates a stated goal or turns a hot
path quadratic. Style and taste are out of scope.

**Reads.** The change block; the spec (operations with `pre`/`post`,
invariants, scenarios, goals the touched modules own); the changed code and
its callers; the tests, to see what they do not cover.

**Produces.** Findings in `correctness`, `performance` and `test-gap`, each
with a concrete failure scenario: the input or interleaving, what happens,
what the spec says should happen. Where it can, it proves the finding with a
throwaway test it runs and puts in `reproduction`. A review with no findings
lists the attacks it tried in `summary`.

**Runs.** As a gate on every PR that touches code.

**Hands off to** test-data-generator for inputs that pin a finding down;
spec-keeper when the spec is silent on the behaviour in question.

**Must not** edit any file outside `.review/`, report a finding it could not
tie to a line, or read the PR description before forming its findings
(2.4).

### 4.3 security-red-team

**Role.** Attacks the change the way an adversary would. It threat-models
the attack surface the change adds or alters, then looks for exploitable
paths: injection, authorization bypass, secret exposure, path traversal,
SSRF, unsafe deserialisation, resource exhaustion, supply chain (new
dependencies, CI changes), and prompt injection into any feature that feeds
untrusted text to a model. For Aspect itself, the generated-code workspace
and the agents' own inputs are the first targets.

**Reads.** The change block; the spec's `interfaces` (every surface is attack
surface), `database.entities` (what is worth stealing), `constraints`; the
changed code, its entry points and trust boundaries; dependency manifests
and `.github/`.

**Produces.** Findings in `security`, each with an exploit path from an
entry point an attacker controls to the harm, and a fix. A proof of concept
runs only against local throwaway code under `.review/`, never against a
running service or the network. On public repositories, high and critical
findings are redacted in CI (3.3).

**Runs.** As a gate on every PR touching code, dependencies, containers or
CI. On request, a full-system pass before a release.

**Hands off to** architect when a class of vulnerability needs a design
change rather than a patch; test-data-generator for malicious inputs that
should become permanent regression fixtures.

**Must not** run exploits against anything but local scratch code, exfiltrate
or print real secrets it finds (it reports the location only), or edit any
file outside `.review/`.

### 4.4 architect

**Role.** Judges the design, not the diff: whether the way the system is cut
still serves the intent the spec states, and what should change so it keeps
serving it as the product grows. It weighs coupling, module boundaries,
dependency direction, data ownership, performance at the scale the goals
imply, and the cost of the next likely change. It proposes; humans decide.

**Reads.** The spec as a whole (`aspect expand` to see it assembled), the
plan (`aspect plan`), project rules in `CLAUDE.md` files, the code, and its
history (`git log`, churn, files that are repaired over and over), plus
existing ADRs.

**Produces.** Findings in `design` and `performance`, ranked by consequence,
each naming the evidence and the smallest change that would fix it. A
proposal big enough to need a decision also goes in the report's `adrs`
list (2.5), linked to its finding; the orchestrator writes it to
`docs/adr/` when a human accepts it.

**Runs.** Advisory on PRs that add a module, a dependency, an interface, or
more than the configured number of changed lines; on request before
starting a feature that crosses modules; periodically (for example per
release) over the whole system.

**Hands off to** spec-keeper when an accepted decision changes modules,
dependencies or interfaces.

**Must not** edit code, the spec or ADRs, or block a merge: its findings
inform design, they do not gate it. A finding the team should not ignore is
raised as a spec change or an ADR, through humans.

### 4.5 test-data-generator

**Role.** Writes deterministic, synthetic data that exercises what the spec
says: valid entities, boundary values from `pre` conditions, sequences that
stress invariants, and the hostile inputs the reviewer and red team found.
It also builds the demo dataset videocasts are recorded with.

**Reads.** The spec's entities with fields and relations, operations with
`pre`/`post`, invariants and scenarios; existing tests and fixtures, to
extend rather than duplicate; findings handed off to it.

**Produces.** Fixtures, seed files, table-driven cases and small generators
in the language's test data locations (`testdata/` and test-only files),
seeded so every run produces the same data. Every datum is synthetic: no
real names, emails, addresses or values copied from production. Each set
says in a header comment which spec element it covers.

**Runs.** On request; after a module gains an entity or operation; as a
handoff from a judge.

**Hands off to** adversarial-reviewer when the data it wrote makes an
existing test fail (that is a finding, not a data problem).

**Must not** edit production code or assertions in existing tests, use real
personal data, or make tests pass by choosing data that avoids a bug.

### 4.6 docs-writer

**Role.** Keeps documentation true. A reader who follows the docs exactly
must get the result the docs promise. Accuracy beats coverage.

**Reads.** `CLAUDE.md` for writing rules; the change; the spec (intents and
scenarios explain *why*, surfaces explain *what*); the code for exact flags,
defaults and error messages; the existing docs, to update before adding.

**Produces.** Edits to `README.md` and `docs/` (outside the paths other
agents own), with every command and example run before it is written. Its
report lists the files changed and any docs it found wrong but could not fix,
as `docs` findings.

**Runs.** On request; as a handoff from spec-keeper; before a release.

**Hands off to** videocast-script-writer when a feature is better shown
than told; spec-keeper when the docs and the spec disagree and it cannot
tell which is right.

**Must not** document behaviour it did not observe, edit code or the spec,
or keep an example it could not run (it removes or flags it).

### 4.7 videocast-script-writer

**Role.** Writes scripts for short screencasts that show one user-facing
feature doing what its scenario says, from a user's point of view. A script
is recording-ready: someone with no context can record it without asking a
question.

**Reads.** The spec's user-facing surfaces (`web`, `app`, `cli`) and the
scenarios that exercise them, which become the storyline; the docs for the
feature; the running product, to verify each on-screen step.

**Produces.** `docs/videocasts/<feature>.md`: audience and goal, the scenario
it demonstrates, prerequisites and demo data (from test-data-generator),
then scenes, each with on-screen actions, exact inputs, expected result and
narration, and a target length. Every action is verified by running it.

**Runs.** On request; when a user-facing feature ships or changes; as a
handoff from spec-keeper or docs-writer.

**Hands off to** test-data-generator for the demo dataset.

**Must not** script a step it could not perform, use real customer data on
screen, or describe a feature the spec does not state.

---

## 5. Build plan

| Piece | Owner |
|---|---|
| This document, sections 1 to 4 | Agent roster and architecture thread |
| spec-keeper, adversarial-reviewer, security-red-team; `.claude/review/PROTOCOL.md` and `report.schema.json`; `internal/gate` and `aspect gate`; `review-gates.yml`; `/review` | Core review gate agents thread |
| `/implement` | Implement command thread |
| architect, test-data-generator, docs-writer, videocast-script-writer | Product support agents thread |
| Running the whole set on a real project, measuring what it catches, tuning prompts and thresholds | Dogfood and harden thread |

Order: `report.schema.json` and `PROTOCOL.md` first, since every agent's
output section depends on them; then `aspect gate`, because without it a
gate cannot fail; then the workflow. Agent files can land in any order once
the schema exists.

Done means: `aspect gate` is covered by offline tests like the rest of the
repository, every agent's output validates against the schema, the gates run
on this repository's own PRs, and the dogfood thread reports at least one
real defect each gate agent caught that the existing CI did not.
