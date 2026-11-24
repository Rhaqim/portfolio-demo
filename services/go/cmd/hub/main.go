// Command hub serves the cross-demo endpoints: the "tell me what you think"
// feedback form every demo page embeds, and a token-protected reader for it.
package main

import (
	"bytes"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rhaqim/demo/internal/platform"
)

var demos = map[string]bool{"ussd": true, "buckt": true, "loom": true, "captions": true, "general": true}

type server struct {
	db         *sql.DB
	adminToken string
	webhookURL string
	http       *http.Client
}

func main() {
	db, err := platform.OpenDB(platform.Env("DEMO_DB", "./data/demo.db"))
	if err != nil {
		panic(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS demo_feedback (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		demo       TEXT NOT NULL,
		rating     INTEGER,
		message    TEXT NOT NULL,
		contact    TEXT,
		visitor    TEXT,
		user_agent TEXT,
		created_at TEXT NOT NULL
	)`); err != nil {
		panic(err)
	}

	s := &server{
		db:         db,
		adminToken: platform.Env("ADMIN_TOKEN", ""),
		webhookURL: platform.Env("FEEDBACK_WEBHOOK_URL", ""),
		http:       &http.Client{Timeout: 8 * time.Second},
	}

	perIP := platform.NewLimiter(6, time.Hour)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", platform.Health)
	mux.HandleFunc("POST /api/feedback", perIP.Limit(s.submit))
	mux.HandleFunc("GET /api/feedback", s.list)

	platform.Serve("hub", platform.Env("ADDR", ":8080"), platform.WithVisitor(mux), func() { db.Close() })
}

type feedback struct {
	Demo    string `json:"demo"`
	Rating  int    `json:"rating"`
	Message string `json:"message"`
	Contact string `json:"contact"`
	Website string `json:"website"` // honeypot: humans never see this field
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	var f feedback
	if err := platform.Decode(w, r, 16<<10, &f); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if f.Website != "" {
		// Bot filled the hidden field. Pretend it worked.
		platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	f.Message = strings.TrimSpace(f.Message)
	f.Contact = strings.TrimSpace(f.Contact)
	switch {
	case !demos[f.Demo]:
		platform.Error(w, http.StatusBadRequest, "unknown demo")
		return
	case f.Message == "" && f.Rating == 0:
		platform.Error(w, http.StatusBadRequest, "Say something, or pick a rating.")
		return
	case len(f.Message) > 4000 || len(f.Contact) > 200:
		platform.Error(w, http.StatusBadRequest, "That's a bit long. Keep it under 4,000 characters.")
		return
	case f.Rating < 0 || f.Rating > 5:
		platform.Error(w, http.StatusBadRequest, "rating must be 1 to 5")
		return
	}
	var rating any
	if f.Rating > 0 {
		rating = f.Rating
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO demo_feedback (demo, rating, message, contact, visitor, user_agent, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		f.Demo, rating, f.Message, f.Contact, platform.Visitor(r), truncate(r.UserAgent(), 300),
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		slog.Error("store feedback", "err", err)
		platform.Error(w, http.StatusInternalServerError, "Couldn't save that. Try again in a moment.")
		return
	}
	go s.notify(f)
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// notify forwards new feedback to a chat webhook. The payload carries both
// "text" (Slack, ntfy-style) and "content" (Discord), so either works.
func (s *server) notify(f feedback) {
	if s.webhookURL == "" {
		return
	}
	stars := ""
	if f.Rating > 0 {
		stars = strings.Repeat("★", f.Rating) + strings.Repeat("☆", 5-f.Rating) + " "
	}
	text := fmt.Sprintf("New feedback on the %s demo: %s%s", f.Demo, stars, truncate(f.Message, 1500))
	if f.Contact != "" {
		text += "\nContact: " + f.Contact
	}
	body, _ := json.Marshal(map[string]string{"text": text, "content": text})
	resp, err := s.http.Post(s.webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		slog.Warn("feedback webhook failed", "err", err)
		return
	}
	resp.Body.Close()
}

// list returns the latest feedback. Requires ADMIN_TOKEN as a bearer token;
// disabled entirely when no token is configured.
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.adminToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) != 1 {
		platform.Error(w, http.StatusNotFound, "not found")
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, demo, COALESCE(rating, 0), message, COALESCE(contact, ''), created_at
		 FROM demo_feedback ORDER BY id DESC LIMIT 200`)
	if err != nil {
		platform.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type row struct {
		ID        int    `json:"id"`
		Demo      string `json:"demo"`
		Rating    int    `json:"rating"`
		Message   string `json:"message"`
		Contact   string `json:"contact"`
		CreatedAt string `json:"created_at"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.ID, &x.Demo, &x.Rating, &x.Message, &x.Contact, &x.CreatedAt); err == nil {
			out = append(out, x)
		}
	}
	platform.JSON(w, http.StatusOK, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
