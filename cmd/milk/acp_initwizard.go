package main

// ACP's /config init wizard drive. The step machine itself lives in
// initwizard_core.go (shared with the TUI); this file feeds it answers over
// the richest input surface the client supports for each step:
//
//  1. elicitation form dialogs (elicitation/create, form mode) on
//     form-capable clients — the standard's structured input: titled
//     selects and pre-populated defaults;
//  2. clickable choice prompts (session/request_permission options) — the
//     one prompt surface every ACP client renders as buttons (it is how
//     tool approvals work): one click per choice or example, a "use
//     default" button standing in for the empty turn ACP chat can't send,
//     and "type my own value…" handing the step to typed input;
//  3. typed chat answers in later turns (as.pendingInit) — the universal
//     floor, every question carrying what 'default' means for it.
//
// Two rules cross all surfaces. Credentials never go through the form
// surface (the spec forbids secrets in form mode): the API-key step is
// skipped with one click or typed in chat, and chat text is never sent to
// the model. And nothing user-visible ever degrades into an error: a
// dismissed dialog, a dismissed prompt, or a client whose permission
// responses carry no recognizable outcome all fall back to typed input with
// every applied answer kept (the last case is remembered in
// acpSession.noChoice so that client is not asked again).
//
// The dialogs and prompts run inside the triggering turn (like tool
// permission prompts), so /config init can finish a whole first-run setup
// in one command on a client with forms or buttons.

import (
	"context"
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/transport/acp"
)

// initWizardFormFallbackNote is emitted when a question hands back to typed
// input after a dialog was shown: forms can't carry it (the step holds a
// credential), the user dismissed the dialog, or the round trip failed.
const initWizardFormFallbackNote = "[milk] continuing the setup wizard in chat — type your answer, 'default' to accept the bracketed default, or 'cancel' to abort\n"

// initWizardOptions is the ACP flavor of the wizard's host options: no
// editor question (the client *is* an editor — /config open names the path),
// and the configured agent names as the agent-tools step's choices.
func (as *acpSession) initWizardOptions() initWizardOpts {
	names := make([]string, 0, len(as.st.cfg.Agents))
	for _, a := range as.st.cfg.Agents {
		names = append(names, a.Name)
	}
	return initWizardOpts{
		NumAgents:       len(as.st.cfg.Agents),
		AgentNames:      names,
		OfferOpenEditor: false,
	}
}

// commitInitWizard applies the wizard's committed config to the live
// session — rebuilding its runners so the configured agent takes over
// without a restart. Returns a note when the reload failed; the save itself
// already succeeded and sessions created afterwards pick it up (session/new
// re-reads the config from disk). Returns "" when nothing was committed.
func (as *acpSession) commitInitWizard(res initWizardResult) string {
	if res.Committed == nil {
		return ""
	}
	if err := as.buildRunners(*res.Committed); err != nil {
		return "\n" + milkTag() + " config saved, but this session could not reload it (" +
			err.Error() + ") — new sessions will pick it up\n"
	}
	return ""
}

// initWizardFieldHint describes what the blank/'default' answer does for a
// field — attached to form fields so the client can show it next to the
// input (initWizardChatHintFor is the typed-input flavour).
func initWizardFieldHint(f initWizardField) string {
	switch {
	case f.Default != "":
		return "leave empty or type 'default' to accept '" + f.Default + "'"
	case f.Required:
		return "an answer is required"
	default:
		return "leave empty or type 'default' to skip"
	}
}

// initWizardChatHintFor is the typed-input flavour of the same hint: no
// "leave empty" (ACP chat can't send an empty turn), plus the escape hatch.
// Appended to every question asked as typed input — not just the first.
func initWizardChatHintFor(f initWizardField) string {
	what := "type 'default' to skip"
	if f.Default != "" {
		what = "type 'default' to accept '" + f.Default + "'"
	} else if f.Required {
		return milkTag() + " (an answer is required — or 'cancel' to abort)\n"
	}
	return milkTag() + " (" + what + ", or 'cancel' to abort)\n"
}

// initWizardQuestionText is one question as typed input: the step's prompt
// plus its per-question 'default' hint.
func initWizardQuestionText(st *initWizardState, f initWizardField) string {
	return initWizardPrompt(st) + initWizardChatHintFor(f)
}

// acpChoiceKind says where a step's answer came from over the choice prompt
// (or why there is none).
type acpChoiceKind int

const (
	acpChoicePicked acpChoiceKind = iota // the answer carries the clicked value
	acpChoiceChat                        // nothing clicked — hand the step to typed input
	acpChoiceAbort                       // the user cancelled the whole wizard
)

