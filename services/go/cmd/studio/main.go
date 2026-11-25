// Command studio serves Noewright Studio's API under /api/studio. The
// frontend is a separate site; the gateway routes /api/studio/* here.
//
//	STUDIO_DB      SQLite file (default ./data/studio.db): Studio keeps its own
//	               database so visitors' graphs and sessions stay apart from
//	               the demos'
//	STUDIO_STEPS   agent steps per visitor per day (default 300)
//	ADDR           listen address (default :8080)
package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rhaqim/noewright"
	"github.com/rhaqim/noewright/schema"

	"github.com/rhaqim/demo/internal/platform"
	"github.com/rhaqim/demo/internal/studio"
)

func main() {
	ctx := context.Background()
	path := platform.Env("STUDIO_DB", "./data/studio.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	db, err := platform.OpenDB(path)
	if err != nil {
		panic(err)
	}
	if err := schema.NewLoader(schema.DialectSQLite).Apply(ctx, db); err != nil {
		panic(err)
	}
	steps, _ := strconv.Atoi(platform.Env("STUDIO_STEPS", "300"))
	s, err := studio.New(ctx, studio.Config{DB: db, Dialect: noewright.DialectSQLite, StepsPerDay: steps})
	if err != nil {
		panic(err)
	}
	platform.Serve("studio", platform.Env("ADDR", ":8080"), s.Handler(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Close(ctx)
		db.Close()
	})
}
