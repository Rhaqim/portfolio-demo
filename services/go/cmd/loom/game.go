package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"

	"github.com/rhaqim/demo/internal/platform"
)

// Multiplayer tabletop. One Noewright session holds the whole table's story; every
// change to the party (a join, a referee verdict, a DM's adjustment) is a
// Noewright step whose post-hook edits the typed game state in State.Snapshot, so
// Noewright checkpoints it and a fork at the end of any round rewinds everyone.
//
// The host picks the model and supplies the key. The key is held in process
// memory for that game only (never in the database), and every AI step in
// the game uses it, whoever's action triggered it.

const (
	ttOwner        = "" // tabletop agents are global; every game shares them
	maxPlayers     = 6
	gameStepBudget = 400 // agent steps per game per day, enforced by Noewright
	keyTTL         = 6 * time.Hour
)

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

type npc struct {
	Name    string `json:"name"`
	Persona string `json:"persona"`
}

// gameState is the typed state Noewright checkpoints with every step.
type gameState struct {
	Round   int               `json:"round"`
	Players map[string]*sheet `json:"players"` // by visitor id
}

func loadState(st *noewright.State) gameState {
	gs := gameState{Players: map[string]*sheet{}}
	_ = json.Unmarshal(st.Snapshot, &gs)
	if gs.Players == nil {
		gs.Players = map[string]*sheet{}
	}
	return gs
}

func saveState(st *noewright.State, gs gameState) {
	b, _ := json.Marshal(gs)
	st.Snapshot = b
	if st.Vars == nil {
		st.Vars = map[string]any{}
	}
	st.Vars["party_text"] = partyText(gs)
}

func partyText(gs gameState) string {
	if len(gs.Players) == 0 {
		return "No player characters yet."
	}
	lines := make([]string, 0, len(gs.Players))
	for _, sh := range gs.Players {
		lines = append(lines, "- "+sh.text())
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func (gs gameState) byName(name string) *sheet {
	for _, sh := range gs.Players {
		if strings.EqualFold(strings.TrimSpace(sh.Name), strings.TrimSpace(name)) {
			return sh
		}
	}
	return nil
}

type game struct {
	Code        string  `json:"code"`
	SessionID   string  `json:"-"`
	Host        string  `json:"-"`
	Title       string  `json:"title"`
	World       string  `json:"world"`
	Mode        string  `json:"mode"` // "ai" | "human"
	Bookkeeping bool    `json:"bookkeeping"`
	NPCs        []npc   `json:"npcs"`
	Model       string  `json:"model"` // provider/model, "" = dry run
	Status      string  `json:"status"`
	Round       int     `json:"round"`
	TotalUSD    float64 `json:"total_usd"`
	TotalIn     int     `json:"total_input_tokens"`
	TotalOut    int     `json:"total_output_tokens"`
	Priced      bool    `json:"priced"`
}

type playerRow struct {
	Visitor   string
	Name      string
	Archetype string
	Pending   string
}

// ---------------------------------------------------------------------------
// Rooms: live fan-out per game, plus the per-game run lock.
// ---------------------------------------------------------------------------

type event struct {
	ID    int64  // feed seq; 0 for live-only events
	Name  string // SSE event name
	Data  any
	Round int
}

type room struct {
	run   sync.Mutex // one resolution at a time
	mu    sync.Mutex
	subs  map[chan event]struct{}
	busy  bool
	rolls []roll // human-DM checks awaiting the next narration
}

type keyEntry struct {
	key     string
	model   string
	expires time.Time
}

type tables struct {
	mu    sync.Mutex
	rooms map[string]*room
	keys  map[string]keyEntry
}

func (t *tables) room(code string) *room {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rooms[code]
	if !ok {
		r = &room{subs: map[chan event]struct{}{}}
		t.rooms[code] = r
	}
	return r
}

func (t *tables) setKey(code, key, model string) {
	t.mu.Lock()
	t.keys[code] = keyEntry{key: key, model: model, expires: time.Now().Add(keyTTL)}
	t.mu.Unlock()
}

func (t *tables) key(code string) (keyEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k, ok := t.keys[code]
	if !ok || time.Now().After(k.expires) {
		delete(t.keys, code)
		return keyEntry{}, false
	}
	k.expires = time.Now().Add(keyTTL) // sliding: an active game keeps its key
	t.keys[code] = k
	return k, true
}

func (t *tables) dropKey(code string) {
	t.mu.Lock()
	delete(t.keys, code)
	t.mu.Unlock()
}

func (r *room) subscribe() chan event {
	ch := make(chan event, 256)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch
}

func (r *room) unsubscribe(ch chan event) {
	r.mu.Lock()
	delete(r.subs, ch)
	r.mu.Unlock()
}

func (r *room) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

func (r *room) broadcast(e event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- e:
		default: // a stalled client misses live chunks; the feed backfills the rest
		}
	}
}

// ---------------------------------------------------------------------------
// Schema and persistence
// ---------------------------------------------------------------------------

