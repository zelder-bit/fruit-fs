// Package httpx holds the small JSON helpers shared by the API handlers.
package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func JSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func Error(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Decode reads a small JSON request body into v. The body must arrive within
// 15 seconds, so a stalled client can't hold a connection open forever.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		Error(w, http.StatusBadRequest, "bad request body")
		return false
	}
	return true
}

// IdleBody wraps a long request body (an upload) with a rolling deadline: the
// transfer may take hours, but it fails if no data arrives for idle.
func IdleBody(w http.ResponseWriter, r *http.Request, idle time.Duration) io.Reader {
	rc := http.NewResponseController(w)
	return readerFunc(func(p []byte) (int, error) {
		rc.SetReadDeadline(time.Now().Add(idle))
		return r.Body.Read(p)
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
