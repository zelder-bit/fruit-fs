package files

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func setup(t *testing.T) (*Service, string, http.Handler) {
	t.Helper()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	os.MkdirAll(filepath.Join(data, "photos"), 0o755)
	os.WriteFile(filepath.Join(data, "photos", "cat.jpg"), []byte("meow"), 0o644)
	os.WriteFile(filepath.Join(data, "notes.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(data, "page.html"), []byte("<script>alert(1)</script>"), 0o644)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("outside"), 0o644)
	os.Symlink(filepath.Join(base, "secret.txt"), filepath.Join(data, "escape"))
	s, err := New(data, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Register(mux, func(h http.Handler) http.Handler { return h })
	return s, data, mux
}

func do(h http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, rd))
	return w
}

func TestClean(t *testing.T) {
	good := map[string]string{"": ".", "/": ".", "/a/b": "a/b", "a//b/": "a/b", "/a/../b": "b", "../a": "a"}
	for in, want := range good {
		if got, err := Clean(in); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{".fruit-fs", "/.fruit-fs/session.key", "a\x00b", `a\..\b`} {
		if _, err := Clean(bad); err == nil {
			t.Errorf("Clean(%q) should fail", bad)
		}
	}
}

func TestListHidesStateDir(t *testing.T) {
	_, _, h := setup(t)
	w := do(h, "GET", "/api/ls?path=/", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), StateDir) {
		t.Fatal("state dir listed")
	}
	if strings.Contains(w.Body.String(), `"escape"`) {
		t.Fatal("symlink escaping the root was listed")
	}
	var out struct{ Entries []Entry }
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Entries) == 0 || !out.Entries[0].Dir {
		t.Fatalf("folders should sort first: %+v", out.Entries)
	}
}

func TestEscapesBlocked(t *testing.T) {
	_, _, h := setup(t)
	for _, p := range []string{"escape", "../secret.txt", "/.fruit-fs/session.key", "photos/../../secret.txt"} {
		w := do(h, "GET", "/api/dl?path="+url.QueryEscape(p), nil)
		if w.Code == 200 || strings.Contains(w.Body.String(), "outside") {
			t.Errorf("%q: got %d %q", p, w.Code, w.Body)
		}
	}
}

func TestDownloadHeaders(t *testing.T) {
	_, _, h := setup(t)
	w := do(h, "GET", "/api/dl?path=page.html&inline=1", nil)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("html must be served as plain text, got %q", ct)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatal("missing sandbox CSP")
	}
	w = do(h, "GET", "/api/dl?path=photos/cat.jpg", nil)
	if w.Body.String() != "meow" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %q %q", w.Body, w.Header().Get("Content-Disposition"))
	}
}

func TestMoveDeleteRestore(t *testing.T) {
	s, data, h := setup(t)
	if w := do(h, "POST", "/api/mkdir", map[string]string{"path": "docs/new"}); w.Code != 204 {
		t.Fatal("mkdir", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/move", map[string]string{"from": "notes.txt", "to": "docs/notes.txt"}); w.Code != 204 {
		t.Fatal("move", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/move", map[string]string{"from": "photos", "to": "photos/inside"}); w.Code != 400 {
		t.Fatal("moving a folder into itself should fail", w.Code)
	}
	if w := do(h, "POST", "/api/move", map[string]string{"from": "page.html", "to": "docs/notes.txt"}); w.Code != 409 {
		t.Fatal("move must not overwrite", w.Code)
	}
	if w := do(h, "POST", "/api/delete", map[string]any{"paths": []string{"docs/notes.txt"}}); w.Code != 204 {
		t.Fatal("delete", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(data, "docs", "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("file still there after delete")
	}
	items := s.trashItems()
	if len(items) != 1 || items[0].Orig != "docs/notes.txt" {
		t.Fatalf("trash: %+v", items)
	}
	if w := do(h, "POST", "/api/trash/restore", map[string]string{"id": items[0].ID}); w.Code != 204 {
		t.Fatal("restore", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "docs", "notes.txt")); string(b) != "hello" {
		t.Fatal("restore lost the contents")
	}
	if w := do(h, "POST", "/api/delete", map[string]any{"paths": []string{"/"}}); w.Code != 400 {
		t.Fatal("deleting the root must fail", w.Code)
	}
}

func TestZipAndSearch(t *testing.T) {
	_, _, h := setup(t)
	w := do(h, "GET", "/api/zip?path=photos", nil)
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "photos/,photos/cat.jpg" {
		t.Fatalf("zip entries: %v", names)
	}
	w = do(h, "GET", "/api/zip?path=/", nil)
	if strings.Contains(w.Body.String(), "session.key") {
		t.Fatal("zip of root included the state dir")
	}
	w = do(h, "GET", "/api/search?q=CAT", nil)
	if !strings.Contains(w.Body.String(), "photos/cat.jpg") {
		t.Fatalf("search: %s", w.Body)
	}
}

func TestFIFODoesNotHang(t *testing.T) {
	_, data, h := setup(t)
	if err := syscall.Mkfifo(filepath.Join(data, "pipe"), 0o644); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	done := make(chan int)
	go func() { done <- do(h, "GET", "/api/dl?path=pipe", nil).Code }()
	go func() { do(h, "GET", "/api/ls?path=pipe", nil) }()
	select {
	case code := <-done:
		if code == 200 {
			t.Fatal("FIFO was served")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("download of a FIFO hung")
	}
}

func TestPublishNoReplace(t *testing.T) {
	s, data, _ := setup(t)
	os.WriteFile(filepath.Join(data, "a.part"), []byte("new"), 0o644)
	if err := PublishNoReplace(s.root, "a.part", "notes.txt"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("overwrote an existing file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "notes.txt")); string(b) != "hello" {
		t.Fatal("existing file changed")
	}
	if err := PublishNoReplace(s.root, "a.part", "fresh.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "a.part")); !os.IsNotExist(err) {
		t.Fatal("source left behind")
	}
}