// askInitChoice offers a field's picks as clickable options over
// session/request_permission — rendered as buttons by every ACP client —
// and returns the clicked answer. "type my own value…", an explicit dismiss
// and unknown option IDs all yield acpChoiceChat (typed input, no blame);
// a client whose response carries no recognizable outcome at all (or errors)
// is remembered in as.noChoice and never asked again — see
// acp.PermissionOutcome.Outcome's "" case, whose doc anticipates exactly
// this caller.
func (as *acpSession) askInitChoice(ctx context.Context, st *initWizardState, f initWizardField) (string, acpChoiceKind) {
	picks := initWizardPicks(f)
	if as.noChoice || len(picks) <= 1 {
		return "", acpChoiceChat
	}
	opts := make([]acp.PermissionOption, 0, len(picks)+1)
	for i, p := range picks {
		opts = append(opts, acp.PermissionOption{
			OptionID: fmt.Sprintf("pick-%d", i),
			Name:     p.Label,
			Kind:     acp.PermissionAllowOnce,
		})
	}
	opts = append(opts, acp.PermissionOption{OptionID: "cancel", Name: "cancel setup", Kind: acp.PermissionRejectOnce})
	out, err := as.host.host.RequestPermission(ctx, acp.PermissionRequest{
		Title:       "milk setup — " + f.Label,
		Description: stripANSI(initWizardPrompt(st)),
		Options:     opts,
	})
	switch {
	case err != nil || out.Outcome == "":
		as.noChoice = true
		return "", acpChoiceChat
	case out.Outcome != "selected": // dismissed — typed input
		return "", acpChoiceChat
	case out.OptionID == "cancel":
		return "", acpChoiceAbort
	}
	for i, p := range picks {
		if out.OptionID == fmt.Sprintf("pick-%d", i) {
			if p.Custom {
				return "", acpChoiceChat
			}
			return p.Value, acpChoicePicked
		}
	}
	return "", acpChoiceChat
}

// runInitDialogs drives st as far as the client's input surfaces allow in
// this turn and returns the text to show plus whether the wizard finished.
// Per step: a form dialog (form-capable clients, non-secret steps), else a
// clickable choice prompt, else typed input — the question then comes back
// with its hint and the answer arrives in a later turn (see
// acpSession.runTurn). The next question is never echoed twice: whatever
// asks it (dialog message, prompt description, returned text) *is* the
// question. Every applied answer is kept on any fallback.
func (as *acpSession) runInitDialogs(ctx context.Context, st *initWizardState) (string, bool) {
	opts := as.initWizardOptions()
	var b strings.Builder
	for {
		if st.step == initStepDone || st.step == initStepOpenConfig {
			// No question left to ask (the editor step is TUI-only and
			// commits before prompting — see initWizardOpts.OfferOpenEditor).
			return b.String(), true
		}
		field := initWizardFieldFor(st, opts)
		var answer string
		if as.formElicit && !field.Secret {
			res, err := as.host.host.Elicit(ctx, acp.ElicitationRequest{
				Message: stripANSI(initWizardPrompt(st)),
				Schema:  initWizardSchema(field),
			})
			if err != nil {
				fmt.Fprintf(&b, "%s form dialog failed (%v) — ", milkTag(), err)
				b.WriteString(initWizardFormFallbackNote)
				b.WriteString(initWizardQuestionText(st, field))
				return b.String(), false
			}
			if res.Action != acp.ElicitationAccept {
				b.WriteString(initWizardFormFallbackNote)
				b.WriteString(initWizardQuestionText(st, field))
				return b.String(), false
			}
			answer = initWizardFieldValue(res.Content, field)
		} else {
			pick, kind := as.askInitChoice(ctx, st, field)
			switch kind {
			case acpChoiceAbort:
				b.WriteString(milkTag() + " setup wizard cancelled — restart with /config init\n")
				return b.String(), true
			case acpChoiceChat:
				if field.Secret && as.formElicit {
					b.WriteString(initWizardSecretNote())
				}
				b.WriteString(initWizardQuestionText(st, field))
				return b.String(), false
			default:
				answer = pick
			}
		}
		wr := initWizardApply(st, answer, opts)
		b.WriteString(wr.Output)
		b.WriteString(as.commitInitWizard(wr))
		if wr.Done {
			return b.String(), true
		}
	}
}

// initWizardSecretNote explains why the credential step dropped to typed
// input on a form-capable client — the spec forbids secrets in form mode
// (see runInitDialogs' doc).
func initWizardSecretNote() string {
	return milkTag() + " credentials can't go through form dialogs — this step is asked in chat (the chat text is never sent to the model)\n" +
		initWizardFormFallbackNote
}

// initWizardSchema maps one wizard field onto ACP's ElicitationSchema: a
// flat single-field form (the elicitation spec allows primitive and select
// property kinds only). Selects carry titled choices — clients render them
// as clickable options — and every Default rides along so clients that
// support schema defaults pre-populate the field; the property description
// spells out the 'default' word for clients that pre-populate nothing. A
// missing content key on accept therefore means "the untouched default",
// which initWizardFieldValue already maps to the blank answer
// initWizardApply treats as the default.
func initWizardSchema(f initWizardField) acp.ElicitationSchema {
	var prop acp.ElicitationProperty
	switch f.Kind {
	case "select":
		opts := make([]acp.EnumOption, 0, len(f.Options))
		for _, o := range f.Options {
			opts = append(opts, acp.EnumOption{Const: o.Value, Title: o.Label})
		}
		prop = acp.SelectProperty(f.Label, opts...)
		prop.Default = f.Default
	case "multiselect":
		vals := make([]string, 0, len(f.Options))
		for _, o := range f.Options {
			vals = append(vals, o.Value)
		}
		prop = acp.MultiSelectProperty(f.Label, acp.StringMultiSelect(vals))
	default:
		prop = acp.ElicitationProperty{Type: "string", Title: f.Label, Default: f.Default}
	}
	prop.Description = initWizardFieldHint(f)
	s := acp.ElicitationSchema{
		Type:       "object",
		Title:      f.Label,
		Properties: map[string]acp.ElicitationProperty{f.Name: prop},
	}
	if f.Required {
		s.Required = []string{f.Name}
	}
	return s
}
