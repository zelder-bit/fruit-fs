// Package drop lets open browser tabs send files and text to each other.
//
// Every tab opens a WebSocket and gets a fruity name ("Zesty Orange"). Tabs see
// each other's names and pass small JSON messages (offers, answers, text) over it.
// File bytes do not go over the WebSocket: once the receiver accepts, the sender
// POSTs each file and the receiver GETs it, and the server joins the two requests
// with a pipe. Nothing is written to disk unless the receiver picks "save to
// fruit-fs", in which case the server itself plays receiver.
package drop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zelder-bit/fruit-fs/internal/auth"
	"github.com/zelder-bit/fruit-fs/internal/files"
	"github.com/zelder-bit/fruit-fs/internal/httpx"
)

const (
	maxText      = 64 << 10
	maxFiles     = 500
	maxClients   = 64
	maxPending   = 8               // open transfers per sending tab
	offerTimeout = 2 * time.Minute // how long an offer waits for an answer
	joinTimeout  = time.Minute     // how long one side of a file waits for the other
	idle         = 2 * time.Minute // a stream fails if it makes no progress for this long
	InboxDir     = "Drop inbox"
)

var errGone = errors.New("the other side went away")

// msg is every WebSocket message in either direction; unused fields stay empty.
type msg struct {
	Type     string     `json:"type"`
	You      *peer      `json:"you,omitempty"`
	Peers    []peer     `json:"peers,omitempty"`
	To       string     `json:"to,omitempty"`
	From     *peer      `json:"from,omitempty"`
	Transfer string     `json:"transfer,omitempty"`
	Files    []fileInfo `json:"files,omitempty"`
	Text     string     `json:"text,omitempty"`
	Accept   bool       `json:"accept,omitempty"`
	Save     bool       `json:"save,omitempty"`
	Error    string     `json:"error,omitempty"`
}

type peer struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	User   string `json:"user"`
	Device string `json:"device"` // phone, tablet, laptop
}

type fileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Type string `json:"type,omitempty"`
}

type client struct {
	peer
	login string // who owns this tab; checked when file bytes arrive
	out   chan msg
}

type transfer struct {
	id       string
	from, to *client
	files    []fileInfo
	accepted bool
	save     bool // the server saves to the inbox instead of a browser receiving
	slots    []*slot
	created  time.Time
}

// slot joins one sender request to one receiver for a single file. Each side
// can be claimed once, so a retried request can't mix bytes into the stream.
type slot struct {
	recvOnce sync.Once
	sendOnce sync.Once
	ready    chan struct{} // closed when the receiver is attached
	pr       *io.PipeReader
	pw       *io.PipeWriter
	consumed bool
}

// claim runs once.Do and reports whether this caller won.
func claim(once *sync.Once) bool {
	won := false
	once.Do(func() { won = true })
	return won
}

func newSlot() *slot {
	pr, pw := io.Pipe()
	return &slot{ready: make(chan struct{}), pr: pr, pw: pw}
}

type Hub struct {
	mu        sync.Mutex
	clients   map[string]*client
	transfers map[string]*transfer
	root      *os.Root
	auth      *auth.Auth
	maxSave   int64 // largest file "save to fruit-fs" accepts (0 = no limit)
	log       *slog.Logger
}

func New(root *os.Root, a *auth.Auth, maxSave int64, log *slog.Logger) *Hub {
	return &Hub{clients: map[string]*client{}, transfers: map[string]*transfer{}, root: root, auth: a, maxSave: maxSave, log: log}
}

func (h *Hub) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /api/drop/ws", wrap(http.HandlerFunc(h.ws)))
	mux.Handle("POST /api/drop/{transfer}/{index}", wrap(http.HandlerFunc(h.send)))
	mux.Handle("GET /api/drop/{transfer}/{index}", wrap(http.HandlerFunc(h.receive)))
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	adjectives = []string{"Zesty", "Juicy", "Sunny", "Brave", "Mellow", "Tangy", "Breezy", "Peppy", "Plucky", "Golden", "Cheeky", "Dapper", "Fuzzy", "Jolly", "Snappy", "Ripe"}
	fruits     = []string{"Orange", "Banana", "Mango", "Kiwi", "Lemon", "Peach", "Plum", "Cherry", "Papaya", "Lime", "Grape", "Melon", "Apricot", "Guava", "Fig", "Pear"}
)

