package studio

import (
	"errors"
	"github.com/rhaqim/demo/internal/platform"
	"net/http"
	"regexp"
	"strings"
)

// Model is a model a visitor can run with their own key.
type Model struct {
	Provider string  `json:"provider"`
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	InPer1M  float64 `json:"input_per_1m,omitempty"`  // USD; 0 = unpriced (shown as unknown)
	OutPer1M float64 `json:"output_per_1m,omitempty"` // USD
}

func (m Model) slug() string { return m.Provider + ":" + m.ID }

// defaultModels are first-party list prices for Anthropic. OpenAI models are
// left unpriced rather than guessed: Studio shows their cost as unknown.
func defaultModels() []Model {
	return []Model{
		{Provider: "anthropic", ID: "claude-opus-5-5", Label: "Claude Opus 5.5", InPer1M: 4, OutPer1M: 20},
		{Provider: "anthropic", ID: "claude-sonnet-5-5", Label: "Claude Sonnet 5.5", InPer1M: 2, OutPer1M: 10},
		{Provider: "anthropic", ID: "claude-haiku-5-5", Label: "Claude Haiku 5.5", InPer1M: 0.10, OutPer1M: 0.50},
		{Provider: "openai", ID: "gpt-5", Label: "GPT-5"},
		{Provider: "openai", ID: "gpt-5-mini", Label: "GPT-5 mini"},
	}
}

func (s *Server) listModels(w http.ResponseWriter, _ *http.Request) {
	platform.JSON(w, http.StatusOK, s.models)
}

// runner is what one run needs: which generator, and the overrides carrying
// the visitor's key. The key lives only in this value, for one request.
type runner struct {
	generator string
	overrides map[string]any
	model     Model
	simulated bool
}

var keyShape = regexp.MustCompile(`^[A-Za-z0-9_\-]{20,300}$`)

// runnerFor reads the visitor's model and key from the request headers
// (X-LLM-Model, X-LLM-Key). With no key the run is simulated.
func (s *Server) runnerFor(r *http.Request) (runner, error) {
	key := strings.TrimSpace(r.Header.Get("X-LLM-Key"))
	if key == "" {
		return runner{generator: simulateSlug, simulated: true}, nil
	}
	m, ok := s.byID[r.Header.Get("X-LLM-Model")]
	if !ok {
		return runner{}, errors.New("pick a model from the list")
	}
	if !keyShape.MatchString(key) {
		return runner{}, errors.New("that doesn't look like an API key")
	}
	if m.Provider == "anthropic" && !strings.HasPrefix(key, "sk-ant-") {
		return runner{}, errors.New("Anthropic keys start with sk-ant-")
	}
	return runner{generator: m.slug(), overrides: map[string]any{"api_key": key}, model: m}, nil
}

// verifyModel checks the visitor's key against the provider's models endpoint
// (no tokens are spent) and returns the model's details.
func (s *Server) verifyModel(w http.ResponseWriter, r *http.Request) {
	run, err := s.runnerFor(r)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if run.simulated {
		platform.Error(w, http.StatusBadRequest, "Add your API key to check it")
		return
	}
	info, err := s.e.VerifyGenerator(r.Context(), run.generator, run.overrides)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, "The provider rejected that key or model")
		return
	}
	platform.JSON(w, http.StatusOK, map[string]any{"model": run.model, "info": info})
}