func (s *server) migrateGames(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS demo_tt_games (
	code TEXT PRIMARY KEY, session_id TEXT NOT NULL, host TEXT NOT NULL,
	title TEXT NOT NULL, world TEXT NOT NULL, mode TEXT NOT NULL, bookkeeping INTEGER NOT NULL,
	npcs TEXT NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL, round INTEGER NOT NULL DEFAULT 0,
	total_usd REAL NOT NULL DEFAULT 0, total_in INTEGER NOT NULL DEFAULT 0, total_out INTEGER NOT NULL DEFAULT 0,
	priced INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS demo_tt_players (
	code TEXT NOT NULL, visitor TEXT NOT NULL, name TEXT NOT NULL, archetype TEXT NOT NULL,
	pending TEXT NOT NULL DEFAULT '', joined_at TEXT NOT NULL, PRIMARY KEY (code, visitor));
CREATE TABLE IF NOT EXISTS demo_tt_rounds (
	code TEXT NOT NULL, round INTEGER NOT NULL, session_id TEXT NOT NULL, checkpoint INTEGER NOT NULL,
	usd REAL NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, priced INTEGER NOT NULL,
	PRIMARY KEY (code, round));
CREATE TABLE IF NOT EXISTS demo_tt_feed (
	code TEXT NOT NULL, seq INTEGER NOT NULL, round INTEGER NOT NULL, kind TEXT NOT NULL,
	payload TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (code, seq));`)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *server) loadGame(ctx context.Context, code string) (*game, error) {
	g := &game{}
	var npcs string
	var bk, priced int
	err := s.db.QueryRowContext(ctx, `SELECT code, session_id, host, title, world, mode, bookkeeping, npcs, model,
		status, round, total_usd, total_in, total_out, priced FROM demo_tt_games WHERE code = ?`, code).
		Scan(&g.Code, &g.SessionID, &g.Host, &g.Title, &g.World, &g.Mode, &bk, &npcs, &g.Model,
			&g.Status, &g.Round, &g.TotalUSD, &g.TotalIn, &g.TotalOut, &priced)
	if err != nil {
		return nil, err
	}
	g.Bookkeeping, g.Priced = bk == 1, priced == 1
	_ = json.Unmarshal([]byte(npcs), &g.NPCs)
	return g, nil
}

func (s *server) players(ctx context.Context, code string) ([]playerRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT visitor, name, archetype, pending FROM demo_tt_players WHERE code = ? ORDER BY joined_at`, code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []playerRow
	for rows.Next() {
		var p playerRow
		if rows.Scan(&p.Visitor, &p.Name, &p.Archetype, &p.Pending) == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// post appends a feed entry and broadcasts it. The feed is what late joiners
// and reconnecting clients replay.
func (s *server) post(code string, round int, kind string, payload any) {
	b, _ := json.Marshal(payload)
	var seq int64
	err := s.db.QueryRow(`INSERT INTO demo_tt_feed (code, seq, round, kind, payload, created_at)
		VALUES (?, COALESCE((SELECT MAX(seq) FROM demo_tt_feed WHERE code = ?), 0) + 1, ?, ?, ?, ?) RETURNING seq`,
		code, code, round, kind, string(b), now()).Scan(&seq)
	if err != nil {
		slog.Error("feed insert", "err", err)
		return
	}
	s.games.room(code).broadcast(event{ID: seq, Name: "feed", Round: round, Data: feedItem{Seq: seq, Round: round, Kind: kind, Payload: b}})
}

type feedItem struct {
	Seq     int64           `json:"seq"`
	Round   int             `json:"round"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// ---------------------------------------------------------------------------
// Agents and hooks
// ---------------------------------------------------------------------------

type ttAgent struct {
	slug, generator, system, template string
}

var gameAgents = []ttAgent{
	{"tt-rules", dryRunSlug, `You are the rules referee for a multiplayer tabletop roleplaying game that uses a simple d20 system.
For each player's action this round, decide whether it needs an ability check. Safe, trivial or purely descriptive
actions need none. Risky, contested or uncertain ones need one: pick the fitting ability (STR, DEX, CON, INT, WIS or
CHA) and a difficulty class (8 easy, 12 moderate, 15 hard, 18 very hard).
Reply with only a JSON array, one object per player listed, no prose and no code fences:
[{"name": "Ada", "check": true, "ability": "DEX", "dc": 12, "reason": "one short sentence"}]`,
		`Party:
{{.Vars.party_text}}

Scene so far:
{{.Vars.last_scene}}

Actions this round:
{{.Inputs.Actions}}`},

	{"tt-gm", dryRunSlug, `You are the game master of a multiplayer tabletop roleplaying game. Narrate in present tense, addressing the
player characters by name, in 150 to 260 words. Be vivid and specific, and keep it suitable for a general audience.
Rules you must follow:
- Resolve every player's action this round. Dice results are binding: a failure goes wrong, a success works.
- Voice the host's NPCs in character when they appear. Do not invent more than one new named character per scene.
- Do not state hit point numbers; a referee tracks them. You may describe injuries.
- Respect the world the host wrote. End with a short question asking the party what they do next.`,
		`World (written by the host):
{{.Vars.world}}

NPCs:
{{.Vars.npcs_text}}

Party:
{{.Vars.party_text}}

Story so far: {{.Vars.summary}}

Previous scene:
{{.Vars.last_scene}}

Actions this round:
{{.Inputs.Actions}}

Dice:
{{.Inputs.Rolls}}

Narrate what happens next.`},

	// Recorded with RecordStep / TurnRequest.LeadResult: a person writes the
	// result, so the generator is never called.
	{"tt-human-dm", dryRunSlug, "Narration written by the human game master.", `(recorded)`},

	{"tt-referee", dryRunSlug, `You are the bookkeeping referee for a multiplayer tabletop roleplaying game. Read the narration and decide its
mechanical consequences for each player character. Be conservative: most scenes change nothing for most characters.
A typical hit deals 1 to 4 damage, a severe blow up to 6. Heal only when the narration clearly heals.
Reply with only a JSON object keyed by character name, including only characters who changed, no prose, no fences:
{"Ada": {"hp_delta": -2, "gold_delta": 0, "items_gained": [], "items_lost": [], "condition": "", "note": "one short sentence"}}`,
		`Party before this scene:
{{.Vars.party_text}}

Dice:
{{.Inputs.Rolls}}

Narration:
{{.Inputs.Narration}}`},

	{"tt-chronicler", dryRunSlug, `You keep the running summary of a multiplayer tabletop roleplaying game. Merge the new scene into the existing
summary. Keep names, promises, clues and unresolved threats, and who did what. At most 110 words, plain prose, past
tense. Reply with the summary only.`,
		`Existing summary: {{.Vars.summary}}

New scene:
{{.Inputs.Narration}}`},

	{"tt-suggest", dryRunSlug, `Suggest two distinct next actions for each player character, given the scene. Each at most six words,
imperative mood: one cautious, one bold.
Reply with only a JSON object keyed by character name, no prose and no code fences: {"Ada": ["...", "..."]}`,
		`Party:
{{.Vars.party_text}}

Scene:
{{.Inputs.Narration}}`},

	{"tt-npc", dryRunSlug, "You voice one non-player character in a tabletop roleplaying game.", `Scene:
{{.Inputs.Scene}}

Direction from the game master: {{.Inputs.Direction}}

Speak as {{.Inputs.NpcName}}.`},

	{"tt-join", dryRunSlug, "A player joins the table.", `(recorded)`},
	{"tt-adjust", dryRunSlug, "The game master adjusts a character sheet.", `(recorded)`},
}

// effort per agent, sent as output_config.effort by the Anthropic adapter
// (other generators ignore it). Bookkeeping agents don't need deep thought.
var agentEffort = map[string]string{
	"tt-rules": "low", "tt-gm": "medium", "tt-referee": "low", "tt-chronicler": "low", "tt-suggest": "low", "tt-npc": "low",
}

func effort(slug string) map[string]any {
	if e, ok := agentEffort[slug]; ok {
		return map[string]any{"effort": e}
	}
	return nil
}

func (s *server) seedGames(ctx context.Context) error {
	for _, a := range gameAgents {
		if _, err := s.upsertAgent(ctx, ttOwner, "tabletop", a.slug, a.generator, a.system, a.template); err != nil {
			return fmt.Errorf("seed %s: %w", a.slug, err)
		}
	}
	return nil
}

func (s *server) registerGameHooks() {
	s.e.Hooks().RegisterPost("tabletop-state", func(_ context.Context, req *noewright.StepRequest, res noewright.Result) (noewright.Result, error) {
		if req.Session == nil || !strings.HasPrefix(req.AgentSlug, "tt-") {
			return res, nil
		}
		st := &req.Session.State
		if st.Vars == nil {
			st.Vars = map[string]any{}
		}
		text := noewright.ResultText(res)
		gs := loadState(st)
		switch req.AgentSlug {
		case "tt-gm", "tt-human-dm":
			st.Vars["last_scene"] = clip(text, 1800)
			gs.Round++
		case "tt-chronicler":
			// Followers run in parallel; since v0.0.3 (then Noewright) each step's state
			// merges three-way against what it started from, so the
			// chronicler's summary and the referee's sheets can't clobber
			// each other.
			st.Vars["summary"] = clip(text, 1000)
			return res, nil
		case "tt-referee":
			var verdicts map[string]verdict
			if extractJSON(text, &verdicts) == nil {
				for name, v := range verdicts {
					if sh := gs.byName(name); sh != nil {
						v.apply(sh)
					}
				}
			}
		case "tt-join":
			var j struct {
				Visitor string `json:"visitor"`
				Sheet   sheet  `json:"sheet"`
			}
			if raw, _ := req.Inputs["Join"].(string); json.Unmarshal([]byte(raw), &j) == nil && j.Visitor != "" {
				sh := j.Sheet
				gs.Players[j.Visitor] = &sh
			}
		case "tt-adjust":
			var a struct {
				Visitor string  `json:"visitor"`
				Verdict verdict `json:"verdict"`
			}
			if raw, _ := req.Inputs["Adjust"].(string); json.Unmarshal([]byte(raw), &a) == nil {
				if sh := gs.Players[a.Visitor]; sh != nil {
					a.Verdict.apply(sh)
				}
			}
		default:
			return res, nil
		}
		saveState(st, gs)
		return res, nil
	})
}

// ---------------------------------------------------------------------------
// Running steps for a game
// ---------------------------------------------------------------------------

// gameRunner resolves which generator a game's AI steps use. A game created
// with a model but whose key has gone (server restart, TTL) cannot run AI
// steps until the host re-enters the key.
func (s *server) gameRunner(g *game) (runner, error) {
	if g.Model == "" {
		return runner{generator: dryRunSlug, dry: true}, nil
	}
	k, ok := s.games.key(g.Code)
	if !ok {
		return runner{}, errNeedKey
	}
	m := s.models[k.model]
	return runner{generator: k.model, overrides: map[string]any{"api_key": k.key}, model: m}, nil
}

var errNeedKey = errors.New("The host's API key isn't loaded (the server restarted or it expired). The host needs to re-enter it.")

// stepCost is one agent step as the cost panel shows it.
type stepCost struct {
	Agent      string  `json:"agent"`
	Role       string  `json:"role"`
	In         int     `json:"input_tokens"`
	Out        int     `json:"output_tokens"`
	USD        float64 `json:"usd"`
	Priced     bool    `json:"priced"`
	DurationMs int     `json:"duration_ms"`
}

type ledger struct {
	steps []stepCost
}

func (l *ledger) add(v stepView) {
	if v.Agent == "tt-human-dm" || v.Agent == "tt-join" || v.Agent == "tt-adjust" || v.Error != "" {
		return
	}
	l.steps = append(l.steps, stepCost{Agent: v.Agent, Role: v.Role, In: v.InTokens, Out: v.OutTokens, USD: v.USD, Priced: v.Priced, DurationMs: v.DurationMs})
}

func (l *ledger) totals() (usd float64, in, out int, priced bool) {
	priced = true
	for _, s := range l.steps {
		usd += s.USD
		in += s.In
		out += s.Out
		priced = priced && s.Priced
	}
	return
}

// charge adds a ledger to the game's running totals and posts the cost entry.
func (s *server) charge(g *game, round int, label string, l *ledger, dry bool) {
	if len(l.steps) == 0 {
		return
	}
	usd, in, out, priced := l.totals()
	g.TotalUSD += usd
	g.TotalIn += in
	g.TotalOut += out
	g.Priced = g.Priced && priced
	_, _ = s.db.Exec(`UPDATE demo_tt_games SET total_usd = ?, total_in = ?, total_out = ?, priced = ?, updated_at = ? WHERE code = ?`,
		g.TotalUSD, g.TotalIn, g.TotalOut, boolInt(g.Priced), now(), g.Code)
	s.post(g.Code, round, "cost", map[string]any{
		"label": label, "steps": l.steps, "usd": usd, "input_tokens": in, "output_tokens": out,
		"priced": priced, "dry_run": dry, "game_usd": g.TotalUSD,
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *server) session(ctx context.Context, g *game) (*noewright.Session, error) {
	id, err := uuid.Parse(g.SessionID)
	if err != nil {
		return nil, err
	}
	return s.e.Sessions().GetHeader(ctx, id)
}

// joinStep makes a visitor a Noewright participant and records their character
// sheet as a step, so both are checkpointed: a fork from before the join
// drops the player and their sheet together.
func (s *server) joinStep(ctx context.Context, sess *noewright.Session, visitor, role string, sh *sheet) error {
	if err := s.e.AddParticipant(ctx, sess, noewright.Participant{ID: visitor, Role: role}); err != nil && !errors.Is(err, noewright.ErrParticipantExists) {
		return err
	}
	if sh == nil {
		return nil // a human game master has no sheet
	}
	payload, _ := json.Marshal(map[string]any{"visitor": visitor, "sheet": sh})
	_, err := s.e.RecordStep(ctx, sess, noewright.ExternalStep{
		AgentSlug: "tt-join", Owner: ttOwner, Author: "participant:" + visitor,
		Result: noewright.NewTextResult(fmt.Sprintf("%s the %s joins the party.", sh.Name, sh.Archetype), "stop", 0, 0),
		Inputs: map[string]any{"Join": string(payload)},
	})
	if errors.Is(err, noewright.ErrSessionNotPersisted) {
		return nil
	}
	return err
}

func (s *server) setBusy(code string, busy bool) {
	r := s.games.room(code)
	r.mu.Lock()
	r.busy = busy
	r.mu.Unlock()
	r.broadcast(event{Name: "busy", Data: map[string]bool{"busy": busy}})
}

// resolveRound runs one AI-DM round: the rules agent, Go dice, then a Noewright
// turn (streaming GM lead + referee, chronicler and suggester in parallel).
// It runs in the background; everything reaches players through the room.
func (s *server) resolveRound(code string, opening bool) {
	rm := s.games.room(code)
	if !rm.run.TryLock() {
		return // already resolving
	}
	defer rm.run.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	g, err := s.loadGame(ctx, code)
	if err != nil {
		return
	}
	run, err := s.gameRunner(g)
	if err != nil {
		s.post(code, g.Round, "system", map[string]string{"text": err.Error(), "tone": "error"})
		s.broadcastState(ctx, code)
		return
	}
	players, _ := s.players(ctx, code)
	var lines []string
	for _, p := range players {
		if p.Pending != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", p.Name, p.Pending))
		}
	}
	if !opening && len(lines) == 0 {
		return
	}
	actions := strings.Join(lines, "\n")
	if opening {
		actions = "(The adventure begins. Introduce the setting, bring the party together and give them a reason to act.)"
	}
	round := g.Round + 1

	s.setBusy(code, true)
	defer s.setBusy(code, false)

	sess, err := s.session(ctx, g)
	if err != nil {
		s.post(code, g.Round, "system", map[string]string{"text": "The game's session is missing.", "tone": "error"})
		return
	}
	gs := loadState(&sess.State)
	var led ledger
	views := func(role string, step *noewright.Step) stepView {
		v := s.viewStep(ctx, role, step)
		applyCost(&v, step, s.costs(ctx, sess.ID))
		return v
	}

	// 1. The rules agent decides who needs a check.
	var rolls []string
	if !opening {
		step, err := s.e.RunStep(ctx, sess, noewright.StepRequest{
			AgentSlug: "tt-rules", Owner: ttOwner, Inputs: map[string]any{"Actions": actions},
			GeneratorOverride: run.generator, Overrides: run.overrides, Params: effort("tt-rules"),
		})
		if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
			s.post(code, g.Round, "system", map[string]string{"text": explain(err), "tone": "error"})
			return
		}
		led.add(views("rules", step))

		// 2. Go rolls the dice the agent asked for.
		var decisions []struct {
			Name    string `json:"name"`
			Check   bool   `json:"check"`
			Ability string `json:"ability"`
			DC      int    `json:"dc"`
			Reason  string `json:"reason"`
		}
		_ = extractJSON(noewright.ResultText(step.Result), &decisions)
		for _, d := range decisions {
			sh := gs.byName(d.Name)
			if !d.Check || sh == nil {
				continue
			}
			r := check(*sh, d.Ability, d.DC, d.Reason)
			rolls = append(rolls, r.Text)
			s.post(code, round, "roll", r)
		}
	}
	if len(rolls) == 0 {
		rolls = []string{"No checks this round."}
	}

	// 3. One Noewright turn: the GM streams to every player, then three followers
	//    work on its narration in parallel.
	fa := func(slug string) noewright.FlowAgent {
		return noewright.FlowAgent{AgentSlug: slug, GeneratorOverride: run.generator, Overrides: run.overrides, Params: effort(slug)}
	}
	lead := fa("tt-gm")
	lead.Stream, lead.OutputKey = true, "Narration"
	turn, err := s.e.RunTurn(ctx, sess, noewright.TurnRequest{
		Owner:  ttOwner,
		Flow:   noewright.Flow{Slug: "tabletop-round", Lead: lead, Followers: []noewright.FlowAgent{fa("tt-referee"), fa("tt-chronicler"), fa("tt-suggest")}},
		Action: &noewright.Action{Kind: noewright.ActionFreeText, Payload: map[string]any{"text": actions}},
		Inputs: map[string]any{"Actions": actions, "Rolls": strings.Join(rolls, "\n")},
		// Players watch through Engine.Subscribe (see events); Noewright publishes
		// the lead's chunks to subscribers only when the caller streams.
		OnChunk: func(noewright.Chunk) {},
		OnStep: func(role string, step *noewright.Step, err error) {
			if err == nil && step != nil {
				led.add(views(role, step))
			}
		},
	})
	if err != nil {
		s.post(code, g.Round, "system", map[string]string{"text": explain(err), "tone": "error"})
		s.charge(g, g.Round, "failed round", &led, run.dry)
		return
	}
	s.finishRound(ctx, g, sess, turn, round, "Game master (AI)", &led, run.dry)
}

// finishRound records a narrated round: the feed entry, suggestions, cost,
// the checkpoint a rewind would fork at, and cleared actions.
func (s *server) finishRound(ctx context.Context, g *game, sess *noewright.Session, turn *noewright.Turn, round int, author string, led *ledger, dry bool) {
	var suggestions map[string][]string
	if fs, ok := turn.Followers["tt-suggest"]; ok {
		_ = extractJSON(noewright.ResultText(fs.Result), &suggestions)
	}
	checkpoint := 0
	for _, x := range turn.Steps {
		checkpoint = max(checkpoint, x.Index)
	}
	s.post(g.Code, round, "narration", map[string]any{
		"author": author, "text": noewright.ResultText(turn.Lead.Result), "suggestions": suggestions,
		"summary": sess.State.Vars["summary"],
	})
	usd, in, out, priced := led.totals()
	_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO demo_tt_rounds (code, round, session_id, checkpoint, usd, input_tokens, output_tokens, priced)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, g.Code, round, sess.ID.String(), checkpoint, usd, in, out, boolInt(priced && len(led.steps) > 0))
	_, _ = s.db.ExecContext(ctx, `UPDATE demo_tt_players SET pending = '' WHERE code = ?`, g.Code)
	_, _ = s.db.ExecContext(ctx, `UPDATE demo_tt_games SET round = ?, status = 'playing', updated_at = ? WHERE code = ?`, round, now(), g.Code)
	g.Round, g.Status = round, "playing"
	s.charge(g, round, fmt.Sprintf("round %d", round), led, dry)
	s.broadcastState(ctx, g.Code)
}

// ---------------------------------------------------------------------------
// Public state
// ---------------------------------------------------------------------------

type playerView struct {
	Name      string `json:"name"`
	Archetype string `json:"archetype"`
	Sheet     *sheet `json:"sheet"`
	Submitted bool   `json:"submitted"`
}

type gameView struct {
	*game
	Players     []playerView `json:"players"`
	ModelLabel  string       `json:"model_label"`
	InputPer1M  float64      `json:"input_per_1m"`
	OutputPer1M float64      `json:"output_per_1m"`
	KeyLoaded   bool         `json:"key_loaded"`
	Busy        bool         `json:"busy"`
	Watching    int          `json:"watching"`
	Rounds      []roundCost  `json:"rounds"`
	Summary     any          `json:"summary"`
}

type roundCost struct {
	Round  int     `json:"round"`
	USD    float64 `json:"usd"`
	In     int     `json:"input_tokens"`
	Out    int     `json:"output_tokens"`
	Priced bool    `json:"priced"`
}

func (s *server) view(ctx context.Context, code string) (*gameView, error) {
	g, err := s.loadGame(ctx, code)
	if err != nil {
		return nil, err
	}
	v := &gameView{game: g, Players: []playerView{}, Rounds: []roundCost{}}
	var gs gameState
	if sess, err := s.session(ctx, g); err == nil {
		gs = loadState(&sess.State)
		v.Summary = sess.State.Vars["summary"]
	}
	ps, _ := s.players(ctx, code)
	for _, p := range ps {
		v.Players = append(v.Players, playerView{Name: p.Name, Archetype: p.Archetype, Sheet: gs.Players[p.Visitor], Submitted: p.Pending != ""})
	}
	if m, ok := s.models[g.Model]; ok {
		v.ModelLabel, v.InputPer1M, v.OutputPer1M = m.Label, m.In, m.Out
	}
	_, v.KeyLoaded = s.games.key(code)
	if g.Model == "" {
		v.KeyLoaded = true
	}
	rm := s.games.room(code)
	rm.mu.Lock()
	v.Busy = rm.busy
	v.Watching = len(rm.subs)
	rm.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT round, usd, input_tokens, output_tokens, priced FROM demo_tt_rounds WHERE code = ? ORDER BY round`, code)
	if err == nil {
		for rows.Next() {
			var rc roundCost
			var pr int
			if rows.Scan(&rc.Round, &rc.USD, &rc.In, &rc.Out, &pr) == nil {
				rc.Priced = pr == 1
				v.Rounds = append(v.Rounds, rc)
			}
		}
		rows.Close()
	}
	return v, nil
}

func (s *server) broadcastState(ctx context.Context, code string) {
	if v, err := s.view(ctx, code); err == nil {
		s.games.room(code).broadcast(event{Name: "state", Data: v})
	}
}

// ---------------------------------------------------------------------------
// HTTP: creating, joining, watching
// ---------------------------------------------------------------------------

var (
	codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	nameRe       = regexp.MustCompile(`^[\p{L}][\p{L} '\-.]{0,19}$`)
)

func newCode() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	out := make([]byte, 6)
	for i := range b {
		out[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(out)
}

type createReq struct {
	Title       string `json:"title"`
	World       string `json:"world"`
	Mode        string `json:"mode"`
	Bookkeeping bool   `json:"bookkeeping"`
	NPCs        []npc  `json:"npcs"`
	Character   *struct {
		Name      string `json:"name"`
		Archetype string `json:"archetype"`
	} `json:"character"`
}

func (s *server) createGame(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := platform.Decode(w, r, 16<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Title = clip(strings.TrimSpace(req.Title), 60)
	req.World = clip(strings.TrimSpace(req.World), 1500)
	switch {
	case req.Title == "" || req.World == "":
		platform.Error(w, http.StatusBadRequest, "Give the game a title and describe the world.")
		return
	case req.Mode != "ai" && req.Mode != "human":
		platform.Error(w, http.StatusBadRequest, "Choose who runs the game.")
		return
	case len(req.NPCs) > 4:
		platform.Error(w, http.StatusBadRequest, "Up to four NPCs.")
		return
	case req.Mode == "ai" && req.Character == nil:
		platform.Error(w, http.StatusBadRequest, "With an AI game master, the host plays a character too.")
		return
	}
	var npcs []npc
	var npcLines []string
	for _, n := range req.NPCs {
		n.Name, n.Persona = clip(strings.TrimSpace(n.Name), 40), clip(strings.TrimSpace(n.Persona), 400)
		if n.Name != "" {
			npcs = append(npcs, n)
			npcLines = append(npcLines, fmt.Sprintf("- %s: %s", n.Name, n.Persona))
		}
	}
	if len(npcLines) == 0 {
		npcLines = []string{"None defined. Improvise minor characters as needed."}
	}

	run, err := s.runnerFor(r)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	var hostSheet sheet
	if req.Character != nil {
		name := strings.TrimSpace(req.Character.Name)
		sh, ok := newSheet(name, req.Character.Archetype)
		if !ok || !nameRe.MatchString(name) {
			platform.Error(w, http.StatusBadRequest, "Names are 1 to 20 letters, and pick an archetype.")
			return
		}
		hostSheet = sh
	}

	ctx := r.Context()
	if !run.dry {
		if _, err := s.verifyKey(ctx, run.model, run.overrides["api_key"].(string)); err != nil {
			platform.Error(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	code := newCode()
	for range 5 {
		var n int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM demo_tt_games WHERE code = ?`, code).Scan(&n)
		if n == 0 {
			break
		}
		code = newCode()
	}

	gs := gameState{Players: map[string]*sheet{}}
	sess := &noewright.Session{
		PlatformID: "game:" + code,
		Tags:       []string{"tabletop", "multiplayer"},
		State: noewright.State{Modality: noewright.ModalityText, Vars: map[string]any{
			"world": req.World, "npcs_text": strings.Join(npcLines, "\n"),
			"summary": "The adventure is just beginning.", "last_scene": "None yet. This is the opening scene.",
		}},
	}
	saveState(&sess.State, gs)
	if err := s.e.Sessions().Create(ctx, sess); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't start a session")
		return
	}
	if err := s.ensureGameBudget(ctx, code); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't set up the game's budget")
		return
	}

	npcJSON, _ := json.Marshal(npcs)
	modelSlug := ""
	if !run.dry {
		modelSlug = run.generator
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO demo_tt_games (code, session_id, host, title, world, mode, bookkeeping, npcs, model,
		status, round, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'lobby', 0, ?, ?)`,
		code, sess.ID.String(), platform.Visitor(r), req.Title, req.World, req.Mode, boolInt(req.Bookkeeping || req.Mode == "ai"),
		string(npcJSON), modelSlug, now(), now()); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't save the game")
		return
	}
	if !run.dry {
		s.games.setKey(code, run.overrides["api_key"].(string), run.generator)
	}
	s.post(code, 0, "system", map[string]string{"text": fmt.Sprintf("%s was created. Share the code %s so others can join.", req.Title, code)})
	if req.Mode == "human" {
		// The human game master is a participant too, so their narration
		// steps carry Action.ParticipantID and Step.Author.
		if err := s.joinStep(ctx, sess, platform.Visitor(r), "dm", nil); err != nil {
			platform.Error(w, http.StatusInternalServerError, "couldn't seat the game master")
			return
		}
	}
	if req.Character != nil {
		if err := s.addPlayer(ctx, code, sess, platform.Visitor(r), hostSheet, req.Character.Archetype); err != nil {
			platform.Error(w, http.StatusInternalServerError, "couldn't add your character")
			return
		}
	}
	platform.JSON(w, http.StatusCreated, map[string]string{"code": code})
}

func (s *server) ensureGameBudget(ctx context.Context, code string) error {
	return s.e.Budgets().Create(ctx, &noewright.Budget{
		Name:     "game-daily-steps",
		Target:   noewright.BudgetTarget{Kind: noewright.TargetPlatformID, Key: "game:" + code},
		Window:   noewright.BudgetWindowDay,
		Limit:    noewright.BudgetLimit{Steps: gameStepBudget},
		OnExceed: noewright.BudgetBlock,
		Active:   true,
	})
}

func (s *server) addPlayer(ctx context.Context, code string, sess *noewright.Session, visitor string, sh sheet, archetypeID string) error {
	if err := s.joinStep(ctx, sess, visitor, "player", &sh); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO demo_tt_players (code, visitor, name, archetype, joined_at) VALUES (?, ?, ?, ?, ?)`,
		code, visitor, sh.Name, archetypeID, now()); err != nil {
		return err
	}
	s.post(code, 0, "join", map[string]string{"name": sh.Name, "archetype": sh.Archetype})
	s.broadcastState(ctx, code)
	return nil
}

// gameFor loads the game named in the path. Unknown codes count against a
// per-IP limiter, so codes can't be enumerated quickly.
func (s *server) gameFor(w http.ResponseWriter, r *http.Request) (*game, bool) {
	code := strings.ToUpper(strings.TrimSpace(r.PathValue("code")))
	if len(code) != 6 {
		platform.Error(w, http.StatusNotFound, "No game with that code.")
		return nil, false
	}
	g, err := s.loadGame(r.Context(), code)
	if err != nil {
		if !s.misses.Allow(platform.ClientIP(r)) {
			platform.Error(w, http.StatusTooManyRequests, "Too many wrong codes. Wait a while.")
			return nil, false
		}
		platform.Error(w, http.StatusNotFound, "No game with that code.")
		return nil, false
	}
	return g, true
}

func (s *server) getGame(w http.ResponseWriter, r *http.Request) {
	g, ok := s.gameFor(w, r)
	if !ok {
		return
	}
	v, err := s.view(r.Context(), g.Code)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't load the game")
		return
	}
	me := map[string]any{"host": g.Host == platform.Visitor(r)}
	if ps, _ := s.players(r.Context(), g.Code); ps != nil {
		for _, p := range ps {
			if p.Visitor == platform.Visitor(r) {
				me["name"], me["pending"] = p.Name, p.Pending
			}
		}
	}
	platform.JSON(w, http.StatusOK, map[string]any{"game": v, "me": me})
}

func (s *server) joinGame(w http.ResponseWriter, r *http.Request) {
	g, ok := s.gameFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Name      string `json:"name"`
		Archetype string `json:"archetype"`
	}
	if err := platform.Decode(w, r, 1<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	sh, valid := newSheet(name, req.Archetype)
	if !valid || !nameRe.MatchString(name) {
		platform.Error(w, http.StatusBadRequest, "Names are 1 to 20 letters, and pick an archetype.")
		return
	}
	ctx := r.Context()
	if g.Status == "ended" {
		platform.Error(w, http.StatusConflict, "That game has ended.")
		return
	}
	if g.Host == platform.Visitor(r) && g.Mode == "human" {
		platform.Error(w, http.StatusConflict, "You're the game master of this table.")
		return
	}
	ps, _ := s.players(ctx, g.Code)
	for _, p := range ps {
		if p.Visitor == platform.Visitor(r) {
			platform.Error(w, http.StatusConflict, "You're already at this table.")
			return
		}
		if strings.EqualFold(p.Name, name) {
			platform.Error(w, http.StatusConflict, "Someone at this table already has that name.")
			return
		}
	}
	if len(ps) >= maxPlayers {
		platform.Error(w, http.StatusConflict, "This table is full.")
		return
	}
	rm := s.games.room(g.Code)
	if !rm.run.TryLock() { // never change the party mid-round
		platform.Error(w, http.StatusConflict, "A round is being played out. Try again in a moment.")
		return
	}
	defer rm.run.Unlock()
	sess, err := s.session(ctx, g)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't load the game")
		return
	}
	if err := s.addPlayer(ctx, g.Code, sess, platform.Visitor(r), sh, req.Archetype); err != nil {
		platform.Error(w, http.StatusInternalServerError, explain(err))
		return
	}
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// events streams the game to one client: the feed after Last-Event-ID, the
// current state, then live chunks, steps and feed entries.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	g, ok := s.gameFor(w, r)
	if !ok {
		return
	}
	rm := s.games.room(g.Code)
	if rm.count() >= 24 {
		platform.Error(w, http.StatusTooManyRequests, "This table has too many viewers.")
		return
	}
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if q := r.URL.Query().Get("after"); q != "" && after == 0 {
		after, _ = strconv.ParseInt(q, 10, 64)
	}
	st, ok := newStream(w)
	if !ok {
		return
	}
	ch := rm.subscribe()
	defer rm.unsubscribe(ch)

	sent := after
	rows, err := s.db.QueryContext(r.Context(), `SELECT seq, round, kind, payload FROM demo_tt_feed WHERE code = ? AND seq > ? ORDER BY seq`, g.Code, after)
	if err == nil {
		for rows.Next() {
			var it feedItem
			var payload string
			if rows.Scan(&it.Seq, &it.Round, &it.Kind, &payload) == nil {
				it.Payload = json.RawMessage(payload)
				st.sendID(it.Seq, "feed", it)
				sent = it.Seq
			}
		}
		rows.Close()
	}
	if v, err := s.view(r.Context(), g.Code); err == nil {
		st.send("state", v)
	}

	// Live agent activity comes straight from Noewright: Engine.Subscribe on the
	// game's session. A rewind forks a new session, so the room tells every
	// stream to resubscribe; a lagging stream is dropped by Noewright and resumes.
	ctx := r.Context()
	sub, live, sid := s.subscribeGame(ctx, g.Code)
	defer func() {
		if sub != nil {
			sub.Close()
		}
	}()

	ping := time.NewTicker(20 * time.Second) // Cloudflare drops idle streams after 100s
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			st.comment("ping")
		case ev, ok := <-live:
			if !ok {
				if ctx.Err() != nil {
					return
				}
				if sub != nil {
					sub.Close()
				}
				sub, live, sid = s.subscribeGame(ctx, g.Code)
				continue
			}
			s.forward(ctx, st, sid, ev)
		case e := <-ch:
			if e.Name == resubscribe {
				if sub != nil {
					sub.Close()
				}
				sub, live, sid = s.subscribeGame(ctx, g.Code)
				continue
			}
			if e.ID > 0 {
				if e.ID <= sent {
					continue
				}
				sent = e.ID
				st.sendID(e.ID, e.Name, e.Data)
			} else {
				st.send(e.Name, e.Data)
			}
		}
	}
}

// resubscribe is an internal room event: the game's session changed.
const resubscribe = "_resubscribe"

// subscribeGame subscribes to the game's current Noewright session, live events
// only (FromStep -1: nothing replayed; the feed already carries history).
// On failure it returns a nil channel, which simply never fires.
func (s *server) subscribeGame(ctx context.Context, code string) (*noewright.Subscription, <-chan noewright.SessionEvent, uuid.UUID) {
	g, err := s.loadGame(ctx, code)
	if err != nil {
		return nil, nil, uuid.Nil
	}
	sid, err := uuid.Parse(g.SessionID)
	if err != nil {
		return nil, nil, uuid.Nil
	}
	sub, err := s.e.Subscribe(ctx, sid, noewright.SubscribeOptions{FromStep: -1})
	if err != nil {
		slog.Warn("subscribe", "code", code, "err", err)
		return nil, nil, sid
	}
	return sub, sub.Events, sid
}

// forward turns Noewright session events into the page's "step" and "chunk"
// events. Recorded bookkeeping steps (joins, adjustments) stay out of the
// live list; they show up in the feed instead.
func (s *server) forward(ctx context.Context, st *stream, sid uuid.UUID, ev noewright.SessionEvent) {
	if ev.Agent == "tt-join" || ev.Agent == "tt-adjust" {
		return
	}
	role := ev.TurnRole
	switch {
	case ev.Agent == "tt-rules":
		role = "rules"
	case ev.Agent == "tt-npc":
		role = "npc"
	}
	switch ev.Type {
	case noewright.EventChunk:
		st.send("chunk", map[string]string{"text": ev.Chunk})
	case noewright.EventStepStarted:
		st.send("step_started", map[string]string{"agent": ev.Agent, "role": role})
	case noewright.EventStepFailed:
		st.send("step", stepView{Role: role, Agent: ev.Agent, Error: redact(ev.Err)})
	case noewright.EventStepDone:
		if ev.Step == nil {
			return
		}
		v := s.viewStep(ctx, role, ev.Step)
		applyCost(&v, ev.Step, s.costs(ctx, sid))
		st.send("step", v)
	}
}

// ---------------------------------------------------------------------------
// HTTP: playing
// ---------------------------------------------------------------------------

func (s *server) hostOnly(w http.ResponseWriter, r *http.Request) (*game, bool) {
	g, ok := s.gameFor(w, r)
	if !ok {
		return nil, false
	}
	if g.Host != platform.Visitor(r) {
		platform.Error(w, http.StatusForbidden, "Only the host can do that.")
		return nil, false
	}
	if g.Status == "ended" {
		platform.Error(w, http.StatusConflict, "That game has ended.")
		return nil, false
	}
	return g, true
}

func (s *server) startGame(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	if g.Status != "lobby" {
		platform.Error(w, http.StatusConflict, "The game has already started.")
		return
	}
	if g.Mode == "human" {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE demo_tt_games SET status = 'playing', updated_at = ? WHERE code = ?`, now(), g.Code)
		s.post(g.Code, 0, "system", map[string]string{"text": "The game master opens the table. Over to you, DM."})
		s.broadcastState(r.Context(), g.Code)
		platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if _, err := s.gameRunner(g); err != nil {
		platform.Error(w, http.StatusConflict, err.Error())
		return
	}
	go s.resolveRound(g.Code, true)
	platform.JSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *server) act(w http.ResponseWriter, r *http.Request) {
	g, ok := s.gameFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if err := platform.Decode(w, r, 2<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	action := clip(strings.TrimSpace(req.Action), 300)
	ctx := r.Context()
	switch {
	case action == "":
		platform.Error(w, http.StatusBadRequest, "Say what you do.")
		return
	case g.Status != "playing":
		platform.Error(w, http.StatusConflict, "The game hasn't started yet.")
		return
	}
	ps, _ := s.players(ctx, g.Code)
	var me *playerRow
	for i := range ps {
		if ps[i].Visitor == platform.Visitor(r) {
			me = &ps[i]
		}
	}
	if me == nil {
		platform.Error(w, http.StatusForbidden, "Join the table first.")
		return
	}
	if sess, err := s.session(ctx, g); err == nil {
		if sh := loadState(&sess.State).Players[me.Visitor]; sh != nil && sh.Fallen {
			platform.Error(w, http.StatusConflict, "Your character has fallen. The host can rewind, or the DM can heal you.")
			return
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE demo_tt_players SET pending = ? WHERE code = ? AND visitor = ?`, action, g.Code, me.Visitor); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't save that")
		return
	}
	s.post(g.Code, g.Round+1, "action", map[string]string{"name": me.Name, "text": action})
	s.broadcastState(ctx, g.Code)

	// With an AI game master, the round resolves once everyone standing has acted.
	if g.Mode == "ai" {
		all := true
		sess, _ := s.session(ctx, g)
		var gs gameState
		if sess != nil {
			gs = loadState(&sess.State)
		}
		ps, _ = s.players(ctx, g.Code)
		for _, p := range ps {
			if sh := gs.Players[p.Visitor]; p.Pending == "" && (sh == nil || !sh.Fallen) {
				all = false
			}
		}
		if all {
			go s.resolveRound(g.Code, false)
		}
	}
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) resolveNow(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	if g.Mode != "ai" || g.Status != "playing" {
		platform.Error(w, http.StatusConflict, "Only an AI-run game in progress resolves rounds.")
		return
	}
	go s.resolveRound(g.Code, false)
	platform.JSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// narrate records the human DM's narration as the lead of a Noewright turn. With
// bookkeeping on, AI referee, chronicler and suggester follow it in parallel,
// exactly as they follow the AI game master.
func (s *server) narrate(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := platform.Decode(w, r, 8<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	text := clip(strings.TrimSpace(req.Text), 3000)
	if g.Mode != "human" || text == "" {
		platform.Error(w, http.StatusBadRequest, "Write some narration.")
		return
	}
	var run runner
	var err error
	if g.Bookkeeping {
		if run, err = s.gameRunner(g); err != nil {
			platform.Error(w, http.StatusConflict, err.Error())
			return
		}
	}
	rm := s.games.room(g.Code)
	if !rm.run.TryLock() {
		platform.Error(w, http.StatusConflict, "Still working on the last step.")
		return
	}
	defer rm.run.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sess, err := s.session(ctx, g)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't load the game")
		return
	}
	ps, _ := s.players(ctx, g.Code)
	var lines []string
	for _, p := range ps {
		if p.Pending != "" {
			lines = append(lines, fmt.Sprintf("%s: %s", p.Name, p.Pending))
		}
	}
	rm.mu.Lock()
	var rolls []string
	for _, x := range rm.rolls {
		rolls = append(rolls, x.Text)
	}
	rm.rolls = nil
	rm.mu.Unlock()
	if len(rolls) == 0 {
		rolls = []string{"No checks this round."}
	}
	if len(lines) == 0 {
		lines = []string{"(No player actions this round.)"}
	}

	s.setBusy(g.Code, true)
	defer s.setBusy(g.Code, false)

	// A human-led turn: Noewright records the host's text as the lead step
	// (TurnRequest.LeadResult, no generator, no cost) and runs the AI
	// followers on it exactly as it would on the AI game master's narration.
	lead := noewright.FlowAgent{AgentSlug: "tt-human-dm", OutputKey: "Narration"}
	var followers []noewright.FlowAgent
	if g.Bookkeeping {
		for _, slug := range []string{"tt-referee", "tt-chronicler", "tt-suggest"} {
			followers = append(followers, noewright.FlowAgent{AgentSlug: slug, GeneratorOverride: run.generator, Overrides: run.overrides, Params: effort(slug)})
		}
	}
	var led ledger
	round := g.Round + 1
	turn, err := s.e.RunTurn(ctx, sess, noewright.TurnRequest{
		Owner:      ttOwner,
		Flow:       noewright.Flow{Slug: "tabletop-human-round", Lead: lead, Followers: followers},
		Action:     &noewright.Action{Kind: noewright.ActionFreeText, Payload: map[string]any{"text": text}, ParticipantID: g.Host},
		LeadResult: noewright.NewTextResult(text, "stop", 0, 0),
		LeadAuthor: "participant:" + g.Host,
		Inputs:     map[string]any{"Actions": strings.Join(lines, "\n"), "Rolls": strings.Join(rolls, "\n")},
		OnStep: func(role string, step *noewright.Step, err error) {
			if err == nil && step != nil {
				v := s.viewStep(ctx, role, step)
				applyCost(&v, step, s.costs(ctx, sess.ID))
				led.add(v)
			}
		},
	})
	if err != nil {
		platform.Error(w, http.StatusBadGateway, explain(err))
		return
	}
	s.finishRound(ctx, g, sess, turn, round, "Game master", &led, run.dry)
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// callCheck: the human DM asks a player for a roll; Go rolls it.
func (s *server) callCheck(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	var req struct {
		Player  string `json:"player"`
		Ability string `json:"ability"`
		DC      int    `json:"dc"`
	}
	if err := platform.Decode(w, r, 1<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, err := s.session(r.Context(), g)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't load the game")
		return
	}
	sh := loadState(&sess.State).byName(req.Player)
	if sh == nil {
		platform.Error(w, http.StatusBadRequest, "No such player.")
		return
	}
	rl := check(*sh, req.Ability, req.DC, "Called by the game master")
	rm := s.games.room(g.Code)
	rm.mu.Lock()
	rm.rolls = append(rm.rolls, rl)
	rm.mu.Unlock()
	s.post(g.Code, g.Round+1, "roll", rl)
	platform.JSON(w, http.StatusOK, rl)
}

// npcSpeak voices one of the host's NPCs. One agent serves every NPC: the
// persona goes in as a literal system prompt for this call only.
func (s *server) npcSpeak(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	var req struct {
		NPC       string `json:"npc"`
		Direction string `json:"direction"`
	}
	if err := platform.Decode(w, r, 2<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	i := slices.IndexFunc(g.NPCs, func(n npc) bool { return n.Name == req.NPC })
	if i < 0 {
		platform.Error(w, http.StatusBadRequest, "No such NPC.")
		return
	}
	n := g.NPCs[i]
	run, err := s.gameRunner(g)
	if err != nil {
		platform.Error(w, http.StatusConflict, err.Error())
		return
	}
	rm := s.games.room(g.Code)
	if !rm.run.TryLock() {
		platform.Error(w, http.StatusConflict, "Still working on the last step.")
		return
	}
	defer rm.run.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	sess, err := s.session(ctx, g)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't load the game")
		return
	}
	persona := fmt.Sprintf(`You are %s, a non-player character in a tabletop roleplaying game.
Persona: %s
World: %s
Reply in character in one to three sentences: spoken dialogue in quotes, plus at most one short action.
Never speak or decide for the player characters. Keep it suitable for a general audience.`, n.Name, n.Persona, clip(g.World, 600))
	scene, _ := sess.State.Vars["last_scene"].(string)
	step, err := s.e.RunStep(ctx, sess, noewright.StepRequest{
		AgentSlug: "tt-npc", Owner: ttOwner,
		SystemPromptOverride: &noewright.PromptRef{Literal: persona},
		Inputs:               map[string]any{"Scene": scene, "Direction": clip(strings.TrimSpace(req.Direction), 300), "NpcName": n.Name},
		GeneratorOverride:    run.generator, Overrides: run.overrides, Params: effort("tt-npc"),
	})
	if err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		platform.Error(w, http.StatusBadGateway, explain(err))
		return
	}
	v := s.viewStep(ctx, "npc", step)
	applyCost(&v, step, s.costs(ctx, sess.ID))
	var led ledger
	led.add(v)
	s.post(g.Code, g.Round, "npc", map[string]string{"name": n.Name, "text": noewright.ResultText(step.Result)})
	s.charge(g, g.Round, "NPC: "+n.Name, &led, run.dry)
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// adjust is the human DM's direct edit of a character, recorded as a step so
// it is checkpointed like everything else.
func (s *server) adjust(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	var req struct {
		Player string  `json:"player"`
		Change verdict `json:"change"`
	}
	if err := platform.Decode(w, r, 2<<10, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	rm := s.games.room(g.Code)
	if !rm.run.TryLock() {
		platform.Error(w, http.StatusConflict, "Still working on the last step.")
		return
	}
	defer rm.run.Unlock()
	ctx := r.Context()
	sess, err := s.session(ctx, g)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't load the game")
		return
	}
	gs := loadState(&sess.State)
	visitor := ""
	for v, sh := range gs.Players {
		if strings.EqualFold(sh.Name, req.Player) {
			visitor = v
		}
	}
	if visitor == "" {
		platform.Error(w, http.StatusBadRequest, "No such player.")
		return
	}
	payload, _ := json.Marshal(map[string]any{"visitor": visitor, "verdict": req.Change})
	desc := describeChange(req.Player, req.Change)
	if _, err := s.e.RecordStep(ctx, sess, noewright.ExternalStep{AgentSlug: "tt-adjust", Owner: ttOwner, Author: "participant:" + g.Host,
		Result: noewright.NewTextResult(desc, "stop", 0, 0),
		Inputs: map[string]any{"Adjust": string(payload)}}); err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		platform.Error(w, http.StatusInternalServerError, explain(err))
		return
	}
	s.post(g.Code, g.Round, "adjust", map[string]string{"text": desc})
	s.broadcastState(ctx, g.Code)
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func describeChange(name string, v verdict) string {
	var parts []string
	if v.HPDelta != 0 {
		parts = append(parts, fmt.Sprintf("%+d HP", v.HPDelta))
	}
	if v.GoldDelta != 0 {
		parts = append(parts, fmt.Sprintf("%+d gold", v.GoldDelta))
	}
	for _, it := range v.ItemsGained {
		parts = append(parts, "gains "+it)
	}
	for _, it := range v.ItemsLost {
		parts = append(parts, "loses "+it)
	}
	if v.Condition != "" {
		parts = append(parts, "now "+v.Condition)
	}
	if len(parts) == 0 {
		parts = []string{"no change"}
	}
	return fmt.Sprintf("The game master adjusts %s: %s.", name, strings.Join(parts, ", "))
}

// rewind forks the game's session at the end of an earlier round. Noewright
// rebuilds the branch from that step's checkpoint, so every sheet, the
// summary and the last scene go back together.
func (s *server) rewind(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	var req struct {
		Round int `json:"round"`
	}
	if err := platform.Decode(w, r, 256, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	rm := s.games.room(g.Code)
	if !rm.run.TryLock() {
		platform.Error(w, http.StatusConflict, "Still working on the last step.")
		return
	}
	defer rm.run.Unlock()
	ctx := r.Context()
	var sid string
	var cp int
	if err := s.db.QueryRowContext(ctx, `SELECT session_id, checkpoint FROM demo_tt_rounds WHERE code = ? AND round = ?`, g.Code, req.Round).Scan(&sid, &cp); err != nil {
		platform.Error(w, http.StatusBadRequest, "No such round.")
		return
	}
	parent, _ := uuid.Parse(sid)
	branch, err := s.e.Sessions().Fork(ctx, parent, cp)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't rewind")
		return
	}
	// Players who joined after that round are re-added on the new branch.
	gs := loadState(&branch.State)
	ps, _ := s.players(ctx, g.Code)
	for _, p := range ps {
		if gs.Players[p.Visitor] == nil {
			if sh, ok := newSheet(p.Name, p.Archetype); ok {
				_ = s.joinStep(ctx, branch, p.Visitor, "player", &sh)
			}
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE demo_tt_games SET session_id = ?, round = ?, updated_at = ? WHERE code = ?`, branch.ID.String(), req.Round, now(), g.Code)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM demo_tt_rounds WHERE code = ? AND round > ?`, g.Code, req.Round)
	_, _ = s.db.ExecContext(ctx, `UPDATE demo_tt_players SET pending = '' WHERE code = ?`, g.Code)
	rm.mu.Lock()
	rm.rolls = nil
	rm.mu.Unlock()
	s.games.room(g.Code).broadcast(event{Name: resubscribe})
	s.post(g.Code, req.Round, "rewind", map[string]any{"to_round": req.Round, "text": fmt.Sprintf("The host rewound the story to the end of round %d. loom forked the session at step %d; the old timeline still exists.", req.Round, cp)})
	s.broadcastState(ctx, g.Code)
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) setGameKey(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	run, err := s.runnerFor(r)
	if err != nil || run.dry {
		platform.Error(w, http.StatusBadRequest, "Enter your API key and model.")
		return
	}
	if g.Model != "" && run.generator != g.Model {
		platform.Error(w, http.StatusBadRequest, "Use the same model the game was created with.")
		return
	}
	if _, err := s.verifyKey(r.Context(), run.model, run.overrides["api_key"].(string)); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	s.games.setKey(g.Code, run.overrides["api_key"].(string), run.generator)
	s.post(g.Code, g.Round, "system", map[string]string{"text": "The host reconnected their API key."})
	s.broadcastState(r.Context(), g.Code)
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) endGame(w http.ResponseWriter, r *http.Request) {
	g, ok := s.hostOnly(w, r)
	if !ok {
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `UPDATE demo_tt_games SET status = 'ended', updated_at = ? WHERE code = ?`, now(), g.Code)
	s.games.dropKey(g.Code)
	s.post(g.Code, g.Round, "system", map[string]string{"text": "The host ended the game. Thanks for playing."})
	s.broadcastState(r.Context(), g.Code)
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// gameJanitor ends idle games (dropping their keys) and deletes old ones.
func (s *server) gameJanitor() {
	for range time.Tick(15 * time.Minute) {
		idle := time.Now().UTC().Add(-12 * time.Hour).Format(time.RFC3339)
		old := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
		rows, err := s.db.Query(`SELECT code FROM demo_tt_games WHERE status != 'ended' AND updated_at < ?`, idle)
		if err == nil {
			var codes []string
			for rows.Next() {
				var c string
				if rows.Scan(&c) == nil {
					codes = append(codes, c)
				}
			}
			rows.Close()
			for _, c := range codes {
				_, _ = s.db.Exec(`UPDATE demo_tt_games SET status = 'ended' WHERE code = ?`, c)
				s.games.dropKey(c)
			}
		}
		for _, q := range []string{
			`DELETE FROM demo_tt_feed WHERE code IN (SELECT code FROM demo_tt_games WHERE updated_at < ?)`,
			`DELETE FROM demo_tt_players WHERE code IN (SELECT code FROM demo_tt_games WHERE updated_at < ?)`,
			`DELETE FROM demo_tt_rounds WHERE code IN (SELECT code FROM demo_tt_games WHERE updated_at < ?)`,
			`DELETE FROM demo_tt_games WHERE updated_at < ?`,
		} {
			_, _ = s.db.Exec(q, old)
		}
	}
}

var _ = sql.ErrNoRows
