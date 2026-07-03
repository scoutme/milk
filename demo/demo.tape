# milk demo — TUI walkthrough
# Requires: vhs, milk binary, copilot-ghe and claude-cli configured in ~/.milk/config.json
# Run from the milk repo root: vhs demo/demo.tape
# Output: docs/images/milk-tui.gif

Output docs/images/milk-tui.gif

Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 700
Set Theme "Dracula"
Set TypingSpeed 60ms
Set PlaybackSpeed 0.8

# ── Launch ─────────────────────────────────────────────────────────────────────
Type "cd /home/scoutme/altworkspace/milk && milk --new"
Enter
Sleep 2s

# ── Beat 1: quick local lookup ─────────────────────────────────────────────────
# copilot-ghe handles it with a grep tool call; no escalation.
Type "what files are in internal/agent/?"
Sleep 200ms
Enter
Sleep 8s

# ── Beat 2: @-path tab completion ──────────────────────────────────────────────
# Type a partial @-path, Tab cycles through completions, first Enter accepts,
# second Enter submits.
Type "walk me through @internal/router/r"
Sleep 600ms
Tab
Sleep 500ms
Tab
Sleep 500ms
Enter
Sleep 300ms
Enter
Sleep 24s

# ── Beat 3: /escalate pinning ──────────────────────────────────────────────────
# /escalate alone pins all subsequent turns to the claude escalation agent.
Type "/escalate"
Sleep 300ms
Enter
Sleep 300ms
Enter
Sleep 800ms
Type "summarize how auto-sticky escalation works"
Sleep 200ms
Enter
Sleep 18s

# ── Beat 4: /primary to unpin ──────────────────────────────────────────────────
Type "/primary"
Sleep 300ms
Enter
Sleep 600ms

# ── Beat 5: slash-command tab completion ───────────────────────────────────────
# Type /ex, Tab fills in /export, first Enter accepts, second Enter runs it.
Type "/ex"
Sleep 600ms
Tab
Sleep 500ms
Enter
Sleep 300ms
Enter
Sleep 2s

# Hold on final frame.
Sleep 2s
