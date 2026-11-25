package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rhaqim/noewright"

	"github.com/rhaqim/demo/internal/platform"
)

// Sermon captions. A speaker's lines become live captions in Yorùbá, Igbo
// and Nigerian Pidgin, with Scripture quoted from real Bibles rather than
// translated by a model.
//
// Per line:
//   step      sc-detect     finds Bible references (book code, chapter, verse, and whether the
//                           speaker read the passage aloud, named it, or paraphrased it). Go
//                           keeps only references that exist, and looks each one up in the
//                           KJV and Biblica's Yorùbá and Igbo Bibles.
//   turn lead sc-caption    streams a clean English caption of the spoken line
//        1    sc-yo, sc-ig, sc-pcm   translate in parallel; each is handed its own language's
//                           Bible text for any passage read aloud
//        hook scripture guard   on each translator: a passage read aloud must appear word for
//                           word. The first miss sends the step back with the reason
//                           (ErrRetryWith); a second miss is let through and flagged.
//        2    sc-judge      reads all three captions (so Noewright plans it after them) and
//                           scores each for faithfulness
//   step      sc-line       a recorded step holding the finished line; followers' screens are
//                           fed from it through Engine.Subscribe
//
// The session is the service: a speaker opens it, and anyone with its follow
// link watches the captions arrive on their phone.

type captionLang struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	Key   string `json:"key"`   // the OutputKey later agents read
	Bible string `json:"bible"` // where its scripture is quoted from
}

var captionLangs = []captionLang{
	{"yo", "Yorùbá", "YO", "Biblica Yorùbá Òde-Òní (CC BY-SA 4.0)"},
	{"ig", "Igbo", "IG", "Biblica Baịbụlụ Nsọ nʼIgbo (CC BY-SA 4.0)"},
	{"pcm", "Nigerian Pidgin", "PCM", "King James Version (public domain)"},
}

const (
	captionLineMax    = 400 // characters per spoken line
	captionHistoryMax = 200 // lines kept in a service
	captionViewersMax = 50  // follow streams per service
	guardThreshold    = 0.6 // share of a passage's words a caption must carry, in order
)

type lineRef struct {
	scriptureRef
	Label string            `json:"label"`
	Texts map[string]string `json:"texts"` // en, yo, ig, pcm
}

type judgeScore struct {
	Score      int    `json:"score"`
	Confidence string `json:"confidence"`
	Note       string `json:"note"`
}

type captionLine struct {
	N        int                   `json:"n"`
	Spoken   string                `json:"spoken"`
	English  string                `json:"en"`
	Captions map[string]string     `json:"captions"`
	Refs     []lineRef             `json:"refs"`
	Judge    map[string]judgeScore `json:"judge"`
	Guard    map[string]string     `json:"guard"` // lang -> "retried" | "flagged"
	DryRun   bool                  `json:"dry_run"`
	At       time.Time             `json:"at"`
}

type captionState struct {
	Title string        `json:"title"`
	Lines []captionLine `json:"lines"`
}

func loadCaptions(st *noewright.State) captionState {
	var c captionState
	_ = json.Unmarshal(st.Snapshot, &c)
	if c.Lines == nil {
		c.Lines = []captionLine{}
	}
	return c
}

func saveCaptions(st *noewright.State, c captionState) {
	b, _ := json.Marshal(c)
	st.Snapshot = b
}

// sampleSermon is a short sermon written for this demo. Line 2 reads a verse
// with its numbers spoken, line 5 names one and paraphrases it, line 7 reads
// one with digits, and line 9 reads one without naming it at all.
var sampleSermon = []string{
	"Good morning, church. This morning I want us to look at rest, and at where real strength comes from.",
	"Jesus said in Matthew eleven twenty-eight, Come unto me, all ye that labour and are heavy laden, and I will give you rest.",
	"Notice he didn't say come to a programme, or come when you've fixed yourself. He said come unto me.",
	"Some of us are tired in ways that sleep can't fix.",
	"Isaiah forty verse thirty-one tells us that the people who wait on the Lord will get their strength back.",
	"Waiting isn't wasting. Waiting is trusting God with the timing.",
	"Proverbs 3:5 says, Trust in the LORD with all thine heart; and lean not unto thine own understanding.",
	"Your understanding has limits. His does not.",
	"And we know that all things work together for good to them that love God.",
	"So whatever you carried in here this morning, you don't have to carry it out.",
}

