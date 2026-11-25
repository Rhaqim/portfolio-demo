// Command loom is the loom demo. Visitors bring their own LLM API key and run
// multi-agent apps on a real Noewright engine (the Forecast Desk and sermon
// captions are described in desk.go and captions.go):
//
//   - Tabletop: a solo tabletop RPG. A rules agent decides whether an action
//     needs a dice check, Go rolls the dice, then one Noewright turn runs a
//     streaming game-master lead with three followers (referee, chronicler,
//     suggester) in parallel. Hooks fold the referee's verdict into typed
//     session state, so any turn can be rewound with a session fork.
//   - Pipeline: a visitor-editable agent graph (an incident postmortem by
//     default). Noewright validates the wiring, plans it into dependency layers
//     and runs each layer concurrently.
//
// Keys travel per request in a header, are handed to the generator through
// StepRequest.Overrides, and are never written anywhere: Noewright persists only
// the rendered prompts, never Overrides.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/generator/anthropic"
	"github.com/rhaqim/noewright/generator/openai"
	"github.com/rhaqim/noewright/schema"

	"github.com/rhaqim/demo/internal/platform"
)

// model is one selectable provider/model pair. Each gets its own registered
// generator, so the model recorded on results (and priced) is the one used.
type model struct {
	Provider string  `json:"provider"`
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	In       float64 `json:"input_per_1m,omitempty"`
	Out      float64 `json:"output_per_1m,omitempty"`
}

func (m model) slug() string { return m.Provider + "/" + m.ID }

type server struct {
	db      *sql.DB
	e       *noewright.Engine
	models  map[string]model // by slug
	ordered []model

	runs      *platform.Limiter
	busy      sync.Map // visitor -> struct{}: one run at a time per visitor
	slots     chan struct{}
	budgetsMu sync.Mutex
	budgeted  map[string]bool

	games  *tables           // multiplayer rooms and hosts' in-memory keys
	misses *platform.Limiter // wrong game codes per IP

	bibles    map[string]bible // sermon captions: kjv, yor, ibo
	house     *houseKey        // the demo's own key for captions; nil when unset
	viewers   sync.Map         // captions session -> *int64 follow streams
	viewersMu sync.Mutex
}

const (
	dryRunSlug    = "dryrun"
	stepsPerDay   = 150
	maxConcurrent = 8
)

func main() {
	ctx := context.Background()
	db, err := platform.OpenDB(platform.Env("DEMO_DB", "./data/demo.db"))
	if err != nil {
		panic(err)
	}
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		panic(err)
	}

	s := newServer(ctx, db)
	e := s.e
	go s.gameJanitor()
	gcCtx, stopGC := context.WithCancel(context.Background())
	go e.GC().Run(gcCtx)

	mux := s.routes()

	platform.Serve("loom", platform.Env("ADDR", ":8080"), platform.WithVisitor(mux), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopGC()
		_ = e.Close(ctx)
		db.Close()
	})
}

// newServer builds the server and its Noewright engine on db: models,
// generators (the dry-run one included), seeded agents, hooks and the graph
// functions the demos call. main adds the background workers and routes.
func newServer(ctx context.Context, db *sql.DB) *server {
	var err error
	s := &server{
		db:       db,
		models:   map[string]model{},
		runs:     platform.NewLimiter(40, time.Hour),
		slots:    make(chan struct{}, maxConcurrent),
		budgeted: map[string]bool{},
		games:    &tables{rooms: map[string]*room{}, keys: map[string]keyEntry{}},
		misses:   platform.NewLimiter(40, time.Hour),
	}
	if err := s.migrateGames(ctx); err != nil {
		panic(err)
	}
	if s.bibles, err = loadBibles(); err != nil {
		panic(err)
	}

	// Anthropic prices are first-party API list prices. OpenAI models are left
	// unpriced on purpose: the demo shows "—" rather than invent a number.
	s.addModels(
		model{Provider: "anthropic", ID: "claude-opus-5-5", Label: "Claude Opus 5.5", In: 4, Out: 20},
		model{Provider: "anthropic", ID: "claude-sonnet-5-5", Label: "Claude Sonnet 5.5", In: 2, Out: 10},
		model{Provider: "anthropic", ID: "claude-haiku-5-5", Label: "Claude Haiku 5.5", In: 0.10, Out: 0.50},
	)
	for _, id := range strings.Split(platform.Env("OPENAI_MODELS", "gpt-5-mini,gpt-5"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			s.addModels(model{Provider: "openai", ID: id, Label: id})
		}
	}

	s.setupHouseKey()

	dry := &dryRun{bibles: s.bibles}
	gens := map[string]noewright.Generator{dryRunSlug: dry}
	pricing := map[string]noewright.ModelPrice{}
	for _, m := range s.ordered {
		// Generators are built with no key. The visitor's key arrives per
		// request in Overrides["api_key"]; base_url overrides stay disabled,
		// so a request can never redirect a key to another host.
		switch m.Provider {
		case "anthropic":
			gens[m.slug()] = anthropic.NewChatGenerator("", m.ID)
		case "openai":
			gens[m.slug()] = openai.NewChatGenerator("", m.ID)
		}
		if m.In > 0 {
			pricing[m.ID] = noewright.ModelPrice{InputPer1M: m.In, OutputPer1M: m.Out}
		}
	}

	e, err := noewright.New(noewright.Config{
		DB:         db,
		Dialect:    noewright.DialectSQLite,
		Generators: gens,
		Pricing:    pricing,
		Cache:      noewright.NewInProcessCache(),
		MaxRetries: 1,
		BranchGC:   gcConfig(),
	})
	if err != nil {
		panic(err)
	}
	s.e = e
	dry.e = e

	if err := s.seedGames(ctx); err != nil {
		panic(err)
	}
	s.registerGameHooks()
	if err := s.seedDesk(ctx); err != nil {
		panic(err)
	}
	s.registerDeskHooks()
	if err := s.seedCaptions(ctx); err != nil {
		panic(err)
	}
	s.registerCaptionHooks()
	s.registerCaptionGraph()
	s.registerGraphFuncs()
	return s
}

