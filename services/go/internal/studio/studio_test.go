package studio

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/schema"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "studio.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, Config{DB: db, Dialect: noewright.DialectSQLite})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); _ = s.Close(context.Background()); db.Close() })
	return s, ts
}

type client struct {
	t  *testing.T
	ts *httptest.Server
	c  *http.Client
}

func newClient(t *testing.T, ts *httptest.Server) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, ts: ts, c: &http.Client{Jar: jar, Timeout: time.Minute}}
}

func (c *client) do(method, path string, body any, headers ...string) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type event struct {
	Name string
	Data map[string]any
}

func (c *client) run(slug string, body runRequest) []event {
	c.t.Helper()
	code, out := c.do("POST", "/api/studio/graphs/"+slug+"/run", body)
	if code != http.StatusOK {
		c.t.Fatalf("run %s: %d %s", slug, code, out)
	}
	var evs []event
	var cur event
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
		case line == "" && cur.Name != "":
			evs = append(evs, cur)
			cur = event{}
		}
	}
	return evs
}

func last(evs []event, name string) map[string]any {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Name == name {
			return evs[i].Data
		}
	}
	return nil
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestStudioEndToEnd(t *testing.T) {
	_, ts := newTestServer(t)
	c := newClient(t, ts)

	if code, out := c.do("GET", "/api/studio/session", nil); code != http.StatusOK || !strings.Contains(string(out), "visitor") {
		t.Fatalf("session: %d %s", code, out)
	}
	code, out := c.do("GET", "/api/studio/graphs", nil)
	var graphs []graphSummary
	_ = json.Unmarshal(out, &graphs)
	if code != http.StatusOK || len(graphs) != 3 {
		t.Fatalf("graphs: %d %s", code, out)
	}
	code, out = c.do("GET", "/api/studio/graphs/incident-postmortem", nil)
	if code != http.StatusOK || !strings.Contains(string(out), `"Data":true`) {
		t.Fatalf("graph plan should include inferred data edges: %d %.300s", code, out)
	}

	// Support desk: route, wait, resume, route again — simulated.
	evs := c.run("support-desk", runRequest{Say: "I was charged twice this month."})
	sess := last(evs, "session")
	if sess["simulated"] != true {
		t.Fatalf("session event = %v", sess)
	}
	done := last(evs, "done")
	if done["waiting"] != "next" || !slices.Contains(strs(done["ran"]), "refund") {
		t.Fatalf("first call done = %v", done)
	}
	if last(evs, "chunk") == nil {
		t.Fatal("the reply did not stream")
	}
	id := sess["session"].(string)
	evs = c.run("support-desk", runRequest{Session: id, Say: "Now the app crashes on start."})
	done = last(evs, "done")
	if done["resumed"] != "next" || !slices.Contains(strs(done["ran"]), "bug") {
		t.Fatalf("second call done = %v", done)
	}

	// Onboarding: text, then a choice.
	evs = c.run("onboarding", runRequest{Say: "hi"})
	oid := last(evs, "session")["session"].(string)
	if last(evs, "done")["waiting"] != "name" {
		t.Fatalf("onboarding start = %v", last(evs, "done"))
	}
	evs = c.run("onboarding", runRequest{Session: oid, Say: "Ada"})
	done = last(evs, "done")
	if done["waiting"] != "goal" || len(strs(done["options"])) != 3 {
		t.Fatalf("onboarding name = %v", done)
	}
	evs = c.run("onboarding", runRequest{Session: oid, Select: "build"})
	done = last(evs, "done")
	if done["waiting"] != "" || !slices.Contains(strs(done["ran"]), "plan") {
		t.Fatalf("onboarding goal = %v", done)
	}

	// Another visitor cannot use this conversation.
	other := newClient(t, ts)
	if code, _ := other.do("POST", "/api/studio/graphs/onboarding/run", runRequest{Session: oid, Say: "x"}); code != http.StatusNotFound {
		t.Fatalf("another visitor's conversation: %d", code)
	}

	// Keys are checked before anything runs.
	if code, out := c.do("POST", "/api/studio/graphs/onboarding/run", runRequest{Say: "x"}, "X-LLM-Key", "sk-ant-short", "X-LLM-Model", "claude-haiku-5-5"); code != http.StatusBadRequest {
		t.Fatalf("bad key: %d %s", code, out)
	}
	if code, out := c.do("POST", "/api/studio/graphs/onboarding/run", runRequest{Say: "x"}, "X-LLM-Key", "sk-proj-"+strings.Repeat("a", 30), "X-LLM-Model", "claude-haiku-5-5"); code != http.StatusBadRequest || !strings.Contains(string(out), "sk-ant-") {
		t.Fatalf("wrong provider key: %d %s", code, out)
	}

	// The bundle downloads, names its generator "chat", and loads elsewhere.
	code, out = c.do("GET", "/api/studio/graphs/support-desk/bundle", nil)
	if code != http.StatusOK {
		t.Fatalf("bundle: %d %s", code, out)
	}
	b, err := noewright.ParseBundle(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Requires.Generators) != 1 || b.Requires.Generators[0].Slug != "chat" || strings.Contains(string(out), "studio:") {
		t.Fatalf("bundle requires %+v (owner leaked: %v)", b.Requires, strings.Contains(string(out), "studio:"))
	}
	s2, _ := newTestServer(t)
	if _, err := s2.Engine().Bundles().Import(context.Background(), b, noewright.ImportOptions{Owner: "elsewhere", Generators: map[string]string{"chat": simulateSlug}}); err != nil {
		t.Fatalf("import the download elsewhere: %v", err)
	}
}

