package main

// captions_graph.go — a captioned line as one Noewright graph.
//
//	          ┌─ detect ─▶ lookup (Go) ─┐
//	start ────┤                          ├─▶ yo, ig, pcm ─▶ line (Go, recorded sc-line)
//	          └─ caption (streams) ─────┘
//
//	then, after the session is released:  judge ─▶ score (recorded sc-score)
//
// The English caption needs only the spoken line, so it streams at once,
// alongside the detector. The translators start when both have finished. The
// line goes to the speaker and every follower as soon as the translations
// are in, and the line's run ends there, releasing the service for the next
// line. The judge then runs as its own one-node graph in a scratch session,
// and its scores are recorded on the service afterwards. Each agent can run
// on its own model and key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"
)

// captionGraphAgents are the graph's agent nodes: node id → agent slug.
var captionGraphAgents = []struct{ Node, Agent, Label string }{
	{"detect", "sc-detect", "Detector"},
	{"caption", "sc-caption", "English caption"},
	{"yo", "sc-yo", "Yorùbá"},
	{"ig", "sc-ig", "Igbo"},
	{"pcm", "sc-pcm", "Nigerian Pidgin"},
	{"judge", "sc-judge", "Judge"},
}

func isCaptionAgent(slug string) bool {
	for _, a := range captionGraphAgents {
		if a.Agent == slug {
			return true
		}
	}
	return false
}

// captionGraph builds the graph for one line, each agent on its runner.
func captionGraph(runs map[string]runner) noewright.Graph {
	agent := func(id, slug, out string) noewright.GraphNode {
		r := runs[slug]
		return noewright.GraphNode{ID: id, Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{
			AgentSlug: slug, OutputKey: out, GeneratorOverride: r.generator, Overrides: r.overrides,
			Params: map[string]any{"effort": "low"}, MaxRetries: 1}}
	}
	detect := agent("detect", "sc-detect", "Refs")
	detect.Continue = true // the rule-based detector still runs in lookup
	caption := agent("caption", "sc-caption", "Caption")
	caption.Agent.Stream, caption.Required = true, true
	nodes := []noewright.GraphNode{
		{ID: "start", Kind: noewright.NodeFunc, Func: "sc-start"},
		detect,
		{ID: "lookup", Kind: noewright.NodeFunc, Func: "sc-lookup", OutputKey: "Scripture"},
		caption,
	}
	var translators []string
	for _, l := range captionLangs {
		n := agent(l.Code, "sc-"+l.Code, l.Key)
		n.Continue = true // a failed translation still lets the line through
		nodes = append(nodes, n)
		translators = append(translators, l.Code)
	}
	nodes = append(nodes,
		noewright.GraphNode{ID: "line", Kind: noewright.NodeFunc, Func: "sc-make-line", Record: "sc-line", OutputKey: "LineJSON"})
	edges := []noewright.GraphEdge{
		{From: "start", To: []string{"detect", "caption"}},
		{From: "detect", To: []string{"lookup"}},
		{From: "lookup", To: translators},
		{From: "caption", To: translators},
	}
	for _, t := range translators {
		edges = append(edges, noewright.GraphEdge{From: t, To: []string{"line"}})
	}
	return noewright.Graph{Slug: "sermon-captions", Entry: "start", Inputs: []string{"Spoken", "Previous", "DryRun"},
		Nodes: nodes, Edges: edges}
}

// judgeGraph scores a line once it is on screen. It runs in the service's
// scratch session, so a slow judge never holds up the next line.
func judgeGraph(runs map[string]runner) noewright.Graph {
	r := runs["sc-judge"]
	return noewright.Graph{Slug: "sermon-captions-judge", Entry: "judge", Nodes: []noewright.GraphNode{
		{ID: "judge", Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{AgentSlug: "sc-judge", OutputKey: "Judge",
			GeneratorOverride: r.generator, Overrides: r.overrides, Params: map[string]any{"effort": "low"}, MaxRetries: 1}},
	}}
}

