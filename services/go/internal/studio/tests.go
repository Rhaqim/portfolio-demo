package studio

// tests.go — the quality screen's API: a graph's tests (the starter's own,
// read-only, and the visitor's, saved from conversations), running them on the
// visitor's model or the simulator, and the evidence of their last run.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rhaqim/noewright"

	"github.com/rhaqim/demo/internal/platform"
)

const (
	maxTestsPerGraph = 20
	maxStepsPerTest  = 20
)

// ensureTables creates Studio's own tables beside Noewright's.
func ensureTables(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS studio_tests (
			owner TEXT NOT NULL, graph TEXT NOT NULL, name TEXT NOT NULL,
			body TEXT NOT NULL, created_at TEXT NOT NULL,
			PRIMARY KEY (owner, graph, name))`,
		`CREATE TABLE IF NOT EXISTS studio_results (
			owner TEXT NOT NULL, graph TEXT NOT NULL, test TEXT NOT NULL,
			body TEXT NOT NULL, at TEXT NOT NULL,
			PRIMARY KEY (owner, graph, test))`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// testView is one test as the quality screen shows it.
type testView struct {
	Test   noewright.BundleTest        `json:"test"`
	Source string                      `json:"source"` // "starter" (read-only) or "yours"
	Last   *noewright.BundleTestResult `json:"last,omitempty"`
}

// starterTests are the tests the template that created this graph carries.
func starterTests(rec *noewright.GraphRecord) []noewright.BundleTest {
	for _, t := range templates {
		if t.Graph != rec.Slug {
			continue
		}
		data, err := templateFS.ReadFile("templates/" + t.File)
		if err != nil {
			return nil
		}
		b, err := noewright.ParseBundle(data)
		if err != nil {
			return nil
		}
		var out []noewright.BundleTest
		for _, bt := range b.Tests {
			if bt.Graph == fmt.Sprintf("%s@%d", rec.Slug, rec.Version) {
				out = append(out, bt)
			}
		}
		return out
	}
	return nil
}

func (s *Server) visitorTests(ctx context.Context, owner, graph string) ([]noewright.BundleTest, error) {
	rows, err := s.cfg.DB.QueryContext(ctx, `SELECT body FROM studio_tests WHERE owner = $1 AND graph = $2 ORDER BY created_at`, owner, graph)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []noewright.BundleTest
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var t noewright.BundleTest
		if json.Unmarshal([]byte(body), &t) == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func (s *Server) lastResults(ctx context.Context, owner, graph string) (map[string]*noewright.BundleTestResult, error) {
	rows, err := s.cfg.DB.QueryContext(ctx, `SELECT test, body FROM studio_results WHERE owner = $1 AND graph = $2`, owner, graph)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*noewright.BundleTestResult{}
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			return nil, err
		}
		var r noewright.BundleTestResult
		if json.Unmarshal([]byte(body), &r) == nil {
			out[name] = &r
		}
	}
	return out, rows.Err()
}

// allTests returns the graph's tests, starter ones first, with their last results.
func (s *Server) allTests(ctx context.Context, r *http.Request, rec *noewright.GraphRecord) ([]testView, error) {
	mine, err := s.visitorTests(ctx, owner(r), rec.Slug)
	if err != nil {
		return nil, err
	}
	last, err := s.lastResults(ctx, owner(r), rec.Slug)
	if err != nil {
		return nil, err
	}
	var out []testView
	for _, t := range starterTests(rec) {
		out = append(out, testView{Test: t, Source: "starter", Last: last[t.Name]})
	}
	for _, t := range mine {
		out = append(out, testView{Test: t, Source: "yours", Last: last[t.Name]})
	}
	return out, nil
}

func (s *Server) listTests(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.graphFor(w, r)
	if !ok {
		return
	}
	tests, err := s.allTests(r.Context(), r, rec)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't load the tests.")
		return
	}
	if tests == nil {
		tests = []testView{}
	}
	platform.JSON(w, http.StatusOK, tests)
}

// createTest saves a test, usually the conversation the visitor just had,
// with the expectations the page derived from it.
func (s *Server) createTest(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.graphFor(w, r)
	if !ok {
		return
	}
	var t noewright.BundleTest
	if err := platform.Decode(w, r, 64<<10, &t); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	t.Name = strings.TrimSpace(t.Name)
	t.Graph = fmt.Sprintf("%s@%d", rec.Slug, rec.Version)
	t.Vars, t.Inputs = nil, nil
	if msg := checkTest(t); msg != "" {
		platform.Error(w, http.StatusBadRequest, msg)
		return
	}
	existing, err := s.allTests(r.Context(), r, rec)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't save the test.")
		return
	}
	if len(existing) >= maxTestsPerGraph {
		platform.Error(w, http.StatusBadRequest, fmt.Sprintf("A graph can have up to %d tests.", maxTestsPerGraph))
		return
	}
	for _, e := range existing {
		if strings.EqualFold(e.Test.Name, t.Name) {
			platform.Error(w, http.StatusConflict, "A test with that name already exists.")
			return
		}
	}
	body, _ := json.Marshal(t)
	if _, err := s.cfg.DB.ExecContext(r.Context(), `INSERT INTO studio_tests (owner, graph, name, body, created_at) VALUES ($1, $2, $3, $4, $5)`,
		owner(r), rec.Slug, t.Name, string(body), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't save the test.")
		return
	}
	platform.JSON(w, http.StatusCreated, testView{Test: t, Source: "yours"})
}

// checkTest returns why a test cannot be saved, or "".
func checkTest(t noewright.BundleTest) string {
	switch {
	case t.Name == "" || utf8.RuneCountInString(t.Name) > 80:
		return "Give the test a name of up to 80 characters."
	case len(t.Steps) == 0 || len(t.Steps) > maxStepsPerTest:
		return fmt.Sprintf("A test has 1 to %d steps.", maxStepsPerTest)
	}
	for i, st := range t.Steps {
		if len(st.Say) > 2000 || len(st.Select) > 200 {
			return fmt.Sprintf("Step %d: messages are limited to 2,000 characters.", i+1)
		}
		if len(st.Inputs) > 0 {
			return fmt.Sprintf("Step %d: tests saved in Studio send messages and choices only.", i+1)
		}
		x := st.Expect
		if len(x.Ran)+len(x.Skipped)+len(x.Contains)+len(x.MaxLength) > 50 {
			return fmt.Sprintf("Step %d has too many expectations.", i+1)
		}
		for _, v := range x.Contains {
			if len(v) > 500 {
				return fmt.Sprintf("Step %d: expected text is limited to 500 characters.", i+1)
			}
		}
	}
	return ""
}

func (s *Server) deleteTest(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.graphFor(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	res, err := s.cfg.DB.ExecContext(r.Context(), `DELETE FROM studio_tests WHERE owner = $1 AND graph = $2 AND name = $3`, owner(r), rec.Slug, name)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't delete the test.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		platform.Error(w, http.StatusNotFound, "Only tests you saved can be deleted.")
		return
	}
	_, _ = s.cfg.DB.ExecContext(r.Context(), `DELETE FROM studio_results WHERE owner = $1 AND graph = $2 AND test = $3`, owner(r), rec.Slug, name)
	w.WriteHeader(http.StatusNoContent)
}

// runTests runs the graph's tests (or the named ones) on the visitor's model,
// or the simulator, and keeps each test's result as its latest evidence.
func (s *Server) runTests(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.graphFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Only []string `json:"only,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := platform.Decode(w, r, 8<<10, &req); err != nil {
			platform.Error(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	run, err := s.runnerFor(r)
	if err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	v := platform.Visitor(r)
	// Test sessions are not the visitor's, so the daily step budget does not
	// see them; test runs have their own hourly cap instead.
	if !s.testRuns.Allow(v) {
		platform.Error(w, http.StatusTooManyRequests, "That's a lot of test runs: try again in a while.")
		return
	}
	if _, busy := s.busy.LoadOrStore(v, struct{}{}); busy {
		platform.Error(w, http.StatusTooManyRequests, "One run at a time: wait for the current one to finish.")
		return
	}
	defer s.busy.Delete(v)

	ctx := r.Context()
	views, err := s.allTests(ctx, r, rec)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, "Couldn't load the tests.")
		return
	}
	b := &noewright.Bundle{Format: noewright.BundleFormat}
	for _, tv := range views {
		b.Tests = append(b.Tests, tv.Test)
		if tv.Last != nil {
			b.Evidence = append(b.Evidence, tv.Last.Evidence)
		}
	}
	if len(b.Tests) == 0 {
		platform.Error(w, http.StatusBadRequest, "This graph has no tests yet. Save a conversation as a test first.")
		return
	}
	results, err := s.e.Bundles().Test(ctx, b, noewright.BundleTestOptions{
		Owner: owner(r), Only: req.Only, Overrides: run.overrides, GeneratorOverride: run.generator,
	})
	if err != nil && len(results) == 0 {
		platform.Error(w, http.StatusBadGateway, redact(err.Error()))
		return
	}
	for i := range results {
		results[i].Evidence.Failures = redactAll(results[i].Evidence.Failures)
		body, _ := json.Marshal(results[i])
		_, _ = s.cfg.DB.ExecContext(ctx, `INSERT INTO studio_results (owner, graph, test, body, at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (owner, graph, test) DO UPDATE SET body = excluded.body, at = excluded.at`,
			owner(r), rec.Slug, results[i].Evidence.Test, string(body), results[i].Evidence.At)
	}
	out := map[string]any{"results": results, "simulated": run.simulated, "model": run.model}
	if err != nil {
		out["error"] = redact(err.Error())
	}
	platform.JSON(w, http.StatusOK, out)
}

func redactAll(in []string) []string {
	for i := range in {
		in[i] = redact(in[i])
	}
	return in
}

// bundleTests adds the graph's tests and their latest evidence to an export.
func (s *Server) bundleTests(ctx context.Context, r *http.Request, rec *noewright.GraphRecord, b *noewright.Bundle) error {
	views, err := s.allTests(ctx, r, rec)
	if err != nil {
		return err
	}
	for _, tv := range views {
		b.Tests = append(b.Tests, tv.Test)
		if tv.Last != nil {
			b.Evidence = append(b.Evidence, tv.Last.Evidence)
		}
	}
	return nil
}
