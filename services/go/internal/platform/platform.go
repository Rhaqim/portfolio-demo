// Package platform holds the plumbing every Go demo service shares: the
// shared SQLite database, anonymous visitor identity, per-IP rate limiting,
// JSON helpers and a graceful HTTP server.
package platform

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// Env returns the environment variable or a fallback.
func Env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// OpenDB opens the shared demo database. Every service on the box points at
// the same file; WAL plus a generous busy timeout lets several processes
// write to it without stepping on each other. Each service namespaces its
// tables (loom_*, buckt_*, demo_*), so nothing collides.
func OpenDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ---------------------------------------------------------------------------
// Visitor identity
// ---------------------------------------------------------------------------

const visitorCookie = "demo_vid"

type ctxKey struct{}

// Visitor returns the anonymous visitor ID attached by WithVisitor.
func Visitor(r *http.Request) string {
	v, _ := r.Context().Value(ctxKey{}).(string)
	return v
}

// WithVisitor gives every request a stable anonymous visitor ID, kept in an
// HttpOnly cookie shared across the demo subdomain. It is the sandbox key:
// buckt user IDs, loom owners and platform IDs are all derived from it, so
// one visitor can never see another's files or sessions.
func WithVisitor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ""
		if c, err := r.Cookie(visitorCookie); err == nil && validVisitorID(c.Value) {
			id = c.Value
		} else {
			id = RandomHex(16)
			http.SetCookie(w, &http.Cookie{
				Name:     visitorCookie,
				Value:    id,
				Path:     "/",
				MaxAge:   int((30 * 24 * time.Hour).Seconds()),
				HttpOnly: true,
				Secure:   r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil,
				SameSite: http.SameSiteLaxMode,
			})
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

func validVisitorID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// RandomHex returns n random bytes, hex encoded.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// ClientIP returns the caller's address. The stack runs behind Cloudflare and
// Caddy, so CF-Connecting-IP is authoritative; the origin firewall should only
// accept Cloudflare ranges, otherwise this header can be forged.
func ClientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Limiter is a fixed-window counter per key. Small and dependency-free; the
// demo runs as one instance per service, so in-memory is enough.
type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*bucket
}

type bucket struct {
	count int
	reset time.Time
}

// NewLimiter allows `limit` events per key per `window`.
func NewLimiter(limit int, window time.Duration) *Limiter {
	l := &Limiter{limit: limit, window: window, hits: map[string]*bucket{}}
	go func() {
		for range time.Tick(window) {
			l.mu.Lock()
			now := time.Now()
			for k, b := range l.hits {
				if now.After(b.reset) {
					delete(l.hits, k)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

// Allow records one event for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.hits[key]
	if !ok || now.After(b.reset) {
		b = &bucket{reset: now.Add(l.window)}
		l.hits[key] = b
	}
	b.count++
	return b.count <= l.limit
}

// Limit wraps a handler with a per-IP limiter.
func (l *Limiter) Limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(ClientIP(r)) {
			Error(w, http.StatusTooManyRequests, "Slow down a little, then try again.")
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

// JSON writes v as a JSON response.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes {"error": msg}.
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}

// Decode reads a JSON body of at most max bytes into v.
func Decode(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("request body is larger than %d bytes", max)
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// Serve runs h on addr until SIGINT/SIGTERM, then drains for up to 15s and
// runs the cleanup functions.
func Serve(name, addr string, h http.Handler, cleanup ...func()) {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", name)
	slog.SetDefault(log)

	srv := &http.Server{
		Addr:              addr,
		Handler:           recoverer(h),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	for _, fn := range cleanup {
		fn()
	}
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "path", r.URL.Path, "err", rec)
				Error(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Health is a trivial liveness handler.
func Health(w http.ResponseWriter, _ *http.Request) {
	JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