func pick(list []string) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	return list[n.Int64()]
}

func (h *Hub) fruityName() string {
	for range 20 {
		name := pick(adjectives) + " " + pick(fruits)
		taken := false
		for _, c := range h.clients {
			taken = taken || c.Name == name
		}
		if !taken {
			return name
		}
	}
	return pick(adjectives) + " " + pick(fruits) + " " + strconv.Itoa(len(h.clients)+1)
}

func deviceKind(ua string) string {
	ua = strings.ToLower(ua)
	switch {
	case strings.Contains(ua, "ipad") || strings.Contains(ua, "tablet"):
		return "tablet"
	case strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone"):
		return "phone"
	}
	return "laptop"
}

// --- WebSocket ------------------------------------------------------------

func (h *Hub) ws(w http.ResponseWriter, r *http.Request) {
	// Browsers can't add our X-Fruit header to a WebSocket, so check Origin only.
	if !h.auth.SameOrigin(r, false) || r.Header.Get("Origin") == "" {
		httpx.Error(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	h.mu.Lock()
	full := len(h.clients) >= maxClients
	h.mu.Unlock()
	if full {
		httpx.Error(w, http.StatusServiceUnavailable, "too many devices connected")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(maxText + 64<<10)
	id := auth.From(r.Context())

	h.mu.Lock()
	c := &client{peer: peer{ID: randomID(), Name: h.fruityName(), User: id.Name, Device: deviceKind(r.UserAgent())}, login: id.User, out: make(chan msg, 32)}
	h.clients[c.ID] = c
	h.mu.Unlock()
	h.log.Info("drop: device joined", "name", c.Name, "user", id.User)

	ctx, cancel := context.WithCancel(r.Context())
	defer func() {
		cancel()
		h.leave(c)
		conn.CloseNow()
	}()

	go h.writeLoop(ctx, conn, c, cancel)
	c.out <- msg{Type: "hello", You: &c.peer}
	h.broadcastPeers()

	for {
		var m msg
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		h.handle(c, m)
	}
}

func (h *Hub) writeLoop(ctx context.Context, conn *websocket.Conn, c *client, cancel func()) {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.out:
			b, _ := json.Marshal(m)
			wctx, done := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, b)
			done()
			if err != nil {
				cancel()
				return
			}
		case <-ping.C:
			pctx, done := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

// deliver queues a message without ever blocking the hub on a slow tab.
func deliver(c *client, m msg) {
	select {
	case c.out <- m:
	default:
	}
}

func (h *Hub) leave(c *client) {
	h.mu.Lock()
	delete(h.clients, c.ID)
	for id, t := range h.transfers {
		if t.from == c || t.to == c {
			h.dropTransferLocked(id, "the other device left")
		}
	}
	h.mu.Unlock()
	h.broadcastPeers()
}

func (h *Hub) broadcastPeers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		peers := []peer{}
		for _, o := range h.clients {
			if o != c {
				peers = append(peers, o.peer)
			}
		}
		deliver(c, msg{Type: "peers", Peers: peers})
	}
}

func (h *Hub) handle(c *client, m msg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch m.Type {
	case "text":
		to := h.clients[m.To]
		if to == nil || to == c || len(m.Text) > maxText {
			deliver(c, msg{Type: "error", Error: "couldn't send that note"})
			return
		}
		deliver(to, msg{Type: "text", From: &c.peer, Text: m.Text})

	case "offer":
		to := h.clients[m.To]
		if to == nil || to == c || len(m.Files) == 0 || len(m.Files) > maxFiles {
			deliver(c, msg{Type: "error", Error: "that device isn't around any more"})
			return
		}
		pending := 0
		for _, t := range h.transfers {
			if t.from == c {
				pending++
			}
		}
		if pending >= maxPending {
			deliver(c, msg{Type: "error", Error: "wait for your other sends to finish"})
			return
		}
		for i := range m.Files {
			m.Files[i].Name = safeName(m.Files[i].Name)
			if m.Files[i].Size < 0 {
				deliver(c, msg{Type: "error", Error: "bad file size"})
				return
			}
		}
		t := &transfer{id: randomID(), from: c, to: to, files: m.Files, created: time.Now()}
		for range m.Files {
			t.slots = append(t.slots, newSlot())
		}
		h.transfers[t.id] = t
		deliver(c, msg{Type: "offered", Transfer: t.id, To: to.ID})
		deliver(to, msg{Type: "offer", Transfer: t.id, From: &c.peer, Files: m.Files})
		time.AfterFunc(offerTimeout, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if t := h.transfers[t.id]; t != nil && !t.accepted {
				h.dropTransferLocked(t.id, "no answer")
			}
		})

	case "answer":
		t := h.transfers[m.Transfer]
		if t == nil || t.to != c || t.accepted {
			return
		}
		if !m.Accept {
			h.dropTransferLocked(t.id, "declined")
			return
		}
		if m.Save && h.maxSave > 0 {
			for _, f := range t.files {
				if f.Size > h.maxSave {
					deliver(c, msg{Type: "error", Error: f.Name + " is bigger than the upload limit"})
					return
				}
			}
		}
		t.accepted = true
		t.save = m.Save
		deliver(t.from, msg{Type: "answer", Transfer: t.id, Accept: true, Save: m.Save})
		if m.Save {
			go h.saveToInbox(t)
		}
		// An accepted transfer that never starts moving is dropped too.
		time.AfterFunc(joinTimeout+offerTimeout, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if cur := h.transfers[t.id]; cur != nil && !cur.started() {
				h.dropTransferLocked(t.id, "nothing was sent")
			}
		})

	case "cancel":
		if t := h.transfers[m.Transfer]; t != nil && (t.from == c || t.to == c) {
			h.dropTransferLocked(t.id, "cancelled")
		}
	}
}