func translatorSystem(l captionLang) string {
	style := map[string]string{
		"yo":  "standard Yorùbá with full tone marks and under-dots, as a Lagos or Ibadan congregation would read it",
		"ig":  "standard Igbo (Igbo Izugbe) with its dotted vowels, as an Enugu or Owerri congregation would read it",
		"pcm": "Nigerian Pidgin as it is spoken in a Nigerian church, warm and natural, not a word-for-word gloss",
	}[l.Code]
	return fmt.Sprintf(`You write live captions for a church service in %s. Translate the English caption into %s. Keep the
speaker's meaning and tone; add nothing, drop nothing. Keep names and Bible references (for example "John 3:16").

Scripture rule. The "Scripture" section lists any Bible passage in this line.
- Marked READ ALOUD: the speaker is reading that passage. Put the passage into your caption exactly as given there,
  word for word, from the %s. Do not translate it yourself and do not shorten it.
- Marked NAMED or PARAPHRASED: translate the speaker's own words normally.

Reply with only the caption: no quotation marks around it, no notes, no English unless the speaker used English.`,
		l.Name, style, l.Bible)
}

const translatorTemplate = `English caption:
{{.Inputs.Caption}}

Scripture:
{{.Inputs.Scripture}}`

var captionAgents = []ttAgent{
	{"sc-detect", dryRunSlug, `You find Bible references in one line of a live sermon transcript. The transcript comes from speech,
so numbers may be spelled out ("John three sixteen", "Psalm twenty-three verse one").

For each passage in the line, give the book code, chapter, first verse, last verse if it spans more than one, and a mode:
- "quoted": the speaker reads the passage's words aloud in this line, with or without naming it
- "cited": the speaker names the reference without reading it
- "paraphrase": the speaker gives the passage's meaning in their own words
Only include a passage you are confident about. An ordinary sentence that merely sounds biblical is not a reference.

Book codes: ` + bookCodeList() + `

Reply with only a JSON object, no prose and no code fences:
{"refs": [{"book": "JOH", "chapter": 3, "verse": 16, "to": 0, "mode": "quoted"}]}
Use {"refs": []} when there is none.`,
		`Previous line: {{.Inputs.Previous}}

Line: {{.Inputs.Line}}`},

	{"sc-caption", dryRunSlug, `You turn one line of a live sermon transcript into a clean English caption for a screen. Fix punctuation,
capitals and obvious speech-to-text slips, and drop filler ("uh", "um", a word said twice). Keep the speaker's own words,
voice and order; never summarise, never add. Reply with only the caption.`,
		`{{.Inputs.Line}}`},

	{"sc-yo", dryRunSlug, translatorSystem(captionLangs[0]), translatorTemplate},
	{"sc-ig", dryRunSlug, translatorSystem(captionLangs[1]), translatorTemplate},
	{"sc-pcm", dryRunSlug, translatorSystem(captionLangs[2]), translatorTemplate},

	{"sc-judge", dryRunSlug, `You review live church captions against the English line they translate. For each language, score
faithfulness from 1 to 5 (5: meaning, tone and scripture all kept; 3: understandable but something lost or added;
1: wrong or misleading), give your confidence in that score (low, medium or high; be honest where you know the
language less well), and name the single biggest problem in a short phrase, or "" if none.

Reply with only a JSON object, no prose and no code fences:
{"yo": {"score": 4, "confidence": "medium", "note": ""}, "ig": {...}, "pcm": {...}}`,
		`English: {{.Inputs.Caption}}

Scripture in this line:
{{.Inputs.ScriptureAll}}

Yorùbá: {{.Inputs.YO}}

Igbo: {{.Inputs.IG}}

Nigerian Pidgin: {{.Inputs.PCM}}`},

	{"sc-open", dryRunSlug, "The service opens.", `(recorded)`},
	{"sc-line", dryRunSlug, "A captioned line.", `(recorded)`},
	{"sc-score", dryRunSlug, "The judge's scores for a line.", `(recorded)`},
}

func (s *server) seedCaptions(ctx context.Context) error {
	for _, a := range captionAgents {
		if _, err := s.upsertAgent(ctx, ttOwner, "captions", a.slug, a.generator, a.system, a.template); err != nil {
			return fmt.Errorf("seed %s: %w", a.slug, err)
		}
	}
	return nil
}

