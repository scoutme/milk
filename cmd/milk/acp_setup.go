package main

// /setup over ACP — Telegram remote-oversight status/enable/disable plus
// the interactive setup wizard. The wizard asks its steps over the client's
// best input surface, mirroring /config init's drive (acp_initwizard.go):
//
//   - the bot token is a credential, so it is asked as typed chat on every
//     client — form mode forbids secrets, and the wizard consumes the turn
//     (runTurn) instead of routing it to the model;
//   - the confirm step (send the bot a message, then say so) goes through
//     an elicitation form select on form-capable clients, else a clickable
//     choice prompt (session/request_permission buttons), else typed
//     'done' — the universal floor, so no empty turn is ever needed;
//   - nothing user-visible degrades into an error: a dismissed dialog or a
//     client whose permission responses carry no recognizable outcome falls
//     back to typed input (the latter remembered in as.noChoice, like the
//     init wizard), and 'cancel' — or any slash command — aborts.

import (
	"context"
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpSetup implements /setup over ACP — its only surface is Telegram
// oversight.
func acpSetup(as *acpSession, rest string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(rest)) {
	case "telegram":
		return as.startTelegramWizard(), ""
	case "telegram on":
		return as.telegramSetEnabled(true), ""
	case "telegram off":
		return as.telegramSetEnabled(false), ""
	case "telegram status":
		return telegramStatusText(as.oversightConfig()), ""
	}
	return milkTag() + " usage: /setup telegram | /setup telegram on | /setup telegram off | /setup telegram status", ""
}

// oversightConfig returns the freshest config to read or mutate for
// /setup telegram: the disk-first rule session/new uses (another session's
// wizard or an external edit may have rewritten the file since this session
// started), falling back to the session's own copy when the file can't be
// read.
func (as *acpSession) oversightConfig() config.Config {
	if as.srv != nil {
		return as.srv.currentConfig()
	}
	return as.st.cfg
}

// telegramSetEnabled flips the backend on/off: save, publish to every live
// session, and rebuild the process notifier (which pings and announces when
// enabling — newNotifier's startup message doubles as the test one).
func (as *acpSession) telegramSetEnabled(on bool) string {
	cfg := as.oversightConfig()
	if err := setTelegramEnabledCfg(&cfg, on); err != nil {
		return milkTag() + " " + err.Error()
	}
	as.applyOversight(cfg)
	if on {
		return milkTag() + " Telegram oversight enabled"
	}
	return milkTag() + " Telegram oversight disabled"
}

// --- the wizard ---

// startTelegramWizard starts /setup telegram: intro plus the token question,
// asked as typed chat (a credential — see the file comment).
func (as *acpSession) startTelegramWizard() string {
	as.pendingInit = nil // one wizard at a time (runTurn has already
	// cancelled it for this slash command; keep the invariant anyway)
	as.pendingTelegram = &telegramSetupState{step: telegramStepToken}
	return telegramWizardIntro() + telegramTokenQuestion()
}

// telegramWizardIntro is the TUI wizard's onboarding text, verbatim.
func telegramWizardIntro() string {
	return milkTag() + " Telegram setup\n\n" +
		"1. Message @BotFather on Telegram\n" +
		"2. Send /newbot and follow the prompts\n" +
		"3. BotFather gives you a token like 123456:ABC-DEF...\n\n"
}

// telegramTokenQuestion is asked as typed chat on every client: the turn is
// consumed by the wizard (runTurn), so the token never reaches the model,
// and 'cancel' is spelled out because ACP chat has no esc key.
func telegramTokenQuestion() string {
	return milkTag() + " Bot token (paste it here — the answer is consumed by the wizard, never sent to the model; 'cancel' aborts): "
}

// telegramWaitMsgQuestion is the confirm step's typed floor: message the
// bot, then say so. Sent whenever the nicer surfaces fall through.
func (as *acpSession) telegramWaitMsgQuestion() string {
	return fmt.Sprintf("%s send any message to @%s on Telegram, then type 'done' here ('cancel' aborts): ",
		milkTag(), as.pendingTelegram.botName)
}

