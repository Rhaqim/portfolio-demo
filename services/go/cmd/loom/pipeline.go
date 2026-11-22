package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/rhaqim/noewright"

	"github.com/rhaqim/demo/internal/platform"
)

// A visitor-defined agent graph. The first agent is the lead; everything else
// is a follower. Dependencies are never declared by hand: they come from the
// {{.Inputs.X}} references in each template, and Noewright turns those into
// execution layers.
type pipelineDef struct {
	InputName string    `json:"input_name"`
	Input     string    `json:"input,omitempty"`
	Agents    []pipeAgt `json:"agents"`
}

type pipeAgt struct {
	Slug      string `json:"slug"`
	OutputKey string `json:"output_key"`
	System    string `json:"system"`
	Template  string `json:"template"`
}

var (
	slugRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	outKeyRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{1,30}$`)
)

func (d *pipelineDef) check() error {
	if !outKeyRe.MatchString(d.InputName) {
		return errors.New("the input name must be a CapitalisedWord, like Incident")
	}
	if len(d.Agents) == 0 || len(d.Agents) > 8 {
		return errors.New("a pipeline has 1 to 8 agents")
	}
	slugs, keys := map[string]bool{}, map[string]bool{d.InputName: true}
	for i, a := range d.Agents {
		label := fmt.Sprintf("agent %d", i+1)
		switch {
		case !slugRe.MatchString(a.Slug):
			return fmt.Errorf("%s: names are lowercase letters, digits and dashes", label)
		case slugs[a.Slug]:
			return fmt.Errorf("two agents are called %q", a.Slug)
		case !outKeyRe.MatchString(a.OutputKey):
			return fmt.Errorf("%s: the output name must be a CapitalisedWord", a.Slug)
		case keys[a.OutputKey]:
			return fmt.Errorf("%q is produced twice", a.OutputKey)
		case strings.TrimSpace(a.System) == "" || strings.TrimSpace(a.Template) == "":
			return fmt.Errorf("%s needs a system prompt and a template", a.Slug)
		case len(a.System) > 4000 || len(a.Template) > 4000:
			return fmt.Errorf("%s: prompts are limited to 4,000 characters", a.Slug)
		case strings.Contains(a.Template, "{{define") || strings.Contains(a.Template, "{{template") || strings.Contains(a.Template, "{{block"):
			return fmt.Errorf("%s: define/template/block actions aren't allowed", a.Slug)
		}
		if _, err := template.New(a.Slug).Parse(a.Template); err != nil {
			return fmt.Errorf("%s: template error: %v", a.Slug, err)
		}
		slugs[a.Slug], keys[a.OutputKey] = true, true
	}
	if len(d.Input) > 8000 {
		return errors.New("the input is limited to 8,000 characters")
	}
	return nil
}

// owner scopes every registry record to the visitor: Noewright resolves agent
// slugs inside the owner, so two visitors' "triage" agents never meet.
func owner(r *http.Request) string { return "visitor:" + platform.Visitor(r) }

// record registers (or reuses) each agent under the visitor's owner and
// returns the flow wiring. It is not persisted: Validate and Plan work on an
// unsaved record, and RunTurn takes the flow directly.
func (s *server) record(ctx context.Context, own string, d pipelineDef) (*noewright.FlowRecord, error) {
	rec := &noewright.FlowRecord{Owner: own, Slug: "pipeline", Version: 1, IsActive: true, Inputs: []string{d.InputName}}
	for i, a := range d.Agents {
		ag, err := s.upsertAgent(ctx, own, "pipeline", a.Slug, dryRunSlug, a.System, a.Template)
		if err != nil {
			return nil, err
		}
		rec.Agents = append(rec.Agents, noewright.FlowAgentEntry{
			Position: i, AgentSlug: a.Slug, AgentVersion: ag.Version, OutputKey: a.OutputKey, Stream: i == 0,
		})
	}
	return rec, nil
}

type planView struct {
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors"`
	Layers int      `json:"layers"`
	Nodes  any      `json:"nodes"`
	Edges  any      `json:"edges"`
	Cyclic []string `json:"cyclic"`
}

func (s *server) plan(ctx context.Context, rec *noewright.FlowRecord) (planView, error) {
	p, err := s.e.Flows().Plan(ctx, rec)
	if err != nil {
		return planView{}, err
	}
	v := planView{Layers: p.Layers, Nodes: p.Nodes, Edges: p.Edges, Cyclic: p.Cyclic, Errors: []string{}}
	if verr := s.e.Flows().Validate(ctx, rec); verr != nil {
		for _, line := range strings.Split(verr.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				v.Errors = append(v.Errors, line)
			}
		}
	}
	v.Valid = len(v.Errors) == 0
	return v, nil
}

func (s *server) pipelinePlan(w http.ResponseWriter, r *http.Request) {
	var d pipelineDef
	if err := platform.Decode(w, r, 64<<10, &d); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.check(); err != nil {
		platform.JSON(w, http.StatusOK, planView{Errors: []string{err.Error()}})
		return
	}
	rec, err := s.record(r.Context(), owner(r), d)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't register the agents")
		return
	}
	v, err := s.plan(r.Context(), rec)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't plan the flow")
		return
	}
	platform.JSON(w, http.StatusOK, v)
}

