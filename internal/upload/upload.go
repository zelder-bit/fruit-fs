// Package upload receives files in chunks so a big upload can pick up where it
// stopped after a dropped connection.
//
//	POST   /api/uploads        {"path","size","replace"} -> {"id","offset"}
//	HEAD   /api/uploads/{id}   -> Upload-Offset: bytes received so far
//	PATCH  /api/uploads/{id}   Upload-Offset: n, body = next chunk -> {"offset","done"}
//	DELETE /api/uploads/{id}   cancel
//
// Chunks are appended to .fruit-fs/uploads/<id>.part; the last one renames the
// file into place, so a half-finished upload never shows up in the browser.
package upload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zelder-bit/fruit-fs/internal/auth"
	"github.com/zelder-bit/fruit-fs/internal/files"
	"github.com/zelder-bit/fruit-fs/internal/httpx"
)

const (
	dir        = files.StateDir + "/uploads"
	maxStale   = 24 * time.Hour
	maxPending = 256              // unfinished uploads across everyone
	idle       = 2 * time.Minute // a chunk fails if no bytes arrive for this long
)

var validID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type meta struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Replace bool      `json:"replace"`
	User    string    `json:"user"`
	Created time.Time `json:"created"`
}

type Service struct {
	root      *os.Root
	maxUpload int64
	log       *slog.Logger
	locks     sync.Map // id -> *sync.Mutex
	active    sync.Map // id -> context.CancelFunc of the chunk being received
}

func New(root *os.Root, maxUpload int64, log *slog.Logger) (*Service, error) {
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Service{root: root, maxUpload: maxUpload, log: log}, nil
}

func (s *Service) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("POST /api/uploads", wrap(http.HandlerFunc(s.create)))
	mux.Handle("HEAD /api/uploads/{id}", wrap(http.HandlerFunc(s.status)))
	mux.Handle("PATCH /api/uploads/{id}", wrap(http.HandlerFunc(s.patch)))
	mux.Handle("DELETE /api/uploads/{id}", wrap(http.HandlerFunc(s.cancel)))
}

func partPath(id string) string { return path.Join(dir, id+".part") }
func metaPath(id string) string { return path.Join(dir, id+".json") }

func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string
		Size    int64
		Replace bool
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	p, err := files.Clean(body.Path)
	if err != nil || p == "." {
		httpx.Error(w, http.StatusBadRequest, "that path is not allowed")
		return
	}
	if body.Size < 0 {
		httpx.Error(w, http.StatusBadRequest, "bad size")
		return
	}
	if s.maxUpload > 0 && body.Size > s.maxUpload {
		httpx.Error(w, http.StatusRequestEntityTooLarge, "file is bigger than the upload limit")
		return
	}
	if fi, err := s.root.Lstat(p); err == nil && (!body.Replace || fi.IsDir()) {
		httpx.Error(w, http.StatusConflict, "something with that name already exists")
		return
	}
	if des, _ := fs.ReadDir(s.root.FS(), dir); len(des) >= 2*maxPending {
		httpx.Error(w, http.StatusTooManyRequests, "too many unfinished uploads; finish or cancel some first")
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	m := meta{Path: p, Size: body.Size, Replace: body.Replace, User: auth.From(r.Context()).User, Created: time.Now()}
	mb, _ := json.Marshal(m)
	if err := s.root.WriteFile(metaPath(id), mb, 0o600); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.root.WriteFile(partPath(id), nil, 0o644); err != nil {
		s.serverError(w, err)
		return
	}
	if body.Size == 0 {
		if err := s.finish(id, m); err != nil {
			s.finishError(w, err)
			return
		}
		httpx.JSON(w, map[string]any{"id": id, "offset": 0, "done": true})
		return
	}
	httpx.JSON(w, map[string]any{"id": id, "offset": 0})
}

// load finds an upload belonging to the caller.
func (s *Service) load(w http.ResponseWriter, r *http.Request) (string, meta, int64, bool) {
	id := r.PathValue("id")
	var m meta
	if !validID.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "no such upload")
		return "", m, 0, false
	}
	b, err := s.root.ReadFile(metaPath(id))
	if err != nil || json.Unmarshal(b, &m) != nil || m.User != auth.From(r.Context()).User {
		httpx.Error(w, http.StatusNotFound, "no such upload")
		return "", m, 0, false
	}
	fi, err := s.root.Stat(partPath(id))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "no such upload")
		return "", m, 0, false
	}
	return id, m, fi.Size(), true
}