// registerCaptionHooks installs the scripture guard and the line recorder.
func (s *server) registerCaptionHooks() {
	s.e.Hooks().RegisterPost("scripture-guard", func(_ context.Context, req *noewright.StepRequest, res noewright.Result) (noewright.Result, error) {
		code, ok := strings.CutPrefix(req.AgentSlug, "sc-")
		if !ok || req.Session == nil || !isCaptionLang(code) {
			return res, nil
		}
		raw, _ := req.Inputs["Guard"].(string)
		var must []struct{ Label, Text string }
		if raw == "" || json.Unmarshal([]byte(raw), &must) != nil || len(must) == 0 {
			return res, nil
		}
		out := noewright.ResultText(res)
		var missed []string
		for _, p := range must {
			if coverage(out, p.Text) < guardThreshold {
				missed = append(missed, fmt.Sprintf("%s: «%s»", p.Label, p.Text))
			}
		}
		vars := req.Session.State.Vars
		if vars == nil {
			vars = map[string]any{}
			req.Session.State.Vars = vars
		}
		switch {
		case len(missed) == 0 && req.Attempt() > 0:
			vars["guard_"+code] = "retried"
		case len(missed) > 0 && req.Attempt() == 0:
			// Noewright records this attempt, then retries with the reason in the prompt.
			return nil, noewright.ErrRetryWith(noewright.RetryAnnotation{Reason: "The scripture guard rejected this caption. " +
				"The speaker read these passages aloud, so the caption must quote them word for word as given: " +
				strings.Join(missed, "; ") + "."})
		case len(missed) > 0:
			vars["guard_"+code] = "flagged"
		}
		return res, nil
	})

	s.e.Hooks().RegisterPost("captions-state", func(_ context.Context, req *noewright.StepRequest, res noewright.Result) (noewright.Result, error) {
		if req.Session != nil && req.AgentSlug == "sc-score" {
			// The judge scores a line after it is on screen; attach the scores.
			var sc struct {
				N     int                   `json:"n"`
				Judge map[string]judgeScore `json:"judge"`
			}
			if json.Unmarshal([]byte(noewright.ResultText(res)), &sc) == nil {
				c := loadCaptions(&req.Session.State)
				for i := range c.Lines {
					if c.Lines[i].N == sc.N {
						c.Lines[i].Judge = sc.Judge
					}
				}
				saveCaptions(&req.Session.State, c)
			}
			return res, nil
		}
		if req.Session == nil || req.AgentSlug != "sc-line" {
			return res, nil
		}
		var line captionLine
		if json.Unmarshal([]byte(noewright.ResultText(res)), &line) != nil {
			return res, nil
		}
		c := loadCaptions(&req.Session.State)
		c.Lines = append(c.Lines, line)
		if len(c.Lines) > captionHistoryMax {
			c.Lines = c.Lines[len(c.Lines)-captionHistoryMax:]
		}
		saveCaptions(&req.Session.State, c)
		return res, nil
	})
}