// Every template's own bundle tests pass against the simulator.
func TestTemplatesPassTheirTests(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	for _, tpl := range templates {
		data, err := templateFS.ReadFile("templates/" + tpl.File)
		if err != nil {
			t.Fatal(err)
		}
		b, err := noewright.ParseBundle(data)
		if err != nil {
			t.Fatalf("%s: %v", tpl.File, err)
		}
		owner := "test-" + tpl.Graph
		if _, err := s.e.Bundles().Import(ctx, b, noewright.ImportOptions{Owner: owner, Activate: true, Generators: map[string]string{"chat": simulateSlug}}); err != nil {
			t.Fatalf("%s: import: %v", tpl.File, err)
		}
		results, err := s.e.Bundles().Test(ctx, b, noewright.BundleTestOptions{Owner: owner})
		if err != nil {
			t.Fatalf("%s: %v", tpl.File, err)
		}
		if len(results) == 0 {
			t.Fatalf("%s has no tests", tpl.File)
		}
		for _, r := range results {
			if !r.Evidence.Passed {
				t.Errorf("%s: %q failed: %v", tpl.File, r.Evidence.Test, r.Evidence.Failures)
			}
		}
	}
}

// The quality screen: starter tests run and keep their last result; a
// conversation saved as a test runs too; the bundle carries both, with evidence.
func TestStudioTests(t *testing.T) {
	_, ts := newTestServer(t)
	c := newClient(t, ts)
	c.do("GET", "/api/studio/session", nil)

	code, out := c.do("GET", "/api/studio/graphs/support-desk/tests", nil)
	var views []testView
	_ = json.Unmarshal(out, &views)
	if code != http.StatusOK || len(views) != 1 || views[0].Source != "starter" || views[0].Last != nil {
		t.Fatalf("starter tests: %d %s", code, out)
	}

	type runOut struct {
		Results   []noewright.BundleTestResult `json:"results"`
		Simulated bool                         `json:"simulated"`
	}
	code, out = c.do("POST", "/api/studio/graphs/support-desk/tests/run", map[string]any{})
	var ro runOut
	_ = json.Unmarshal(out, &ro)
	if code != http.StatusOK || len(ro.Results) != 1 || !ro.Results[0].Evidence.Passed || !ro.Simulated {
		t.Fatalf("run starter tests: %d %s", code, out)
	}
	if ro.Results[0].Evidence.Model != simulateSlug || ro.Results[0].Texts["step1.triage"] != "REFUND" {
		t.Fatalf("evidence model %q, texts %v", ro.Results[0].Evidence.Model, ro.Results[0].Texts)
	}
	_, out = c.do("GET", "/api/studio/graphs/support-desk/tests", nil)
	_ = json.Unmarshal(out, &views)
	if views[0].Last == nil || !views[0].Last.Evidence.Passed {
		t.Fatalf("last result not kept: %s", out)
	}

	// Save a conversation as a test, with one expectation that will fail.
	mine := noewright.BundleTest{Name: "general question", Steps: []noewright.BundleTestStep{
		{Say: "How do I export my data?", Expect: noewright.BundleExpect{Waiting: "next", Ran: []string{"triage", "general"}}},
		{Say: "Thanks", Expect: noewright.BundleExpect{Waiting: "done"}},
	}}
	if code, out := c.do("POST", "/api/studio/graphs/support-desk/tests", mine); code != http.StatusCreated {
		t.Fatalf("save test: %d %s", code, out)
	}
	if code, _ := c.do("POST", "/api/studio/graphs/support-desk/tests", mine); code != http.StatusConflict {
		t.Fatalf("duplicate name: %d", code)
	}
	if code, _ := c.do("POST", "/api/studio/graphs/support-desk/tests", noewright.BundleTest{Name: "empty"}); code != http.StatusBadRequest {
		t.Fatalf("test with no steps: %d", code)
	}
	code, out = c.do("POST", "/api/studio/graphs/support-desk/tests/run", map[string]any{"only": []string{"general question"}})
	_ = json.Unmarshal(out, &ro)
	if code != http.StatusOK || len(ro.Results) != 1 || ro.Results[0].Evidence.Passed ||
		!strings.Contains(strings.Join(ro.Results[0].Evidence.Failures, " "), `waits at "next"`) {
		t.Fatalf("run my test: %d %s", code, out)
	}

	// Only your own tests can be deleted.
	if code, _ := c.do("DELETE", "/api/studio/graphs/support-desk/tests/a%20refund,%20then%20a%20bug,%20in%20one%20conversation", nil); code != http.StatusNotFound {
		t.Fatalf("delete a starter test: %d", code)
	}

	// The bundle carries both tests and the evidence of their last runs.
	code, out = c.do("GET", "/api/studio/graphs/support-desk/bundle", nil)
	b, err := noewright.ParseBundle(out)
	if code != http.StatusOK || err != nil || len(b.Tests) != 2 || len(b.Evidence) != 2 {
		t.Fatalf("bundle: %d, %v, %d tests, %d evidence", code, err, len(b.Tests), len(b.Evidence))
	}
	s2, _ := newTestServer(t)
	if probs := s2.Engine().Bundles().Verify(context.Background(), b, noewright.ImportOptions{Generators: map[string]string{"chat": simulateSlug}}); len(probs) != 0 {
		t.Fatalf("downloaded bundle does not verify: %v", probs)
	}

	if code, _ := c.do("DELETE", "/api/studio/graphs/support-desk/tests/general%20question", nil); code != http.StatusNoContent {
		t.Fatalf("delete my test: %d", code)
	}
}