// dropTransferLocked ends a transfer and tells both sides. Caller holds h.mu.
func (h *Hub) dropTransferLocked(id, reason string) {
	t := h.transfers[id]
	if t == nil {
		return
	}
	delete(h.transfers, id)
	err := errors.New(reason)
	for _, s := range t.slots {
		s.pr.CloseWithError(err)
		s.pw.CloseWithError(err)
	}
	for _, c := range []*client{t.from, t.to} {
		deliver(c, msg{Type: "cancel", Transfer: id, Error: reason})
	}
}

func safeName(n string) string {
	n = path.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, n)
	if n == "" || n == "." || n == ".." || n == "/" {
		n = "file"
	}
	return n
}

// --- file bytes ------------------------------------------------------------

// started reports whether any file has begun streaming.
func (t *transfer) started() bool {
	for _, s := range t.slots {
		select {
		case <-s.ready:
			return true
		default:
		}
	}
	return false
}

// slotFor finds the transfer slot and checks the caller is the right party.
func (h *Hub) slotFor(r *http.Request, sender bool) (*transfer, *slot, int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.transfers[r.PathValue("transfer")]
	i, err := strconv.Atoi(r.PathValue("index"))
	if t == nil || err != nil || i < 0 || i >= len(t.slots) {
		return nil, nil, 0, errors.New("no such transfer")
	}
	if !t.accepted {
		return nil, nil, 0, errors.New("not accepted yet")
	}
	party := t.to
	if sender {
		party = t.from
	}
	if party.login != auth.From(r.Context()).User {
		return nil, nil, 0, errors.New("no such transfer")
	}
	return t, t.slots[i], i, nil
}

// send streams one file from the sender into the pipe.
func (h *Hub) send(w http.ResponseWriter, r *http.Request) {
	t, s, i, err := h.slotFor(r, true)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if !claim(&s.sendOnce) {
		httpx.Error(w, http.StatusConflict, "already being sent")
		return
	}
	// If the sender disconnects, end the stream so the receiver isn't left waiting.
	defer context.AfterFunc(r.Context(), func() { s.pw.CloseWithError(errGone) })()
	select {
	case <-s.ready:
	case <-time.After(joinTimeout):
		httpx.Error(w, http.StatusGatewayTimeout, "the receiver never started downloading")
		return
	case <-r.Context().Done():
		return
	}
	want := t.files[i].Size
	n, err := io.Copy(s.pw, io.LimitReader(httpx.IdleBody(w, r, idle), want))
	if err == nil && n != want {
		err = fmt.Errorf("sent %d of %d bytes", n, want)
	}
	if err != nil {
		s.pw.CloseWithError(err)
		httpx.Error(w, http.StatusBadRequest, "transfer interrupted")
		return
	}
	s.pw.Close()
	if !t.save { // for inbox saves, saveToInbox reports completion once the file is on disk
		h.finishSlot(t, i)
	}
	w.WriteHeader(http.StatusNoContent)
}

