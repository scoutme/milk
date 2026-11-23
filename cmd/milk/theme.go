package main

import (
	"fmt"
	"image/color"
	"math"
	"os"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/scoutme/milk/internal/sgr"
	"github.com/scoutme/milk/internal/theme"
)

// Switchable TUI themes are registry data (internal/theme): the builtins
// "off" and "supermilk" plus drop-in JSON themes from ~/.milk/themes/*.json
// and <project>/.milk/themes/*.json. This file owns the *runtime* shape only
// — themeDef and the tokens the render call sites read; the schema,
// extends-resolution and derivations live in internal/theme (plan:
// docs/internal/plans/config-themes-carousel-wizard-plan.md). A skin
// restyles only the chrome (header, status bar, turn labels, panel titles,
// scrollbars), paints the whole screen and, on exit, hands the terminal's
// own colours back. Panel *layout* is theme-independent since ADR-0098 —
// all themes share the same free/grouped width budget.

type themeColor struct{ dark, light string }

// fixed makes a token that reads the same on any terminal background.
func fixed(hex string) themeColor { return themeColor{hex, hex} }

// adaptive is the lipgloss colour for style-based rendering.
func (c themeColor) adaptive() color.Color {
	return adaptiveColor(c.light, c.dark)
}

// darkBackground tracks the terminal background. It defaults to dark and is
// corrected when the terminal answers tea.RequestBackgroundColor.
var darkBackground = true

func hasDarkBackground() bool { return darkBackground }

// setDarkBackground records a detected background and reports whether it
// changed, so callers rebuild the colors only when needed.
func setDarkBackground(isDark bool) bool {
	if darkBackground == isDark {
		return false
	}
	darkBackground = isDark
	return true
}

// refreshThemeColors re-resolves everything derived from the background.
func refreshThemeColors() {
	resolveChrome()
	applyThinkSurface()
	pulseColors = buildPulse()
	rebuildGlamour()
	rebuildStyles()
}

func init() { rebuildStyles() }

// adaptiveColor picks the light or dark hex by the terminal background.
func adaptiveColor(light, dark string) color.Color {
	return lipgloss.LightDark(hasDarkBackground())(lipgloss.Color(light), lipgloss.Color(dark))
}

func (c themeColor) pick() string {
	if hasDarkBackground() {
		return c.dark
	}
	return c.light
}

func hexRGB(h string) (r, g, b int) {
	if len(h) < 7 { // empty or partial — absent tokens must never panic the render path
		return 0, 0, 0
	}
	v, _ := strconv.ParseUint(h[1:], 16, 32)
	return int(v >> 16), int(v >> 8 & 0xff), int(v & 0xff)
}

// sgrBg / sgrFg are raw truecolor escapes, "" when not a TTY.
func (c themeColor) sgrBg() string {
	h := c.pick()
	if !isTTY || h == "" {
		return ""
	}
	r, g, b := hexRGB(h)
	return fmt.Sprintf("\033[48;2;%d;%d;%dm", r, g, b)
}