// routes is the demo's HTTP API.
func (s *server) routes() *http.ServeMux {
	creates := platform.NewLimiter(10, time.Hour)
	verifies := platform.NewLimiter(30, time.Hour)
	acts := platform.NewLimiter(300, time.Hour)
	G := "/api/loom/games/{code}"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/loom/health", platform.Health)
	mux.HandleFunc("GET /api/loom/config", s.config)
	mux.HandleFunc("POST /api/loom/models/verify", verifies.Limit(s.verifyModel))

	mux.HandleFunc("POST /api/loom/games", creates.Limit(s.createGame))
	mux.HandleFunc("GET "+G, s.getGame)
	mux.HandleFunc("GET "+G+"/events", s.events)
	mux.HandleFunc("POST "+G+"/join", acts.Limit(s.joinGame))
	mux.HandleFunc("POST "+G+"/start", acts.Limit(s.startGame))
	mux.HandleFunc("POST "+G+"/act", acts.Limit(s.act))
	mux.HandleFunc("POST "+G+"/resolve", acts.Limit(s.resolveNow))
	mux.HandleFunc("POST "+G+"/narrate", acts.Limit(s.narrate))
	mux.HandleFunc("POST "+G+"/check", acts.Limit(s.callCheck))
	mux.HandleFunc("POST "+G+"/npc", acts.Limit(s.npcSpeak))
	mux.HandleFunc("POST "+G+"/adjust", acts.Limit(s.adjust))
	mux.HandleFunc("POST "+G+"/rewind", acts.Limit(s.rewind))
	mux.HandleFunc("POST "+G+"/key", verifies.Limit(s.setGameKey))
	mux.HandleFunc("POST "+G+"/end", acts.Limit(s.endGame))

	desk := platform.NewLimiter(120, time.Hour)
	mux.HandleFunc("POST /api/loom/desk", creates.Limit(s.deskStart))
	mux.HandleFunc("GET /api/loom/desk/backtest", desk.Limit(s.deskBacktest))
	mux.HandleFunc("GET /api/loom/desk/{id}", s.deskGet)
	mux.HandleFunc("POST /api/loom/desk/{id}/play", s.runs.Limit(s.deskPlay))
	mux.HandleFunc("POST /api/loom/desk/{id}/rewind", desk.Limit(s.deskRewind))

	lines := platform.NewLimiter(240, time.Hour)
	follows := platform.NewLimiter(120, time.Hour)
	mux.HandleFunc("GET /api/loom/captions/config", s.captionsConfig)
	mux.HandleFunc("POST /api/loom/captions", creates.Limit(s.captionsStart))
	mux.HandleFunc("GET /api/loom/captions/{id}", s.captionsGet)
	mux.HandleFunc("POST /api/loom/captions/{id}/line", lines.Limit(s.captionsLine))
	mux.HandleFunc("GET /api/loom/captions/{id}/follow", follows.Limit(s.captionsFollow))

	mux.HandleFunc("GET /api/loom/pipeline/preset", s.pipelinePreset)
	mux.HandleFunc("POST /api/loom/pipeline/plan", s.pipelinePlan)
	mux.HandleFunc("POST /api/loom/pipeline/run", s.runs.Limit(s.pipelineRun))
	return mux
}

