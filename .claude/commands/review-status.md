---
description: Check the live status of code reviews, see active reviewers, and open an auto-refreshing dashboard panel.
argument-hint: "[panel | --web | --port 9922]"
---

Check the real-time status of Aspect code reviews and inspect active reviewer personas.

Follow these steps based on arguments and environment:

1. **Check Arguments:**
   - If `$ARGUMENTS` contains `panel`, `web`, or `--web`: open the live auto-refreshing web panel in the user's browser.
   - Otherwise, display the current review status and active reviewer breakdown in chat.

2. **In Claude Desktop (MCP):**
   - **Status Check:** Call the MCP tool:
     `get_review_status()`
     This displays in-progress review runs, which reviewer agents are running on Gemini (`gemini-flash-latest`), elapsed duration, completed reviewers, and blocker tallies.
   - **Open Live Panel:** If the user requested the panel (`/review-status panel`) or wants continuous auto-refresh:
     Call the MCP tool:
     `open_review_panel()`
     This launches the local review dashboard server and opens `http://localhost:9922` in their browser with auto-refresh every 2 seconds.

3. **In Claude Code / Terminal:**
   - **Terminal Live Dashboard (auto-refreshes every 1s):**
     Run:
     `aspect-review dashboard`
   - **Web Panel in Browser (auto-refreshes every 2s):**
     Run:
     `aspect-review panel`
     (or `aspect-review dashboard --web`)
   - **Quick One-Shot Status:**
     Run:
     `aspect-review status`
