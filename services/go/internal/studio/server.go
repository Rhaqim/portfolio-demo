// Package studio is the backend of Noewright Studio: a hosted playground where
// anyone can open a starter graph, run it, and download it as a bundle to load
// into their own app. The frontend is a separate site (noewright-studio).
//
// Each visitor is anonymous, identified by the demo visitor cookie, and their
// records live in their own Noewright owner scope ("studio:<visitor>"). Runs
// are simulated unless the visitor sends their own API key with the request:
// Studio has no key of its own and never stores or logs a visitor's.
package studio

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/generator/anthropic"
	"github.com/rhaqim/noewright/generator/openai"

	"github.com/rhaqim/demo/internal/platform"
)

// Config configures a Server.
type Config struct {
	DB      *sql.DB
	Dialect noewright.Dialect
	// StepsPerDay caps the agent steps one visitor can run per day, simulated
	// or not, to keep the service responsive for everyone.
	StepsPerDay int
}

// Server is the Studio backend.
type Server struct {
	cfg    Config
	e      *noewright.Engine
	models []Model
	byID   map[string]Model
	busy   sync.Map // visitor → struct{}: one run at a time per visitor
	limit  *platform.Limiter
	// testRuns caps test runs per visitor per hour: test sessions do not
	// count against the visitor's daily step budget.
	testRuns *platform.Limiter
	seeded   sync.Map // visitor → struct{}: starter templates loaded
}

const simulateSlug = "simulate"

// New builds a Server and its Noewright engine.
func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.DB == nil {
		return nil, errors.New("studio: a database is required")
	}
	if cfg.StepsPerDay == 0 {
		cfg.StepsPerDay = 300
	}
	s := &Server{cfg: cfg, models: defaultModels(), byID: map[string]Model{},
		limit: platform.NewLimiter(120, time.Minute), testRuns: platform.NewLimiter(20, time.Hour)}
	if err := ensureTables(ctx, cfg.DB); err != nil {
		return nil, fmt.Errorf("studio: tables: %w", err)
	}

	sim := &simulator{}
	gens := map[string]noewright.Generator{simulateSlug: sim}
	pricing := map[string]noewright.ModelPrice{}
	for _, m := range s.models {
		s.byID[m.ID] = m
		// Built with no key: the visitor's key arrives per request in
		// Overrides["api_key"]. Base-URL overrides stay disabled, so a request
		// can never send a key to another host.
		switch m.Provider {
		case "anthropic":
			gens[m.slug()] = anthropic.NewChatGenerator("", m.ID)
		case "openai":
			gens[m.slug()] = openai.NewChatGenerator("", m.ID)
		}
		if m.InPer1M > 0 {
			pricing[m.ID] = noewright.ModelPrice{InputPer1M: m.InPer1M, OutputPer1M: m.OutPer1M}
		}
	}
	e, err := noewright.New(noewright.Config{
		DB:         cfg.DB,
		Dialect:    cfg.Dialect,
		Generators: gens,
		Pricing:    pricing,
		Cache:      noewright.NewInProcessCache(),
		MaxRetries: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("studio: engine: %w", err)
	}
	sim.e = e
	s.e = e
	return s, nil
}

// Engine returns the server's Noewright engine.
func (s *Server) Engine() *noewright.Engine { return s.e }

// Close stops the engine.
func (s *Server) Close(ctx context.Context) error { return s.e.Close(ctx) }

// Handler is the HTTP API, under /api/studio, behind the demo visitor cookie.
func (s *Server) Handler() http.Handler {
	const A = "/api/studio"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+A+"/health", platform.Health)
	mux.HandleFunc("GET "+A+"/session", s.session)
	mux.HandleFunc("GET "+A+"/models", s.listModels)
	mux.HandleFunc("POST "+A+"/models/verify", s.verifyModel)
	mux.HandleFunc("GET "+A+"/templates", s.listTemplates)
	mux.HandleFunc("GET "+A+"/graphs", s.listGraphs)
	mux.HandleFunc("GET "+A+"/graphs/{slug}", s.getGraph)
	mux.HandleFunc("POST "+A+"/graphs/{slug}/run", s.runGraph)
	mux.HandleFunc("GET "+A+"/graphs/{slug}/bundle", s.downloadBundle)
	mux.HandleFunc("GET "+A+"/graphs/{slug}/tests", s.listTests)
	mux.HandleFunc("POST "+A+"/graphs/{slug}/tests", s.createTest)
	mux.HandleFunc("DELETE "+A+"/graphs/{slug}/tests/{name}", s.deleteTest)
	mux.HandleFunc("POST "+A+"/graphs/{slug}/tests/run", s.runTests)
	return platform.WithVisitor(s.perVisitorLimit(mux))
}

// perVisitorLimit caps API requests per visitor per minute.
func (s *Server) perVisitorLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.limit.Allow(platform.Visitor(r)) {
			platform.Error(w, http.StatusTooManyRequests, "Slow down: too many requests this minute.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func owner(r *http.Request) string { return "studio:" + platform.Visitor(r) }

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureStarter(r.Context(), r); err != nil {
		slog.Error("starter templates", "err", err)
		platform.Error(w, http.StatusInternalServerError, "Couldn't set up your workspace.")
		return
	}
	platform.JSON(w, http.StatusOK, map[string]any{
		"visitor":       platform.Visitor(r),
		"steps_per_day": s.cfg.StepsPerDay,
		"simulate":      "Without your own API key, runs use a simulated model: the graph, routing, waits and costs are real, the text is not.",
	})
}