func isCaptionLang(code string) bool {
	for _, l := range captionLangs {
		if l.Code == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The demo's own key. Visitors can bring a key; without one, and while the
// hourly allowance lasts, lines run on the demo's key so a first visit shows
// real captions. With neither, the run is a dry run.
// ---------------------------------------------------------------------------

type houseKey struct {
	key       string
	model     model
	perHour   *platform.Limiter // across all visitors
	perVisit  *platform.Limiter // per visitor per day
	perHourN  int
	perVisitN int
}

func (s *server) captionRunner(r *http.Request) (runner, error) {
	run, err := s.runnerFor(r)
	if err != nil || !run.dry || s.house == nil {
		return run, err
	}
	vid := platform.Visitor(r)
	if !s.house.perVisit.Allow(vid) || !s.house.perHour.Allow("all") {
		return run, nil // a dry run, and the page says why
	}
	return runner{generator: s.house.model.slug(), overrides: map[string]any{"api_key": s.house.key}, model: s.house.model, house: true}, nil
}

func (s *server) setupHouseKey() {
	key := strings.TrimSpace(platform.Env("CAPTIONS_ANTHROPIC_API_KEY", ""))
	if key == "" {
		return
	}
	m, ok := s.models[platform.Env("CAPTIONS_MODEL", "anthropic/claude-sonnet-5-5")]
	if !ok || m.Provider != "anthropic" {
		slog.Warn("captions: CAPTIONS_MODEL is not a listed Anthropic model; the demo key is off")
		return
	}
	perHour := atoiOr(platform.Env("CAPTIONS_LINES_PER_HOUR", "120"), 120)
	perVisit := atoiOr(platform.Env("CAPTIONS_LINES_PER_VISITOR", "30"), 30)
	s.house = &houseKey{key: key, model: m, perHourN: perHour, perVisitN: perVisit,
		perHour: platform.NewLimiter(perHour, time.Hour), perVisit: platform.NewLimiter(perVisit, 24*time.Hour)}
}

func atoiOr(s string, d int) int {
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil || n < 1 {
		return d
	}
	return n
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

type captionView struct {
	ID     string        `json:"id"`
	Title  string        `json:"title"`
	Lines  []captionLine `json:"lines"`
	Owner  bool          `json:"owner"`
	Langs  []captionLang `json:"langs"`
	Sample []string      `json:"sample,omitempty"`
}

func (s *server) captionView(sess *noewright.Session, owner bool) captionView {
	c := loadCaptions(&sess.State)
	v := captionView{ID: sess.ID.String(), Title: c.Title, Lines: c.Lines, Owner: owner, Langs: captionLangs}
	if owner {
		v.Sample = sampleSermon
	}
	return v
}

func (s *server) captionsConfig(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"langs": captionLangs, "sample": sampleSermon, "line_max": captionLineMax, "house": false,
		"graph": captionShape()}
	if s.house != nil {
		out["house"] = true
		out["house_model"] = s.house.model.Label
		out["house_per_visitor"] = s.house.perVisitN
	}
	platform.JSON(w, http.StatusOK, out)
}

func (s *server) captionsStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	_ = platform.Decode(w, r, 512, &req)
	title := clip(strings.TrimSpace(req.Title), 80)
	if title == "" {
		title = "Sunday service"
	}
	ctx := r.Context()
	sess := &noewright.Session{PlatformID: platform.Visitor(r), Tags: []string{"captions"},
		State: noewright.State{Modality: noewright.ModalityText, Vars: map[string]any{}}}
	saveCaptions(&sess.State, captionState{Title: title, Lines: []captionLine{}})
	if err := s.e.Sessions().Create(ctx, sess); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't open a service")
		return
	}
	if _, err := s.e.RecordStep(ctx, sess, noewright.ExternalStep{AgentSlug: "sc-open", Owner: ttOwner, Author: "speaker",
		Result: noewright.NewTextResult("The service opens: "+title, "stop", 0, 0)}); err != nil && !errors.Is(err, noewright.ErrSessionNotPersisted) {
		platform.Error(w, http.StatusInternalServerError, "couldn't open a service")
		return
	}
	platform.JSON(w, http.StatusCreated, s.captionView(sess, true))
}

// captionSession loads a service. Anyone with its id may watch; only the
// visitor who opened it may speak.
func (s *server) captionSession(w http.ResponseWriter, r *http.Request) (*noewright.Session, bool, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platform.Error(w, http.StatusNotFound, "service not found")
		return nil, false, false
	}
	sess, err := s.e.Sessions().GetHeader(r.Context(), id)
	if err != nil || !hasTag(sess.Tags, "captions") {
		platform.Error(w, http.StatusNotFound, "service not found")
		return nil, false, false
	}
	return sess, sess.PlatformID == platform.Visitor(r), true
}

func hasTag(tags []string, t string) bool {
	for _, x := range tags {
		if x == t {
			return true
		}
	}
	return false
}

func (s *server) captionsGet(w http.ResponseWriter, r *http.Request) {
	if sess, owner, ok := s.captionSession(w, r); ok {
		platform.JSON(w, http.StatusOK, s.captionView(sess, owner))
	}
}

func scriptureBlock(refs []lineRef, lang string) string {
	if len(refs) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, r := range refs {
		switch r.Mode {
		case "quoted":
			fmt.Fprintf(&b, "- %s, READ ALOUD. Use this text word for word:\n  %s\n", r.Label, r.Texts[lang])
		case "paraphrase":
			fmt.Fprintf(&b, "- %s, PARAPHRASED by the speaker.\n", r.Label)
		default:
			fmt.Fprintf(&b, "- %s, NAMED by the speaker.\n", r.Label)
		}
	}
	return strings.TrimSpace(b.String())
}

func guardInput(refs []lineRef, lang string) string {
	var must []struct{ Label, Text string }
	for _, r := range refs {
		if r.Mode == "quoted" && r.Texts[lang] != "" {
			must = append(must, struct{ Label, Text string }{r.Label, r.Texts[lang]})
		}
	}
	if len(must) == 0 {
		return ""
	}
	b, _ := json.Marshal(must)
	return string(b)
}

