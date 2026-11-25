package studio

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rhaqim/noewright"
)

// simulator stands in for a model when the visitor has not brought a key. The
// orchestration around it is real: steps, routing, waits, costs (priced at the
// placeholder rate and marked estimated). Its text is deterministic:
//
//   - When the system prompt asks for one of a fixed set of answers
//     ("Answer with one of: REFUND, BUG, GENERAL"), it picks the answer whose
//     word, or a word from its hint list, appears in the user prompt, so
//     graphs that route on an agent's answer route the way a model would.
//   - Otherwise it says it is simulated and summarises what it was given.
type simulator struct{ e *noewright.Engine }

var _ noewright.StreamingGenerator = (*simulator)(nil)

func (s *simulator) Modality() noewright.Modality { return noewright.ModalityText }

func (s *simulator) Generate(_ context.Context, req noewright.GenerateRequest) (noewright.Result, error) {
	text := simulatedReply(req)
	return noewright.NewTextResult(text, "stop", tokens(req.SystemPrompt+req.UserPrompt), tokens(text)), nil
}

func (s *simulator) GenerateStream(ctx context.Context, req noewright.GenerateRequest) (<-chan noewright.Chunk, <-chan noewright.Result, error) {
	text := simulatedReply(req)
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
				time.Sleep(15 * time.Millisecond)
			}
		}
		results <- noewright.NewTextResult(text, "stop", tokens(req.SystemPrompt+req.UserPrompt), tokens(text))
	}()
	return chunks, results, nil
}

func tokens(s string) int { return max(1, len(s)/4) }

// oneOf matches "one of: A, B, C" (case-insensitive), optionally followed by
// hints in brackets: "REFUND [money, charge], BUG [crash, error]".
var oneOf = regexp.MustCompile(`(?i)one of:\s*([^\n.]+)`)
var option = regexp.MustCompile(`([A-Z][A-Z_]+)(?:\s*\[([^\]]*)\])?`)

func simulatedReply(req noewright.GenerateRequest) string {
	if m := oneOf.FindStringSubmatch(req.SystemPrompt); m != nil {
		opts := option.FindAllStringSubmatch(m[1], -1)
		if len(opts) > 0 {
			prompt := strings.ToLower(req.UserPrompt)
			for _, o := range opts {
				words := []string{strings.ToLower(o[1])}
				for _, h := range strings.Split(o[2], ",") {
					if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
						words = append(words, h)
					}
				}
				for _, w := range words {
					if strings.Contains(prompt, w) {
						return o[1]
					}
				}
			}
			return opts[len(opts)-1][1] // the last option is the catch-all
		}
	}
	summary := strings.Join(strings.Fields(req.UserPrompt), " ")
	if len(summary) > 220 {
		summary = summary[:220] + "…"
	}
	return fmt.Sprintf("(Simulated: add your API key for a real model.) Given %q, a model would answer here.", summary)
}