// receive streams one file out of the pipe to the receiving browser.
func (h *Hub) receive(w http.ResponseWriter, r *http.Request) {
	t, s, i, err := h.slotFor(r, false)
	if err != nil {
		httpx.Error(w, http.StatusNotFound, err.Error())
		return
	}
	if t.save || !claim(&s.recvOnce) {
		httpx.Error(w, http.StatusConflict, "already being received")
		return
	}
	close(s.ready)
	defer context.AfterFunc(r.Context(), func() { s.pr.CloseWithError(errGone) })()
	f := t.files[i]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+asciiName(f.Name)+`"; filename*=UTF-8''`+pathEscape(f.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(idleWriter(w, idle), s.pr); err != nil {
		s.pr.CloseWithError(err)
		panic(http.ErrAbortHandler) // cut the connection so the browser marks it failed
	}
}

// saveToInbox plays the receiver itself, writing each file into "Drop inbox/".
func (h *Hub) saveToInbox(t *transfer) {
	fail := func(i int, err error) {
		h.log.Warn("drop: save failed", "file", t.files[i].Name, "err", err)
		h.mu.Lock()
		h.dropTransferLocked(t.id, "couldn't save "+t.files[i].Name)
		h.mu.Unlock()
	}
	if err := h.root.MkdirAll(InboxDir, 0o755); err != nil {
		fail(0, err)
		return
	}
	for i, s := range t.slots {
		if !claim(&s.recvOnce) {
			continue
		}
		close(s.ready)
		tmp := path.Join(files.StateDir, "uploads", "drop-"+t.id+"-"+strconv.Itoa(i)+".part")
		f, err := h.root.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			s.pr.CloseWithError(err)
			fail(i, err)
			return
		}
		_, err = io.Copy(f, s.pr)
		err = errors.Join(err, f.Close())
		var name string
		if err == nil {
			name, err = publishFree(h.root, tmp, InboxDir, t.files[i].Name)
		}
		if err != nil {
			s.pr.CloseWithError(err)
			h.root.Remove(tmp)
			fail(i, err)
			return
		}
		h.log.Info("drop: saved", "path", name)
		h.finishSlot(t, i)
	}
}

// publishFree moves tmp into dir as name, or "name (2)", "name (3)"... if taken.
func publishFree(root *os.Root, tmp, dir, name string) (string, error) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 1; n < 1000; n++ {
		p := path.Join(dir, name)
		if n > 1 {
			p = path.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		}
		err := files.PublishNoReplace(root, tmp, p)
		if !errors.Is(err, fs.ErrExist) {
			return p, err
		}
	}
	return "", errors.New("too many files with that name")
}

// finishSlot forgets the transfer once every file has gone through.
func (h *Hub) finishSlot(t *transfer, i int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.transfers[t.id] != t {
		return // already cancelled
	}
	t.slots[i].consumed = true
	for _, s := range t.slots {
		if !s.consumed {
			return
		}
	}
	delete(h.transfers, t.id)
	deliver(t.from, msg{Type: "done", Transfer: t.id})
	deliver(t.to, msg{Type: "done", Transfer: t.id})
}

func asciiName(n string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, n)
}

func pathEscape(n string) string {
	var b strings.Builder
	for _, c := range []byte(n) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// idleWriter fails a download if the receiver stops reading for d.
func idleWriter(w http.ResponseWriter, d time.Duration) io.Writer {
	rc := http.NewResponseController(w)
	return writeFunc(func(p []byte) (int, error) {
		rc.SetWriteDeadline(time.Now().Add(d))
		return w.Write(p)
	})
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }
