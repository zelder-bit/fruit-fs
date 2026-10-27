package files

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/zelder-bit/fruit-fs/internal/httpx"
)

// Deleting moves things into .fruit-fs/trash/<id>/<name> with a <id>.json note of
// where they came from, so a slip of the finger can be undone for 30 days.
const (
	trashDir  = StateDir + "/trash"
	trashKeep = 30 * 24 * time.Hour
)

type trashItem struct {
	ID      string    `json:"id"`
	Orig    string    `json:"orig"`
	Deleted time.Time `json:"deleted"`
	Dir     bool      `json:"dir"`
	Size    int64     `json:"size"`
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) delete(w http.ResponseWriter, r *http.Request) {
	var body struct{ Paths []string }
	if !httpx.Decode(w, r, &body) {
		return
	}
	for _, raw := range body.Paths {
		p, err := Clean(raw)
		if err == nil && p == "." {
			err = errBadPath
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		fi, err := s.root.Lstat(p)
		if err != nil {
			s.fail(w, err)
			return
		}
		item := trashItem{ID: newID(), Orig: p, Deleted: time.Now(), Dir: fi.IsDir(), Size: fi.Size()}
		dir := path.Join(trashDir, item.ID)
		if err := s.root.Mkdir(dir, 0o700); err != nil {
			s.fail(w, err)
			return
		}
		if err := s.root.Rename(p, path.Join(dir, path.Base(p))); err != nil {
			s.root.Remove(dir)
			s.fail(w, err)
			return
		}
		meta, _ := json.Marshal(item)
		if err := s.root.WriteFile(dir+".json", meta, 0o600); err != nil {
			s.fail(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) trashItems() []trashItem {
	items := []trashItem{}
	des, err := fs.ReadDir(s.root.FS(), trashDir)
	if err != nil {
		return items
	}
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := s.root.ReadFile(path.Join(trashDir, de.Name()))
		var it trashItem
		if err == nil && json.Unmarshal(b, &it) == nil && it.ID+".json" == de.Name() {
			items = append(items, it)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Deleted.After(items[j].Deleted) })
	return items
}

func (s *Service) trashList(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, map[string]any{"items": s.trashItems()})
}

func (s *Service) trashRestore(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if !httpx.Decode(w, r, &body) {
		return
	}
	for _, it := range s.trashItems() {
		if it.ID != body.ID {
			continue
		}
		if parent := path.Dir(it.Orig); parent != "." {
			s.root.MkdirAll(parent, 0o755)
		}
		dir := path.Join(trashDir, it.ID)
		if err := PublishNoReplace(s.root, path.Join(dir, path.Base(it.Orig)), it.Orig); err != nil {
			s.fail(w, err)
			return
		}
		s.root.Remove(dir)
		s.root.Remove(dir + ".json")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.fail(w, fs.ErrNotExist)
}

func (s *Service) trashEmpty(w http.ResponseWriter, r *http.Request) {
	s.purge(0)
	w.WriteHeader(http.StatusNoContent)
}

// purge removes trash older than keep (0 = everything).
func (s *Service) purge(keep time.Duration) {
	for _, it := range s.trashItems() {
		if keep == 0 || time.Since(it.Deleted) > keep {
			dir := path.Join(trashDir, it.ID)
			if err := s.root.RemoveAll(dir); err == nil {
				s.root.Remove(dir + ".json")
			}
		}
	}
}

// Janitor empties old trash once an hour until stop is closed.
func (s *Service) Janitor(stop <-chan struct{}) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.purge(trashKeep)
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}