// captionShape is the graph for the page to draw: the same builder, so the
// diagram is always what runs.
func captionShape() map[string]any {
	g := captionGraph(map[string]runner{})
	// The judge's run follows the line's; draw them as one picture.
	g.Nodes = append(g.Nodes, judgeGraph(map[string]runner{}).Nodes[0],
		noewright.GraphNode{ID: "score", Kind: noewright.NodeFunc})
	g.Edges = append(g.Edges, noewright.GraphEdge{From: "line", To: []string{"judge"}}, noewright.GraphEdge{From: "judge", To: []string{"score"}})
	type node struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Agent string `json:"agent,omitempty"`
		Label string `json:"label"`
	}
	labels := map[string]string{"start": "Line arrives", "lookup": "Bible lookup", "line": "Line on screen", "score": "Scores on screen"}
	for _, a := range captionGraphAgents {
		labels[a.Node] = a.Label
	}
	var nodes []node
	for _, n := range g.Nodes {
		nd := node{ID: n.ID, Kind: string(n.Kind), Label: labels[n.ID]}
		if n.Agent != nil {
			nd.Agent = n.Agent.AgentSlug
		}
		nodes = append(nodes, nd)
	}
	var edges [][2]string
	for _, e := range g.Edges {
		for _, to := range e.To {
			edges = append(edges, [2]string{e.From, to})
		}
	}
	return map[string]any{"nodes": nodes, "edges": edges}
}

// lookupPayload is what the lookup node hands the translators and judge.
type lookupPayload struct {
	Refs  []lineRef                    `json:"refs"`
	All   string                       `json:"all"`
	Langs map[string]map[string]string `json:"langs"` // code → scripture, guard
}

// registerCaptionGraph registers the graph's Go functions and the pre-hook
// that hands each translator its own language's scripture.
func (s *server) registerCaptionGraph() {
	s.e.RegisterFunc("sc-start", func(context.Context, noewright.FuncCall) (noewright.FuncResult, error) {
		return noewright.FuncResult{}, nil
	})

	// lookup keeps the detector's references that exist, adds exact citations
	// a rule can read, and looks every passage up in the three Bibles.
	s.e.RegisterFunc("sc-lookup", func(_ context.Context, call noewright.FuncCall) (noewright.FuncResult, error) {
		var found struct {
			Refs []scriptureRef `json:"refs"`
		}
		if raw, _ := call.Inputs["Refs"].(string); raw != "" {
			_ = extractJSON(raw, &found)
		}
		spoken, _ := call.Inputs["Spoken"].(string)
		kjv := s.bibles["kjv"]
		refs := normalizeRefs(append(found.Refs, detectRefs(spoken, kjv)...), kjv)
		p := lookupPayload{Refs: []lineRef{}, Langs: map[string]map[string]string{}}
		for _, ref := range refs {
			lr := lineRef{scriptureRef: ref, Label: ref.label(), Texts: map[string]string{"en": ref.passage(kjv)}}
			for _, l := range captionLangs {
				lr.Texts[l.Code] = ref.passage(s.bibles[bibleLangs[l.Code]])
			}
			p.Refs = append(p.Refs, lr)
		}
		p.All = scriptureBlock(p.Refs, "en")
		for _, l := range captionLangs {
			p.Langs[l.Code] = map[string]string{"scripture": scriptureBlock(p.Refs, l.Code), "guard": guardInput(p.Refs, l.Code)}
		}
		b, _ := json.Marshal(p)
		return noewright.FuncResult{Text: string(b)}, nil
	})

	// line assembles the captioned line; recorded as sc-line, it reaches the
	// service's history (captions-state hook) and every follower.
	s.e.RegisterFunc("sc-make-line", func(_ context.Context, call noewright.FuncCall) (noewright.FuncResult, error) {
		var p lookupPayload
		raw, _ := call.Inputs["Scripture"].(string)
		_ = json.Unmarshal([]byte(raw), &p)
		var c captionState
		_ = json.Unmarshal(call.Snapshot, &c)
		spoken, _ := call.Inputs["Spoken"].(string)
		english, _ := call.Inputs["Caption"].(string)
		dry, _ := call.Inputs["DryRun"].(bool)
		line := captionLine{N: len(c.Lines) + 1, Spoken: spoken, English: strings.TrimSpace(english),
			Captions: map[string]string{}, Refs: p.Refs, Judge: map[string]judgeScore{}, Guard: map[string]string{},
			DryRun: dry, At: time.Now().UTC()}
		if line.Refs == nil {
			line.Refs = []lineRef{}
		}
		for _, l := range captionLangs {
			if t, _ := call.Inputs[l.Key].(string); t != "" {
				line.Captions[l.Code] = strings.TrimSpace(t)
			}
			if g, _ := call.Vars["guard_"+l.Code].(string); g != "" {
				line.Guard[l.Code] = g
			}
		}
		b, _ := json.Marshal(line)
		return noewright.FuncResult{Text: string(b)}, nil
	})

	// In a graph the scripture exists only once lookup has run, so this
	// pre-hook gives each translator its own language's passages and guard,
	// and the judge the English ones, from lookup's output.
	s.e.Hooks().RegisterPre("caption-inputs", func(_ context.Context, req *noewright.StepRequest) error {
		if !strings.HasPrefix(req.AgentSlug, "sc-") || req.Inputs == nil {
			return nil
		}
		raw, _ := req.Inputs["Scripture"].(string)
		if !strings.HasPrefix(raw, "{") {
			return nil
		}
		var p lookupPayload
		if json.Unmarshal([]byte(raw), &p) != nil {
			return nil
		}
		code := strings.TrimPrefix(req.AgentSlug, "sc-")
		switch {
		case isCaptionLang(code):
			req.Inputs["Scripture"] = p.Langs[code]["scripture"]
			req.Inputs["Guard"] = p.Langs[code]["guard"]
		case req.AgentSlug == "sc-judge":
			req.Inputs["ScriptureAll"] = p.All
		}
		return nil
	})
}

