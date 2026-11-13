package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	loom "github.com/rhaqim/loom"
)

// dryRun is a loom Generator used when a visitor has not supplied a key. The
// engine, hooks, layering, budgets and cost ledger all run for real; only the
// model text is canned. For visitor-built pipelines it echoes the rendered
// prompt, which makes the data flow between agents visible.
type dryRun struct{ e *loom.Engine }

var _ loom.StreamingGenerator = (*dryRun)(nil)

func (d *dryRun) Modality() loom.Modality { return loom.ModalityText }

func (d *dryRun) Generate(ctx context.Context, req loom.GenerateRequest) (loom.Result, error) {
	text := d.reply(ctx, req)
	time.Sleep(250 * time.Millisecond) // long enough to see layers run in parallel
	return loom.NewTextResult(text, "stop", len(req.SystemPrompt+req.UserPrompt)/4, len(text)/4), nil
}

func (d *dryRun) GenerateStream(ctx context.Context, req loom.GenerateRequest) (<-chan loom.Chunk, <-chan loom.Result, error) {
	text := d.reply(ctx, req)
	chunks := make(chan loom.Chunk, 64)
	results := make(chan loom.Result, 1)
	go func() {
		defer close(chunks)
		defer close(results)
		for _, word := range strings.SplitAfter(text, " ") {
			select {
			case <-ctx.Done():
				results <- loom.NewFailedResult(loom.ModalityText, ctx.Err())
				return
			case chunks <- loom.Chunk{Content: word}:
				time.Sleep(18 * time.Millisecond)
			}
		}
		results <- loom.NewTextResult(text, "stop", len(req.SystemPrompt+req.UserPrompt)/4, len(text)/4)
	}()
	return chunks, results, nil
}

func (d *dryRun) reply(ctx context.Context, req loom.GenerateRequest) string {
	slug := ""
	if a, err := d.e.Agents().GetByID(ctx, req.AgentID); err == nil {
		slug = a.Slug
	}
	actors := names(req.Inputs["Actions"])
	switch slug {
	case "tt-rules":
		var out []map[string]any
		for _, n := range actors {
			out = append(out, map[string]any{"name": n, "check": true, "ability": "DEX", "dc": 12, "reason": "Dry run: every action gets a DEX check."})
		}
		b, _ := json.Marshal(out)
		return string(b)
	case "tt-gm":
		who := "the party"
		if len(actors) > 0 {
			who = strings.Join(actors, " and ")
		}
		return fmt.Sprintf("(Dry run: no model was called.) Lantern light gutters as %s press on. Somewhere below, water "+
			"drips on stone in a slow, patient rhythm. Every action, every dice result and the whole party's sheets "+
			"reached this agent in its prompt. The host can add an API key to hear a real game master. What do you do?", who)
	case "tt-referee":
		// Anyone whose roll failed takes a scrape, so the sheets visibly change.
		out := map[string]any{}
		rolls, _ := req.Inputs["Rolls"].(string)
		for _, line := range strings.Split(rolls, "\n") {
			if n, rest, ok := strings.Cut(line, ":"); ok && strings.Contains(rest, "failure") {
				out[strings.TrimSpace(n)] = map[string]any{"hp_delta": -1, "note": "Dry run: a failed roll costs a scrape."}
			}
		}
		b, _ := json.Marshal(out)
		return string(b)
	case "tt-chronicler":
		return "Dry run summary: the party pressed deeper, and every failed roll left a mark."
	case "tt-suggest":
		out := map[string][]string{}
		for _, n := range actors {
			out[n] = []string{"Search the room", "Charge the door"}
		}
		b, _ := json.Marshal(out)
		return string(b)
	case "fd-oddsmaker":
		card, _ := req.Inputs["Card"].(string)
		odds, _ := req.Inputs["Odds"].(string)
		first, _, _ := strings.Cut(card, "\n")
		return fmt.Sprintf("(Dry run: no model was called.) %s. On paper the home side has the edge, but recent form muddies it, "+
			"and one match is a coin with a weight on it, not a certainty. The desk prices it %s. Add an API key to hear a real host.", first, odds)
	case "fd-value":
		return `{"pick": "home", "probabilities": {"home": 0.52, "draw": 0.26, "away": 0.22}, "reason": "Dry run: the home side's attack outweighs the price."}`
	case "fd-contrarian":
		return `{"pick": "away", "probabilities": {"home": 0.38, "draw": 0.27, "away": 0.35}, "reason": "Dry run: the desk is overrating home advantage."}`
	case "fd-bankroll":
		// Deliberately over the contrarian's limit on the first try, so the dry
		// run shows the risk referee rejecting it and the retry resizing.
		if strings.Contains(req.SystemPrompt, "risk referee rejected") {
			return `{"value": {"stake": 40, "note": "Dry run: quarter-Kelly on a modest edge."}, "contrarian": {"stake": 60, "note": "Dry run: resized under the cap."}}`
		}
		return `{"value": {"stake": 40, "note": "Dry run: quarter-Kelly on a modest edge."}, "contrarian": {"stake": 250, "note": "Dry run: far too keen."}}`
	case "fd-commentator":
		res, _ := req.Inputs["Result"].(string)
		return fmt.Sprintf("(Dry run) Full time: %s. Somebody read that right and somebody will be rewinding it.", res)
	case "tt-npc":
		n, _ := req.Inputs["NpcName"].(string)
		return fmt.Sprintf("(Dry run) %s looks you over and says, \"Not here. Not with the tide coming in.\"", n)
	}
	preview := req.UserPrompt
	if len(preview) > 600 {
		preview = preview[:600] + "…"
	}
	return fmt.Sprintf("[dry run · %s] This agent received the prompt below. Add an API key to get real output.\n\n%s", slug, preview)
}

// names pulls the character names out of "Name: action" lines.
func names(v any) []string {
	s, _ := v.(string)
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if n, _, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(strings.TrimSpace(n), "(") {
			out = append(out, strings.TrimSpace(n))
		}
	}
	return out
}