func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	_, _, off, ok := s.load(w, r)
	if !ok {
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(off, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) lock(id string) (*sync.Mutex, bool) {
	l, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	return mu, mu.TryLock()
}

func (s *Service) patch(w http.ResponseWriter, r *http.Request) {
	id, m, off, ok := s.load(w, r)
	if !ok {
		return
	}
	mu, ok := s.lock(id)
	if !ok {
		httpx.Error(w, http.StatusConflict, "this upload is already receiving a chunk")
		return
	}
	defer mu.Unlock()
	// Re-read the size now that we hold the lock.
	if fi, err := s.root.Stat(partPath(id)); err == nil {
		off = fi.Size()
	}
	claimed, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || claimed != off {
		w.Header().Set("Upload-Offset", strconv.FormatInt(off, 10))
		httpx.Error(w, http.StatusConflict, "offset mismatch")
		return
	}
	f, err := s.root.OpenFile(partPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.serverError(w, err)
		return
	}
	ctx, stop := context.WithCancel(r.Context())
	s.active.Store(id, stop)
	defer func() { s.active.Delete(id); stop() }()
	body := cancellable(ctx, httpx.IdleBody(w, r, idle))
	remaining := m.Size - off
	n, copyErr := io.Copy(f, io.LimitReader(body, remaining))
	closeErr := f.Close()
	off += n
	if ctx.Err() != nil {
		return // cancelled, or the client went away
	}
	if extra, _ := io.ReadFull(body, make([]byte, 1)); extra > 0 {
		httpx.Error(w, http.StatusRequestEntityTooLarge, "more data than the declared size")
		return
	}
	if err := errors.Join(copyErr, closeErr); err != nil {
		// A dropped connection is normal; the client resumes from Upload-Offset.
		w.Header().Set("Upload-Offset", strconv.FormatInt(off, 10))
		httpx.Error(w, http.StatusBadRequest, "chunk interrupted")
		return
	}
	// Cancelled while we were writing? Then the metadata is gone.
	if _, err := s.root.Stat(metaPath(id)); err != nil {
		httpx.Error(w, http.StatusNotFound, "no such upload")
		return
	}
	done := off == m.Size
	if done {
		if err := s.finish(id, m); err != nil {
			s.finishError(w, err)
			return
		}
	}
	httpx.JSON(w, map[string]any{"offset": off, "done": done})
}

var errExists = errors.New("exists")

func (s *Service) finish(id string, m meta) error {
	if parent := path.Dir(m.Path); parent != "." {
		if err := s.root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	var err error
	if m.Replace {
		if fi, lerr := s.root.Lstat(m.Path); lerr == nil && fi.IsDir() {
			return errExists
		}
		err = s.root.Rename(partPath(id), m.Path)
	} else {
		err = files.PublishNoReplace(s.root, partPath(id), m.Path)
	}
	if errors.Is(err, fs.ErrExist) {
		return errExists
	}
	if err != nil {
		return err
	}
	s.root.Remove(metaPath(id))
	s.locks.Delete(id)
	s.log.Info("upload finished", "path", m.Path, "bytes", m.Size, "user", m.User)
	return nil
}

func (s *Service) finishError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errExists):
		httpx.Error(w, http.StatusConflict, "something with that name appeared while uploading")
	case errors.Is(err, fs.ErrNotExist), strings.Contains(err.Error(), "not a directory"):
		httpx.Error(w, http.StatusConflict, "the destination folder is not usable")
	default:
		s.serverError(w, err)
	}
}

func (s *Service) cancel(w http.ResponseWriter, r *http.Request) {
	id, _, _, ok := s.load(w, r)
	if !ok {
		return
	}
	// Stop a chunk that's still arriving, then wait for it to let go.
	if stop, ok := s.active.Load(id); ok {
		stop.(context.CancelFunc)()
	}
	l, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	s.root.Remove(partPath(id))
	s.root.Remove(metaPath(id))
	mu.Unlock()
	s.locks.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) serverError(w http.ResponseWriter, err error) {
	s.log.Error("upload failed", "err", err)
	httpx.Error(w, http.StatusInternalServerError, "something went wrong")
}

// Janitor deletes uploads abandoned for more than a day, hourly until stop closes.
func (s *Service) Janitor(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		des, _ := fs.ReadDir(s.root.FS(), dir)
		for _, de := range des {
			id, isPart := strings.CutSuffix(de.Name(), ".part")
			// The .part file's time moves with every chunk, so it says whether the upload is alive.
			if fi, err := de.Info(); isPart && err == nil && time.Since(fi.ModTime()) > maxStale {
				mu, ok := s.lock(id)
				if !ok {
					continue // a chunk is arriving right now, so it isn't abandoned
				}
				s.root.Remove(partPath(id))
				s.root.Remove(metaPath(id))
				mu.Unlock()
				s.locks.Delete(id)
			}
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// cancellable stops a read as soon as ctx is cancelled.
func cancellable(ctx context.Context, r io.Reader) io.Reader {
	return readFunc(func(p []byte) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return r.Read(p)
	})
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }
