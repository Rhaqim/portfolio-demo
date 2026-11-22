package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"

	"github.com/rhaqim/demo/internal/platform"
)

// The Forecast Desk. A visitor plays a fictional league against two AI
// forecasters whose stakes a third agent manages and a hook polices.
//
// One Noewright turn per match:
//   lead      fd-oddsmaker  streams a preview of the card and what the odds imply
//   layer 1   fd-value, fd-contrarian   pick in parallel (each reads the preview)
//   layer 2   fd-bankroll   sizes both stakes (reads both picks, so Noewright plans it after them)
//   hook      risk referee  on fd-bankroll: an over-limit stake sends it back with the
//                           reason (ErrRetryWith); a second breach is trimmed
// then Go settles the match from its seed (a recorded step, so it is checkpointed)
// and fd-commentator calls it. Play money only, fictional teams only.

type bettor struct {
	Pick  string  `json:"pick"` // home | draw | away | "" (sat out)
	Stake int     `json:"stake"`
	Prob  float64 `json:"prob,omitempty"`
	Note  string  `json:"note,omitempty"`
	Delta int     `json:"delta"`
}

type deskResult struct {
	Matchday int                `json:"matchday"`
	Home     string             `json:"home"`
	Away     string             `json:"away"`
	Score    string             `json:"score"`
	Outcome  string             `json:"outcome"`
	Odds     map[string]float64 `json:"odds"`
	Bets     map[string]bettor  `json:"bets"`
}

type deskState struct {
	Matchday int               `json:"matchday"` // the next match
	Seed     uint64            `json:"seed"`
	Chips    map[string]int    `json:"chips"` // you, value, contrarian
	History  []deskResult      `json:"history"`
	Pending  map[string]bettor `json:"pending"` // agents' approved bets for the match in play
	Risk     []string          `json:"risk"`    // the referee's notes for the match in play
}

var deskBettors = []string{"you", "value", "contrarian"}

func loadDesk(st *noewright.State) deskState {
	var d deskState
	_ = json.Unmarshal(st.Snapshot, &d)
	if d.Chips == nil {
		d.Chips = map[string]int{}
	}
	return d
}

func saveDesk(st *noewright.State, d deskState) {
	b, _ := json.Marshal(d)
	st.Snapshot = b
}

var deskAgents = []ttAgent{
	{"fd-oddsmaker", dryRunSlug, `You host a forecasting desk for a fictional football league. Write a 90 to 130 word preview of the match
card: who is stronger and why, what recent form suggests, and what the odds imply as percentages. Be concrete and
lively, never certain: anything can happen in one match. The teams are fictional. Never mention real teams, real
bookmakers or real money, and never encourage gambling. Plain prose, no headings.`,
		`Matchday {{.Inputs.Matchday}}
{{.Inputs.Card}}

Desk odds (decimal, implied probability): {{.Inputs.Odds}}`},

	{"fd-value", dryRunSlug, `You are a value-seeking forecaster on a fictional league. From the card, estimate the probability of each
outcome yourself, without copying the odds. Pick the outcome where your probability times its decimal odds is
highest, but only if that product is above 1.05; otherwise pass.
Reply with only a JSON object, no prose and no code fences:
{"pick": "home|draw|away|pass", "probabilities": {"home": 0.45, "draw": 0.27, "away": 0.28}, "reason": "one sentence"}`,
		`{{.Inputs.Card}}

Odds: {{.Inputs.Odds}}

Desk preview:
{{.Inputs.Preview}}`},

	{"fd-contrarian", dryRunSlug, `You are a contrarian forecaster on a fictional league. Look for where the desk's preview and odds lean too
hard one way, and take the other side when the case is real. If the favourite genuinely deserves it, pass.
Reply with only a JSON object, no prose and no code fences:
{"pick": "home|draw|away|pass", "probabilities": {"home": 0.45, "draw": 0.27, "away": 0.28}, "reason": "one sentence"}`,
		`{{.Inputs.Card}}

Odds: {{.Inputs.Odds}}

Desk preview:
{{.Inputs.Preview}}`},

	{"fd-bankroll", dryRunSlug, `You manage the chip stacks of two forecasters. For each pick, size a stake with a quarter-Kelly fraction:
stake = chips * ((p * odds - 1) / (odds - 1)) / 4, using the forecaster's own probability p for its pick. A pass
stakes 0. Round down to whole chips. Respect each bettor's limit; the risk referee rejects anything above it.
Reply with only a JSON object, no prose and no code fences:
{"value": {"stake": 0, "note": "one short sentence"}, "contrarian": {"stake": 0, "note": "one short sentence"}}`,
		`Chips and limits: {{.Inputs.Chips}}
Odds: {{.Inputs.Odds}}

Value forecaster's pick:
{{.Inputs.ValuePick}}

Contrarian forecaster's pick:
{{.Inputs.ContrarianPick}}`},

	{"fd-commentator", dryRunSlug, `You call the full-time result for a fictional football league in 50 to 80 words: the score, how it fits or
defies the preview, and a line on who read it right among the three forecasters. Light and specific. Never
mention real teams or real money.`,
		`{{.Inputs.Card}}

Full time: {{.Inputs.Result}}

Bets: {{.Inputs.Bets}}`},

	{"fd-open", dryRunSlug, "The desk opens.", `(recorded)`},
	{"fd-settle", dryRunSlug, "The match is settled.", `(recorded)`},
}

