package main

// graphs.go — the demos' requests as Noewright graphs.
//
// A tabletop round and a forecast-desk match used to be several engine calls
// with Go in between: run the rules agent, roll dice, run a turn, record the
// settlement. Each is now one RunGraph call. The Go parts are graph functions
// registered once on the engine, and the run's steps, costs and checkpoint
// come back together.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rhaqim/noewright"
)

// registerGraphFuncs registers the Go functions the demo graphs call.
func (s *server) registerGraphFuncs() {
	s.e.RegisterFunc("tt-roll-dice", s.rollDice)
	s.e.RegisterFunc("fd-settle-match", settleMatch)
}

// ---------------------------------------------------------------------------
// Tabletop round
// ---------------------------------------------------------------------------

// tabletopRound is one AI-run round: the rules agent decides who needs a
// check, Go rolls the dice, the game master narrates (streaming), then the
// referee, chronicler and suggester work on the narration in parallel. The
// opening round has no actions to check, so it starts at the game master.
func tabletopRound(opening bool, run runner) noewright.Graph {
	agent := func(id, slug, out string) noewright.GraphNode {
		return noewright.GraphNode{ID: id, Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{
			AgentSlug: slug, OutputKey: out, GeneratorOverride: run.generator, Params: effort(slug)}}
	}
	rules := agent("rules", "tt-rules", "Rules")
	rules.Required = true
	gm := agent("gm", "tt-gm", "Narration")
	gm.Agent.Stream, gm.Required = true, true
	g := noewright.Graph{
		Slug:   "tabletop-round",
		Entry:  "rules",
		Inputs: []string{"Actions", "Rolls", "GameCode", "Round"},
		Nodes: []noewright.GraphNode{
			rules,
			{ID: "roll", Kind: noewright.NodeFunc, Func: "tt-roll-dice", OutputKey: "Rolls"},
			gm,
			agent("referee", "tt-referee", ""),
			agent("chronicler", "tt-chronicler", ""),
			agent("suggest", "tt-suggest", ""),
		},
		Edges: []noewright.GraphEdge{
			{From: "rules", To: []string{"roll"}},
			{From: "roll", To: []string{"gm"}},
			{From: "gm", To: []string{"referee", "chronicler", "suggest"}},
		},
	}
	if opening {
		g.Entry = "gm"
		g.Nodes = g.Nodes[2:]
		g.Edges = g.Edges[2:]
	}
	return g
}

// tabletopRole names a round's node the way the page labels steps.
func tabletopRole(node, agent string) string {
	switch node {
	case "rules":
		return "rules"
	case "gm":
		return "lead"
	}
	return "follower:" + agent
}

// rollDice rolls the checks the rules agent asked for, against the sheets in
// the game's state, and posts each roll to the game feed as it happens.
func (s *server) rollDice(_ context.Context, call noewright.FuncCall) (noewright.FuncResult, error) {
	gs := gameState{Players: map[string]*sheet{}}
	_ = json.Unmarshal(call.Snapshot, &gs)
	var decisions []struct {
		Name    string `json:"name"`
		Check   bool   `json:"check"`
		Ability string `json:"ability"`
		DC      int    `json:"dc"`
		Reason  string `json:"reason"`
	}
	rulesText, _ := call.Inputs["Rules"].(string)
	_ = extractJSON(rulesText, &decisions)
	code, _ := call.Inputs["GameCode"].(string)
	round, _ := call.Inputs["Round"].(int)
	var rolls []string
	for _, d := range decisions {
		sh := gs.byName(d.Name)
		if !d.Check || sh == nil {
			continue
		}
		r := check(*sh, d.Ability, d.DC, d.Reason)
		rolls = append(rolls, r.Text)
		if code != "" {
			s.post(code, round, "roll", r)
		}
	}
	if len(rolls) == 0 {
		rolls = []string{"No checks this round."}
	}
	return noewright.FuncResult{Text: strings.Join(rolls, "\n")}, nil
}

// ---------------------------------------------------------------------------
// Forecast desk
// ---------------------------------------------------------------------------

// deskMatch is one matchday: the oddsmaker previews it (streaming), the value
// and contrarian bettors pick in parallel, the bankroll agent sizes stakes,
// then Go plays the match from the desk's seed and settles every bet. The
// settlement is recorded as an fd-settle step, so the result and every chip
// change are checkpointed for rewinds.
func deskMatch(run runner) noewright.Graph {
	agent := func(id, slug, out string) noewright.GraphNode {
		return noewright.GraphNode{ID: id, Kind: noewright.NodeAgent, Agent: &noewright.FlowAgent{
			AgentSlug: slug, OutputKey: out, GeneratorOverride: run.generator, Params: map[string]any{"effort": "low"}}}
	}
	odds := agent("odds", "fd-oddsmaker", "Preview")
	odds.Agent.Stream, odds.Agent.Params, odds.Required = true, map[string]any{"effort": "medium"}, true
	value, contrarian := agent("value", "fd-value", "ValuePick"), agent("contrarian", "fd-contrarian", "ContrarianPick")
	bankroll := agent("bankroll", "fd-bankroll", "Stakes")
	bankroll.Agent.MaxRetries = 1
	// A failed pick or bankroll must not stop the match being played: the
	// match settles either way, as it always has.
	value.Continue, contrarian.Continue, bankroll.Continue = true, true, true
	return noewright.Graph{
		Slug:   "forecast-match",
		Entry:  "odds",
		Inputs: []string{"Matchday", "Card", "Odds", "Chips", "You"},
		Nodes: []noewright.GraphNode{odds, value, contrarian, bankroll,
			{ID: "settle", Kind: noewright.NodeFunc, Func: "fd-settle-match", Record: "fd-settle", OutputKey: "Result"}},
		Edges: []noewright.GraphEdge{
			{From: "odds", To: []string{"value", "contrarian"}},
			{From: "value", To: []string{"bankroll"}},
			{From: "contrarian", To: []string{"bankroll"}},
			{From: "bankroll", To: []string{"settle"}},
		},
	}
}

// deskRole names a match's node the way the page labels steps.
func deskRole(node, agent string) string {
	if node == "odds" {
		return "lead"
	}
	return "follower:" + agent
}

// settleMatch plays the match from the desk's seed. Its structured output
// carries the settlement the desk-state hook applies, and the risk referee's
// notes from the bankroll step, which the settlement then clears.
func settleMatch(_ context.Context, call noewright.FuncCall) (noewright.FuncResult, error) {
	var d deskState
	if err := json.Unmarshal(call.Snapshot, &d); err != nil {
		return noewright.FuncResult{}, fmt.Errorf("read the desk: %w", err)
	}
	matchday, err := strconv.Atoi(fmt.Sprint(call.Inputs["Matchday"])) // templates read it as text
	if err != nil {
		return noewright.FuncResult{}, fmt.Errorf("matchday %v: %w", call.Inputs["Matchday"], err)
	}
	you, _ := call.Inputs["You"].(bettor)
	f := makeFixture(d.Seed, matchday)
	h, a, outcome := f.play(d.Seed)
	score := fmt.Sprintf("%d-%d", h, a)
	settle, _ := json.Marshal(map[string]any{"matchday": f.Matchday, "outcome": outcome, "score": score, "you": you})
	text := fmt.Sprintf("Full time: %s %s %s.", f.Home.Name, score, f.Away.Name)
	return noewright.FuncResult{Text: text, Data: map[string]any{
		"text": text, "settle": string(settle), "score": score, "outcome": outcome,
		"risk": d.Risk, "pending": d.Pending,
	}}, nil
}
