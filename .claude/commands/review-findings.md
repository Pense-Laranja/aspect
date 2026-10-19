---
description: Inspect detailed code review findings (title, severity, file, line and recommendation).
argument-hint: "[run-id, default the current run] [-severity critical|high|medium|low|info] [-agent <name>]"
---

Inspect the findings of an Aspect review run, most severe first.

Run via Bash:
- `aspect review findings <run-id>` (omit the run id for the current run)
- by severity: `aspect review findings <run-id> -severity high`
- by reviewer: `aspect review findings <run-id> -agent adversarial-reviewer`

Pass `$ARGUMENTS` through. For each finding, give its location, what is wrong and the fix it recommends; the full evidence is in `.review/<run-id>/<agent>.json`.