// gcConfig turns on Noewright's GC with retention rules for a public demo:
// pipeline runs are scratch, games outlive their 7-day table rows by a day.
func gcConfig() *noewright.BranchGCConfig {
	c := noewright.DefaultBranchGCConfig()
	c.DryRun = false
	c.RunInterval = 6 * time.Hour
	c.Retention = []noewright.RetentionRule{
		{Name: "pipeline-runs", Tags: []string{"pipeline"}, MaxIdle: 48 * time.Hour, Action: noewright.RetentionPurge},
		{Name: "tabletop-games", Tags: []string{"tabletop"}, MaxIdle: 8 * 24 * time.Hour, Action: noewright.RetentionPurge},
		{Name: "forecast-desks", Tags: []string{"forecast"}, MaxIdle: 7 * 24 * time.Hour, Action: noewright.RetentionPurge},
		{Name: "sermon-captions", Tags: []string{"captions"}, MaxIdle: 7 * 24 * time.Hour, Action: noewright.RetentionPurge},
	}
	return &c
}

func (s *server) addModels(ms ...model) {
	for _, m := range ms {
		s.models[m.slug()] = m
		s.ordered = append(s.ordered, m)
	}
}

func (s *server) config(w http.ResponseWriter, _ *http.Request) {
	platform.JSON(w, http.StatusOK, map[string]any{
		"models":          s.ordered,
		"steps_per_day":   stepsPerDay,
		"game_step_limit": gameStepBudget,
		"max_players":     maxPlayers,
		"archetypes":      archetypes,
		"worlds":          worldPresets,
	})
}

// ---------------------------------------------------------------------------
// Per-request model selection and the visitor's key
// ---------------------------------------------------------------------------

// runner is everything one run needs: which generator, and the overrides
// carrying the visitor's key. The key lives only in this struct, on the
// stack of one request.
type runner struct {
	generator string
	overrides map[string]any
	model     model
	dry       bool
	house     bool // the demo's own key (sermon captions only)
}

var keyShape = regexp.MustCompile(`^[A-Za-z0-9_\-\.]{20,300}$`)

// runnerFor reads X-LLM-Model ("provider/model") and X-LLM-Key. With no key
// the run uses the dry-run generator: the orchestration is real, the model
// output is canned.
func (s *server) runnerFor(r *http.Request) (runner, error) {
	key := strings.TrimSpace(r.Header.Get("X-LLM-Key"))
	if key == "" {
		return runner{generator: dryRunSlug, dry: true}, nil
	}
	m, ok := s.models[r.Header.Get("X-LLM-Model")]
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

// acquire enforces one run at a time per visitor and a global cap, so a few
// visitors cannot tie up the box.
func (s *server) acquire(vid string) (func(), error) {
	if _, loaded := s.busy.LoadOrStore(vid, struct{}{}); loaded {
		return nil, errors.New("You already have a run going. Let it finish first.")
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.busy.Delete(vid)
		return nil, errors.New("The demo is busy right now. Try again in a minute.")
	}
	return func() { <-s.slots; s.busy.Delete(vid) }, nil
}

// ensureBudget gives each visitor a daily step budget, enforced by Noewright
// itself. It caps load on this server; the visitor's key pays for tokens.
func (s *server) ensureBudget(ctx context.Context, vid string) error {
	s.budgetsMu.Lock()
	defer s.budgetsMu.Unlock()
	if s.budgeted[vid] {
		return nil
	}
	existing, err := s.e.Budgets().List(ctx, noewright.TargetPlatformID, vid)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		if err := s.e.Budgets().Create(ctx, &noewright.Budget{
			Name:     "demo-daily-steps",
			Target:   noewright.BudgetTarget{Kind: noewright.TargetPlatformID, Key: vid},
			Window:   noewright.BudgetWindowDay,
			Limit:    noewright.BudgetLimit{Steps: stepsPerDay},
			OnExceed: noewright.BudgetBlock,
			Active:   true,
		}); err != nil {
			return err
		}
	}
	s.budgeted[vid] = true
	return nil
}

// ---------------------------------------------------------------------------
// Server-sent events
// ---------------------------------------------------------------------------

type stream struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu sync.Mutex
}

func newStream(w http.ResponseWriter) (*stream, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &stream{w: w, f: f}, true
}

// send writes one SSE event. Safe to call from concurrent followers.
func (st *stream) send(event string, v any) {
	b, _ := json.Marshal(v)
	st.mu.Lock()
	defer st.mu.Unlock()
	fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", event, b)
	st.f.Flush()
}

// sendID writes an event with an id, so EventSource resends it as
// Last-Event-ID on reconnect and the server replays only what was missed.
func (st *stream) sendID(id int64, event string, v any) {
	b, _ := json.Marshal(v)
	st.mu.Lock()
	defer st.mu.Unlock()
	fmt.Fprintf(st.w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, b)
	st.f.Flush()
}