// captionRuns picks a runner for each agent: the model the visitor chose for
// it (with their key for that provider), or the line's default runner (their
// one key, the demo's key, or a dry run). An agent whose provider has no key
// runs dry.
func (s *server) captionRuns(r *http.Request, models map[string]string) (map[string]runner, runner, error) {
	def, err := s.captionRunner(r)
	if err != nil {
		return nil, def, err
	}
	keys := map[string]string{
		"anthropic": strings.TrimSpace(r.Header.Get("X-LLM-Key-Anthropic")),
		"openai":    strings.TrimSpace(r.Header.Get("X-LLM-Key-OpenAI")),
	}
	if !def.dry && !def.house {
		if keys[def.model.Provider] == "" {
			keys[def.model.Provider], _ = def.overrides["api_key"].(string)
		}
	}
	for p, k := range keys {
		if k == "" {
			continue
		}
		if !keyShape.MatchString(k) {
			return nil, def, errors.New("one of your keys doesn't look like an API key")
		}
		if p == "anthropic" && !strings.HasPrefix(k, "sk-ant-") {
			return nil, def, errors.New("Anthropic keys start with sk-ant-")
		}
	}
	runs := map[string]runner{}
	for _, a := range captionGraphAgents {
		slug, ok := models[a.Agent]
		if !ok || slug == "" {
			runs[a.Agent] = def
			continue
		}
		m, ok := s.models[slug]
		if !ok {
			return nil, def, fmt.Errorf("pick a model from the list for the %s", strings.ToLower(a.Label))
		}
		if k := keys[m.Provider]; k != "" {
			runs[a.Agent] = runner{generator: m.slug(), overrides: map[string]any{"api_key": k}, model: m}
		} else {
			runs[a.Agent] = runner{generator: dryRunSlug, dry: true}
		}
	}
	for slug := range models {
		if !isCaptionAgent(slug) {
			return nil, def, fmt.Errorf("%q is not one of this demo's agents", slug)
		}
	}
	return runs, def, nil
}

// acquireWait waits for the visitor's run slot instead of refusing: a line
// sent while the previous line's judge is still scoring queues behind it.
func (s *server) acquireWait(ctx context.Context, vid string) (func(), error) {
	for {
		release, err := s.acquire(vid)
		if err == nil {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// lineClock times one line: t0 is when its request arrived, waited how long
// it queued behind the previous line (which holds the visitor's lock until
// its judge is done).
type lineClock struct {
	t0     time.Time
	waited time.Duration

	mu    sync.Mutex
	start map[string]time.Time // node → when it started
	first map[string]time.Time // node → its first streamed token
}

func (c *lineClock) ms(t time.Time) int { return int(t.Sub(c.t0).Milliseconds()) }

func (c *lineClock) started(node string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start == nil {
		c.start = map[string]time.Time{}
	}
	now := time.Now()
	c.start[node] = now
	return c.ms(now)
}

func (c *lineClock) token(node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.first == nil {
		c.first = map[string]time.Time{}
	}
	if _, ok := c.first[node]; !ok {
		c.first[node] = time.Now()
	}
}

// stamp fills a step view's timing for node, which has just finished.
func (c *lineClock) stamp(v *stepView, node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.start[node]
	if !ok {
		return
	}
	v.StartMs, v.WallMs = c.ms(st), int(time.Since(st).Milliseconds())
	if f, ok := c.first[node]; ok {
		v.FirstTokenMs = int(f.Sub(st).Milliseconds())
	}
}

// runCaptionLine runs one line's graph and streams it to the speaker. It
// holds the visitor's lock (release) until the line is on screen, then
// releases it and runs the judge.
func (s *server) runCaptionLine(ctx context.Context, st *stream, sess *noewright.Session, spoken string,
	runs map[string]runner, def runner, clock *lineClock, vid string, release func()) {
	c := loadCaptions(&sess.State)
	previous := ""
	if n := len(c.Lines); n > 0 {
		previous = c.Lines[n-1].Spoken
	}
	if sess.State.Vars == nil {
		sess.State.Vars = map[string]any{}
	}
	for _, l := range captionLangs {
		delete(sess.State.Vars, "guard_"+l.Code)
	}
	scratch, err := s.judgeSession(ctx, sess, vid)
	if err != nil {
		st.fail(err)
		return
	}

	allDry := true
	agents := map[string]any{}
	for _, a := range captionGraphAgents {
		r := runs[a.Agent]
		mode, label := "simulated", "dry run"
		switch {
		case r.house:
			mode, label = "house", r.model.Label+" (the demo's key)"
		case !r.dry:
			mode, label = "key", r.model.Label
		}
		if !r.dry {
			allDry = false
		}
		agents[a.Agent] = map[string]string{"mode": mode, "model": label}
	}
	label := def.model.Label
	if def.house {
		label += " (the demo's key)"
	}
	st.send("session", map[string]any{"session_id": sess.ID, "dry_run": allDry, "house": def.house, "model": label, "agents": agents,
		"waited_ms": int(clock.waited.Milliseconds()), "at_ms": clock.ms(time.Now())})

	// What the judge reads, gathered as the line's nodes finish.
	var mu sync.Mutex
	judgeIn := map[string]any{}
	var line *captionLine
	_, err = s.e.RunGraph(ctx, sess, noewright.GraphRequest{
		Graph:  captionGraph(runs),
		Owner:  ttOwner,
		Inputs: map[string]any{"Line": spoken, "Spoken": spoken, "Previous": previous, "DryRun": allDry},
		OnChunk: func(node string, ch noewright.Chunk) {
			clock.token(node)
			if node == "caption" {
				st.send("chunk", map[string]string{"text": ch.Content})
			}
		},
		OnNode: s.captionNodes(ctx, st, clock, sess.ID, func(ev noewright.NodeEvent) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case ev.Node == "lookup":
				judgeIn["Scripture"] = ev.Text
			case ev.Node == "caption":
				judgeIn["Caption"] = ev.Text
			case isCaptionLang(ev.Node):
				for _, l := range captionLangs {
					if l.Code == ev.Node {
						judgeIn[l.Key] = strings.TrimSpace(ev.Text)
					}
				}
			case ev.Node == "line":
				var ln captionLine
				if json.Unmarshal([]byte(ev.Text), &ln) == nil {
					line = &ln
					st.send("line", ln)
				}
			}
		}),
	})
	release() // the next line can start now; the judge doesn't need the service
	if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		slog.Error("captions line", "session", sess.ID, "err", err)
		st.fail(err)
		return
	}
	if line != nil {
		s.judgeLine(ctx, st, clock, sess.ID, scratch, vid, runs, judgeIn, line.N)
	}
	st.send("done", map[string]any{"ok": true, "total_ms": clock.ms(time.Now())})
}

// captionNodes forwards a run's node events to the speaker: progress with
// its time on the line's clock, each agent's step, the scripture found, and
// each caption. done sees every finished node.
func (s *server) captionNodes(ctx context.Context, st *stream, clock *lineClock, costsOf uuid.UUID, done func(noewright.NodeEvent)) func(noewright.NodeEvent) {
	agentOf := map[string]string{}
	for _, a := range captionGraphAgents {
		agentOf[a.Node] = a.Agent
	}
	return func(ev noewright.NodeEvent) {
		nv := map[string]any{"node": ev.Node, "kind": ev.Kind, "agent": agentOf[ev.Node], "at_ms": clock.ms(time.Now())}
		if ev.Kind == noewright.NodeStarted {
			nv["at_ms"] = clock.started(ev.Node)
		}
		if ev.Err != nil {
			nv["error"] = explain(ev.Err)
		}
		st.send("node", nv)
		switch {
		case ev.Kind == noewright.NodeFailed && agentOf[ev.Node] != "":
			v := stepView{Role: ev.Node, Agent: agentOf[ev.Node], Error: explain(ev.Err)}
			clock.stamp(&v, ev.Node)
			st.send("step", v)
			return
		case ev.Kind != noewright.NodeDone:
			return
		case ev.Node == "lookup":
			var p lookupPayload
			if json.Unmarshal([]byte(ev.Text), &p) == nil {
				st.send("scripture", p.Refs)
			}
		case ev.Step != nil && agentOf[ev.Node] != "":
			v := s.viewStep(ctx, ev.Node, ev.Step)
			applyCost(&v, ev.Step, s.costs(ctx, costsOf))
			clock.stamp(&v, ev.Node)
			st.send("step", v)
			if isCaptionLang(ev.Node) {
				st.send("caption", map[string]string{"lang": ev.Node, "text": strings.TrimSpace(ev.Text)})
			}
		}
		done(ev)
	}
}

// judgeSession returns the service's scratch session, where its judge runs,
// creating it on first use. sess is held under the visitor's lock, and the
// line's run saves the id with the service.
func (s *server) judgeSession(ctx context.Context, sess *noewright.Session, vid string) (*noewright.Session, error) {
	if raw, _ := sess.State.Vars["judge_session"].(string); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			if js, err := s.e.Sessions().GetHeader(ctx, id); err == nil {
				return js, nil
			}
		}
	}
	js := &noewright.Session{PlatformID: vid, Tags: []string{"captions", "captions-judge"},
		State: noewright.State{Modality: noewright.ModalityText, Vars: map[string]any{}}}
	if err := s.e.Sessions().Create(ctx, js); err != nil {
		return nil, errors.New("couldn't set up the judge")
	}
	sess.State.Vars["judge_session"] = js.ID.String()
	return js, nil
}

// judgeLine scores line n in the scratch session, sends the scores to the
// speaker at once, then records them on the service (taking the visitor's
// lock briefly, after any line in flight) so followers and history get them.
func (s *server) judgeLine(ctx context.Context, st *stream, clock *lineClock, service uuid.UUID, scratch *noewright.Session,
	vid string, runs map[string]runner, in map[string]any, n int) {
	var text string
	_, err := s.e.RunGraph(ctx, scratch, noewright.GraphRequest{
		Graph: judgeGraph(runs), Owner: ttOwner, Inputs: in, Restart: true,
		OnNode: s.captionNodes(ctx, st, clock, scratch.ID, func(ev noewright.NodeEvent) { text = ev.Text }),
	})
	if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) || text == "" {
		if err != nil {
			slog.Warn("captions judge", "session", service, "err", err)
		}
		return
	}
	judge := map[string]judgeScore{}
	_ = extractJSON(text, &judge)
	sc := map[string]any{"n": n, "judge": judge}
	b, _ := json.Marshal(sc)
	st.send("node", map[string]any{"node": "score", "kind": noewright.NodeDone, "at_ms": clock.ms(time.Now())})
	st.send("judge", sc)

	wait, stop := context.WithTimeout(ctx, 90*time.Second)
	release, err := s.acquireWait(wait, vid)
	stop()
	if err != nil {
		slog.Warn("captions judge: scores not saved", "session", service, "err", err)
		return
	}
	defer release()
	sess, err := s.e.Sessions().GetHeader(ctx, service)
	if err == nil {
		_, err = s.e.RecordStep(ctx, sess, noewright.ExternalStep{AgentSlug: "sc-score", Owner: ttOwner, Author: "judge",
			Result: noewright.NewTextResult(string(b), "stop", 0, 0)})
	}
	if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		slog.Warn("captions judge: scores not saved", "session", service, "err", err)
	}
}
