package studio

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"

	"github.com/rhaqim/demo/internal/platform"
)

//go:embed templates/*.yaml
var templateFS embed.FS

// Template is a starter graph every visitor gets in their workspace.
type Template struct {
	File        string `json:"-"`
	Graph       string `json:"graph"` // slug of the graph it adds
	Title       string `json:"title"`
	Description string `json:"description"`
	Shows       string `json:"shows"` // what in the engine it demonstrates
}

var templates = []Template{
	{File: "support-desk.yaml", Graph: "support-desk", Title: "Support desk",
		Description: "Sorts a customer message, answers it with the right specialist, then waits for the next message.",
		Shows:       "Routing on an agent's answer, a wait, a loop back"},
	{File: "incident-postmortem.yaml", Graph: "incident-postmortem", Title: "Incident postmortem",
		Description: "Turns raw incident notes into a triage, timeline, impact, root cause, status update and action items.",
		Shows:       "Fan-out, joins, data edges inferred from templates"},
	{File: "onboarding.yaml", Graph: "onboarding", Title: "Onboarding",
		Description: "Greets a new user, asks their name and goal, then writes a first-week plan.",
		Shows:       "Waits for text and for a choice, resumed across calls"},
}

func (s *Server) listTemplates(w http.ResponseWriter, _ *http.Request) {
	platform.JSON(w, http.StatusOK, templates)
}

// ensureStarter loads the starter templates into the visitor's workspace.
// Import skips records that are already there, so this is safe to repeat;
// the in-memory set only saves the work within one process.
func (s *Server) ensureStarter(ctx context.Context, r *http.Request) error {
	v := platform.Visitor(r)
	if _, done := s.seeded.Load(v); done {
		return nil
	}
	for _, t := range templates {
		data, err := templateFS.ReadFile("templates/" + t.File)
		if err != nil {
			return err
		}
		b, err := noewright.ParseBundle(data)
		if err != nil {
			return fmt.Errorf("template %s: %w", t.File, err)
		}
		_, err = s.e.Bundles().Import(ctx, b, noewright.ImportOptions{Owner: owner(r), Activate: true,
			Generators: map[string]string{"chat": simulateSlug}})
		if err != nil && !errors.Is(err, noewright.ErrBundleConflict) {
			return fmt.Errorf("template %s: %w", t.File, err)
		}
	}
	s.seeded.Store(v, struct{}{})
	return nil
}

// graphSummary is one graph in the visitor's workspace.
type graphSummary struct {
	Slug     string    `json:"slug"`
	Version  int       `json:"version"`
	Active   bool      `json:"active"`
	Nodes    int       `json:"nodes"`
	Template *Template `json:"template,omitempty"`
}

func (s *Server) listGraphs(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureStarter(r.Context(), r); err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't set up your workspace")
		return
	}
	recs, err := s.e.Graphs().List(r.Context(), owner(r), "")
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't list your graphs")
		return
	}
	var out []graphSummary
	for _, rec := range recs {
		g := graphSummary{Slug: rec.Slug, Version: rec.Version, Active: rec.IsActive, Nodes: len(rec.Graph.Nodes)}
		for i := range templates {
			if templates[i].Graph == rec.Slug {
				g.Template = &templates[i]
			}
		}
		out = append(out, g)
	}
	platform.JSON(w, http.StatusOK, out)
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (s *Server) graphFor(w http.ResponseWriter, r *http.Request) (*noewright.GraphRecord, bool) {
	slug := r.PathValue("slug")
	if !slugRe.MatchString(slug) {
		platform.Error(w, http.StatusNotFound, "No such graph")
		return nil, false
	}
	if err := s.ensureStarter(r.Context(), r); err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't set up your workspace")
		return nil, false
	}
	rec, err := s.e.Graphs().Get(r.Context(), owner(r), slug, 0)
	if err != nil {
		platform.Error(w, http.StatusNotFound, "No such graph")
		return nil, false
	}
	return rec, true
}

// getGraph returns a graph and its plan: nodes, edges (inferred data edges
// marked), problems, and each agent's prompts for the inspector.
func (s *Server) getGraph(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.graphFor(w, r)
	if !ok {
		return
	}
	plan, err := s.e.Graphs().Plan(r.Context(), owner(r), rec.Graph)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't plan the graph")
		return
	}
	type agentView struct {
		Slug         string `json:"slug"`
		Version      int    `json:"version"`
		SystemPrompt string `json:"system_prompt"`
		UserTemplate string `json:"user_template"`
	}
	agents := map[string]agentView{}
	for _, n := range rec.Graph.Nodes {
		if n.Agent == nil {
			continue
		}
		a, err := s.e.Agents().Get(r.Context(), owner(r), n.Agent.AgentSlug, n.Agent.AgentVersion)
		if err != nil {
			continue
		}
		av := agentView{Slug: a.Slug, Version: a.Version}
		if p, err := s.e.Prompts().GetByID(r.Context(), a.SystemPromptID); err == nil {
			av.SystemPrompt = p.Body
		}
		if p, err := s.e.Prompts().GetByID(r.Context(), a.UserTemplateID); err == nil {
			av.UserTemplate = p.Body
		}
		agents[n.ID] = av
	}
	platform.JSON(w, http.StatusOK, map[string]any{"record": rec, "plan": plan, "agents": agents})
}

// runRequest is one call: a message, a choice for a choice wait, or neither.
type runRequest struct {
	Session string         `json:"session,omitempty"` // continue this conversation; empty starts one
	Say     string         `json:"say,omitempty"`
	Select  string         `json:"select,omitempty"`
	Inputs  map[string]any `json:"inputs,omitempty"`
	Restart bool           `json:"restart,omitempty"`
}

// nodeView is one node's progress, as the page shows it.
type nodeView struct {
	Node       string  `json:"node"`
	Kind       string  `json:"kind"` // started, done, failed, skipped
	Agent      string  `json:"agent,omitempty"`
	Text       string  `json:"text,omitempty"`
	Route      string  `json:"route,omitempty"`
	Error      string  `json:"error,omitempty"`
	Prompt     string  `json:"prompt,omitempty"`
	InTokens   int     `json:"in_tokens,omitempty"`
	OutTokens  int     `json:"out_tokens,omitempty"`
	USD        float64 `json:"usd,omitempty"`
	Estimated  bool    `json:"estimated,omitempty"`
	DurationMs int     `json:"duration_ms,omitempty"`
}

// runGraph runs one call of a graph in a conversation and streams it as SSE:
// session, node (per node event), chunk (streamed text), done.
func (s *Server) runGraph(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.graphFor(w, r)
	if !ok {
		return
	}
	var req runRequest
	if err := platform.Decode(w, r, 32<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Say) > 8000 {
		platform.Error(w, http.StatusBadRequest, "Messages are limited to 8,000 characters")
		return
	}
	run, err := s.runnerFor(r)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	v := platform.Visitor(r)
	if _, busy := s.busy.LoadOrStore(v, struct{}{}); busy {
		platform.Error(w, http.StatusTooManyRequests, "One run at a time: wait for the current one to finish")
		return
	}
	defer s.busy.Delete(v)
	ctx := r.Context()
	if err := s.ensureBudget(ctx, v); err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't set up your step budget")
		return
	}

	sess, err := s.conversation(ctx, v, req.Session)
	if err != nil {
		platform.Error(w, http.StatusNotFound, err.Error())
		return
	}
	st, ok := newStream(w)
	if !ok {
		platform.Error(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}
	st.send("session", map[string]any{"session": sess.ID, "simulated": run.simulated, "model": run.model})

	agentOf := map[string]string{}
	for _, n := range rec.Graph.Nodes {
		if n.Agent != nil {
			agentOf[n.ID] = n.Agent.AgentSlug
		}
	}
	gr := noewright.GraphRequest{
		Inputs:            req.Inputs,
		Overrides:         run.overrides,
		GeneratorOverride: run.generator,
		Restart:           req.Restart,
		OnChunk: func(node string, c noewright.Chunk) {
			st.send("chunk", map[string]string{"node": node, "text": c.Content})
		},
		OnNode: func(ev noewright.NodeEvent) {
			nv := nodeView{Node: ev.Node, Kind: string(ev.Kind), Agent: agentOf[ev.Node], Text: ev.Text, Route: ev.Route}
			if ev.Err != nil {
				nv.Error = redact(ev.Err.Error())
			}
			if ev.Step != nil {
				s.withCost(ctx, sess.ID, ev.Step, &nv)
			}
			st.send("node", nv)
		},
	}
	switch {
	case req.Select != "":
		gr.Action = &noewright.Action{Kind: noewright.ActionSelect, Payload: map[string]any{"option_id": req.Select}}
	case req.Say != "":
		gr.Action = &noewright.Action{Kind: noewright.ActionFreeText, Payload: map[string]any{"text": req.Say}}
	}
	res, err := s.e.RunGraphBySlug(ctx, sess, owner(r), rec.Slug, 0, gr)
	if res == nil {
		st.send("error", map[string]string{"error": redact(errText(err))})
		return
	}
	errs := map[string]string{}
	for k, e := range res.Errors {
		errs[k] = redact(e.Error())
	}
	done := map[string]any{
		"run": res.ID, "waiting": res.Waiting, "prompt": res.Prompt, "resumed": res.Resumed,
		"ran": res.Ran, "skipped": res.Skipped, "path": res.Path, "errors": errs,
		"cost": map[string]any{"usd": res.Cost.TotalUSD, "tokens": res.Cost.TotalTokens, "steps": res.Cost.StepCount, "priced": res.Cost.PricingConfigured},
	}
	if wait := waitNode(rec.Graph, res.Waiting); wait != nil && wait.Wait != nil {
		done["expect"], done["options"] = wait.Wait.Expect, wait.Wait.Options
	}
	if err != nil {
		done["error"] = redact(err.Error())
	}
	st.send("done", done)
}

func waitNode(g noewright.Graph, id string) *noewright.GraphNode {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

func errText(err error) string {
	if err == nil {
		return "the run did not start"
	}
	return err.Error()
}

// keyRe finds anything shaped like an API key, so it never reaches the page.
var keyRe = regexp.MustCompile(`(sk-[A-Za-z0-9_\-]{8,}|[A-Za-z0-9_\-]{32,})`)

func redact(s string) string { return keyRe.ReplaceAllString(s, "[redacted]") }

// conversation returns the visitor's session id, or starts one.
func (s *Server) conversation(ctx context.Context, v, id string) (*noewright.Session, error) {
	if id == "" {
		sess := &noewright.Session{PlatformID: v, Tags: []string{"studio"}, State: noewright.State{Modality: noewright.ModalityText}}
		if err := s.e.Sessions().Create(ctx, sess); err != nil {
			return nil, errors.New("couldn't start a conversation")
		}
		return sess, nil
	}
	sid, err := uuid.Parse(id)
	if err != nil {
		return nil, errors.New("no such conversation")
	}
	sess, err := s.e.Sessions().GetHeader(ctx, sid)
	if err != nil || sess.PlatformID != v {
		return nil, errors.New("no such conversation")
	}
	return sess, nil
}

// withCost adds a step's tokens and price from Noewright's cost ledger.
func (s *Server) withCost(ctx context.Context, sessionID uuid.UUID, st *noewright.Step, nv *nodeView) {
	nv.DurationMs = st.DurationMs
	if p := st.Request.UserPrompt; len(p) > 2000 {
		nv.Prompt = p[:2000] + "\n…"
	} else {
		nv.Prompt = p
	}
	rows, err := s.e.Cost().ByStep(ctx, sessionID)
	if err != nil {
		return
	}
	for _, c := range rows {
		if c.StepID == st.ID {
			nv.InTokens, nv.OutTokens, nv.USD, nv.Estimated = c.InputTokens, c.OutputTokens, c.USDCost, c.Estimated
		}
	}
}

// ensureBudget gives a visitor a daily step cap, the first time they run.
func (s *Server) ensureBudget(ctx context.Context, v string) error {
	existing, err := s.e.Budgets().List(ctx, noewright.TargetPlatformID, v)
	if err == nil && len(existing) > 0 {
		return nil
	}
	return s.e.Budgets().Create(ctx, &noewright.Budget{
		Name:     "studio-daily-steps",
		Target:   noewright.BudgetTarget{Kind: noewright.TargetPlatformID, Key: v},
		Window:   noewright.BudgetWindowDay,
		Limit:    noewright.BudgetLimit{Steps: s.cfg.StepsPerDay},
		OnExceed: noewright.BudgetBlock,
		Active:   true,
	})
}

// downloadBundle exports the graph and everything it uses as a bundle file,
// with the generators named "chat" so the importer maps them to their own.
func (s *Server) downloadBundle(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.graphFor(w, r)
	if !ok {
		return
	}
	b, err := s.e.Bundles().Export(r.Context(), owner(r), noewright.BundleRoot{Kind: "graph", Slug: rec.Slug, Version: rec.Version})
	if err != nil {
		slog.Error("export", "graph", rec.Slug, "err", err)
		platform.Error(w, http.StatusInternalServerError, "Couldn't export the graph")
		return
	}
	for i := range b.Agents {
		if b.Agents[i].Generator == simulateSlug {
			b.Agents[i].Generator = "chat"
		}
	}
	for i := range b.Requires.Generators {
		if b.Requires.Generators[i].Slug == simulateSlug {
			b.Requires.Generators[i].Slug = "chat"
		}
	}
	b.Name = rec.Slug
	b.CreatedBy = "Noewright Studio"
	b.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := b.Marshal()
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't write the bundle")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.noewright.yaml"`, strings.ReplaceAll(rec.Slug, `"`, "")))
	_, _ = w.Write(data)
}
