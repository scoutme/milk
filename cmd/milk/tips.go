package main

// Idle tip line (ADR-0061): one dim line between the input area and the
// status bar that surfaces milk's feature surface while the user is idle at
// the prompt. Three content sources, in precedence order:
//
//  1. A contextual hint derived from live state (contextualTip) — actionable
//     right now and fresh by construction, because it is computed per render.
//  2. The curated list (curatedTips) — hand-written one-liners for features
//     and key bindings, in priority order.
//  3. Registry-derived tips (registryTips) — generated from the slash-command
//     registry (slashcmd.go, issue #226) so every command gets a tip without
//     hand-written copy that could drift from the real command set.
//
// The rotation (tipsTick) advances through buildTipList() while the user is
// idle, so the contents stay fresh over a long session without ever
// repeating mid-cycle. The line's row is reserved whenever tips are enabled
// (hintRegion) — it yields to completion hints but never to busy state, so
// the layout does not jump when a turn starts or ends.
//
// Browsing (tips management): /tips next|prev|pick|search and the
// Alt+Up/Alt+Down shortcuts step through the pool manually — pick jumps to
// a numbered (or text-matched) tip, search lists matches. A manual browse
// pins the rotation's entry over contextual hints (tipPinned) until the
// next automatic advance, and the rendered line carries the shortcut
// reference itself (tipNavRef). The same registry entry drives all of it.

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	rw "github.com/mattn/go-runewidth"
)

// tipRotateInterval is how often the idle tip advances while the user is
// idle at the prompt: slow enough to read a one-liner without the line
// feeling jittery, fast enough that a session sees many tips.
const tipRotateInterval = 20 * time.Second

// tipsTickMsg advances the idle tip rotation (see tipsTick). gen must match
// model.tipsGen: a manual browse restarts the cycling timer (restartTipsTick),
// so a tick scheduled before the restart carries an older gen and is dropped
// — the same generation check as toastTickMsg (issue #168's lesson).
type tipsTickMsg struct{ gen int }

// curatedTips is the hand-written half of the built tip list, in priority
// order — the rotation starts at the top, so a fresh session shows the most
// valuable tips first (the ordering is how this list expresses priority; it
// has no separate weights).
//
// Markup: "{span}" is highlighted in the rendered line (yellow), everything
// else is dim. A tip that names a command (e.g. "{/learn <fact>} …") marks
// that command as covered — registryTips skips it so the same command is
// never tipped twice. Keep the command name as the first token inside the
// span; that is what the coverage scan reads.
var curatedTips = []string{
	`Type {@} followed by a filename to fuzzy-find and reference it — {Tab} cycles the candidates`,
	`Start a line with {!} to run a shell command directly, e.g. {!git status}`,
	`{Ctrl+R} searches your input history — {Ctrl+S} searches forward`,
	`{F1}–{F6} toggle the memory / tasks / background / workflow / project / git panels (same as {/panel <name>})`,
	`{/think view} shows the model's reasoning on its own — {Ctrl+T} toggles the last thinking block`,
	`{/escalate <msg>} forces one turn to the escalation agent — bare {/escalate} pins every turn`,
	`{/learn <fact>} stores a persistent memory — {/memory} lists them, {/forget} deletes one`,
	`{/bg start <task>} runs a background agent — {Ctrl+Enter} while a turn runs starts one too`,
	`{/attach <file>} stages a file or image for the next turn — {/paste} grabs one from the clipboard`,
	`{/export} prints this session's transcript — {/export <path>} writes it to a file`,
	`{/sessions} lists stored conversations — {/resume <id>} switches to one`,
	`{/workflow <name>} runs a multi-stage workflow (dev, pair, swarm)`,
	`{/usage} shows token usage by model and role`,
	`{/config init} runs the setup wizard`,
	`{/mcp auth <server>} connects an MCP server with OAuth`,
	`{/open <file>} edits a file in $EDITOR`,
	`{/permissions} tunes when unanswered tool prompts auto-allow or auto-deny`,
	`{/setup telegram} lets you approve tool prompts from your phone`,
	`{/colorize off|fenced|balanced|full} adjusts markdown highlighting`,
	`{Ctrl+C} interrupts the current turn — press it twice to exit`,
}

