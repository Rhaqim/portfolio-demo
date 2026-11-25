package main

// captions_graph.go — a captioned line as one Noewright graph.
//
//	          ┌─ detect ─▶ lookup (Go) ─┐
//	start ────┤                          ├─▶ yo, ig, pcm ─▶ line (Go, recorded sc-line) ─▶ judge ─▶ score (Go, recorded sc-score)
//	          └─ caption (streams) ─────┘
//
// The English caption needs only the spoken line, so it streams at once,
// alongside the detector. The translators start when both have finished. The
// line goes to the speaker and every follower as soon as the translations
// are in; the judge scores it afterwards and its scores follow. Each agent can
// run on its own model and key.

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
		noewright.GraphNode{ID: "line", Kind: noewright.NodeFunc, Func: "sc-make-line", Record: "sc-line", OutputKey: "LineJSON"},
		agent("judge", "sc-judge", "Judge"),
		noewright.GraphNode{ID: "score", Kind: noewright.NodeFunc, Func: "sc-score-line", Record: "sc-score"},
	)
	edges := []noewright.GraphEdge{
		{From: "start", To: []string{"detect", "caption"}},
		{From: "detect", To: []string{"lookup"}},
		{From: "lookup", To: translators},
		{From: "caption", To: translators},
	}
	for _, t := range translators {
		edges = append(edges, noewright.GraphEdge{From: t, To: []string{"line"}})
	}
	edges = append(edges,
		noewright.GraphEdge{From: "line", To: []string{"judge"}},
		noewright.GraphEdge{From: "judge", To: []string{"score"}},
	)
	return noewright.Graph{Slug: "sermon-captions", Entry: "start", Inputs: []string{"Spoken", "Previous", "DryRun"},
		Nodes: nodes, Edges: edges}
}

// captionShape is the graph for the page to draw: the same builder, so the
// diagram is always what runs.
func captionShape() map[string]any {
	g := captionGraph(map[string]runner{})
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

	// score turns the judge's reply into scores for the line; recorded as
	// sc-score, the captions-state hook attaches them to the stored line.
	s.e.RegisterFunc("sc-score-line", func(_ context.Context, call noewright.FuncCall) (noewright.FuncResult, error) {
		var line captionLine
		raw, _ := call.Inputs["LineJSON"].(string)
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			return noewright.FuncResult{}, errors.New("the line to score is missing")
		}
		judge := map[string]judgeScore{}
		text, _ := call.Inputs["Judge"].(string)
		_ = extractJSON(text, &judge)
		b, _ := json.Marshal(map[string]any{"n": line.N, "judge": judge})
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

// runCaptionLine runs one line's graph and streams it to the speaker.
func (s *server) runCaptionLine(ctx context.Context, st *stream, sess *noewright.Session, spoken string,
	runs map[string]runner, def runner, clock *lineClock) {
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

	agentOf := map[string]string{}
	for _, a := range captionGraphAgents {
		agentOf[a.Node] = a.Agent
	}
	_, err := s.e.RunGraph(ctx, sess, noewright.GraphRequest{
		Graph:  captionGraph(runs),
		Owner:  ttOwner,
		Inputs: map[string]any{"Line": spoken, "Spoken": spoken, "Previous": previous, "DryRun": allDry},
		OnChunk: func(node string, ch noewright.Chunk) {
			clock.token(node)
			if node == "caption" {
				st.send("chunk", map[string]string{"text": ch.Content})
			}
		},
		OnNode: func(ev noewright.NodeEvent) {
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
			case ev.Kind != noewright.NodeDone:
			case ev.Node == "lookup":
				var p lookupPayload
				if json.Unmarshal([]byte(ev.Text), &p) == nil {
					st.send("scripture", p.Refs)
				}
			case ev.Node == "line":
				var line captionLine
				if json.Unmarshal([]byte(ev.Text), &line) == nil {
					st.send("line", line)
				}
			case ev.Node == "score":
				var sc map[string]any
				if json.Unmarshal([]byte(ev.Text), &sc) == nil {
					st.send("judge", sc)
				}
			case ev.Step != nil && agentOf[ev.Node] != "":
				v := s.viewStep(ctx, ev.Node, ev.Step)
				applyCost(&v, ev.Step, s.costs(ctx, sess.ID))
				clock.stamp(&v, ev.Node)
				st.send("step", v)
				if isCaptionLang(ev.Node) {
					st.send("caption", map[string]string{"lang": ev.Node, "text": strings.TrimSpace(ev.Text)})
				}
			}
		},
	})
	if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		slog.Error("captions line", "session", sess.ID, "err", err)
		st.fail(err)
		return
	}
	st.send("done", map[string]any{"ok": true, "total_ms": clock.ms(time.Now())})
}
