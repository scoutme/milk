package main

// ACP's form-driven /config init wizard. The step machine itself lives in
// initwizard_core.go (shared with the TUI); this file drives it through ACP
// elicitation form dialogs (elicitation/create, form mode) when the client
// advertised form support — one dialog per step, that step's prompt as the
// dialog message, the field's choices as a titled select/multi-select and
// its default pre-populated. That is the input surface the chat flow cannot
// offer: ACP clients generally won't send an empty prompt (so "accept the
// default" has nothing to click), and typing six answers blind is exactly
// the adoption friction this removes.
//
// Two deliberate fallbacks keep the wizard working everywhere:
//   - credentials never go through form elicitation (the spec forbids
//     secrets in form mode), so the API-key step — and the rest of the run
//     if the user prefers typing — continues in chat, where the words
//     "default" / "-" stand in for the empty answer (initWizardDefaultWord);
//   - a dismissed dialog or a failed round trip hands the current step back
//     to chat with every applied answer kept.
//
// The dialogs run inside the triggering turn (like session/request_permission
// prompts do), so /config init can finish a whole first-run setup in one
// command.

import (
	"context"
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/transport/acp"
)

// initWizardChatHint is appended to the chat wizard's first prompt: the
// stand-ins for the empty turn ACP chat can't send, and the escape hatch.
const initWizardChatHint = "[milk] (type 'default' to accept the bracketed default, 'cancel' to abort)\n"

// initWizardFormFallbackNote is emitted when a question hands back to chat:
// forms can't carry it (the step holds a credential), the user dismissed the
// dialog, or the dialog round trip failed.
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

// runInitForms drives st through elicitation form dialogs — one per step,
// that step's prompt as the dialog message — as far as it safely can, and
// returns the text to show plus whether the wizard finished. It hands the
// current step back to the chat flow (done=false, prompt appended) at the
// credential step and whenever the user dismisses a dialog or the round trip
// fails; every answer already applied is kept either way.
func (as *acpSession) runInitForms(ctx context.Context, st *initWizardState) (string, bool) {
	opts := as.initWizardOptions()
	var b strings.Builder
	for {
		if st.step == initStepDone || st.step == initStepOpenConfig {
			// No question left to ask (the editor step is TUI-only and
			// commits before prompting — see initWizardOpts.OfferOpenEditor).
			return b.String(), true
		}
		field := initWizardFieldFor(st, opts)
		if field.Secret {
			b.WriteString(initWizardSecretNote())
			b.WriteString(initWizardPrompt(st))
			return b.String(), false
		}
		res, err := as.host.host.Elicit(ctx, acp.ElicitationRequest{
			Message: stripANSI(initWizardPrompt(st)),
			Schema:  initWizardSchema(field),
		})
		if err != nil {
			fmt.Fprintf(&b, "%s form dialog failed (%v) — ", milkTag(), err)
			b.WriteString(initWizardFormFallbackNote)
			b.WriteString(initWizardPrompt(st))
			return b.String(), false
		}
		if res.Action != acp.ElicitationAccept {
			b.WriteString(initWizardFormFallbackNote)
			b.WriteString(initWizardPrompt(st))
			return b.String(), false
		}
		wr := initWizardApply(st, initWizardFieldValue(res.Content, field), opts)
		b.WriteString(wr.Output)
		b.WriteString(as.commitInitWizard(wr))
		if wr.Done {
			return b.String(), true
		}
		// wr.Prompt is not echoed as text: the next dialog's message *is*
		// the question (it gets appended only on the chat fallback).
	}
}

// initWizardSecretNote explains why the credential step dropped out of the
// form flow — the spec forbids secrets in form mode (see runInitForms' doc).
func initWizardSecretNote() string {
	return milkTag() + " credentials can't go through form dialogs — this step is asked in chat (the chat text is never sent to the model)\n" +
		initWizardFormFallbackNote
}

// initWizardSchema maps one wizard field onto ACP's ElicitationSchema: a
// flat single-field form (the elicitation spec allows primitive and select
// property kinds only). Selects carry titled choices — clients render them
// as clickable options — and every Default rides along so clients that
// support schema defaults pre-populate the field. A missing content key on
// accept therefore means "the untouched default", which initWizardFieldValue
// already maps to the blank answer initWizardApply treats as the default.
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