func (s *server) pipelineRun(w http.ResponseWriter, r *http.Request) {
	var d pipelineDef
	if err := platform.Decode(w, r, 64<<10, &d); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := d.check(); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(d.Input) == "" {
		platform.Error(w, http.StatusBadRequest, "Give the pipeline something to work on.")
		return
	}
	run, err := s.runnerFor(r)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	vid, own := platform.Visitor(r), owner(r)
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

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	rec, err := s.record(ctx, own, d)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't register the agents")
		return
	}
	pv, err := s.plan(ctx, rec)
	if err != nil || !pv.Valid {
		msg := "the wiring has problems; check the plan"
		if len(pv.Errors) > 0 {
			msg = pv.Errors[0]
		}
		platform.Error(w, http.StatusBadRequest, msg)
		return
	}

	sess := &noewright.Session{PlatformID: vid, Tags: []string{"pipeline"}, State: noewright.State{Modality: noewright.ModalityText}}
	if err := s.e.Sessions().Create(ctx, sess); err != nil {
		platform.Error(w, http.StatusInternalServerError, "couldn't start a session")
		return
	}

	flow := rec.Flow()
	flow.Lead.GeneratorOverride, flow.Lead.Overrides = run.generator, run.overrides
	for i := range flow.Followers {
		flow.Followers[i].GeneratorOverride, flow.Followers[i].Overrides = run.generator, run.overrides
	}

	st, ok := newStream(w)
	if !ok {
		platform.Error(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	st.send("plan", pv)
	st.send("session", map[string]any{"session_id": sess.ID, "dry_run": run.dry, "model": run.model.Label})

	started := time.Now()
	turn, err := s.e.RunTurn(ctx, sess, noewright.TurnRequest{
		Owner:  own,
		Flow:   flow,
		Inputs: map[string]any{d.InputName: d.Input},
		OnChunk: func(c noewright.Chunk) {
			st.send("chunk", map[string]string{"agent": flow.Lead.AgentSlug, "text": c.Content})
		},
		OnStep: func(role string, step *noewright.Step, err error) {
			if err != nil || step == nil {
				st.send("step", stepView{Role: role, Agent: strings.TrimPrefix(role, "follower:"), Error: explain(err)})
				return
			}
			v := s.viewStep(ctx, role, step)
			applyCost(&v, step, s.costs(ctx, sess.ID))
			st.send("step", v)
		},
	})
	if err != nil {
		st.fail(err)
		return
	}
	st.send("done", map[string]any{
		"elapsed_ms": time.Since(started).Milliseconds(),
		"steps":      len(turn.Steps),
		"failed":     len(turn.Errors),
	})
}

func (s *server) pipelinePreset(w http.ResponseWriter, _ *http.Request) {
	platform.JSON(w, http.StatusOK, postmortemPreset)
}

// postmortemPreset turns raw incident notes into a postmortem. Its wiring
// plans into four layers: triage; timeline and impact together; root cause
// and the status update together; then action items.
var postmortemPreset = pipelineDef{
	InputName: "Incident",
	Input: `Pager went off 14:02 UTC. payments-api 502s climbing, p99 latency 9s.
14:05 on-call (Ada) acks. Dashboards: DB connections pinned at max (200/200).
14:09 recent deploys: payments-api v2.31 went out at 13:48. Changelog mentions "retry wrapper for ledger writes".
14:15 Ada scales payments-api from 6 to 10 pods. Errors get WORSE.
14:22 Tunde joins, notices each retry opens a new DB connection and never releases the first on timeout.
14:30 decision to roll back to v2.30.
14:41 rollback complete, connections drain, 502s stop by 14:44.
Roughly 18% of checkout attempts failed between 14:00 and 14:44. Support got ~140 tickets. Some customers were charged twice? Finance checking.
No alert fired on connection pool saturation, only on 5xx rate.`,
	Agents: []pipeAgt{
		{Slug: "triage", OutputKey: "Triage",
			System:   "You are an incident commander. From raw incident notes, write a five-line triage: one-sentence summary, severity (SEV1-SEV4 with a one-line justification), affected services, detection method, current status. Be factual; do not speculate beyond the notes.",
			Template: "Incident notes:\n{{.Inputs.Incident}}"},
		{Slug: "timeline", OutputKey: "Timeline",
			System:   "You write incident timelines. Produce a chronological bullet list in UTC, one event per line, marking each as DETECT, RESPOND, MITIGATE or RESOLVE. Use only times present in the notes.",
			Template: "Triage:\n{{.Inputs.Triage}}\n\nRaw notes:\n{{.Inputs.Incident}}"},
		{Slug: "impact", OutputKey: "Impact",
			System:   "You assess incident impact. Cover customer impact, business impact and open questions, as three short labelled paragraphs. Flag anything the notes leave unconfirmed.",
			Template: "Triage:\n{{.Inputs.Triage}}\n\nRaw notes:\n{{.Inputs.Incident}}"},
		{Slug: "root-cause", OutputKey: "RootCause",
			System:   "You run blameless root-cause analysis. Using the timeline and impact, walk five whys from symptom to root cause, then list contributing factors separately. Blame systems and processes, never people.",
			Template: "Timeline:\n{{.Inputs.Timeline}}\n\nImpact:\n{{.Inputs.Impact}}"},
		{Slug: "status-update", OutputKey: "StatusUpdate",
			System:   "You write customer-facing status page updates. Plain language, no internal names, no blame, under 90 words. Say what happened, who was affected, that it is resolved, and what happens next.",
			Template: "Triage:\n{{.Inputs.Triage}}\n\nImpact:\n{{.Inputs.Impact}}"},
		{Slug: "actions", OutputKey: "Actions",
			System:   "You turn root-cause analyses into action items. Write 4 to 6 items as a list: [P0/P1/P2] action, owner role, and how we will know it is done. Each item must trace to a cause or contributing factor.",
			Template: "Root cause analysis:\n{{.Inputs.RootCause}}"},
	},
}
