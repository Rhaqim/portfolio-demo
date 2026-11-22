package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rhaqim/noewright"

	"github.com/rhaqim/demo/internal/platform"
)

// verifyKey checks a key with Noewright's VerifyGenerator before any tokens are
// spent: the adapter asks the provider's models endpoint, which is free,
// proves the key works and proves it can see the chosen model.
func (s *server) verifyKey(ctx context.Context, m model, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := s.e.VerifyGenerator(ctx, m.slug(), map[string]any{"api_key": key})
	switch {
	case err == nil:
		if info.DisplayName != "" {
			return info.DisplayName, nil
		}
		return m.Label, nil
	case errors.Is(err, noewright.ErrKeyRejected):
		return "", fmt.Errorf("%s rejected that API key.", providerName(m.Provider))
	case errors.Is(err, context.DeadlineExceeded):
		return "", fmt.Errorf("Couldn't reach %s to check the key. Try again.", providerName(m.Provider))
	}
	// Anything else (most often a 404) means this key can't use the model.
	return "", fmt.Errorf("Couldn't use %s with this key: %s", m.Label, redact(err.Error()))
}

func providerName(p string) string {
	if p == "openai" {
		return "OpenAI"
	}
	return "Anthropic"
}

// Typical token use of one AI-run round (rules + GM + referee + chronicler +
// suggester) at a table of three. Used only for the up-front estimate; the
// cost panel shows real figures from Noewright's ledger after each round.
const (
	estRoundIn  = 7000
	estRoundOut = 1800
)

// verifyModel answers the page's "check key & price" button.
func (s *server) verifyModel(w http.ResponseWriter, r *http.Request) {
	run, err := s.runnerFor(r)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if run.dry {
		platform.JSON(w, http.StatusOK, map[string]any{"dry_run": true})
		return
	}
	name, err := s.verifyKey(r.Context(), run.model, run.overrides["api_key"].(string))
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	m := run.model
	out := map[string]any{
		"verified": true, "provider": m.Provider, "model": m.ID, "label": m.Label, "display_name": name,
		"priced": m.In > 0, "input_per_1m": m.In, "output_per_1m": m.Out,
	}
	// Priced by Noewright with the same table it records costs with.
	if est, err := s.e.Cost().Estimate(r.Context(), noewright.EstimateRequest{Model: m.ID, InputTokens: estRoundIn, OutputTokens: estRoundOut}); err == nil && est.PricingConfigured {
		out["estimate_round_usd"] = est.USDHigh
		out["estimate_basis"] = fmt.Sprintf("about %dk input and %.1fk output tokens per round", estRoundIn/1000, float64(estRoundOut)/1000)
	}
	platform.JSON(w, http.StatusOK, out)
}
