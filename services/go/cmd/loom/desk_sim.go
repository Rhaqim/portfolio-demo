package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
)

// The Forecast Desk's world: a fictional eight-team league whose matches are
// decided in Go from a seed. The same seed and matchday always produce the
// same match card and the same score, which is what makes "what if I'd bet
// differently?" (a session fork) meaningful.

type team struct {
	Name    string  `json:"name"`
	Attack  float64 `json:"attack"`
	Defense float64 `json:"defense"`
}

var league = []team{
	{"Harbour Rovers", 1.22, 1.10},
	{"Kiln City", 1.10, 0.96},
	{"Northgate Athletic", 0.94, 1.18},
	{"Ashford Meridian", 1.04, 1.02},
	{"Port Laine", 0.86, 0.90},
	{"Vale United", 1.16, 0.88},
	{"Calder Town", 0.90, 1.06},
	{"Brightwater", 0.98, 0.94},
}

const (
	goalBase   = 1.32
	homeEdge   = 1.12
	deskMargin = 1.06 // the odds carry a 6% margin, as a real desk's would
	startChips = 1000
)

var outcomes = []string{"home", "draw", "away"}

type side struct {
	team
	Form string `json:"form"` // last five, newest last: W D L
}

type fixture struct {
	Matchday int                `json:"matchday"`
	Home     side               `json:"home"`
	Away     side               `json:"away"`
	Odds     map[string]float64 `json:"odds"`
	Implied  map[string]float64 `json:"implied"` // from the odds, margin included
	fair     map[string]float64 // the model's true probabilities; never sent
	lambda   [2]float64
}

func rng(seed uint64, matchday int, salt uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed^uint64(matchday)*0x9E3779B97F4A7C15, salt))
}

func makeFixture(seed uint64, matchday int) fixture {
	r := rng(seed, matchday, 1)
	h := r.IntN(len(league))
	a := (h + 1 + r.IntN(len(league)-1)) % len(league)
	home, away := league[h], league[a]
	lh := goalBase * home.Attack / away.Defense * homeEdge
	la := goalBase * away.Attack / home.Defense
	fair := outcomeProbs(lh, la)
	f := fixture{
		Matchday: matchday,
		Home:     side{home, form(r, home)},
		Away:     side{away, form(r, away)},
		Odds:     map[string]float64{},
		Implied:  map[string]float64{},
		fair:     fair,
		lambda:   [2]float64{lh, la},
	}
	for _, o := range outcomes {
		odds := math.Max(1.01, math.Round(100/(fair[o]*deskMargin))/100)
		f.Odds[o] = odds
		f.Implied[o] = math.Round(1000/odds) / 1000
	}
	return f
}

// form invents a recent-results string that leans on the team's strength.
func form(r *rand.Rand, t team) string {
	strength := (t.Attack + t.Defense) / 2
	var b strings.Builder
	for range 5 {
		x := r.Float64() * strength
		switch {
		case x > 0.62:
			b.WriteByte('W')
		case x > 0.38:
			b.WriteByte('D')
		default:
			b.WriteByte('L')
		}
	}
	return b.String()
}

func poisson(k int, l float64) float64 {
	return math.Exp(-l) * math.Pow(l, float64(k)) / math.Gamma(float64(k)+1)
}

func outcomeProbs(lh, la float64) map[string]float64 {
	p := map[string]float64{"home": 0, "draw": 0, "away": 0}
	for i := 0; i <= 10; i++ {
		for j := 0; j <= 10; j++ {
			q := poisson(i, lh) * poisson(j, la)
			switch {
			case i > j:
				p["home"] += q
			case i == j:
				p["draw"] += q
			default:
				p["away"] += q
			}
		}
	}
	total := p["home"] + p["draw"] + p["away"]
	for k := range p {
		p[k] /= total
	}
	return p
}

// play decides the match. Knuth's Poisson sampler on the fixture's own seed.
func (f fixture) play(seed uint64) (int, int, string) {
	r := rng(seed, f.Matchday, 2)
	draw := func(l float64) int {
		L, k, p := math.Exp(-l), 0, 1.0
		for {
			p *= r.Float64()
			if p <= L {
				return k
			}
			k++
		}
	}
	h, a := draw(f.lambda[0]), draw(f.lambda[1])
	switch {
	case h > a:
		return h, a, "home"
	case h == a:
		return h, a, "draw"
	}
	return h, a, "away"
}

func (f fixture) card() string {
	return fmt.Sprintf("%s (home) vs %s (away)\n%s: attack %.2f, defence %.2f, form %s\n%s: attack %.2f, defence %.2f, form %s",
		f.Home.Name, f.Away.Name,
		f.Home.Name, f.Home.Attack, f.Home.Defense, f.Home.Form,
		f.Away.Name, f.Away.Attack, f.Away.Defense, f.Away.Form)
}

func (f fixture) oddsText() string {
	return fmt.Sprintf("home %.2f (%.0f%%), draw %.2f (%.0f%%), away %.2f (%.0f%%)",
		f.Odds["home"], f.Implied["home"]*100, f.Odds["draw"], f.Implied["draw"]*100, f.Odds["away"], f.Implied["away"]*100)
}