// comment writes an SSE comment line; used as a keep-alive.
func (st *stream) comment(text string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	fmt.Fprintf(st.w, ": %s\n\n", text)
	st.f.Flush()
}

// fail reports a run error as an event, translating Noewright's typed errors into
// something a visitor can act on.
func (st *stream) fail(err error) {
	st.send("error", map[string]string{"message": explain(err)})
}

func explain(err error) string {
	var be *noewright.BudgetExceededError
	var ge *noewright.GenerationError
	switch {
	case errors.As(err, &be):
		return fmt.Sprintf("You've used today's %d demo steps (Noewright's budget guard). It resets %s.",
			stepsPerDay, be.ResetsAt.UTC().Format("15:04 UTC"))
	case errors.As(err, &ge) && ge.Kind == noewright.GenerationRejected:
		return "The model declined that request (a safety refusal). Try a different action."
	case errors.As(err, &ge):
		msg := ge.Error()
		switch {
		case strings.Contains(msg, "401"), strings.Contains(msg, "authentication"), strings.Contains(msg, "invalid x-api-key"), strings.Contains(msg, "Incorrect API key"):
			return "The provider rejected that API key."
		case strings.Contains(msg, "429"):
			return "The provider rate-limited this key. Wait a moment and try again."
		case strings.Contains(msg, "404"), strings.Contains(msg, "model"):
			return "The provider couldn't run that model with this key. Try another model."
		}
		return "The model call failed: " + redact(msg)
	case errors.Is(err, context.DeadlineExceeded):
		return "That took too long and was stopped."
	}
	slog.Error("run failed", "err", err)
	return "Something went wrong running the agents."
}

var keyLike = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`)

// redact strips anything key-shaped from a provider error before it is shown.
func redact(s string) string {
	s = keyLike.ReplaceAllString(s, "sk-…")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// stepView is what the UI's run inspector shows for each agent step.
type stepView struct {
	Role       string `json:"role"`
	Agent      string `json:"agent"`
	Index      int    `json:"index"`
	Output     string `json:"output"`
	Prompt     string `json:"prompt"`
	DurationMs int    `json:"duration_ms"`
	// Timing within a graph run, in ms from the request's arrival: when the
	// node started, how long it ran (model calls, retries and hooks), and
	// when its first streamed token arrived.
	StartMs      int `json:"start_ms,omitempty"`
	WallMs       int `json:"wall_ms,omitempty"`
	FirstTokenMs int `json:"first_token_ms,omitempty"`
	// ReasoningTokens is the share of OutTokens a reasoning model spent
	// thinking before it answered, when the provider reports it.
	ReasoningTokens int     `json:"reasoning_tokens,omitempty"`
	InTokens        int     `json:"input_tokens"`
	OutTokens       int     `json:"output_tokens"`
	USD             float64 `json:"usd"`
	Priced          bool    `json:"priced"`
	Error           string  `json:"error,omitempty"`
}

func (s *server) viewStep(ctx context.Context, role string, st *noewright.Step) stepView {
	v := stepView{Role: role, Index: st.Index, Output: noewright.ResultText(st.Result), DurationMs: st.DurationMs}
	if st.Result != nil {
		switch n := st.Result.Metadata()["reasoning_tokens"].(type) {
		case int:
			v.ReasoningTokens = n
		case float64:
			v.ReasoningTokens = int(n)
		}
	}
	if a, err := s.e.Agents().GetByID(ctx, st.AgentID); err == nil {
		v.Agent = a.Slug
	}
	if p := st.Request.UserPrompt; len(p) > 2500 {
		v.Prompt = p[:2500] + "\n…"
	} else {
		v.Prompt = p
	}
	return v
}

// costs indexes Noewright's per-step cost ledger for a session. Cost rows commit
// in the same transaction as their step, so they are readable immediately.
func (s *server) costs(ctx context.Context, sessionID uuid.UUID) map[uuid.UUID]noewright.StepUsage {
	out := map[uuid.UUID]noewright.StepUsage{}
	rows, err := s.e.Cost().ByStep(ctx, sessionID)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.StepID] = r
	}
	return out
}

func applyCost(v *stepView, st *noewright.Step, ledger map[uuid.UUID]noewright.StepUsage) {
	if c, ok := ledger[st.ID]; ok {
		v.InTokens, v.OutTokens = c.InputTokens, c.OutputTokens
		v.Priced = !c.Estimated
		if v.Priced {
			// Unpriced rows carry Noewright's placeholder rate; never show that as money.
			v.USD = c.USDCost
		}
	}
}
