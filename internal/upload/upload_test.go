package upload

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zelder-bit/fruit-fs/internal/auth"
)

func setup(t *testing.T, user string) (string, http.Handler) {
	t.Helper()
	data := t.TempDir()
	root, err := os.OpenRoot(data)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(root, 1<<20, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{User: user})))
		})
	})
	return data, mux
}

func call(h http.Handler, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func create(t *testing.T, h http.Handler, path string, size int) string {
	b, _ := json.Marshal(map[string]any{"path": path, "size": size})
	w := call(h, "POST", "/api/uploads", bytes.NewReader(b), nil)
	if w.Code != 200 {
		t.Fatal("create", w.Code, w.Body)
	}
	var out struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.ID
}

// interrupted reads part of its data and then fails, like a dropped connection.
type interrupted struct{ r io.Reader }

func (i *interrupted) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestResumeAfterInterruption(t *testing.T) {
	data, h := setup(t, "alice")
	content := bytes.Repeat([]byte("banana"), 1000) // 6000 bytes
	id := create(t, h, "fruit/bananas.txt", len(content))

	// First chunk dies part way through.
	call(h, "PATCH", "/api/uploads/"+id, &interrupted{bytes.NewReader(content[:2500])}, map[string]string{"Upload-Offset": "0"})

	w := call(h, "HEAD", "/api/uploads/"+id, nil, nil)
	off, _ := strconv.Atoi(w.Header().Get("Upload-Offset"))
	if off != 2500 {
		t.Fatalf("offset after interruption = %d", off)
	}
	// A wrong offset is refused.
	if w := call(h, "PATCH", "/api/uploads/"+id, bytes.NewReader(content), map[string]string{"Upload-Offset": "0"}); w.Code != 409 {
		t.Fatal("stale offset accepted", w.Code)
	}
	w = call(h, "PATCH", "/api/uploads/"+id, bytes.NewReader(content[off:]), map[string]string{"Upload-Offset": strconv.Itoa(off)})
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"done":true`)) {
		t.Fatal("final chunk", w.Code, w.Body)
	}
	got, err := os.ReadFile(filepath.Join(data, "fruit", "bananas.txt"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatal("file contents differ", err)
	}
}

func TestNoOverwriteAndLimits(t *testing.T) {
	data, h := setup(t, "alice")
	os.WriteFile(filepath.Join(data, "taken.txt"), []byte("x"), 0o644)
	for _, body := range []string{
		`{"path":"taken.txt","size":1}`,             // exists
		`{"path":".fruit-fs/session.key","size":1}`, // state dir
		`{"path":"huge.bin","size":2097152}`,        // over the 1 MiB limit
	} {
		if w := call(h, "POST", "/api/uploads", bytes.NewReader([]byte(body)), nil); w.Code == 200 {
			t.Errorf("%s accepted", body)
		}
	}
	// "../" is cleaned away, so this lands inside the data dir.
	create(t, h, "../escape.txt", 0)
	if _, err := os.Stat(filepath.Join(filepath.Dir(data), "escape.txt")); err == nil {
		t.Fatal("upload escaped the data dir")
	}
}

func TestOtherUsersCannotTouchUpload(t *testing.T) {
	data, h := setup(t, "alice")
	id := create(t, h, "a.txt", 3)
	// Same data dir, different user.
	root, _ := os.OpenRoot(data)
	s, _ := New(root, 0, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	s.Register(mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{User: "mallory"})))
		})
	})
	if w := call(mux, "PATCH", "/api/uploads/"+id, bytes.NewReader([]byte("bad")), map[string]string{"Upload-Offset": "0"}); w.Code != 404 {
		t.Fatal("another user wrote to the upload", w.Code)
	}
}
