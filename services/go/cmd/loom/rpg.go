package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	loom "github.com/rhaqim/loom"
)

// Rules shared by every tabletop game: character sheets, archetypes, dice,
// the referee's verdicts and the agent registry helper.

type sheet struct {
	Name      string         `json:"name"`
	Archetype string         `json:"archetype"`
	HP        int            `json:"hp"`
	MaxHP     int            `json:"max_hp"`
	Gold      int            `json:"gold"`
	Abilities map[string]int `json:"abilities"`
	Items     []string       `json:"items"`
	Condition string         `json:"condition"`
	Fallen    bool           `json:"fallen"`
}

func (s sheet) modifier(ability string) int {
	score, ok := s.Abilities[ability]
	if !ok {
		return 0
	}
	return (score - 10) / 2 // d20 convention; scores here never go below 8
}

func (s sheet) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s the %s. HP %d/%d. Gold %d. ", s.Name, s.Archetype, s.HP, s.MaxHP, s.Gold)
	for _, a := range abilityOrder {
		fmt.Fprintf(&b, "%s %+d ", a, s.modifier(a))
	}
	b.WriteString(". Carrying: ")
	if len(s.Items) == 0 {
		b.WriteString("nothing")
	} else {
		b.WriteString(strings.Join(s.Items, ", "))
	}
	if s.Condition != "" {
		b.WriteString(". Condition: " + s.Condition)
	}
	return b.String()
}

var abilityOrder = []string{"STR", "DEX", "CON", "INT", "WIS", "CHA"}

type archetype struct {
	ID    string         `json:"id"`
	Label string         `json:"label"`
	Blurb string         `json:"blurb"`
	HP    int            `json:"-"`
	Stats map[string]int `json:"-"`
	Items []string       `json:"-"`
}

var archetypes = []archetype{
	{ID: "fighter", Label: "Fighter", Blurb: "Hits hard, takes a beating.", HP: 14,
		Stats: map[string]int{"STR": 16, "DEX": 12, "CON": 15, "INT": 9, "WIS": 11, "CHA": 10},
		Items: []string{"longsword", "shield", "rope (50 ft)", "torch"}},
	{ID: "rogue", Label: "Rogue", Blurb: "Quick hands, quicker exits.", HP: 10,
		Stats: map[string]int{"STR": 10, "DEX": 16, "CON": 12, "INT": 13, "WIS": 11, "CHA": 14},
		Items: []string{"twin daggers", "lockpicks", "dark cloak", "caltrops"}},
	{ID: "wizard", Label: "Wizard", Blurb: "Knows things. Fragile about it.", HP: 8,
		Stats: map[string]int{"STR": 8, "DEX": 13, "CON": 12, "INT": 16, "WIS": 13, "CHA": 10},
		Items: []string{"oak staff", "spellbook", "chalk", "candle stubs"}},
	{ID: "cleric", Label: "Cleric", Blurb: "Patches wounds, stares down the dark.", HP: 11,
		Stats: map[string]int{"STR": 13, "DEX": 10, "CON": 14, "INT": 10, "WIS": 16, "CHA": 12},
		Items: []string{"mace", "holy symbol", "bandages", "lantern"}},
}

func newSheet(name, archetypeID string) (sheet, bool) {
	i := slices.IndexFunc(archetypes, func(a archetype) bool { return a.ID == archetypeID })
	if i < 0 {
		return sheet{}, false
	}
	a := archetypes[i]
	return sheet{Name: name, Archetype: a.Label, HP: a.HP, MaxHP: a.HP, Gold: 5,
		Abilities: maps(a.Stats), Items: slices.Clone(a.Items)}, true
}

func maps(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// worldPresets seed the host's world editor; hosts can write their own.
type worldPreset struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	World string `json:"world"`
	NPCs  []npc  `json:"npcs"`
}

var worldPresets = []worldPreset{
	{ID: "crypt", Label: "The Drowned Crypt",
		World: "A fishing village's founders lie in a crypt the tide has started to reclaim. Fishermen hear bells under the water at night, and two have gone missing. The village elders want it quiet; the missing men's families want answers.",
		NPCs: []npc{
			{Name: "Maren Holt", Persona: "The harbourmaster. Gruff, protective of the village's reputation, secretly paid to keep people away from the crypt."},
			{Name: "Old Teodor", Persona: "A half-deaf bell-ringer who talks in riddles and remembers the night the crypt was sealed."},
		}},
	{ID: "market", Label: "The Clockwork Bazaar",
		World: "A night market inside a stopped clock tower, where smugglers trade in stolen hours. Someone is selling a key that should not exist, and three factions want it before the tower's bells start again at dawn.",
		NPCs: []npc{
			{Name: "Vesh", Persona: "A fast-talking broker with brass fingers who never gives a straight price and always knows who is listening."},
			{Name: "Inspector Odile", Persona: "A tired, sharp city watch officer pretending to shop. She wants the key off the street, not anyone arrested."},
		}},
	{ID: "pass", Label: "The Hollow Pass",
		World: "A mountain pass where the snow never settles and travellers' footprints walk on without them. A caravan is three days overdue, and the only inn before the pass is half empty.",
		NPCs: []npc{
			{Name: "Brannoc", Persona: "The innkeeper. Warm, superstitious, keeps a candle burning for every traveller still on the mountain."},
		}},
}

// ---------------------------------------------------------------------------
// Referee verdicts
// ---------------------------------------------------------------------------

type verdict struct {
	HPDelta     int      `json:"hp_delta"`
	GoldDelta   int      `json:"gold_delta"`
	ItemsGained []string `json:"items_gained"`
	ItemsLost   []string `json:"items_lost"`
	Condition   string   `json:"condition"`
	Note        string   `json:"note"`
}

