---
description: Check the status of code reviews and their reviewers, live while a review runs.
argument-hint: "[run-id, default the current run]"
---

Check the status of an Aspect review run and its reviewer agents.

Run via Bash:
`aspect review status $ARGUMENTS`

It shows the run (the current one, `.review/current_run.json`, unless a run id is given): its state, outcome, cost, and each agent's mode, state (pending, queued for a model slot, running, completed, failed), verdict, blocking and warning counts, diff parts done and any error. A running review rewrites its `status.json` at every change, so running the command again shows its progress.

`aspect review runs` lists the repository's runs, newest first.