// tipCoveredCmds returns the set of canonical command names (with leading
// "/") a curated tip already explains. Derived from the markup spans — the
// first token of a "{/cmd …}" span — so adding a command to a curated tip
// automatically retires its registry-derived duplicate.
func tipCoveredCmds() map[string]bool {
	covered := map[string]bool{}
	for _, tip := range curatedTips {
		for _, seg := range tipSegments(tip) {
			if !seg.hl || !strings.HasPrefix(seg.s, "/") {
				continue
			}
			name := strings.Fields(seg.s)[0]
			if spec, ok := specFor(name); ok {
				covered[spec.name] = true
				for _, a := range spec.aliases {
					covered[a] = true
				}
			}
		}
	}
	return covered
}

// registryTips derives tips from the command registry: every non-hidden
// command no curated tip mentions gets "{/name <hint>} — desc", built from
// the registry's own desc/hint. A command added to the registry gets a tip
// the day it lands, and a renamed or re-described command cannot leave a
// stale tip behind — the issue #226 single-source invariant, applied to
// tips. Hidden commands (/list's deprecation window) are not advertised.
//
// The rendered hint is host-aware: registryTip uses hintFor(hostTUI), so a
// command whose TUI vocabulary diverges from ACP's (/think) shows its TUI
// form here while ACP keeps advertising the base hint.
func registryTips() []string {
	covered := tipCoveredCmds()
	var tips []string
	for _, s := range commandRegistry {
		if s.hidden || !s.in(hostTUI) || covered[s.name] {
			continue
		}
		tips = append(tips, registryTip(s))
	}
	return tips
}

// registryTip renders one registry-derived tip line: the command name with
// its TUI-host display hint (TUI overrides like /think's fold vocabulary
// apply), then the registry desc.
func registryTip(s cmdSpec) string {
	name := s.name
	if h := s.hintFor(hostTUI); h != "" {
		name += " " + h
	}
	return "{" + name + "} — " + s.desc
}

// buildTipList is the full rotation pool: curated tips first (priority
// order), then the registry-derived long tail. Deterministic — the rotation
// is a plain cycle, so tests can pin positions and a session never sees a
// tip twice before the pool is exhausted.
func buildTipList() []string {
	return append(append([]string{}, curatedTips...), registryTips()...)
}

// tipSeg is one run of tip text: highlighted (hl) or plain.
type tipSeg struct {
	s  string
	hl bool
}

// tipSegments parses the "{span}" markup into alternating plain/highlight
// runs. A stray "{" without a closing "}" is literal text.
func tipSegments(text string) []tipSeg {
	var segs []tipSeg
	for {
		open := strings.IndexByte(text, '{')
		if open < 0 {
			if text != "" {
				segs = append(segs, tipSeg{s: text})
			}
			return segs
		}
		if open > 0 {
			segs = append(segs, tipSeg{s: text[:open]})
		}
		rest := text[open+1:]
		close := strings.IndexByte(rest, '}')
		if close < 0 {
			segs = append(segs, tipSeg{s: text[open:]})
			return segs
		}
		segs = append(segs, tipSeg{s: rest[:close], hl: true})
		text = rest[close+1:]
	}
}

// tipNavRef is the shortcut reference every rendered tip line ends with —
// the "tips visualization" teaches the browsing keys (Alt+Up/Down, same as
// /tips prev|next) itself, so they are discoverable without reading /help.
const tipNavRef = "prev/next: Alt+Up/Down"