// apply folds a verdict into a sheet, clamped so a confused model (or a
// heavy-handed DM) cannot one-shot a character or mint a fortune.
func (v verdict) apply(sh *sheet) {
	sh.HP = clamp(sh.HP+clamp(v.HPDelta, -8, 8), 0, sh.MaxHP)
	sh.Gold = max(0, sh.Gold+clamp(v.GoldDelta, -100, 100))
	for _, lost := range v.ItemsLost {
		sh.Items = slices.DeleteFunc(sh.Items, func(it string) bool { return strings.EqualFold(it, strings.TrimSpace(lost)) })
	}
	for _, it := range v.ItemsGained {
		it = clip(strings.TrimSpace(it), 40)
		if it != "" && len(sh.Items) < 14 && !slices.Contains(sh.Items, it) {
			sh.Items = append(sh.Items, it)
		}
	}
	if c := strings.TrimSpace(v.Condition); c != "" || v.HPDelta != 0 {
		sh.Condition = clip(c, 40)
	}
	sh.Fallen = sh.HP == 0
	if sh.Fallen {
		sh.Condition = "fallen"
	}
}

// extractJSON tolerates models that wrap JSON in prose or code fences.
func extractJSON(text string, v any) error {
	text = strings.TrimSpace(text)
	start := strings.IndexAny(text, "{[")
	end := strings.LastIndexAny(text, "}]")
	if start < 0 || end <= start {
		return errors.New("no JSON found")
	}
	return json.Unmarshal([]byte(text[start:end+1]), v)
}

// ---------------------------------------------------------------------------
// Dice: plain Go, between agent calls.
// ---------------------------------------------------------------------------

type roll struct {
	Player  string `json:"player"`
	Ability string `json:"ability"`
	DC      int    `json:"dc"`
	Die     int    `json:"die"`
	Mod     int    `json:"mod"`
	Total   int    `json:"total"`
	Success bool   `json:"success"`
	Reason  string `json:"reason,omitempty"`
	Text    string `json:"text"`
}

func d20() int {
	n, _ := rand.Int(rand.Reader, big.NewInt(20))
	return int(n.Int64()) + 1
}

func check(sh sheet, ability string, dc int, reason string) roll {
	ability = strings.ToUpper(strings.TrimSpace(ability))
	if _, ok := sh.Abilities[ability]; !ok {
		ability = "DEX"
	}
	r := roll{Player: sh.Name, Ability: ability, DC: clamp(dc, 5, 25), Die: d20(), Mod: sh.modifier(ability), Reason: clip(reason, 160)}
	r.Total = r.Die + r.Mod
	r.Success = r.Die == 20 || (r.Die != 1 && r.Total >= r.DC)
	outcome := "failure"
	switch {
	case r.Die == 20:
		outcome = "natural 20, critical success"
	case r.Die == 1:
		outcome = "natural 1, critical failure"
	case r.Success:
		outcome = "success"
	}
	r.Text = fmt.Sprintf("%s: %s check DC %d, rolled %d %+d = %d, %s", sh.Name, ability, r.DC, r.Die, r.Mod, r.Total, outcome)
	return r
}

// ---------------------------------------------------------------------------
// Agent registry helper
// ---------------------------------------------------------------------------

var inputRef = regexp.MustCompile(`\{\{\s*\.Inputs\.([A-Za-z_][A-Za-z0-9_]*)`)

// upsertAgent makes sure the latest version of slug under owner has exactly
// these prompts and generator. Agents and prompts are immutable in loom, so a
// change is a new version; an unchanged definition reuses the existing one.
func (s *server) upsertAgent(ctx context.Context, owner, category, slug, generator, system, template string) (*loom.Agent, error) {
	vars := []string{}
	for _, m := range inputRef.FindAllStringSubmatch(template, -1) {
		if !slices.Contains(vars, m[1]) {
			vars = append(vars, m[1])
		}
	}

	version := 1
	if cur, err := s.e.Agents().Latest(ctx, owner, slug); err == nil {
		sp, err1 := s.e.Prompts().GetByID(ctx, cur.SystemPromptID)
		up, err2 := s.e.Prompts().GetByID(ctx, cur.UserTemplateID)
		if err1 == nil && err2 == nil && sp.Body == system && up.Body == template && cur.GeneratorSlug == generator {
			return cur, nil
		}
		version = cur.Version + 1
	} else if !errors.Is(err, loom.ErrNotFound) {
		return nil, err
	}

	promptVersion := func(pslug string) int {
		if p, err := s.e.Prompts().Latest(ctx, owner, pslug); err == nil {
			return p.Version + 1
		}
		return 1
	}
	sys := &loom.Prompt{Slug: slug + "-system", Owner: owner, Version: promptVersion(slug + "-system"),
		Kind: loom.PromptKindSystem, Category: category, Body: system}
	if err := s.e.Prompts().Create(ctx, sys); err != nil {
		return nil, err
	}
	usr := &loom.Prompt{Slug: slug + "-user", Owner: owner, Version: promptVersion(slug + "-user"),
		Kind: loom.PromptKindUserTemplate, Category: category, Body: template, Variables: vars}
	if err := s.e.Prompts().Create(ctx, usr); err != nil {
		return nil, err
	}
	a := &loom.Agent{Slug: slug, Owner: owner, Version: version, Category: category, Modal: loom.ModalityText,
		GeneratorSlug: generator, SystemPromptID: sys.ID, UserTemplateID: usr.ID}
	if err := s.e.Agents().Create(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// clip shortens s to at most n runes, never splitting a character.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }
