---
description: Compare review findings side by side between two review runs.
argument-hint: "<run-a> <run-b>"
---

Compare two Aspect review runs, for example the same change reviewed by two models or before and after a fix.

1. **Run IDs:** Extract `<run-a>` and `<run-b>` from `$ARGUMENTS`. If omitted, list the runs first with `aspect review runs`.

2. Run via Bash:
   `aspect review compare <run-a> <run-b>`
   It shows each run's engine, outcome and totals, then every agent's findings and blocking findings in both runs and the difference.

3. For a difference worth explaining, read both runs' findings (`aspect review findings <run-id> -agent <name>`).
