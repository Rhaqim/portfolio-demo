// Command buckt is the buckt demo: every visitor gets a private, short-lived
// sandbox in a real buckt client (shared SQLite + local disk), and can upload,
// organise, trash, restore and inspect files while watching buckt's lifecycle
// events, derivatives, dedup and metrics happen.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Rhaqim/buckt"
	"github.com/Rhaqim/buckt/pkg/events"
	"github.com/Rhaqim/buckt/pkg/metrics"
	"github.com/Rhaqim/buckt/pkg/scan"

	"github.com/rhaqim/demo/internal/platform"
)

const (
	maxFileSize   = 5 << 20  // per file
	maxQuotaBytes = 25 << 20 // per sandbox
	maxQuotaFiles = 40       // per sandbox
	fileTTL       = time.Hour
	sandboxIdle   = 2 * time.Hour
)

// Content types the upload scanner lets through. Everything is sniffed from
// the bytes; the client's claimed type is ignored. No SVG or HTML: they can
// carry script, and this is a public upload form.
var allowed = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"application/pdf": true, "text/plain; charset=utf-8": true,
}

type server struct {
	db      *sql.DB
	bk      *buckt.Client
	metrics *metrics.Collector
	feed    *eventFeed
	derive  chan string
}

func main() {
	db, err := platform.OpenDB(platform.Env("DEMO_DB", "./data/demo.db"))
	if err != nil {
		panic(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS demo_buckt_sandboxes (
		visitor    TEXT PRIMARY KEY,
		root_id    TEXT NOT NULL,
		created_at TEXT NOT NULL,
		last_seen  TEXT NOT NULL
	)`); err != nil {
		panic(err)
	}

	s := &server{db: db, metrics: metrics.NewCollector(), feed: newEventFeed(), derive: make(chan string, 256)}

	bk, err := buckt.New(buckt.Config{
		Log:            buckt.LogConfig{LogTerminal: true, Silence: true},
		MediaDir:       platform.Env("BUCKT_MEDIA_DIR", "./data/buckt-media"),
		FlatNameSpaces: true,
	},
		buckt.WithDB(buckt.SQLite, db),
		buckt.WithTablePrefix("buckt_"),
		buckt.WithCache(buckt.CacheConfig{FileCacheConfig: buckt.FileCacheConfig{MaxCost: 64 << 20}}),
		buckt.WithMaxFileSize(maxFileSize),
		buckt.WithDedup(),
		buckt.WithMetrics(s.metrics),
		buckt.WithImageDerivatives(
			buckt.DerivativeSpec{Name: "thumbnail", MaxWidth: 240},
			buckt.DerivativeSpec{Name: "medium", MaxWidth: 960},
		),
		buckt.WithUploadScanner(scan.ScannerFunc(sniffScanner)),
		buckt.WithEventHandler(s.onEvent),
		buckt.WithExpirySweeper(time.Minute),
	)
	if err != nil {
		panic(err)
	}
	s.bk = bk

	// Derivatives are generated off the request path by two workers, which is
	// the pattern buckt's docs recommend: the event handler only enqueues.
	for range 2 {
		go func() {
			for id := range s.derive {
				if err := s.bk.GenerateDerivatives(id); err != nil {
					slog.Warn("derivatives", "file", id, "err", err)
				}
			}
		}()
	}
	go s.janitor()

	upload := platform.NewLimiter(60, time.Hour)
	general := platform.NewLimiter(600, time.Hour)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/buckt/health", platform.Health)
	mux.HandleFunc("GET /api/buckt/tree", general.Limit(s.tree))
	mux.HandleFunc("POST /api/buckt/files", upload.Limit(s.upload))
	mux.HandleFunc("GET /api/buckt/files/{id}/raw", general.Limit(s.raw))
	mux.HandleFunc("GET /api/buckt/files/{id}/derivatives/{name}", general.Limit(s.derivative))
	mux.HandleFunc("PUT /api/buckt/files/{id}/metadata", general.Limit(s.setMetadata))
	mux.HandleFunc("POST /api/buckt/files/{id}/move", general.Limit(s.move))
	mux.HandleFunc("POST /api/buckt/files/{id}/restore", general.Limit(s.restore))
	mux.HandleFunc("DELETE /api/buckt/files/{id}", general.Limit(s.deleteFile))
	mux.HandleFunc("POST /api/buckt/folders", general.Limit(s.newFolder))
	mux.HandleFunc("DELETE /api/buckt/folders/{id}", general.Limit(s.deleteFolder))
	mux.HandleFunc("POST /api/buckt/reset", general.Limit(s.reset))
	mux.HandleFunc("GET /api/buckt/events", general.Limit(s.events))

	platform.Serve("buckt", platform.Env("ADDR", ":8080"), platform.WithVisitor(mux), func() {
		bk.Close()
		db.Close()
	})
}

// userID maps an anonymous visitor to a buckt owner.
func userID(r *http.Request) string { return "visitor-" + platform.Visitor(r) }

// ---------------------------------------------------------------------------
// Sandbox lifecycle
// ---------------------------------------------------------------------------

// sandbox returns the visitor's root folder, creating and seeding it on first
// use, and bumps last_seen so the janitor leaves active sandboxes alone.
func (s *server) sandbox(ctx context.Context, r *http.Request) (string, error) {
	vid, user := platform.Visitor(r), userID(r)
	now := time.Now().UTC().Format(time.RFC3339)

	var root string
	err := s.db.QueryRowContext(ctx, `SELECT root_id FROM demo_buckt_sandboxes WHERE visitor = ?`, vid).Scan(&root)
	if err == nil {
		if f, ferr := s.bk.GetFolderWithContentContext(ctx, user, root); ferr == nil && f.UserID == user && f.ID.String() == root {
			_, _ = s.db.ExecContext(ctx, `UPDATE demo_buckt_sandboxes SET last_seen = ? WHERE visitor = ?`, now, vid)
			return root, nil
		}
		// The folder vanished (swept or reset); fall through and recreate.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	root, err = s.bk.NewFolderContext(ctx, user, "", "sandbox-"+vid[:8], "Demo sandbox")
	if err != nil {
		return "", fmt.Errorf("create sandbox: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO demo_buckt_sandboxes (visitor, root_id, created_at, last_seen) VALUES (?, ?, ?, ?)
		 ON CONFLICT(visitor) DO UPDATE SET root_id = excluded.root_id, last_seen = excluded.last_seen`,
		vid, root, now, now); err != nil {
		return "", err
	}
	s.seed(ctx, user, root)
	return root, nil
}

// seed drops a couple of files into a fresh sandbox so the first screen is
// not empty and the derivative pipeline has something to chew on.
func (s *server) seed(ctx context.Context, user, root string) {
	photos, err := s.bk.NewFolderContext(ctx, user, root, "photos", "")
	if err != nil {
		slog.Warn("seed folder", "err", err)
		return
	}
	if id, err := s.bk.UploadFileContext(ctx, user, photos, "gradient.png", "image/png", samplePNG()); err == nil {
		_ = s.bk.SetFileTTL(id, fileTTL)
		_ = s.bk.SetFileMetadata(id, map[string]string{"source": "seed", "generated": "true"})
	}
	readme := "This sandbox is yours. Upload files, make folders, trash and restore things.\n" +
		"Upload the same file twice in one folder and buckt stores it once.\n" +
		"Everything here deletes itself an hour after upload.\n"
	if id, err := s.bk.UploadFileContext(ctx, user, root, "README.txt", "text/plain; charset=utf-8", []byte(readme)); err == nil {
		_ = s.bk.SetFileTTL(id, fileTTL)
	}
}

// janitor removes sandboxes nobody has touched for a while. File TTLs already
// clear the bytes; this clears the empty folder trees and the trash.
func (s *server) janitor() {
	for range time.Tick(5 * time.Minute) {
		cutoff := time.Now().UTC().Add(-sandboxIdle).Format(time.RFC3339)
		rows, err := s.db.Query(`SELECT visitor, root_id FROM demo_buckt_sandboxes WHERE last_seen < ?`, cutoff)
		if err != nil {
			continue
		}
		type box struct{ vid, root string }
		var stale []box
		for rows.Next() {
			var b box
			if rows.Scan(&b.vid, &b.root) == nil {
				stale = append(stale, b)
			}
		}
		rows.Close()
		for _, b := range stale {
			s.wipe("visitor-"+b.vid, b.root)
			_, _ = s.db.Exec(`DELETE FROM demo_buckt_sandboxes WHERE visitor = ?`, b.vid)
			s.feed.drop("visitor-" + b.vid)
		}
		if len(stale) > 0 {
			slog.Info("janitor swept sandboxes", "count", len(stale))
		}
	}
}

func (s *server) wipe(user, root string) {
	if _, err := s.bk.DeleteFolderPermanently(user, root); err != nil && !errors.Is(err, buckt.ErrNotFound) {
		slog.Warn("wipe sandbox", "user", user, "err", err)
	}
	if trash, err := s.bk.GetTrashFolder(user); err == nil {
		for _, f := range trash.Files {
			_, _ = s.bk.DeleteFilePermanently(f.ID.String())
		}
		for _, f := range trash.Folders {
			_, _ = s.bk.DeleteFolderPermanently(user, f.ID.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type fileView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Size        int64             `json:"size"`
	ContentType string            `json:"content_type"`
	Hash        string            `json:"hash"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	Derivatives bool              `json:"derivatives"`
	CreatedAt   time.Time         `json:"created_at"`
}

type folderView struct {
	ID      string       `json:"id"`
	Name    string       `json:"name"`
	Folders []folderView `json:"folders"`
	Files   []fileView   `json:"files"`
}

func (s *server) tree(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	root, err := s.sandbox(ctx, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	user := userID(r)
	tree, files, bytes, err := s.walk(ctx, user, root, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	trash := []fileView{}
	if t, err := s.bk.GetTrashFolderContext(ctx, user); err == nil {
		for _, f := range t.Files {
			trash = append(trash, viewFile(f.ID.String(), f.Name, f.Size, f.ContentType, f.Hash, f.Metadata, f.ExpiresAt, f.DerivativesBytes > 0, f.CreatedAt))
		}
	}
	snap, _ := s.bk.MetricsSnapshot()
	platform.JSON(w, http.StatusOK, map[string]any{
		"root":  tree,
		"trash": trash,
		"usage": map[string]any{
			"files": files, "bytes": bytes,
			"max_files": maxQuotaFiles, "max_bytes": maxQuotaBytes, "max_file_bytes": maxFileSize,
		},
		"backend": s.bk.BackendName(),
		"metrics": snap,
	})
}

// walk renders a folder subtree. Depth is capped; the UI never nests deeper.
func (s *server) walk(ctx context.Context, user, id string, depth int) (folderView, int, int64, error) {
	f, err := s.bk.GetFolderWithContentContext(ctx, user, id)
	if err != nil {
		return folderView{}, 0, 0, err
	}
	if f.UserID != user {
		return folderView{}, 0, 0, buckt.ErrNotFound
	}
	v := folderView{ID: f.ID.String(), Name: f.Name, Folders: []folderView{}, Files: []fileView{}}
	count, total := 0, int64(0)
	for _, x := range f.Files {
		if x.Pending {
			continue
		}
		v.Files = append(v.Files, viewFile(x.ID.String(), x.Name, x.Size, x.ContentType, x.Hash, x.Metadata, x.ExpiresAt, x.DerivativesBytes > 0, x.CreatedAt))
		count++
		total += x.Size
	}
	if depth < 4 {
		for _, sub := range f.Folders {
			if sub.Name == "__trash__" {
				continue
			}
			child, c, b, err := s.walk(ctx, user, sub.ID.String(), depth+1)
			if err != nil {
				continue
			}
			v.Folders = append(v.Folders, child)
			count += c
			total += b
		}
	}
	return v, count, total, nil
}

func viewFile(id, name string, size int64, ct, hash string, md map[string]string, exp *time.Time, der bool, created time.Time) fileView {
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return fileView{ID: id, Name: name, Size: size, ContentType: ct, Hash: hash, Metadata: md, ExpiresAt: exp, Derivatives: der, CreatedAt: created}
}

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	root, err := s.sandbox(ctx, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	user := userID(r)

	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize+64<<10)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		platform.Error(w, http.StatusRequestEntityTooLarge, "Files can be up to 5 MB.")
		return
	}
	defer r.MultipartForm.RemoveAll()

	parent := r.FormValue("folder_id")
	if parent == "" {
		parent = root
	}
	if !s.ownsFolder(ctx, user, root, parent) {
		platform.Error(w, http.StatusNotFound, "folder not found")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		platform.Error(w, http.StatusBadRequest, "attach a file")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		platform.Error(w, http.StatusBadRequest, "couldn't read the upload")
		return
	}
	if len(data) > maxFileSize {
		platform.Error(w, http.StatusRequestEntityTooLarge, "Files can be up to 5 MB.")
		return
	}

	_, files, used, err := s.walk(ctx, user, root, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	if files >= maxQuotaFiles || used+int64(len(data)) > maxQuotaBytes {
		platform.Error(w, http.StatusInsufficientStorage, "This sandbox is full. Delete something, or reset it.")
		return
	}

	// Record what's already in the folder so we can tell the visitor when
	// buckt deduplicated their upload onto an existing blob.
	before := map[string]bool{}
	if f, err := s.bk.GetFolderWithContentContext(ctx, user, parent); err == nil {
		for _, x := range f.Files {
			before[x.ID.String()] = true
		}
	}

	name := cleanName(header.Filename)
	id, err := s.bk.UploadFileContext(ctx, user, parent, name, http.DetectContentType(data), data)
	if err != nil {
		s.fail(w, err)
		return
	}
	_ = s.bk.SetFileTTL(id, fileTTL)
	platform.JSON(w, http.StatusCreated, map[string]any{"id": id, "deduplicated": before[id]})
}

// sniffScanner is the upload scanner buckt runs before any byte is stored.
func sniffScanner(_ context.Context, name string, data []byte) error {
	ct := http.DetectContentType(data)
	if !allowed[ct] {
		return fmt.Errorf("%s looks like %s; this demo accepts images, PDFs and plain text", name, strings.Split(ct, ";")[0])
	}
	return nil
}

func cleanName(n string) string {
	n = strings.TrimSpace(n)
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, n)
	if len(n) > 120 {
		n = n[len(n)-120:]
	}
	if n == "" || n == "." || n == ".." {
		n = "upload"
	}
	return n
}

// ownedFile loads a file and checks it belongs to this visitor. buckt's
// file-by-ID calls are not owner-scoped, so every handler goes through this.
func (s *server) ownedFile(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	f, err := s.bk.GetFileContext(r.Context(), id)
	if err != nil || f.UserID != userID(r) {
		platform.Error(w, http.StatusNotFound, "file not found")
		return "", false
	}
	return id, true
}

// ownsFolder checks id is the sandbox root or a folder beneath it.
func (s *server) ownsFolder(ctx context.Context, user, root, id string) bool {
	if id == root {
		return true
	}
	f, err := s.bk.GetFolderWithContentContext(ctx, user, id)
	return err == nil && f.UserID == user && f.ID.String() == id
}

func (s *server) raw(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownedFile(w, r)
	if !ok {
		return
	}
	f, body, err := s.bk.GetFileStreamContext(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer body.Close()
	serveSafely(w, f.ContentType, f.Name)
	_, _ = io.Copy(w, body)
}

func (s *server) derivative(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownedFile(w, r)
	if !ok {
		return
	}
	data, ct, err := s.bk.GetDerivative(id, r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	serveSafely(w, ct, r.PathValue("name"))
	_, _ = w.Write(data)
}

// serveSafely sets headers so a user upload can never execute in our origin.
func serveSafely(w http.ResponseWriter, ct, name string) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=300")
	disp := "attachment"
	if strings.HasPrefix(ct, "image/") || ct == "application/pdf" || strings.HasPrefix(ct, "text/plain") {
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, strings.ReplaceAll(name, `"`, "")))
}

func (s *server) setMetadata(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownedFile(w, r)
	if !ok {
		return
	}
	var md map[string]string
	if err := platform.Decode(w, r, 4<<10, &md); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(md) > 12 {
		platform.Error(w, http.StatusBadRequest, "up to 12 keys")
		return
	}
	for k, v := range md {
		if k == "" || len(k) > 40 || len(v) > 200 {
			platform.Error(w, http.StatusBadRequest, "keys up to 40 characters, values up to 200")
			return
		}
	}
	if err := s.bk.SetFileMetadata(id, md); err != nil {
		s.fail(w, err)
		return
	}
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) move(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownedFile(w, r)
	if !ok {
		return
	}
	var body struct {
		FolderID string `json:"folder_id"`
	}
	if err := platform.Decode(w, r, 1<<10, &body); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	root, err := s.sandbox(r.Context(), r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.ownsFolder(r.Context(), userID(r), root, body.FolderID) {
		platform.Error(w, http.StatusNotFound, "folder not found")
		return
	}
	if err := s.bk.MoveFileContext(r.Context(), id, body.FolderID); err != nil {
		s.fail(w, err)
		return
	}
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownedFile(w, r)
	if !ok {
		return
	}
	// buckt's "smart re-delete": deleting something already in the trash
	// hard-deletes it, so one endpoint covers both.
	if _, err := s.bk.DeleteFileContext(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) restore(w http.ResponseWriter, r *http.Request) {
	id, ok := s.ownedFile(w, r)
	if !ok {
		return
	}
	if err := s.bk.RestoreFile(userID(r), id); err != nil {
		s.fail(w, err)
		return
	}
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) newFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
	}
	if err := platform.Decode(w, r, 1<<10, &body); err != nil {
		platform.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	root, err := s.sandbox(ctx, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if body.ParentID == "" {
		body.ParentID = root
	}
	if !s.ownsFolder(ctx, userID(r), root, body.ParentID) {
		platform.Error(w, http.StatusNotFound, "folder not found")
		return
	}
	name := cleanName(body.Name)
	if len(name) > 60 {
		name = name[:60]
	}
	id, err := s.bk.NewFolderContext(ctx, userID(r), body.ParentID, name, "")
	if err != nil {
		s.fail(w, err)
		return
	}
	platform.JSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *server) deleteFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	root, err := s.sandbox(ctx, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	if id == root || !s.ownsFolder(ctx, userID(r), root, id) {
		platform.Error(w, http.StatusNotFound, "folder not found")
		return
	}
	if _, err := s.bk.DeleteFolderContext(ctx, id); err != nil {
		s.fail(w, err)
		return
	}
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) reset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	root, err := s.sandbox(ctx, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.wipe(userID(r), root)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM demo_buckt_sandboxes WHERE visitor = ?`, platform.Visitor(r))
	s.feed.drop(userID(r))
	platform.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	platform.JSON(w, http.StatusOK, s.feed.list(userID(r)))
}

// onEvent is buckt's lifecycle hook. It runs synchronously after each
// operation commits, so it only records and enqueues.
func (s *server) onEvent(_ context.Context, e events.Event) {
	s.feed.add(e)
	if e.Type == events.FileUploaded && strings.HasPrefix(e.ContentType, "image/") &&
		(strings.Contains(e.ContentType, "png") || strings.Contains(e.ContentType, "jpeg")) {
		select {
		case s.derive <- e.FileID:
		default: // queue full; skip rather than block the upload
		}
	}
}

func (s *server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, buckt.ErrNotFound):
		platform.Error(w, http.StatusNotFound, "not found")
	case errors.Is(err, buckt.ErrInvalidID), errors.Is(err, buckt.ErrInvalidName):
		platform.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, buckt.ErrAlreadyExists):
		platform.Error(w, http.StatusConflict, "Something with that name is already in this folder.")
	case errors.Is(err, buckt.ErrFileTooLarge):
		platform.Error(w, http.StatusRequestEntityTooLarge, "Files can be up to 5 MB.")
	case errors.Is(err, buckt.ErrUploadRejected):
		msg := err.Error()
		if i := strings.LastIndex(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		platform.JSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Rejected by the upload scanner: " + msg})
	default:
		slog.Error("buckt", "err", err)
		platform.Error(w, http.StatusInternalServerError, "Something went wrong on our side.")
	}
}

// ---------------------------------------------------------------------------
// Event feed: the last few lifecycle events per visitor, in memory.
// ---------------------------------------------------------------------------

type eventFeed struct {
	mu     sync.Mutex
	byUser map[string][]events.Event
}

func newEventFeed() *eventFeed { return &eventFeed{byUser: map[string][]events.Event{}} }

func (f *eventFeed) add(e events.Event) {
	if !strings.HasPrefix(e.UserID, "visitor-") {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	list := append(f.byUser[e.UserID], e)
	if len(list) > 40 {
		list = list[len(list)-40:]
	}
	f.byUser[e.UserID] = list
}

func (f *eventFeed) list(user string) []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]events.Event, len(f.byUser[user]))
	copy(out, f.byUser[user])
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (f *eventFeed) drop(user string) {
	f.mu.Lock()
	delete(f.byUser, user)
	f.mu.Unlock()
}
