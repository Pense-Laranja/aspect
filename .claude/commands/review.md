---
description: Run the Aspect review gates via Gemini, automatically gating PR merges.
argument-hint: "[base-ref, default origin/main]"
---

Run the Aspect review gates on this branch with `aspect review`, which runs every applicable review agent on Gemini and gates the change.

Follow these steps:

1. **Base Reference:** Determine the base branch: `$ARGUMENTS` (defaults to `origin/main` if omitted).

2. **Run Review Gates:** Run via Bash:
   `aspect review -repo . -base <base-ref>`
   (or `.claude/review/run.sh --publish <base-ref>` where the repository has it, which also publishes the result).
   It needs `GEMINI_API_KEY` (or `GOOGLE_API_KEY`). It exits 0 on pass or warn, 1 when the gates block, and 2 when there is no verdict, which is not a pass. The run is `.review/<run-id>/`; an unchanged diff reuses its finished run, and a head that only merged its base in cleanly (same diff, same agents and spec) carries the verdict of the head before it as a run of its own (`-force` reviews again).

3. **PR Merge Gating & Decision:**
   - With `-publish`, the repository's `.claude/review/publish.sh` posts the result to the open pull request.
   - **If Outcome is BLOCK:**
     State clearly:
     "🛑 **PR Merge Blocked:** Review gates found blocking issues."
     List all blocking findings with their exact file and line locations (`aspect review findings`), explain the defect/vulnerability, and provide the actionable fix.
     **Do NOT proceed with merging the PR or branch while gates are blocking.**
   - **If Outcome is PASS:**
     State clearly:
     "✅ **PR Merge Cleared:** All blocking review gates passed. The branch is approved for merge."