func (s *server) seedDesk(ctx context.Context) error {
	for _, a := range deskAgents {
		if _, err := s.upsertAgent(ctx, ttOwner, "forecast", a.slug, a.generator, a.system, a.template); err != nil {
			return fmt.Errorf("seed %s: %w", a.slug, err)
		}
	}
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS demo_fd_rounds (
		desk TEXT NOT NULL, matchday INTEGER NOT NULL, session_id TEXT NOT NULL, checkpoint INTEGER NOT NULL,
		PRIMARY KEY (desk, matchday))`)
	return err
}

type agentPick struct {
	Pick          string             `json:"pick"`
	Probabilities map[string]float64 `json:"probabilities"`
	Reason        string             `json:"reason"`
}

func (p agentPick) valid() string {
	switch p.Pick {
	case "home", "draw", "away":
		return p.Pick
	}
	return ""
}

func (s *server) registerDeskHooks() {
	s.e.Hooks().RegisterPost("desk-state", func(_ context.Context, req *noewright.StepRequest, res noewright.Result) (noewright.Result, error) {
		if req.Session == nil || !strings.HasPrefix(req.AgentSlug, "fd-") {
			return res, nil
		}
		st := &req.Session.State
		d := loadDesk(st)
		switch req.AgentSlug {
		case "fd-bankroll":
			return s.riskReferee(req, res, st, d)
		case "fd-settle":
			var in struct {
				Matchday int    `json:"matchday"`
				Outcome  string `json:"outcome"`
				Score    string `json:"score"`
				You      bettor `json:"you"`
			}
			raw, _ := req.Inputs["Settle"].(string)
			if json.Unmarshal([]byte(raw), &in) != nil {
				return res, nil
			}
			f := makeFixture(d.Seed, in.Matchday)
			bets := map[string]bettor{"you": in.You}
			for k, v := range d.Pending {
				bets[k] = v
			}
			for who, b := range bets {
				if b.Pick != "" && b.Stake > 0 {
					b.Delta = payout(b.Stake, f.Odds[b.Pick], b.Pick == in.Outcome)
					d.Chips[who] = max(0, d.Chips[who]+b.Delta)
					bets[who] = b
				}
			}
			d.History = append(d.History, deskResult{Matchday: in.Matchday, Home: f.Home.Name, Away: f.Away.Name,
				Score: in.Score, Outcome: in.Outcome, Odds: f.Odds, Bets: bets})
			d.Matchday = in.Matchday + 1
			d.Pending, d.Risk = nil, nil
			saveDesk(st, d)
		}
		return res, nil
	})
}

// riskReferee polices the bankroll manager. The first over-limit stake sends
// the step back with the reason, which Noewright folds into the retry's prompt;
// on the retry anything still over the limit is trimmed and noted.
func (s *server) riskReferee(req *noewright.StepRequest, res noewright.Result, st *noewright.State, d deskState) (noewright.Result, error) {
	var stakes map[string]struct {
		Stake int    `json:"stake"`
		Note  string `json:"note"`
	}
	_ = extractJSON(noewright.ResultText(res), &stakes)
	picks := map[string]agentPick{}
	for who, key := range map[string]string{"value": "ValuePick", "contrarian": "ContrarianPick"} {
		var p agentPick
		raw, _ := req.Inputs[key].(string)
		_ = extractJSON(raw, &p)
		picks[who] = p
	}

	var breaches, notes []string
	pending := map[string]bettor{}
	for _, who := range []string{"value", "contrarian"} {
		p := picks[who]
		pick := p.valid()
		stake := stakes[who].Stake
		lim := limitFor(d.Chips[who])
		if pick == "" {
			stake = 0
		}
		if stake < 0 {
			stake = 0
		}
		if stake > lim.MaxStake {
			if req.Attempt() == 0 {
				breaches = append(breaches, fmt.Sprintf("%s's stake of %d is over its limit of %d (%s)", who, stake, lim.MaxStake, lim.Rule))
			} else {
				notes = append(notes, fmt.Sprintf("Trimmed %s's stake from %d to %d: %s.", who, stake, lim.MaxStake, lim.Rule))
				stake = lim.MaxStake
			}
		}
		b := bettor{Pick: pick, Stake: stake, Note: clip(stakes[who].Note, 160)}
		if pick != "" {
			b.Prob = p.Probabilities[pick]
		}
		if pick == "" {
			b.Stake = 0
		}
		pending[who] = b
	}
	if len(breaches) > 0 {
		// The engine records this attempt, then retries with the reason
		// appended to the system prompt.
		return nil, noewright.ErrRetryWith(noewright.RetryAnnotation{Reason: "The risk referee rejected these stakes: " + strings.Join(breaches, "; ") + ". Resize within the limits."})
	}
	if req.Attempt() > 0 && len(notes) == 0 {
		notes = append(notes, "The bankroll manager resized within the limits after the referee's first rejection.")
	}
	d.Pending, d.Risk = pending, notes
	saveDesk(st, d)
	return res, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

type deskView struct {
	DeskID    string         `json:"desk_id"`
	SessionID string         `json:"session_id"`
	Matchday  int            `json:"matchday"`
	Chips     map[string]int `json:"chips"`
	History   []deskResult   `json:"history"`
	Fixture   fixture        `json:"fixture"`
	Limits    limits         `json:"limits"` // for the visitor's next bet
	Seed      string         `json:"seed"`
}

func (s *server) deskView(sess *noewright.Session) deskView {
	d := loadDesk(&sess.State)
	id, _ := sess.State.Vars["desk_id"].(string)
	if d.History == nil {
		d.History = []deskResult{} // JSON [] rather than null
	}
	return deskView{
		DeskID: id, SessionID: sess.ID.String(), Matchday: d.Matchday, Chips: d.Chips,
		History: d.History, Fixture: makeFixture(d.Seed, d.Matchday), Limits: limitFor(d.Chips["you"]),
		Seed: strconv.FormatUint(d.Seed, 36),
	}
}

func (s *server) deskStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b [8]byte
	_, _ = rand.Read(b[:])
	d := deskState{Matchday: 1, Seed: binary.LittleEndian.Uint64(b[:]), Chips: map[string]int{}}
	for _, who := range deskBettors {
		d.Chips[who] = startChips
	}
	sess := &noewright.Session{PlatformID: platform.Visitor(r), Tags: []string{"forecast"},
		State: noewright.State{Modality: noewright.ModalityText, Vars: map[string]any{}}}
	saveDesk(&sess.State, d)
	if err := s.e.Sessions().Create(ctx, sess); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't open a desk")
		return
	}
	sess.State.Vars["desk_id"] = sess.ID.String()
	// Step 0 is the opening state, so "what if" on matchday 1 has a checkpoint to fork from.
	step, err := s.e.RecordStep(ctx, sess, noewright.ExternalStep{AgentSlug: "fd-open", Owner: ttOwner, Author: "desk",
		Result: noewright.NewTextResult("The desk opens with 1,000 chips each.", "stop", 0, 0)})
	if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		platform.Error(w, http.StatusInternalServerError, "couldn't open a desk")
		return
	}
	_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO demo_fd_rounds (desk, matchday, session_id, checkpoint) VALUES (?, 0, ?, ?)`,
		sess.ID.String(), sess.ID.String(), step.Index)
	platform.JSON(w, http.StatusCreated, s.deskView(sess))
}

