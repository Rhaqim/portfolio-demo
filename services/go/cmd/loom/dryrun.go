package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rhaqim/noewright"
)

// dryRun is a Noewright Generator used when a visitor has not supplied a key. The
// engine, hooks, layering, budgets and cost ledger all run for real; only the
// model text is canned. For visitor-built pipelines it echoes the rendered
// prompt, which makes the data flow between agents visible.
type dryRun struct {
	e      *noewright.Engine
	bibles map[string]bible
}

var _ noewright.StreamingGenerator = (*dryRun)(nil)

func (d *dryRun) Modality() noewright.Modality { return noewright.ModalityText }

func (d *dryRun) Generate(ctx context.Context, req noewright.GenerateRequest) (noewright.Result, error) {
	text := d.reply(ctx, req)
	time.Sleep(250 * time.Millisecond) // long enough to see layers run in parallel
	return noewright.NewTextResult(text, "stop", len(req.SystemPrompt+req.UserPrompt)/4, len(text)/4), nil
}

func (d *dryRun) GenerateStream(ctx context.Context, req noewright.GenerateRequest) (<-chan noewright.Chunk, <-chan noewright.Result, error) {
	text := d.reply(ctx, req)
	chunks := make(chan noewright.Chunk, 64)
	results := make(chan noewright.Result, 1)
	go func() {
		defer close(chunks)
		defer close(results)
		for _, word := range strings.SplitAfter(text, " ") {
			select {
			case <-ctx.Done():
				results <- noewright.NewFailedResult(noewright.ModalityText, ctx.Err())
				return
			case chunks <- noewright.Chunk{Content: word}:
				time.Sleep(18 * time.Millisecond)
			}
		}
		results <- noewright.NewTextResult(text, "stop", len(req.SystemPrompt+req.UserPrompt)/4, len(text)/4)
	}()
	return chunks, results, nil
}

func (d *dryRun) reply(ctx context.Context, req noewright.GenerateRequest) string {
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
	case "sc-detect":
		line, _ := req.Inputs["Line"].(string)
		refs := detectRefs(line, d.bibles["kjv"])
		if refs == nil {
			refs = []scriptureRef{}
		}
		b, _ := json.Marshal(map[string]any{"refs": refs})
		return string(b)
	case "sc-caption":
		line, _ := req.Inputs["Line"].(string)
		return line
	case "sc-yo", "sc-ig", "sc-pcm":
		return dryCaption(slug, req)
	case "sc-judge":
		return `{"yo": {"score": 0, "confidence": "low", "note": "dry run: no judge was called"}, "ig": {"score": 0, "confidence": "low", "note": "dry run: no judge was called"}, "pcm": {"score": 0, "confidence": "low", "note": "dry run: no judge was called"}}`
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

// dryCaption stands in for a translator without a model. It never invents a
// translation: it shows the Bible text the real agent is given for a passage
// read aloud, and a placeholder for the rest. The Igbo stand-in leaves the
// passage out on its first try, so a dry run shows the scripture guard
// rejecting it and the retry passing.
func dryCaption(slug string, req noewright.GenerateRequest) string {
	code := strings.TrimPrefix(slug, "sc-")
	name := code
	for _, l := range captionLangs {
		if l.Code == code {
			name = l.Name
		}
	}
	var must []struct{ Label, Text string }
	if raw, _ := req.Inputs["Guard"].(string); raw != "" {
		_ = json.Unmarshal([]byte(raw), &must)
	}
	out := fmt.Sprintf("(Dry run · %s) [the %s caption of this line appears here when a model runs]", name, name)
	if code == "ig" && len(req.Annotations) == 0 && len(must) > 0 {
		return out
	}
	for _, p := range must {
		out += " " + p.Text
	}
	return out
}
