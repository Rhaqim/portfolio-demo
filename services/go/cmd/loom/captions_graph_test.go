package main

// captions_graph_test.go — a captioned line as a graph, end to end through the
// HTTP API in dry-run mode.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
)

func (h *harness) postH(path string, body any, headers map[string]string) (int, []byte) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", h.ts.URL+path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []byte
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, out
}

func indexOf(evs []sse, match func(sse) bool) int {
	return slices.IndexFunc(evs, match)
}

func TestCaptionLineRunsAsAGraph(t *testing.T) {
	h := newHarness(t)
	code, body := h.post("/api/loom/captions", map[string]any{"title": "Sunday"})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("open service: %d %s", code, body)
	}
	var svc captionView
	_ = json.Unmarshal(body, &svc)

	// The config carries the graph the page draws.
	resp, err := h.client.Get(h.ts.URL + "/api/loom/captions/config")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Graph struct {
			Nodes []struct{ ID, Agent string }
			Edges [][2]string
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if len(cfg.Graph.Nodes) != 10 || !slices.Contains(cfg.Graph.Edges, [2]string{"start", "caption"}) {
		t.Fatalf("config graph = %+v", cfg.Graph)
	}

	line := "Proverbs 3:5 says, Trust in the LORD with all thine heart; and lean not unto thine own understanding."
	code, body = h.postH("/api/loom/captions/"+svc.ID+"/line", map[string]any{"text": line,
		"models": map[string]string{"sc-judge": "anthropic/claude-haiku-5-5"}}, nil)
	if code != http.StatusOK {
		t.Fatalf("line: %d %s", code, body)
	}
	evs := events(t, body)
	for _, ev := range evs {
		if ev.Event == "error" {
			t.Fatalf("error event: %s", ev.Data)
		}
	}
	node := func(id, kind string) func(sse) bool {
		return func(e sse) bool {
			if e.Event != "node" {
				return false
			}
			var n struct{ Node, Kind string }
			_ = json.Unmarshal(e.Data, &n)
			return n.Node == id && n.Kind == kind
		}
	}
	// The caption starts before the detector finishes: they run in parallel.
	if a, b := indexOf(evs, node("caption", "started")), indexOf(evs, node("detect", "done")); a < 0 || b < 0 || a > b {
		t.Fatalf("caption started at %d, detect done at %d: not in parallel", a, b)
	}
	// The line reaches the screen before the judge's scores.
	at := func(name string) int { return indexOf(evs, func(e sse) bool { return e.Event == name }) }
	if at("line") < 0 || at("judge") < 0 || at("line") > at("judge") || at("chunk") < 0 {
		t.Fatalf("line at %d, judge at %d, chunk at %d", at("line"), at("judge"), at("chunk"))
	}
	captions := 0
	for _, ev := range evs {
		if ev.Event == "caption" {
			captions++
		}
	}
	var scripture []lineRef
	_ = json.Unmarshal(evs[at("scripture")].Data, &scripture)
	if captions != 3 || len(scripture) == 0 || scripture[0].Mode != "quoted" {
		t.Fatalf("%d captions, scripture %+v", captions, scripture)
	}
	// The judge was given its own model; with no key for it, it runs dry.
	var session struct {
		Agents map[string]struct{ Mode, Model string }
	}
	_ = json.Unmarshal(evs[at("session")].Data, &session)
	if len(session.Agents) != 6 || session.Agents["sc-judge"].Mode != "simulated" {
		t.Fatalf("session agents = %+v", session.Agents)
	}

	// The stored line carries the captions and, once scored, the judge.
	resp, err = h.client.Get(h.ts.URL + "/api/loom/captions/" + svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&svc)
	resp.Body.Close()
	if len(svc.Lines) != 1 || len(svc.Lines[0].Captions) != 3 || svc.Lines[0].English == "" || len(svc.Lines[0].Judge) == 0 {
		t.Fatalf("stored line = %+v", svc.Lines)
	}

	// Keys are checked before anything runs.
	if code, _ := h.postH("/api/loom/captions/"+svc.ID+"/line", map[string]any{"text": "Amen."},
		map[string]string{"X-LLM-Key-OpenAI": "short"}); code != http.StatusBadRequest {
		t.Fatalf("bad key: %d", code)
	}
	if code, _ := h.postH("/api/loom/captions/"+svc.ID+"/line", map[string]any{"text": "Amen.",
		"models": map[string]string{"sc-nope": "anthropic/claude-haiku-5-5"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d", code)
	}

	// Two lines at once: the second waits for the first instead of failing.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, text := range []string{"Let us pray.", "Amen and amen."} {
		wg.Add(1)
		go func(i int, text string) {
			defer wg.Done()
			codes[i], _ = h.postH("/api/loom/captions/"+svc.ID+"/line", map[string]any{"text": text}, nil)
		}(i, text)
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("concurrent lines: %v", codes)
	}
	resp, _ = h.client.Get(h.ts.URL + "/api/loom/captions/" + svc.ID)
	_ = json.NewDecoder(resp.Body).Decode(&svc)
	resp.Body.Close()
	if len(svc.Lines) != 3 || svc.Lines[2].N != 3 {
		t.Fatalf("after three lines: %d lines", len(svc.Lines))
	}
}
