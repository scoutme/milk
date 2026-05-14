# Architecture Decision Records

* [1. Go + Cobra CLI](0001-go-cobra-cli.md)
* [2. Single llama.cpp Instance for Routing and Coding](0002-single-llama-instance.md)
* [3. Claude via CLI Subprocess, not Direct API](0003-claude-cli-subprocess.md)
* [4. Context Handoff via --append-system-prompt](0004-context-handoff-append-system-prompt.md)
* [5. Session Storage as JSON Files Indexed by cwd](0005-session-json-files.md)
* [6. CLAUDE_WAITING State for Turn-Level Routing](0006-claude-waiting-state.md)
* [7. Local Model Self-Escalation via Function Call](0007-self-escalation-via-function-call.md)
* [8. Branching Strategy and Conventional Commits](0008-branching-strategy.md)
* [9. Router Signal Extractor and Weighted Scorer](0009-router-signal-scorer.md)
* [10. Interactive REPL Mode](0010-interactive-repl-mode.md) *(superseded by 14)*
* [11. Claude Pipe-Only Mode with Stdin Disconnect](0011-claude-pipe-only-mode.md)
* [12. Claude Tool and Directory Permission Handling](0012-claude-permission-handling.md)
* [13. Structured Permission Prompts via --permission-prompt-tool stdio](0013-structured-permission-prompts.md)
* [14. Full TUI with bubbletea viewport+textarea](0014-full-tui-bubbletea.md)
* [15. TUI-native permission prompts via blocking goroutine + channel](0015-tui-permission-prompts.md)