// telegramWizardAnswer applies one typed answer to the pending step, then
// drives the confirm step over the client's dialogs as far as this turn
// allows. Returns the text to show and whether the wizard finished — the
// caller (runTurn) clears as.pendingTelegram.
func (as *acpSession) telegramWizardAnswer(ctx context.Context, answer string) (string, bool) {
	st := as.pendingTelegram
	if st == nil {
		return "", true
	}
	var b strings.Builder
	switch st.step {
	case telegramStepToken:
		token := strings.TrimSpace(answer)
		if initWizardCancelWord(token) {
			return milkTag() + " telegram setup cancelled", true
		}
		if token == "" {
			return milkTag() + " token is required\n" + telegramTokenQuestion(), false
		}
		fmt.Fprintf(&b, "%s validating token…\n", milkTag())
		botName, err := telegramGetMe(token)
		if err != nil {
			// TUI parity: a failed validation ends the wizard — rerun it.
			fmt.Fprintf(&b, "%s token validation failed: %v\n", milkTag(), err)
			return b.String(), true
		}
		st.token, st.botName = token, botName
		st.step = telegramStepWaitMsg
		fmt.Fprintf(&b, "%s bot validated: @%s\n", milkTag(), botName)
		more, finished := as.runTelegramDialogs(ctx)
		return b.String() + more, finished

	case telegramStepWaitMsg:
		if initWizardCancelWord(answer) {
			return milkTag() + " telegram setup cancelled", true
		}
		if !telegramDoneWord(answer) {
			return as.telegramWaitMsgQuestion(), false
		}
		return as.resolveAndCommit(&b)
	}
	return "", true
}

// resolveAndCommit looks up the chat ID from the bot's pending updates and
// saves + activates the config — the wizard's final step. On failure the
// wizard stays at the confirm step so the answer can be retried.
func (as *acpSession) resolveAndCommit(b *strings.Builder) (string, bool) {
	st := as.pendingTelegram
	chatID, err := telegramResolveChatID(st.token)
	if err != nil {
		fmt.Fprintf(b, "%s %v\n", milkTag(), err)
		b.WriteString(as.telegramWaitMsgQuestion())
		return b.String(), false
	}
	cfg := as.oversightConfig()
	if err := commitTelegramCfg(&cfg, st.token, chatID); err != nil {
		fmt.Fprintf(b, "%s %v\n", milkTag(), err)
		return b.String(), true // a config-write failure won't fix itself — rerun /setup telegram
	}
	as.applyOversight(cfg)
	if oversightRemote(as.notifier()) {
		fmt.Fprintf(b, "%s Telegram configured (chat_id: %d) — sending test message…\n", milkTag(), chatID)
		as.notifier().NotifyTurnStart(context.Background(), "milk", "setup", "Telegram oversight configured successfully ✓")
	} else {
		// Never claim a test message that was not sent: newNotifier
		// degrades to Noop when the backend doesn't answer its ping.
		fmt.Fprintf(b, "%s Telegram configured (chat_id: %d), but the backend failed to activate — check the token and chat (see milk's stderr)\n", milkTag(), chatID)
	}
	return b.String(), true
}

// runTelegramDialogs drives the confirm step inside the current turn over
// the client's best surface: an elicitation form select (form-capable
// clients), else a clickable choice prompt (session/request_permission —
// buttons every client renders), else the typed question for a later turn.
// Dismissed dialogs and unrecognized responses land on typed input with no
// blame; a client whose permission responses carry nothing recognizable is
// remembered in as.noChoice and never asked again (the init wizard's rule).
func (as *acpSession) runTelegramDialogs(ctx context.Context) (string, bool) {
	st := as.pendingTelegram
	if st == nil || st.step != telegramStepWaitMsg {
		return "", false
	}

	if as.formElicit {
		res, err := as.host.host.Elicit(ctx, acp.ElicitationRequest{
			Message: fmt.Sprintf("Telegram setup — send any message to @%s on Telegram, then confirm", st.botName),
			Schema: acp.ElicitationSchema{
				Type:  "object",
				Title: "Telegram setup",
				Properties: map[string]acp.ElicitationProperty{
					"confirm": acp.SelectProperty("Confirm",
						acp.EnumOption{Const: "done", Title: "I've sent a message to @" + st.botName},
						acp.EnumOption{Const: "cancel", Title: "cancel setup"}),
				},
				Required: []string{"confirm"},
			},
		})
		if err == nil && res.Action == acp.ElicitationAccept {
			answer, _ := res.Content["confirm"].(string)
			return as.telegramWizardAnswer(ctx, answer) // "done"/"cancel" are already wizard words
		}
		return as.telegramWaitMsgQuestion(), false // dialog failed or dismissed
	}

	if !as.noChoice {
		out, err := as.host.host.RequestPermission(ctx, acp.PermissionRequest{
			Title:       "Telegram setup",
			Description: fmt.Sprintf("Send any message to @%s on Telegram, then pick an option.", st.botName),
			Options: []acp.PermissionOption{
				{OptionID: "done", Name: "I've sent a message to @" + st.botName, Kind: acp.PermissionAllowOnce},
				{OptionID: "cancel", Name: "cancel setup", Kind: acp.PermissionRejectOnce},
			},
		})
		switch {
		case err != nil || out.Outcome == "":
			as.noChoice = true
		case out.Outcome != "selected":
			// dismissed — typed input, no blame
		case out.OptionID == "cancel":
			return milkTag() + " telegram setup cancelled", true
		case out.OptionID == "done":
			var b strings.Builder
			return as.resolveAndCommit(&b)
		}
	}
	return as.telegramWaitMsgQuestion(), false
}
