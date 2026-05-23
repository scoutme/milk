# milk — feature backlog

## Remote oversight interface (Telegram or similar)

Allow the user to follow agent work and approve permission prompts from a mobile device when not at the PC.

- Configurable notification backend (Telegram bot as first target; design for extensibility to other transports)
- Push notifications for agent turns: routing decision, model used, tool calls, escalations
- Permission prompt forwarding: when Claude requests a permission, send it to the remote interface and await approval/denial before unblocking the subprocess
- Bidirectional: user can also send a prompt remotely to inject into the next turn
- Timeout/fallback behavior when remote approval doesn't arrive within a configurable window (auto-approve, auto-deny, or pause)
- Config keys under `remote_oversight` in `~/.milk/config.json`

## Reasoning visibility control

Keep chain-of-thought / thinking tokens separated from conversation history, with user control over display.

- Reasoning content stored separately from the regular message history (never mixed into the transcript context sent to the next turn)
- Per-session toggle: show or hide reasoning blocks (`/think on` / `/think off`)
- Retroactive: toggling applies to all past turns in the current view, not just future ones
- When hidden: show a collapsible placeholder (e.g. `[thinking…]`) that the user can expand inline
- Persisted preference in session state; default configurable in `~/.milk/config.json`
- Applies to both local model `<think>` blocks and Claude extended thinking tokens

## Local Inference Automation (llama.cpp)

Analyze the possibility to automate the llama.cpp process launch (or similar solution) in order to grant local model inference on milk start. The launch should be configurable via milk configuration, and commands and tools must be implemented to interact with llama.cpp for model switching. Keep evolution in mind, since llama.cpp is just an option: in the future we'll add support for remote inference or other inference server, keeping functionalities intact.

## Input area bug

when typing multiline content, sometimes text not fitting in current line disappears, to appear then only when it's long enough to be seen in subsequent line. This doesn't happen between first and second line

## Code linting

Add code colorization

## Check memory decay

I didn't see a single percept decaying

## ~~Move notification into status bar~~ DONE

When trying to submit input during agent response, "[milk] agent is responding..." is added to chat view. Show it in status bar instead

## ~~Slash commands and @files colorization broken when using multiline input~~ DONE

Works only in single line

## Permissions

Claude keeps asking the same permissions, as if milk isn't saving them into claude settings

## Selection hint too present

Selection hint should be visible only when selection is started, that is when mouse as been moved at least a bit after press, before release

## Possible permission issue

Claude is asking many times between different turns the same permission requests, sa if milk isn't updating correctly its configurations

## ~~Input Navigation vs History~~ DONE

When in input area, if up arrow is pressed while at the beginning of the first line, history should be navigated

## Prompt label refactor

The input prompt currently shows the next-turn agent (e.g. `[local]`, `[claude]`). Since the header bar and status bar already carry full agent/mode info, the prompt label could be simplified to just `>` or a very short marker. Evaluate whether removing the agent name from the prompt reduces clutter without losing context.

## ~~Input area: lines beyond the first not visible~~ DONE

When typing a long enough input to overflow the first textarea line, subsequent lines are not visible — only the first line is shown in the input area. The text is being buffered (it can be submitted), but the visual display doesn't grow to show continuation lines.

## Ctrl+Z undo paste

When the user pastes content (via Ctrl+V or right-click) and wants to undo it, Ctrl+Z should revert the textarea to its previous state. Currently there is no undo history in the input area.

## Memory tuning

Nothing decays, all becomes global

## Dangerous permission skip via command

A command should enable permission management mode switching
