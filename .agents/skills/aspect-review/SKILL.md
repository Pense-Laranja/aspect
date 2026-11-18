---
name: aspect-review
description: "Run Aspect review gates in Antigravity using Antigravity credits and Gemini Flash models via run_workflow, without needing GEMINI_API_KEY. Prepares deterministic diff/spec prompt bundles with `aspect review prepare`, dispatches parallel stateless `aspect_gate_reviewer` Flash calls, and finalizes verdicts with `aspect review finalize`."
---

# Aspect Review Gates on Antigravity Flash

Use this skill whenever running Aspect review gates (`.claude/review/gates.yaml`) inside Antigravity. Instead of calling Google's Gemini REST API directly via `GEMINI_API_KEY`, this workflow splits execution into three deterministic phases so all reviewer calls execute on Antigravity's built-in `flash` model using Antigravity credits:

1. **Prepare (`aspect review prepare -json`)** — Resolves git merge-base/head diffs, committed spec excerpts (`_aspect/aspect.yaml`), full-run cache reuse, base-merge carry, and Tier 1/Tier 2 selective agent carry. Emits prompt bundles in `.review/<run-id>/prompts/` and `manifest.json`.
2. **Dispatch (`run_workflow` -> `agent(..., model="flash")`)** — Runs each pending reviewer task in bounded parallel batches using the zero-tool `aspect_gate_reviewer` persona and a closed inline JSON schema.
3. **Finalize (`aspect review finalize`)** — Merges multi-part outputs, normalizes reports into `aspect-review/v1`, runs `gate.Check`, writes `.review/<run-id>/status.json` and `rendered.md`, and optionally publishes PR comments via `.claude/review/publish.sh`.

## Running the Review Workflow

Invoke Antigravity's `run_workflow` tool pointing to `scripts/review_workflow.py` beside this `SKILL.md`:

```python
run_workflow(
    ScriptPath="/path/to/.agents/skills/aspect-review/scripts/review_workflow.py",
    Description="Run Aspect review gates on Flash",
    Args={
        "repo": "/absolute/path/to/repo",
        "base": "origin/main",
        "head": "HEAD",
    },
)
```

### Supported `Args`

| Key | Default | Description |
|---|---|---|
| `repo` | `WORKSPACES[0]` or `cwd` | Repository root containing `.claude/review/gates.yaml` (or using `ASPECT_HOME`). |
| `base` | `"origin/main"` | Base ref; the diff starts at its merge base with `head`. |
| `head` | `"HEAD"` | Head ref under review. |
| `pr` | `""` | Pull request number override (auto-detected when omitted). |
| `publish` | `false` | Run `.claude/review/publish.sh` to post/update the sticky PR review comment. |
| `force` | `false` | Ignore cached/carried runs and review all applicable agents afresh. |
| `agent` | `""` | Run a single planned agent in an isolated `.review/agent-<timestamp>` run (never cached, never published, never set as `current_run.json`). |
| `engine` | `""` | Optional engine label override passed to `aspect review prepare -engine` (default `"antigravity (flash)"`). |
| `max_concurrency` | `4` | Maximum concurrent Flash `agent()` calls per batch (`>= 1`). |
| `aspect_bin` | `""` | Optional explicit path to the `aspect` binary (falls back to `ASPECT_BIN`, trusted `PATH`, `ASPECT_HOME`, and opt-in `allow_repo_exec` for repo-local `.claude/review/lib.sh` or `cmd/aspect`). |
| `allow_repo_exec` | `false` | Opt-in boolean (or `ASPECT_ALLOW_REPO_EXEC=1`) allowing fallback execution of repo-local `.claude/review/lib.sh` or `go run ./cmd/aspect` inside the target repository when `aspect` is not installed on `PATH` or `ASPECT_HOME`. |
| `run_dir` | `""` | Optional path to an already-prepared `.review/<run-id>` directory to dispatch and finalize. |

## Inspecting Results & Gating PR Merges

Every run writes standard `.review/<run-id>/` artifacts compatible with all `aspect review` inspection commands (no API key needed):

```bash
aspect review status -repo <repo> [run-id]
aspect review findings -repo <repo> [run-id] [-severity high] [-agent security-reviewer]
aspect review compare -repo <repo> <run-a> <run-b>
aspect review runs -repo <repo>
```

### Merge Decision Rules

- **`PASS` / `WARN` / `OVERRIDDEN` (`EXIT_CODE: 0`)**:
  All blocking review gates cleared. State clearly:
  `"✅ PR Merge Cleared: All blocking review gates passed."`
- **`BLOCK` (`EXIT_CODE: 1`)**:
  Blocking findings or missing gate reports were detected. State clearly:
  `"🛑 PR Merge Blocked: Review gates found blocking issues."`
  List every blocking finding with its `file:line` location and 3-part remediation, fix the root causes with regression tests, and re-run the workflow before merging.
- **`ERROR` (`EXIT_CODE: 2`)**:
  No verdict was produced (e.g., reviewer execution failed). Treat as non-passing; inspect `<run-dir>/*.error.txt` or `aspect review status` and re-run.