func (s *server) ownedDesk(w http.ResponseWriter, r *http.Request) (*noewright.Session, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platform.Error(w, http.StatusNotFound, "desk not found")
		return nil, false
	}
	sess, err := s.e.Sessions().GetHeader(r.Context(), id)
	if err != nil || sess.PlatformID != platform.Visitor(r) {
		platform.Error(w, http.StatusNotFound, "desk not found")
		return nil, false
	}
	return sess, true
}

func (s *server) deskGet(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.ownedDesk(w, r); ok {
		platform.JSON(w, http.StatusOK, s.deskView(sess))
	}
}

func (s *server) deskPlay(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.ownedDesk(w, r)
	if !ok {
		return
	}
	var req struct {
		Pick  string `json:"pick"`
		Stake int    `json:"stake"`
	}
	if err := platform.Decode(w, r, 512, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	d := loadDesk(&sess.State)
	you := bettor{}
	if req.Pick != "" && req.Pick != "pass" {
		if req.Pick != "home" && req.Pick != "draw" && req.Pick != "away" {
			platform.Error(w, http.StatusBadRequest, "Pick home, draw or away, or sit this one out.")
			return
		}
		lim := limitFor(d.Chips["you"])
		if req.Stake < 1 || req.Stake > lim.MaxStake {
			platform.Error(w, http.StatusBadRequest, fmt.Sprintf("The risk referee says no: stakes run from 1 to %d chips (%s).", lim.MaxStake, lim.Rule))
			return
		}
		you = bettor{Pick: req.Pick, Stake: req.Stake}
	}
	if d.Matchday > 60 {
		platform.Error(w, http.StatusConflict, "The season is over. Open a new desk.")
		return
	}

	run, err := s.runnerFor(r)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	vid := platform.Visitor(r)
	release, err := s.acquire(vid)
	if err != nil {
		platform.Error(w, http.StatusTooManyRequests, err.Error())
		return
	}
	defer release()
	if err := s.ensureBudget(r.Context(), vid); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't set up the step budget")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()

	st, ok := newStream(w)
	if !ok {
		return
	}
	f := makeFixture(d.Seed, d.Matchday)
	st.send("session", map[string]any{"session_id": sess.ID, "dry_run": run.dry, "model": run.model.Label})
	st.send("fixture", f)
	st.send("bet", you)

	chips := []string{}
	for _, who := range []string{"value", "contrarian"} {
		lim := limitFor(d.Chips[who])
		chips = append(chips, fmt.Sprintf("%s has %d chips, limit %d per bet", who, d.Chips[who], lim.MaxStake))
	}
	inputs := map[string]any{
		"Matchday": strconv.Itoa(f.Matchday), "Card": f.card(), "Odds": f.oddsText(), "Chips": strings.Join(chips, "; "),
	}
	step := func(role string, x *noewright.Step) {
		v := s.viewStep(ctx, role, x)
		applyCost(&v, x, s.costs(ctx, sess.ID))
		st.send("step", v)
	}
	fa := func(slug, out string) noewright.FlowAgent {
		return noewright.FlowAgent{AgentSlug: slug, OutputKey: out, GeneratorOverride: run.generator, Overrides: run.overrides, Params: map[string]any{"effort": "low"}}
	}
	lead := fa("fd-oddsmaker", "Preview")
	lead.Stream, lead.Params = true, map[string]any{"effort": "medium"}
	bankroll := fa("fd-bankroll", "Stakes")
	bankroll.MaxRetries = 1

	_, err = s.e.RunTurn(ctx, sess, noewright.TurnRequest{
		Owner:  ttOwner,
		Flow:   noewright.Flow{Slug: "forecast-match", Lead: lead, Followers: []noewright.FlowAgent{fa("fd-value", "ValuePick"), fa("fd-contrarian", "ContrarianPick"), bankroll}},
		Inputs: inputs,
		OnChunk: func(c noewright.Chunk) {
			st.send("chunk", map[string]string{"text": c.Content})
		},
		OnStep: func(role string, x *noewright.Step, err error) {
			if err != nil || x == nil {
				st.send("step", stepView{Role: role, Agent: strings.TrimPrefix(role, "follower:"), Error: explain(err)})
				return
			}
			step(role, x)
		},
	})
	if err != nil {
		st.fail(err)
		return
	}
	d = loadDesk(&sess.State)
	st.send("risk", map[string]any{"notes": d.Risk, "pending": d.Pending})

	// Full time: Go decides the match from its seed; the settlement is a
	// recorded step, so the result and every chip change are checkpointed.
	h, a, outcome := f.play(d.Seed)
	score := fmt.Sprintf("%d-%d", h, a)
	settle, _ := json.Marshal(map[string]any{"matchday": f.Matchday, "outcome": outcome, "score": score, "you": you})
	settled, err := s.e.RecordStep(ctx, sess, noewright.ExternalStep{AgentSlug: "fd-settle", Owner: ttOwner, Author: "desk",
		Result: noewright.NewTextResult(fmt.Sprintf("Full time: %s %s %s.", f.Home.Name, score, f.Away.Name), "stop", 0, 0),
		Inputs: map[string]any{"Settle": string(settle)}})
	if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		st.fail(err)
		return
	}
	deskID, _ := sess.State.Vars["desk_id"].(string)
	_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO demo_fd_rounds (desk, matchday, session_id, checkpoint) VALUES (?, ?, ?, ?)`,
		deskID, f.Matchday, sess.ID.String(), settled.Index)
	d = loadDesk(&sess.State)
	last := d.History[len(d.History)-1]
	st.send("result", last)

	bets := []string{}
	for _, who := range deskBettors {
		b := last.Bets[who]
		if b.Pick == "" {
			bets = append(bets, who+" sat out")
		} else {
			bets = append(bets, fmt.Sprintf("%s staked %d on %s (%+d)", who, b.Stake, b.Pick, b.Delta))
		}
	}
	comment, err := s.e.RunStep(ctx, sess, noewright.StepRequest{
		AgentSlug: "fd-commentator", Owner: ttOwner, GeneratorOverride: run.generator, Overrides: run.overrides,
		Params:  map[string]any{"effort": "low"},
		Inputs:  map[string]any{"Card": f.card(), "Result": fmt.Sprintf("%s %s %s (%s)", f.Home.Name, score, f.Away.Name, outcome), "Bets": strings.Join(bets, "; ")},
		OnChunk: func(c noewright.Chunk) { st.send("comment", map[string]string{"text": c.Content}) },
	})
	if err == nil || errors.Is(err, noewright.ErrSessionNotPersisted) {
		step("commentary", comment)
	}
	st.send("state", s.deskView(sess))
	st.send("done", map[string]bool{"ok": true})
}

// deskRewind forks the desk at the end of the match before `matchday`, so that
// match can be replayed with a different bet. The seed decides the score, so
// the match itself plays out identically: only the choices change.
func (s *server) deskRewind(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.ownedDesk(w, r)
	if !ok {
		return
	}
	var req struct {
		Matchday int `json:"matchday"`
	}
	if err := platform.Decode(w, r, 128, &req); err != nil || req.Matchday < 1 {
		platform.Error(w, http.StatusBadRequest, "Pick a matchday to replay.")
		return
	}
	deskID, _ := sess.State.Vars["desk_id"].(string)
	var sid string
	var cp int
	if err := s.db.QueryRowContext(r.Context(), `SELECT session_id, checkpoint FROM demo_fd_rounds WHERE desk = ? AND matchday = ?`,
		deskID, req.Matchday-1).Scan(&sid, &cp); err != nil {
		platform.Error(w, http.StatusBadRequest, "That matchday hasn't been played on this desk.")
		return
	}
	parent, _ := uuid.Parse(sid)
	branch, err := s.e.Sessions().Fork(r.Context(), parent, cp)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't rewind")
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `DELETE FROM demo_fd_rounds WHERE desk = ? AND matchday >= ?`, deskID, req.Matchday)
	platform.JSON(w, http.StatusOK, s.deskView(branch))
}

func (s *server) deskBacktest(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	n = clamp(n, 20, 500)
	seed, err := strconv.ParseUint(r.URL.Query().Get("seed"), 36, 64)
	if err != nil {
		var b [8]byte
		_, _ = rand.Read(b[:])
		seed = binary.LittleEndian.Uint64(b[:])
	}
	platform.JSON(w, http.StatusOK, map[string]any{"seed": strconv.FormatUint(seed, 36), "matches": n, "strategies": backtest(seed, n), "start": startChips})
}
