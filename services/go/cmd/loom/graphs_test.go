package main

// graphs_test.go — the demos that run as Noewright graphs, end to end through
// the HTTP API in dry-run mode (no API key, canned model output).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright/schema"

	"github.com/rhaqim/demo/internal/platform"
)

type harness struct {
	t      *testing.T
	s      *server
	ts     *httptest.Server
	client *http.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := platform.OpenDB(filepath.Join(t.TempDir(), "demo.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := newServer(ctx, db)
	ts := httptest.NewServer(platform.WithVisitor(s.routes()))
	jar, _ := cookiejar.New(nil)
	t.Cleanup(func() {
		ts.Close()
		_ = s.e.Close(context.Background())
		db.Close()
	})
	return &harness{t: t, s: s, ts: ts, client: &http.Client{Jar: jar, Timeout: time.Minute}}
}

func (h *harness) post(path string, body any) (int, []byte) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := h.client.Post(h.ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

type sse struct {
	Event string
	Data  json.RawMessage
}

func events(t *testing.T, body []byte) []sse {
	t.Helper()
	var out []sse
	var cur sse
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		case line == "" && cur.Event != "":
			out = append(out, cur)
			cur = sse{}
		}
	}
	return out
}

// A forecast-desk matchday is one graph: the preview streams, the two pickers
// and bankroll report their steps, the settlement records the result, and the
// checkpoint it leaves supports a rewind.
func TestDeskMatchRunsAsAGraph(t *testing.T) {
	h := newHarness(t)
	code, body := h.post("/api/loom/desk", map[string]any{})
	if code != http.StatusCreated {
		t.Fatalf("open desk: %d %s", code, body)
	}
	var desk deskView
	_ = json.Unmarshal(body, &desk)

	play := func(matchday int) []sse {
		code, body := h.post("/api/loom/desk/"+desk.SessionID+"/play", map[string]any{"pick": "home", "stake": 10})
		if code != http.StatusOK {
			t.Fatalf("play %d: %d %s", matchday, code, body)
		}
		return events(t, body)
	}
	evs := play(1)
	seen := map[string]int{}
	agents := map[string]string{}
	for _, ev := range evs {
		seen[ev.Event]++
		if ev.Event == "error" {
			t.Fatalf("error event: %s", ev.Data)
		}
		if ev.Event == "step" {
			var v stepView
			_ = json.Unmarshal(ev.Data, &v)
			if v.Error != "" {
				t.Fatalf("step %s failed: %s", v.Agent, v.Error)
			}
			agents[v.Agent] = v.Role
		}
	}
	for _, want := range []string{"chunk", "risk", "result", "state", "done"} {
		if seen[want] == 0 {
			t.Errorf("no %q event in %v", want, seen)
		}
	}
	wantRoles := map[string]string{"fd-oddsmaker": "lead", "fd-value": "follower:fd-value",
		"fd-contrarian": "follower:fd-contrarian", "fd-bankroll": "follower:fd-bankroll", "fd-commentator": "commentary"}
	for a, r := range wantRoles {
		if agents[a] != r {
			t.Errorf("%s reported with role %q, want %q (all: %v)", a, agents[a], r, agents)
		}
	}
	var result deskResult
	for _, ev := range evs {
		if ev.Event == "result" {
			_ = json.Unmarshal(ev.Data, &result)
		}
	}
	if result.Matchday != 1 || result.Score == "" || result.Bets["you"].Stake != 10 {
		t.Fatalf("result = %+v", result)
	}

	play(2)
	code, body = h.post("/api/loom/desk/"+desk.SessionID+"/rewind", map[string]any{"matchday": 2})
	if code != http.StatusOK {
		t.Fatalf("rewind: %d %s", code, body)
	}
	var branch deskView
	_ = json.Unmarshal(body, &branch)
	if branch.Matchday != 2 || len(branch.History) != 1 || branch.History[0].Score != result.Score {
		t.Fatalf("rewound desk = matchday %d, %d results; want matchday 2 with matchday 1's result", branch.Matchday, len(branch.History))
	}
}

// A tabletop round is one graph: the opening round starts at the game master;
// a later round runs the rules agent, rolls the dice it asked for, then
// narrates.
func TestTabletopRoundRunsAsAGraph(t *testing.T) {
	h := newHarness(t)
	code, body := h.post("/api/loom/games", map[string]any{"title": "Bells Under Water", "world": "A drowned city.",
		"mode": "ai", "character": map[string]string{"name": "Ada", "archetype": "rogue"}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct{ Code string }
	_ = json.Unmarshal(body, &created)
	g := created.Code

	feed := func(round int, kind string) []string {
		rows, err := h.s.db.Query(`SELECT payload FROM demo_tt_feed WHERE code = ? AND round = ? AND kind = ? ORDER BY seq`, g, round, kind)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var p string
			_ = rows.Scan(&p)
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	waitFor := func(round int) {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			// The narration posts as soon as the GM finishes; the round is
			// recorded once the followers have too.
			var n int
			_ = h.s.db.QueryRow(`SELECT COUNT(*) FROM demo_tt_rounds WHERE code = ? AND round = ?`, g, round).Scan(&n)
			if len(feed(round, "narration")) > 0 && n > 0 {
				return
			}
			for _, sys := range feed(round-1, "system") {
				if strings.Contains(sys, `"tone":"error"`) {
					t.Fatalf("round %d failed: %s", round, sys)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("round %d never narrated", round)
	}

	if code, body := h.post("/api/loom/games/"+g+"/start", map[string]any{}); code != http.StatusAccepted {
		t.Fatalf("start: %d %s", code, body)
	}
	waitFor(1)
	if rolls := feed(1, "roll"); len(rolls) != 0 {
		t.Fatalf("the opening round rolled dice: %v", rolls)
	}

	if code, body := h.post("/api/loom/games/"+g+"/act", map[string]string{"action": "I pick the lock"}); code >= 300 {
		t.Fatalf("act: %d %s", code, body)
	}
	waitFor(2)
	rolls := feed(2, "roll")
	if len(rolls) != 1 || !strings.Contains(rolls[0], "DEX") {
		t.Fatalf("round 2 rolls = %v, want one DEX check for Ada", rolls)
	}
	narration := feed(2, "narration")[0]
	if !strings.Contains(narration, "Ada") {
		t.Fatalf("round 2 narration does not name the actor: %s", narration)
	}
	var cp1, cp2 int
	_ = h.s.db.QueryRow(`SELECT checkpoint FROM demo_tt_rounds WHERE code = ? AND round = 1`, g).Scan(&cp1)
	_ = h.s.db.QueryRow(`SELECT checkpoint FROM demo_tt_rounds WHERE code = ? AND round = 2`, g).Scan(&cp2)
	// Round 2 ran rules, gm and three followers: five steps after round 1's.
	if cp2-cp1 != 5 {
		t.Fatalf("checkpoints %d → %d: round 2 should add 5 steps", cp1, cp2)
	}
	// And they came from the graph: each step names its node.
	game, err := h.s.loadGame(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := uuid.Parse(game.SessionID)
	steps, err := h.s.e.Sessions().Steps(context.Background(), sid, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, st := range steps {
		if st.Index > cp1 {
			roles = append(roles, fmt.Sprint(st.Diagnostics["turn_role"]))
		}
	}
	slices.Sort(roles)
	want := []string{"node:chronicler", "node:gm", "node:referee", "node:rules", "node:suggest"}
	if !slices.Equal(roles, want) {
		t.Fatalf("round 2 step roles = %v, want %v", roles, want)
	}
}