func (c themeColor) sgrFg() string {
	h := c.pick()
	if !isTTY || h == "" {
		return ""
	}
	r, g, b := hexRGB(h)
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

type themeDef struct {
	name string
	// skin is the resolved `look: skin` shape (the render call sites'
	// shortcut; `look: classic` is the original shape). The schema's two
	// per-theme cosmetic knobs below reach their own call sites — everything
	// else that used to hang off "modern" is the look itself.
	skin                                                                              bool
	toolRows                                                                          theme.ToolRows
	scrollbar                                                                         theme.Scrollbar
	base, raised, toast, hairline, thumb, text, muted, accent, accentLo, think, focus themeColor
	// chrome is the classic surface vocabulary (the schema's chrome block);
	// the classic call sites read it via the chrome* vars below.
	chrome theme.Chrome
	// terminal colours handed to the emulator while the theme is active
	tagline                    string // "" = defaultTagline
	termBg, termFg, termCursor string
	termAnsi                   [16]string // "" = leave the terminal's own palette
}

// defaultTagline is the line under the wordmark; a theme may carry its own.
const defaultTagline = "switch models, not context."

// taglineText is the active theme's tagline.
func taglineText() string {
	if th.tagline != "" {
		return th.tagline
	}
	return defaultTagline
}

// themeOff is the initial theme (and the /theme fallback): the original
// look. Its colours stay in code by design on the classic path (accentStyle,
// the alt-background fallbacks) — focus is the one tint the theme declares
// as a token, so panel render code never carries escapes for it either
// (issue #412). Both builtins resolve from internal/theme's data; the
// pre-registry literals survive as goldens in theme_parity_test.go.
var themeOff = mustThemeDef("off")

// th is the active theme; tk* mirror its tokens for call sites.
var (
	th                                                                        = themeOff
	tkBase, tkRaised, tkToast, tkHairline, tkThumb, tkText, tkMuted, tkAccent themeColor
	tkAccentLo, tkThink, tkFocus                                              themeColor
)

// themeChanged marks the display stale after a theme (re)activation — the
// switch tail every live path shares (/theme <name>, carousel confirm, the
// builder's previews and save, cancel-restore). The per-turn colorized
// transcript caches hold the PREVIOUS theme's colours keyed by raw text
// alone: without dropping them here, a rebuild re-serves the old skin's
// rows and only the chrome visibly changes (the missing live preview).
func (m *model) themeChanged() {
	m.colorizeForce = true
	m.turnColorCache = nil
	m.turnRawCache = nil
	m.wrapCacheValid = false
	m.viewportDirty = true
}

// applyThemeDef activates an already-resolved theme definition and rebuilds
// every style derived from it — setTheme's tail. The theme builder previews
// through here before its candidate is in any registry.
func applyThemeDef(t *themeDef) {
	th = t
	tkBase, tkRaised, tkToast, tkHairline, tkThumb = t.base, t.raised, t.toast, t.hairline, t.thumb
	tkText, tkMuted, tkAccent, tkAccentLo, tkThink, tkFocus = t.text, t.muted, t.accent, t.accentLo, t.think, t.focus
	refreshThemeColors()
}

// setTheme activates a theme by name — registry lookup, so builtins and
// drop-in JSON themes (~/.milk/themes, .milk/themes) alike — and rebuilds
// every style derived from it. Unknown names are rejected.
func setTheme(name string) bool {
	t, ok := themeDefFor(name)
	if !ok {
		return false
	}
	applyThemeDef(t)
	return true
}

// Classic chrome of the original look — the fallbacks a palette-less classic
// theme resolves to. off.json now declares these exact values as its palette
// (themes-as-data: the classic path speaks the same token vocabulary as the
// skin path), so a variant of off moves its chrome via Transform.
var (
	classicHeaderBg     = themeColor{dark: "#1b1b1f", light: "#e9e6df"}
	classicHeaderFg     = themeColor{dark: "#b9b4a8", light: "#4a463e"}
	classicStatusBg     = themeColor{dark: "#25252a", light: "#e5e3dd"}
	classicHeaderBorder = themeColor{dark: "#3a3a41", light: "#c4c0b6"}
	classicAccent       = themeColor{dark: "#ffd03c", light: "#ffd03c"}
	classicToast        = themeColor{dark: "#1a263a", light: "#d9e4f5"}
)

// Classic fallbacks for the schema's chrome block — the same values the
// render layer carried as constants. A classic theme that declares a chrome
// surface gets its own value instead (token-if-present); one that declares
// none renders exactly as before.
var (
	classicPanelTitle     = themeColor{dark: "#888888", light: "#555555"}
	classicPanelSection   = themeColor{dark: "#666666", light: "#777777"}
	classicBorder         = themeColor{dark: "#555555", light: "#aaaaaa"}
	classicPanelAlt       = themeColor{dark: "#18181c", light: "#ececef"}
	classicThinkSurface   = themeColor{dark: "#262626", light: "#262626"}
	classicPermFg         = themeColor{dark: "#1a1a00", light: "#1a1a00"}
	classicPermBg         = themeColor{dark: "#b8860b", light: "#ffd700"}
	classicWordmark       = themeColor{dark: "#ffd060", light: "#ffd060"}
	classicAgentSage      = themeColor{dark: "#a8b58f", light: "#5f6b3a"}
	classicAgentSteel     = themeColor{dark: "#93a9c7", light: "#3f5a80"}
	classicStaleFresh     = themeColor{dark: "#dcdad7", light: "#dcdad7"}
	classicStaleMid       = themeColor{dark: "#c38237", light: "#c38237"}
	classicStaleWarm      = themeColor{dark: "#af4128", light: "#af4128"}
	classicChangeCreated  = themeColor{dark: "#6fbf82", light: "#2f7d32"}
	classicChangeModified = themeColor{dark: "#d0a93c", light: "#8a6a00"}
	classicChangeDeleted  = themeColor{dark: "#e07b70", light: "#a5372b"}
	classicChangeRenamed  = themeColor{dark: "#8ab0e8", light: "#3a5fa8"}
	classicSelection      = themeColor{dark: "#585858", light: "#585858"}
	classicCompletion     = themeColor{dark: "#444444", light: "#444444"}
)

// chrome* are the active classic chrome colors: the theme's tokens where it
// declares them (token-if-present), the classic constants otherwise. Resolved
// once per theme activation in resolveChrome(), so the call sites below keep
// reading the same names. Slots that stay code (documented gaps — the pulse
// ramp, staleness ramp, changeset colors, softSage/softSteel tags, think
// surface, status-perm gold) are not in this map on purpose: one palette
// slot cannot serve two distinct classic roles (off's panel titles are grey
// while its accent is gold).
var (
	chromeHeaderBg       = classicHeaderBg
	chromeHeaderFg       = classicHeaderFg
	chromeStatusBg       = classicStatusBg
	chromeHeaderBorder   = classicHeaderBorder
	chromeAccent         = classicAccent
	chromeToast          = classicToast
	chromePanelTitle     = classicPanelTitle
	chromePanelSection   = classicPanelSection
	chromeBorder         = classicBorder
	chromePanelAlt       = classicPanelAlt
	chromeThinkSurface   = classicThinkSurface
	chromePermFg         = classicPermFg
	chromePermBg         = classicPermBg
	chromeWordmark       = classicWordmark
	chromeAgentSage      = classicAgentSage
	chromeAgentSteel     = classicAgentSteel
	chromeStaleFresh     = classicStaleFresh
	chromeStaleMid       = classicStaleMid
	chromeStaleWarm      = classicStaleWarm
	chromeChangeCreated  = classicChangeCreated
	chromeChangeModified = classicChangeModified
	chromeChangeDeleted  = classicChangeDeleted
	chromeChangeRenamed  = classicChangeRenamed
	chromeSelection      = classicSelection
	chromeCompletion     = classicCompletion
	chromePulse          = originalPulse // [16]string — the theme's chrome.pulse when declared
)

// present reports whether a themeColor carries any variant at all.
func (c themeColor) present() bool { return c.dark != "" || c.light != "" }

// rawBgCode is a ground-picked background escape with no isTTY gate — for
// the input/completion surfaces the original painted with fixed 256-colour
// constants (the cube greys are exact hexes, so the truecolor form is
// visually identical).
func rawBgCode(c themeColor) string {
	r, g, b := hexRGB(c.pick())
	return fmt.Sprintf("\033[48;2;%d;%d;%dm", r, g, b)
}

// resolveChrome points the classic chrome at the active theme's tokens and
// chrome block, falling back to the classic constants for whatever it
// declares nothing about. Run once per theme activation (refreshThemeColors)
// so the call sites keep reading the same names.
func resolveChrome() {
	apply := func(dst *themeColor, src, fb themeColor) {
		if !src.present() {
			*dst = fb
			return
		}
		*dst = src
	}
	applyC := func(dst *themeColor, src theme.Color, fb themeColor) {
		apply(dst, themeColorOf(src), fb)
	}
	c := th.chrome
	apply(&chromeHeaderBg, tkBase, classicHeaderBg)
	// header_bg: a chrome surface that may differ from base (off's bar is
	// #1b1b1f over its standard black base). Fallback: whatever base settled.
	applyC(&chromeHeaderBg, c.HeaderBg, chromeHeaderBg)
	apply(&chromeHeaderFg, tkMuted, classicHeaderFg)
	apply(&chromeStatusBg, tkRaised, classicStatusBg)
	apply(&chromeHeaderBorder, tkHairline, classicHeaderBorder)
	apply(&chromeAccent, tkAccent, classicAccent)
	apply(&chromeToast, tkToast, classicToast)
	applyC(&chromePanelTitle, c.PanelTitle, classicPanelTitle)
	applyC(&chromePanelSection, c.PanelSection, classicPanelSection)
	applyC(&chromeBorder, c.Border, classicBorder)
	applyC(&chromePanelAlt, c.PanelAlt, classicPanelAlt)
	applyC(&chromeThinkSurface, c.ThinkSurface, classicThinkSurface)
	applyC(&chromePermFg, c.PermFg, classicPermFg)
	applyC(&chromePermBg, c.PermBg, classicPermBg)
	applyC(&chromeWordmark, c.Wordmark, classicWordmark)
	applyC(&chromeAgentSage, c.AgentSage, classicAgentSage)
	applyC(&chromeAgentSteel, c.AgentSteel, classicAgentSteel)
	applyC(&chromeStaleFresh, c.StaleFresh, classicStaleFresh)
	applyC(&chromeStaleMid, c.StaleMid, classicStaleMid)
	applyC(&chromeStaleWarm, c.StaleWarm, classicStaleWarm)
	applyC(&chromeChangeCreated, c.ChangeCreated, classicChangeCreated)
	applyC(&chromeChangeModified, c.ChangeModified, classicChangeModified)
	applyC(&chromeChangeDeleted, c.ChangeDeleted, classicChangeDeleted)
	applyC(&chromeChangeRenamed, c.ChangeRenamed, classicChangeRenamed)
	applyC(&chromeSelection, c.Selection, classicSelection)
	applyC(&chromeCompletion, c.Completion, classicCompletion)
	chromePulse = originalPulse
	if len(c.Pulse) == len(chromePulse) {
		// Declared as hex; the spinner ramp is SGR foregrounds.
		for i, hex := range c.Pulse {
			r, g, b := hexRGB(hex)
			chromePulse[i] = fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
		}
	}
	inputSelectionOn = rawBgCode(chromeSelection)
}

// tintChrome keeps a chrome bar's own colors behind its styled spans. Spans
// close themselves with a full reset, which would drop the bar back to the
// terminal's colors midway (a navy bar on a dark terminal showing navy only at
// its ends). The skin ("modern") paints its own ground, so it is left alone.
func tintChrome(bar string, colors ...string) string {
	if th.skin || !isTTY {
		return bar
	}
	return sgr.WithBackground(bar, strings.Join(colors, ""))
}

// rebuildStyles reassigns the lipgloss styles owned by the chrome. For "off"
// these are the original definitions.
func rebuildStyles() {
	styleStatusBarPerm = lipgloss.NewStyle().
		Foreground(chromePermFg.adaptive()).
		Background(chromePermBg.adaptive())
	if !th.skin {
		styleHeaderBar = lipgloss.NewStyle().
			Background(chromeHeaderBg.adaptive()).
			Foreground(chromeHeaderFg.adaptive()).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(chromeHeaderBorder.adaptive())
		styleStatusBar = lipgloss.NewStyle().
			Background(chromeStatusBg.adaptive())
		styleBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderTop(true).
			BorderForeground(chromeBorder.adaptive())
		stylePanelTitle = lipgloss.NewStyle().
			Foreground(chromePanelTitle.adaptive()).
			Bold(true)
		stylePanelSection = lipgloss.NewStyle().
			Foreground(chromePanelSection.adaptive())
		// Changeset (issue #331): the chrome block's roles.
		styleChangeCreated = lipgloss.NewStyle().Foreground(chromeChangeCreated.adaptive())
		styleChangeModified = lipgloss.NewStyle().Foreground(chromeChangeModified.adaptive())
		styleChangeDeleted = lipgloss.NewStyle().Foreground(chromeChangeDeleted.adaptive())
		styleChangeRenamed = lipgloss.NewStyle().Foreground(chromeChangeRenamed.adaptive())
		return
	}
	styleHeaderBar = lipgloss.NewStyle().
		Background(tkBase.adaptive()).
		Foreground(tkMuted.adaptive()).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(tkHairline.adaptive())
	styleStatusBar = lipgloss.NewStyle().
		Background(tkRaised.adaptive()).
		Foreground(tkMuted.adaptive())
	styleBorder = lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderForeground(tkHairline.adaptive())
	stylePanelTitle = lipgloss.NewStyle().Foreground(tkAccent.adaptive()).Bold(true)
	stylePanelSection = lipgloss.NewStyle().Foreground(tkMuted.adaptive()).Bold(true)
	// Changeset (issue #331): the skin's own ANSI ramp keeps both skins' palettes.
	styleChangeCreated = lipgloss.NewStyle().Foreground(lipgloss.Color(th.termAnsi[2]))
	styleChangeModified = lipgloss.NewStyle().Foreground(lipgloss.Color(th.termAnsi[3]))
	styleChangeDeleted = lipgloss.NewStyle().Foreground(lipgloss.Color(th.termAnsi[1]))
	styleChangeRenamed = lipgloss.NewStyle().Foreground(lipgloss.Color(th.termAnsi[4]))
}

// originalPulse is the yellow breathing ramp of the original look.
var originalPulse = [16]string{
	"\033[38;2;60;50;0m", "\033[38;2;67;58;1m", "\033[38;2;81;73;4m", "\033[38;2;110;101;9m",
	"\033[38;2;157;135;15m", "\033[38;2;204;170;20m", "\033[38;2;232;197;25m", "\033[38;2;251;215;28m",
	"\033[38;2;255;220;30m", "\033[38;2;251;215;28m", "\033[38;2;232;197;25m", "\033[38;2;204;170;20m",
	"\033[38;2;157;135;15m", "\033[38;2;110;101;9m", "\033[38;2;81;73;4m", "\033[38;2;67;58;1m",
}

// buildPulse is the 16-step breathing gradient keyed to the spinner frame:
// the original yellow ramp, or accent-floor → accent → floor for a theme.
func buildPulse() [16]string {
	if !th.skin {
		return chromePulse
	}
	var out [16]string
	r0, g0, b0 := hexRGB(tkAccentLo.dark)
	r1, g1, b1 := hexRGB(tkAccent.dark)
	for i := range out {
		t := (1 - math.Cos(2*math.Pi*float64(i)/16)) / 2
		mix := func(a, b int) int { return a + int(math.Round(t*float64(b-a))) }
		out[i] = fmt.Sprintf("\033[38;2;%d;%d;%dm", mix(r0, r1), mix(g0, g1), mix(b0, b1))
	}
	return out
}

// accentStyle is the "gold" emphasis: the theme's accent (off declares the
// original gold), or the theme accent on a skin. The classic form is one
// combined SGR — the byte shape the original look always emitted.
func accentStyle() string {
	if !th.skin {
		r, g, b := hexRGB(chromeAccent.pick())
		return fmt.Sprintf("\033[1;38;2;%d;%d;%dm", r, g, b)
	}
	return ansiBold + tkAccent.sgrFg()
}

// wordmark is the startup-banner "milk".
func wordmark() string {
	if !th.skin {
		r, g, b := hexRGB(chromeWordmark.pick())
		return fmt.Sprintf("\033[1;38;2;%d;%d;%dm", r, g, b) + "milk\033[0m"
	}
	return boldGold("milk")
}

func panelAltBackgroundCode() string {
	if !isTTY {
		return ""
	}
	if th.skin {
		return tkRaised.sgrBg()
	}
	return chromePanelAlt.sgrBg()
}

// focusBgCode is the focused side panel's tint (issue #412): a slightly
// dark-blue background declared per theme (themeDef.focus) — render code
// carries no escapes, only the token. "" when not a TTY or the active theme
// declares no focus tint.
func focusBgCode() string {
	if !isTTY {
		return ""
	}
	if tkFocus.pick() == "" {
		return ""
	}
	return tkFocus.sgrBg()
}

func toastBackgroundCode() string {
	if !isTTY {
		return ""
	}
	if th.skin {
		return tkToast.sgrBg()
	}
	// The classic toast ground: the theme's toast token (off declares the
	// original #1A263A / #D9E4F5), or the classic constant.
	return chromeToast.sgrBg()
}

// Scrollbars: original dim glyphs, or a hairline track with a slim thumb —
// the schema's `scrollbar` knob (default: slim on a skin, classic on classic).
func scrollTrackGlyph() string {
	if th.scrollbar != theme.ScrollbarSlim {
		return dim("│")
	}
	return colorize("│", tkHairline.sgrFg())
}

func scrollThumbGlyph() string {
	if th.scrollbar != theme.ScrollbarSlim {
		return dim("▌")
	}
	return colorize("┃", tkThumb.sgrFg())
}

// Muted tags for the original look's agent labels (no raw ANSI green/blue)
// — the chrome block's agent_sage/agent_steel with the classic fallbacks.
var (
	softSage  = classicAgentSage
	softSteel = classicAgentSteel
)

// agentTag is the label before an agent's reply.
func agentTag(name, role string) string {
	if !th.skin {
		if role == "escalation" {
			return colorize(name+":", ansiBold+softSteel.sgrFg()) + " "
		}
		return colorize(name+":", ansiBold+softSage.sgrFg()) + " "
	}
	return "\n" + colorize(strings.ToUpper(name), ansiBold+tkAccent.sgrFg()) + dim(" · "+strings.ToUpper(role)) + softRule() + "\n"
}

// userEcho is the transcript echo of a submitted line.
func userEcho(label, input string) string {
	if !th.skin {
		return label + input + "\n"
	}
	return "\n" + colorize("YOU", ansiBold+tkAccent.sgrFg()) + softRule() + "\n" + input + "\n"
}

// osc writes one terminal control string.
func osc(format string, a ...any) {
	fmt.Fprintf(os.Stdout, "\x1b]"+format+"\x1b\\", a...)
}

// applyThemeTerminal pushes the theme's colours into the terminal emulator:
// default background/foreground/cursor and the 16-colour ANSI palette, so
// everything milk does not paint itself (edge columns, scrollbar gutter,
// ANSI-coloured text) matches. Returns the restore func. A no-op for "off".
func applyThemeTerminal() func() {
	if !isTTY || !th.skin {
		return func() {}
	}
	osc("11;%s", th.termBg)
	osc("10;%s", th.termFg)
	osc("12;%s", th.termCursor)
	for i, c := range th.termAnsi {
		if c != "" {
			osc("4;%d;%s", i, c)
		}
	}
	return restoreTerminalColors
}

// restoreTerminalColors hands the terminal's own palette back.
func restoreTerminalColors() {
	if !isTTY {
		return
	}
	osc("104")
	osc("110")
	osc("111")
	osc("112")
}

// dimCode is the "recede" style: faint, or the theme's muted ink — SGR faint
// washes out to near-invisible on a light ground.
func dimCode() string {
	if !th.skin {
		return ansiDim
	}
	return tkMuted.sgrFg()
}

// softRule is a short hairline that trails a turn label.
func softRule() string { return " " + colorize(strings.Repeat("─", 18), tkHairline.sgrFg()) }

const originalThinkBgParams = "48;5;235"

// applyThinkSurface points the thinking-surface background at the theme: the
// classic chrome's think_surface, or the raised surface of a skin (readable
// on light). The classic constant stays as the fallback for themes that
// declare no chrome.
func applyThinkSurface() {
	thinkBgParams = originalThinkBgParams
	if th.skin {
		if tkRaised.pick() != "" {
			r, g, b := hexRGB(tkRaised.pick())
			thinkBgParams = fmt.Sprintf("48;2;%d;%d;%d", r, g, b)
		}
	} else if !th.chrome.ThinkSurface.Absent() {
		r, g, b := hexRGB(chromeThinkSurface.pick())
		thinkBgParams = fmt.Sprintf("48;2;%d;%d;%d", r, g, b)
	}
	ansiThinkBg = "\033[" + thinkBgParams + "m"
}

// compactBlankLines drops empty lines so reasoning reads as one tight run.
func compactBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for i, l := range lines {
		if strings.TrimSpace(l) == "" && i != len(lines)-1 {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