func (f fixture) favourite() string {
	best := "home"
	for _, o := range outcomes {
		if f.Odds[o] < f.Odds[best] {
			best = o
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// The risk referee's rules, shared by the hook, the visitor's bets and the
// backtest, so every bettor plays under the same limits.
// ---------------------------------------------------------------------------

type limits struct {
	MaxStake int    `json:"max_stake"`
	Rule     string `json:"rule"`
}

func limitFor(chips int) limits {
	if chips <= 0 {
		return limits{0, "out of chips"}
	}
	if chips < startChips*3/10 {
		return limits{max(1, chips*2/100), "stop-loss: under 30% of starting chips, stakes capped at 2%"}
	}
	return limits{max(1, chips/10), "stakes capped at 10% of chips"}
}

func payout(stake int, odds float64, won bool) int {
	if stake <= 0 {
		return 0
	}
	if won {
		return int(math.Round(float64(stake) * (odds - 1)))
	}
	return -stake
}

// ---------------------------------------------------------------------------
// Backtest: the agents' rules of thumb as plain code over many seeded matches.
// No model calls; this is what each strategy would do with infinite patience.
// ---------------------------------------------------------------------------

type strategyRun struct {
	Name    string  `json:"name"`
	Blurb   string  `json:"blurb"`
	Chips   []int   `json:"chips"` // after each match
	Final   int     `json:"final"`
	Bets    int     `json:"bets"`
	Wins    int     `json:"wins"`
	Brier   float64 `json:"brier"` // mean multi-class Brier score; lower is better
	MaxDraw int     `json:"max_drawdown"`
}

type strategy struct {
	name, blurb string
	// forecast returns probabilities and a pick ("" = pass) with a stake fraction.
	decide func(f fixture, r *rand.Rand) (probs map[string]float64, pick string, frac float64)
}

var strategies = []strategy{
	{"Value hunter", "Noisy model of the match; bets the outcome whose edge over the odds is largest, quarter-Kelly stakes.",
		func(f fixture, r *rand.Rand) (map[string]float64, string, float64) {
			est := map[string]float64{}
			total := 0.0
			for _, o := range outcomes {
				est[o] = math.Max(0.02, f.fair[o]+r.NormFloat64()*0.05)
				total += est[o]
			}
			best, edge := "", 1.05
			for _, o := range outcomes {
				est[o] /= total
				if e := est[o] * f.Odds[o]; e > edge {
					best, edge = o, e
				}
			}
			if best == "" {
				return est, "", 0
			}
			kelly := (est[best]*f.Odds[best] - 1) / (f.Odds[best] - 1)
			return est, best, kelly / 4
		}},
	{"Contrarian", "Fades a strong favourite: backs the other side at a flat 2% when the favourite is priced over 55%.",
		func(f fixture, _ *rand.Rand) (map[string]float64, string, float64) {
			fav := f.favourite()
			probs := shade(f.Implied, fav, -0.06)
			if f.Implied[fav] < 0.55 {
				return probs, "", 0
			}
			other := "away"
			if fav == "away" {
				other = "home"
			}
			return probs, other, 0.02
		}},
	{"Favourite backer", "Always backs the favourite at a flat 3%. What most people do.",
		func(f fixture, _ *rand.Rand) (map[string]float64, string, float64) {
			return normalise(f.Implied), f.favourite(), 0.03
		}},
	{"Coin flip", "Picks at random at a flat 2%: the baseline every strategy has to beat.",
		func(f fixture, r *rand.Rand) (map[string]float64, string, float64) {
			return map[string]float64{"home": 1.0 / 3, "draw": 1.0 / 3, "away": 1.0 / 3}, outcomes[r.IntN(3)], 0.02
		}},
}

func shade(p map[string]float64, from string, by float64) map[string]float64 {
	out := normalise(p)
	out[from] += by
	for _, o := range outcomes {
		if o != from {
			out[o] -= by / 2
		}
	}
	return normalise(out)
}

func normalise(p map[string]float64) map[string]float64 {
	out := map[string]float64{}
	t := 0.0
	for _, o := range outcomes {
		t += math.Max(p[o], 0.001)
	}
	for _, o := range outcomes {
		out[o] = math.Max(p[o], 0.001) / t
	}
	return out
}

func backtest(seed uint64, n int) []strategyRun {
	runs := make([]strategyRun, len(strategies))
	chips := make([]int, len(strategies))
	peak := make([]int, len(strategies))
	brier := make([]float64, len(strategies))
	for i, s := range strategies {
		runs[i] = strategyRun{Name: s.name, Blurb: s.blurb, Chips: make([]int, 0, n)}
		chips[i], peak[i] = startChips, startChips
	}
	for m := 1; m <= n; m++ {
		f := makeFixture(seed, m)
		_, _, result := f.play(seed)
		for i, s := range strategies {
			r := rng(seed, m, 100+uint64(i))
			probs, pick, frac := s.decide(f, r)
			for _, o := range outcomes {
				y := 0.0
				if o == result {
					y = 1
				}
				brier[i] += (probs[o] - y) * (probs[o] - y)
			}
			if pick != "" && chips[i] > 0 {
				stake := min(int(math.Round(frac*float64(chips[i]))), limitFor(chips[i]).MaxStake)
				if stake > 0 {
					runs[i].Bets++
					if pick == result {
						runs[i].Wins++
					}
					chips[i] += payout(stake, f.Odds[pick], pick == result)
				}
			}
			peak[i] = max(peak[i], chips[i])
			runs[i].MaxDraw = max(runs[i].MaxDraw, peak[i]-chips[i])
			runs[i].Chips = append(runs[i].Chips, chips[i])
		}
	}
	for i := range runs {
		runs[i].Final = chips[i]
		runs[i].Brier = math.Round(brier[i]/float64(n)*1000) / 1000
	}
	return runs
}