// renderTip renders one tip for the hint line: a gold "tip" label, dim
// prose, yellow "{span}" highlights — truncated to width display columns
// with an ellipsis (tips are one-liners; a narrow terminal must not wrap
// them into the layout's line budget), then the tipNavRef shortcut
// reference. On narrow terminals the reference is dropped first; the tip
// text is the content, the reference is the affordance.
func renderTip(text string, width int) string {
	const labelPlain = "tip: "
	suffix := " · " + tipNavRef
	budget := width - rw.StringWidth(labelPlain) - rw.StringWidth(suffix)
	if budget < 8 {
		suffix = ""
		budget = width - rw.StringWidth(labelPlain)
	}
	if budget < 8 {
		budget = 8
	}
	var b strings.Builder
	b.WriteString(boldGold("tip") + dim(": "))
	for _, seg := range tipSegments(text) {
		w := rw.StringWidth(seg.s)
		if w <= budget {
			b.WriteString(tipStyled(seg.s, seg.hl))
			budget -= w
			continue
		}
		// Final segment: cut to the remaining budget, then ellipsize.
		cut := 0
		used := 0
		for i, r := range seg.s {
			cw := rw.RuneWidth(r)
			if used+cw > budget-1 {
				break
			}
			used += cw
			cut = i + utf8.RuneLen(r) // byte index past this rune
		}
		b.WriteString(tipStyled(seg.s[:cut], seg.hl))
		b.WriteString(dim("…"))
		break
	}
	b.WriteString(dim(suffix))
	return b.String()
}

// renderTipText renders one tip's markup (yellow highlight spans, dim prose)
// without the "tip:" label, truncation or shortcut reference — for
// transcript output (the /tips pick and /tips search listings), where the
// line budget does not apply and the full tip should be readable.
func renderTipText(text string) string {
	var b strings.Builder
	for _, seg := range tipSegments(text) {
		b.WriteString(tipStyled(seg.s, seg.hl))
	}
	return b.String()
}

// tipStyled applies the tip-line styling: yellow highlight span, dim prose.
func tipStyled(s string, hl bool) string {
	if hl {
		return yellow(s)
	}
	return dim(s)
}

// contextualTip returns a state-derived hint when something on screen is
// worth acting on right now — rendered instead of the rotating tip, so it
// is always current. Empty means "nothing urgent; show the rotation".
//
// Deliberately small: anything the status bar already announces permanently
// (background jobs running, workflow in progress) does not belong here —
// these are the gaps where the user is idle and a command would help.
func (m *model) contextualTip() string {
	if !m.hasInferenceAgent {
		return `no primary agent configured — run {/config init} to set one up`
	}
	if !m.agents.localAvail && !m.agents.escalationAvail {
		return `no agents reachable — {/agent} lists backends, {/agent switch <name>} changes one`
	}
	if m.pendingUpdate != nil {
		return fmt.Sprintf(`milk %s is ready — run {/update install}`, m.pendingUpdate.Tag)
	}
	return ""
}

// currentTip is the tip the hint line shows right now: the contextual hint
// when one applies and the user is not actively browsing, else the
// rotation's current entry. A manual browse (tipPinned) outranks the
// contextual hint until the next automatic advance — stepping through tips
// must never look like a no-op while an urgent hint is up.
func (m *model) currentTip() string {
	if t := m.contextualTip(); t != "" && !m.tipPinned {
		return t
	}
	list := buildTipList()
	if len(list) == 0 {
		return ""
	}
	return list[m.tipIdx%len(list)]
}

// restartTipsTick restarts the rotation's cycling timer: it bumps tipsGen
// (invalidating any tick already in flight) and arms a fresh tick a full
// tipRotateInterval out. Called on every manual browse, so stepping through
// tips resets the countdown instead of being cut short by the old schedule.
func (m *model) restartTipsTick() tea.Cmd {
	m.tipsGen++
	return tipsTick(m.tipsGen)
}

// tipSeek moves the rotation by delta positions (-1 previous, +1 next),
// wrapping at both ends, marks the move as a manual browse (pinned), and
// restarts the cycling timer. Used by the Alt+Up/Down shortcuts and
// /tips prev|next; the returned command must reach tea.Program.
func (m *model) tipSeek(delta int) tea.Cmd {
	list := buildTipList()
	if len(list) == 0 {
		return nil
	}
	m.tipIdx = ((m.tipIdx+delta)%len(list) + len(list)) % len(list)
	m.tipPinned = true
	return m.restartTipsTick()
}

// tipPick jumps the rotation to the 1-based tip number n, pinning the pick
// and restarting the cycling timer. ok is false when n is outside the pool
// (/tips pick <n> renders the usage back) — nothing moves, no timer reset.
func (m *model) tipPick(n int) (ok bool, cmd tea.Cmd) {
	list := buildTipList()
	if n < 1 || n > len(list) {
		return false, nil
	}
	m.tipIdx = n - 1
	m.tipPinned = true
	return true, m.restartTipsTick()
}

// tipPos is the current tip's 1-based position in the pool and the pool
// size — what the /tips listings and pick echo show.
func (m *model) tipPos() (pos, total int) {
	list := buildTipList()
	if len(list) == 0 {
		return 0, 0
	}
	return m.tipIdx%len(list) + 1, len(list)
}

// searchTips returns the indices of the built tips whose text contains
// query (case-insensitive). Matching runs over the raw tip markup, so a
// query can hit highlighted spans ("{@}", "{Ctrl+R}") as well as prose.
func searchTips(query string) []int {
	q := strings.ToLower(strings.TrimSpace(query))
	var hits []int
	for i, tip := range buildTipList() {
		if strings.Contains(strings.ToLower(tip), q) {
			hits = append(hits, i)
		}
	}
	return hits
}

// tipListEntry renders one numbered entry for the transcript listings
// (/tips pick catalog, /tips search results): the 1-based number, a gold
// "*" on the entry the tip line currently shows, and the styled tip text.
func tipListEntry(i int, tip string, current bool) string {
	mark := "  "
	if current {
		mark = boldGold("* ")
	}
	return fmt.Sprintf("%s%3d. %s\n", mark, i+1, renderTipText(tip))
}

// tipEcho prints the current tip to the transcript with its position — the
// confirmation for a deliberate pick (full width, no truncation) and the
// feedback when a browse move has no visible tip line to change.
func (m *model) tipEcho() {
	pos, total := m.tipPos()
	if total == 0 {
		return
	}
	list := buildTipList()
	m.appendTranscript(fmt.Sprintf("%s tip %d/%d: %s\n", milkTag(), pos, total, renderTipText(list[pos-1])))
}

// tipMatchHeader renders the listing header for /tips search and the
// multi-match /tips pick <text> case.
func tipMatchHeader(n int, query string) string {
	noun := "tips match"
	if n == 1 {
		noun = "tip matches"
	}
	return fmt.Sprintf("%s %d %s %q — /tips pick <n> shows one\n", milkTag(), n, noun, query)
}

// tipBrowseEcho is the feedback for a manual browse move (Alt+Up/Down,
// /tips next|prev). While the tip line is visible the changed line is the
// feedback and the transcript stays quiet; when the line is hidden the tip
// is printed to the transcript instead, so a browse is never a silent
// no-op.
func (m *model) tipBrowseEcho() {
	if m.showTips {
		return
	}
	m.tipEcho()
}

// tipsTick schedules the next rotation advance, tagged with the tipsGen it
// was scheduled under. Re-armed on every tipsTickMsg of the current
// generation (and restarted by restartTipsTick on manual browsing), so the
// rotation survives any amount of busy time — it just freezes (see the
// tipsTickMsg case in updateInner) until the user is idle again.
func tipsTick(gen int) tea.Cmd {
	return tea.Tick(tipRotateInterval, func(time.Time) tea.Msg {
		return tipsTickMsg{gen: gen}
	})
}
