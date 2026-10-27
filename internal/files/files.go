// Package files is the file browser API. Every file operation goes through an
// os.Root, which refuses any path (including symlinks) that would leave the data
// directory, so "../../etc/passwd" style requests cannot escape.
package files

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zelder-bit/fruit-fs/internal/httpx"
)

// StateDir is fruit-fs's own folder inside the data directory (trash, partial
// uploads, session key). It is hidden from listings and cannot be touched via the API.
const StateDir = ".fruit-fs"

type Service struct {
	root *os.Root
	dir  string
	log  *slog.Logger
}

func New(dir string, log *slog.Logger) (*Service, error) {
	if err := os.MkdirAll(filepath.Join(dir, StateDir, "trash"), 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Service{root: root, dir: dir, log: log}, nil
}

func (s *Service) Root() *os.Root { return s.root }

var errBadPath = errors.New("bad path")

// Clean turns a client path ("/photos/cat.jpg", "photos//cat.jpg") into a
// root-relative one ("photos/cat.jpg"; "." for the top). It rejects paths that
// climb out or point into StateDir. os.Root is the real guard; this keeps error
// messages friendly and protects StateDir.
func Clean(p string) (string, error) {
	if strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return "", errBadPath
	}
	c := path.Clean("/" + p)[1:]
	if c == "" {
		return ".", nil
	}
	first, _, _ := strings.Cut(c, "/")
	if first == ".." || first == StateDir {
		return "", errBadPath
	}
	return c, nil
}

type Entry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

func entryOf(fi fs.FileInfo) Entry {
	return Entry{Name: fi.Name(), Dir: fi.IsDir(), Size: fi.Size(), ModTime: fi.ModTime()}
}

// Register adds the file routes to mux; wrap is the auth middleware.
func (s *Service) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	h := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, wrap(f)) }
	h("GET /api/ls", s.list)
	h("GET /api/dl", s.download)
	h("GET /api/zip", s.zip)
	h("GET /api/search", s.search)
	h("GET /api/stats", s.stats)
	h("POST /api/mkdir", s.mkdir)
	h("POST /api/move", s.move)
	h("POST /api/delete", s.delete)
	h("GET /api/trash", s.trashList)
	h("POST /api/trash/restore", s.trashRestore)
	h("POST /api/trash/empty", s.trashEmpty)
}

func (s *Service) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadPath):
		httpx.Error(w, http.StatusBadRequest, "that path is not allowed")
	case errors.Is(err, fs.ErrNotExist):
		httpx.Error(w, http.StatusNotFound, "not found")
	case errors.Is(err, fs.ErrExist):
		httpx.Error(w, http.StatusConflict, "something with that name already exists")
	case errors.Is(err, fs.ErrPermission):
		httpx.Error(w, http.StatusForbidden, "permission denied")
	default:
		s.log.Error("file operation failed", "err", err)
		httpx.Error(w, http.StatusInternalServerError, "something went wrong")
	}
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	p, err := Clean(r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	f, err := s.root.Open(p)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		if p == "." && de.Name() == StateDir {
			continue
		}
		// Stat through the root so symlinks are followed only when they stay inside.
		fi, err := s.root.Stat(path.Join(p, de.Name()))
		if err != nil {
			continue
		}
		out = append(out, entryOf(fi))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	httpx.JSON(w, map[string]any{"path": p, "entries": out})
}

// Types the browser may show inline. Everything else is always a download, so an
// uploaded .html file can never run as a page on this site.
func inlineType(name string) (string, bool) {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".txt", ".md", ".log", ".csv", ".json", ".yaml", ".yml", ".toml", ".ini", ".conf",
		".go", ".py", ".js", ".ts", ".sh", ".c", ".h", ".rs", ".java", ".css", ".html", ".xml", ".sql":
		return "text/plain; charset=utf-8", true
	case ".pdf":
		return "application/pdf", true
	}
	ct := mime.TypeByExtension(ext)
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") {
		return ct, true
	}
	return "", false
}

func (s *Service) download(w http.ResponseWriter, r *http.Request) {
	p, err := Clean(r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	f, err := s.root.Open(p)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	if fi.IsDir() {
		httpx.Error(w, http.StatusBadRequest, "that is a folder; download it as a zip")
		return
	}
	name := fi.Name()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	ct, canInline := inlineType(name)
	if r.URL.Query().Get("inline") == "1" && canInline {
		h.Set("Content-Type", ct)
		h.Set("Content-Disposition", disposition("inline", name))
		if ct != "application/pdf" { // browsers refuse to render PDFs in a sandbox
			h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'")
		}
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", disposition("attachment", name))
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	}
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func disposition(kind, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return kind + `; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(name)
}

// zip streams one or more files/folders (?path=a&path=b) as a zip archive.
func (s *Service) zip(w http.ResponseWriter, r *http.Request) {
	var paths []string
	for _, raw := range r.URL.Query()["path"] {
		p, err := Clean(raw)
		if err != nil {
			s.fail(w, err)
			return
		}
		if _, err := s.root.Stat(p); err != nil {
			s.fail(w, err)
			return
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		httpx.Error(w, http.StatusBadRequest, "nothing to zip")
		return
	}
	name := "fruit-basket.zip"
	if len(paths) == 1 && paths[0] != "." {
		name = path.Base(paths[0]) + ".zip"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", disposition("attachment", name))
	zw := zip.NewWriter(w)
	defer zw.Close()
	fsys := s.root.FS()
	for _, p := range paths {
		base := path.Dir(p)
		err := fs.WalkDir(fsys, p, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries rather than truncating the archive
			}
			if fp == StateDir || strings.HasPrefix(fp, StateDir+"/") {
				return fs.SkipDir
			}
			rel := fp
			if base != "." {
				rel = strings.TrimPrefix(fp, base+"/")
			}
			if d.IsDir() {
				if fp != "." {
					zw.Create(rel + "/")
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			hdr, _ := zip.FileInfoHeader(fi)
			hdr.Name = rel
			hdr.Method = zip.Deflate
			if isCompressed(fp) {
				hdr.Method = zip.Store
			}
			dst, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			src, err := fsys.Open(fp)
			if err != nil {
				return nil
			}
			defer src.Close()
			_, err = io.Copy(dst, src)
			return err
		})
		if err != nil {
			s.log.Warn("zip aborted", "err", err)
			return
		}
	}
}

func isCompressed(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".mp4", ".mkv", ".mov", ".webm",
		".mp3", ".m4a", ".ogg", ".opus", ".flac", ".zip", ".gz", ".xz", ".zst", ".7z", ".rar":
		return true
	}
	return false
}

func (s *Service) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		httpx.JSON(w, map[string]any{"results": []any{}})
		return
	}
	type hit struct {
		Path string `json:"path"`
		Entry
	}
	results := []hit{}
	fs.WalkDir(s.root.FS(), ".", func(fp string, d fs.DirEntry, err error) error {
		if err != nil || fp == "." {
			return nil
		}
		if fp == StateDir {
			return fs.SkipDir
		}
		if strings.Contains(strings.ToLower(d.Name()), q) {
			if fi, err := d.Info(); err == nil {
				results = append(results, hit{Path: fp, Entry: entryOf(fi)})
			}
		}
		if len(results) >= 200 {
			return fs.SkipAll
		}
		return nil
	})
	httpx.JSON(w, map[string]any{"results": results})
}

func (s *Service) stats(w http.ResponseWriter, r *http.Request) {
	total, free := diskSpace(s.dir)
	httpx.JSON(w, map[string]uint64{"total": total, "free": free})
}

func (s *Service) mkdir(w http.ResponseWriter, r *http.Request) {
	var body struct{ Path string }
	if !httpx.Decode(w, r, &body) {
		return
	}
	p, err := Clean(body.Path)
	if err == nil && p == "." {
		err = errBadPath
	}
	if err == nil {
		err = s.root.MkdirAll(p, 0o755)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// move renames or moves one item; it never overwrites.
func (s *Service) move(w http.ResponseWriter, r *http.Request) {
	var body struct{ From, To string }
	if !httpx.Decode(w, r, &body) {
		return
	}
	from, err1 := Clean(body.From)
	to, err2 := Clean(body.To)
	if err := errors.Join(err1, err2); err != nil || from == "." || to == "." {
		s.fail(w, errBadPath)
		return
	}
	if to == from || strings.HasPrefix(to, from+"/") {
		httpx.Error(w, http.StatusBadRequest, "can't move a folder into itself")
		return
	}
	if _, err := s.root.Lstat(to); err == nil {
		s.fail(w, fs.ErrExist)
		return
	}
	if err := s.root.Rename(from, to); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
