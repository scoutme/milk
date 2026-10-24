package main

// The wizard's "look before asking" helpers: numbered shortcuts for the URL
// and model questions, and a short probe of a local server so the model
// question can offer what is actually installed instead of a blank to fill.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// initWizardProbeTimeout keeps the probe from stalling the TUI: a local server
// answers (or refuses) at once; only a black-holed host would ever reach it.
const initWizardProbeTimeout = 1500 * time.Millisecond

// initWizardMaxModelChoices caps the numbered model list (single digits stay
// typeable).
const initWizardMaxModelChoices = 9

// initWizardListModelsFn lists the models a server reports — a var so tests
// don't depend on a real server.
var initWizardListModelsFn = initWizardListModels

// initWizardPickURL resolves a URL answer: blank is the provider's default,
// a number picks the matching preset, anything else is taken as typed.
func initWizardPickURL(provider, answer string) string {
	presets := initWizardURLPresets(provider)
	if answer == "" {
		return initWizardURLDefault(provider)
	}
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(presets) {
		return presets[n-1].URL
	}
	return answer
}

// initWizardPickModel resolves a model answer against the discovered list:
// blank is the first model, a number picks that one, anything else is typed.
func initWizardPickModel(st *initWizardState, answer string) string {
	if len(st.modelChoices) == 0 {
		if answer == "" {
			return st.modelDefault
		}
		return answer
	}
	if answer == "" {
		return st.modelChoices[0]
	}
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(st.modelChoices) {
		return st.modelChoices[n-1]
	}
	return answer
}

// initWizardProbe checks the chosen local server and remembers the models it
// reports. It returns a one-line verdict for the transcript ("" when there is
// nothing to probe). A server that is not up yet is not an error: the wizard
// goes on, and the smoke check reports it again at the end.
func initWizardProbe(st *initWizardState) string {
	st.modelChoices = nil
	if st.primary.Provider != "local" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), initWizardProbeTimeout)
	defer cancel()
	models, err := initWizardListModelsFn(ctx, st.primary.URL)
	if err != nil {
		return yellow("  ! nothing answered at "+st.primary.URL) + dim(" — start the server first, or carry on and fix it later") + "\n"
	}
	if len(models) > initWizardMaxModelChoices {
		models = models[:initWizardMaxModelChoices]
	}
	st.modelChoices = models
	return green("  ✓ reached it") + dim(fmt.Sprintf(" — %d model(s) available", len(models))) + "\n"
}

// initWizardListModels asks an OpenAI-compatible server which models it has.
func initWizardListModels(ctx context.Context, baseURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models list: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range body.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
