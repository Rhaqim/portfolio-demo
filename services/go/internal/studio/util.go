package studio

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// stream writes Server-Sent Events.
type stream struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
}

func newStream(w http.ResponseWriter) (*stream, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &stream{w: w, f: f}, true
}

func (st *stream) send(event string, v any) {
	b, _ := json.Marshal(v)
	st.mu.Lock()
	defer st.mu.Unlock()
	fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", event, b)
	st.f.Flush()
}