func (s *server) captionsLine(w http.ResponseWriter, r *http.Request) {
	sess, owner, ok := s.captionSession(w, r)
	if !ok {
		return
	}
	if !owner {
		platform.Error(w, http.StatusForbidden, "Only the speaker who opened this service can add lines.")
		return
	}
	var req struct {
		Text string `json:"text"`
		// Models picks a model per agent ("sc-caption": "anthropic/claude-…");
		// agents left out use the line's default model and key.
		Models map[string]string `json:"models,omitempty"`
	}
	if err := platform.Decode(w, r, 4096, &req); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	spoken := strings.Join(strings.Fields(req.Text), " ")
	if spoken == "" || len([]rune(spoken)) > captionLineMax {
		platform.Error(w, http.StatusBadRequest, fmt.Sprintf("Send one spoken line of 1 to %d characters.", captionLineMax))
		return
	}
	c := loadCaptions(&sess.State)
	if len(c.Lines) >= captionHistoryMax {
		platform.Error(w, http.StatusConflict, "This service is full. Open a new one.")
		return
	}

	runs, def, err := s.captionRuns(r, req.Models)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	vid := platform.Visitor(r)
	// t0 starts the line's clock: the timings the page shows count from here,
	// so a line held up by the previous line's judge says so.
	t0 := time.Now()
	wait, stopWait := context.WithTimeout(r.Context(), 90*time.Second)
	release, err := s.acquireWait(wait, vid)
	stopWait()
	waited := time.Since(t0)
	if err != nil {
		platform.Error(w, http.StatusTooManyRequests, err.Error())
		return
	}
	var once sync.Once
	release = func(r func()) func() { return func() { once.Do(r) } }(release)
	defer release()
	// A line that waited for the previous one must start from the session as
	// that line left it, not as it was when this request arrived.
	if sess, err = s.e.Sessions().GetHeader(r.Context(), sess.ID); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't load the service")
		return
	}
	if err := s.ensureBudget(r.Context(), vid); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't set up the step budget")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	st, ok := newStream(w)
	if !ok {
		return
	}
	s.runCaptionLine(ctx, st, sess, spoken, runs, def, &lineClock{t0: t0, waited: waited}, vid, release)
}

// captionsFollow is the congregation's view: the service's lines so far, then
// live events from Noewright's session bus. It needs only the service id.
func (s *server) captionsFollow(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.captionSession(w, r)
	if !ok {
		return
	}
	n, _ := s.viewers.LoadOrStore(sess.ID, new(int64))
	count := n.(*int64)
	s.viewersMu.Lock()
	if *count >= captionViewersMax {
		s.viewersMu.Unlock()
		platform.Error(w, http.StatusTooManyRequests, "This service has too many followers right now.")
		return
	}
	*count++
	s.viewersMu.Unlock()
	defer func() { s.viewersMu.Lock(); *count--; s.viewersMu.Unlock() }()

	st, ok := newStream(w)
	if !ok {
		return
	}
	ctx := r.Context()
	st.send("history", s.captionView(sess, false))
	sub, err := s.e.Subscribe(ctx, sess.ID, noewright.SubscribeOptions{FromStep: -1})
	if err != nil {
		st.fail(err)
		return
	}
	defer sub.Close()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			st.comment("ping")
		case ev, ok := <-sub.Events:
			if !ok {
				return
			}
			switch {
			case ev.Type == noewright.EventStepStarted && ev.Agent == "sc-caption":
				st.send("speaking", map[string]bool{"on": true})
			case ev.Type == noewright.EventChunk && ev.Agent == "sc-caption":
				st.send("chunk", map[string]string{"text": ev.Chunk})
			case ev.Type == noewright.EventStepDone && ev.Step != nil && ev.Agent == "sc-line":
				var line captionLine
				if json.Unmarshal([]byte(noewright.ResultText(ev.Step.Result)), &line) == nil {
					st.send("line", line)
				}
			case ev.Type == noewright.EventStepDone && ev.Step != nil && ev.Agent == "sc-score":
				var sc map[string]any
				if json.Unmarshal([]byte(noewright.ResultText(ev.Step.Result)), &sc) == nil {
					st.send("judge", sc)
				}
			}
		}
	}
}
