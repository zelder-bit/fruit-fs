package drop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zelder-bit/fruit-fs/internal/auth"
	"github.com/zelder-bit/fruit-fs/internal/config"
)

type env struct {
	srv  *httptest.Server
	data string
}

// Requests from the test client arrive from 127.0.0.1, which is set up as the
// trusted proxy, so the Tailscale-User-Login header picks the user.
func newEnv(t *testing.T) *env {
	t.Helper()
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, ".fruit-fs", "uploads"), 0o700)
	cfg := &config.Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, AllowedUsers: map[string]bool{}}
	a, err := auth.New(cfg, filepath.Join(data, ".fruit-fs"))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := os.OpenRoot(data)
	hub := New(root, a, slog.New(slog.DiscardHandler))
	mux := http.NewServeMux()
	hub.Register(mux, a.Middleware)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{srv: srv, data: data}
}

type tab struct {
	t    *testing.T
	conn *websocket.Conn
	me   peer
}

func (e *env) open(t *testing.T, user string) *tab {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{"Tailscale-User-Login": {user}, "Origin": {e.srv.URL}}
	conn, _, err := websocket.Dial(ctx, strings.Replace(e.srv.URL, "http", "ws", 1)+"/api/drop/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	tb := &tab{t: t, conn: conn}
	tb.me = *tb.wait("hello").You
	return tb
}

func (tb *tab) send(m msg) {
	b, _ := json.Marshal(m)
	tb.conn.Write(context.Background(), websocket.MessageText, b)
}

// wait reads messages until one of the given type arrives.
func (tb *tab) wait(typ string) msg {
	tb.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := tb.conn.Read(ctx)
		if err != nil {
			tb.t.Fatalf("waiting for %s: %v", typ, err)
		}
		var m msg
		json.Unmarshal(b, &m)
		if m.Type == typ {
			return m
		}
	}
}

func (e *env) req(method, path, user string, body io.Reader) (*http.Response, error) {
	r, _ := http.NewRequest(method, e.srv.URL+path, body)
	r.Header.Set("Tailscale-User-Login", user)
	r.Header.Set("X-Fruit", "1")
	return http.DefaultClient.Do(r)
}

func TestTextAndFileRelay(t *testing.T) {
	e := newEnv(t)
	phone := e.open(t, "alice@example.com")
	laptop := e.open(t, "alice@example.com")

	phone.send(msg{Type: "text", To: laptop.me.ID, Text: "hello from the orchard"})
	if m := laptop.wait("text"); m.Text != "hello from the orchard" || m.From.ID != phone.me.ID {
		t.Fatalf("text: %+v", m)
	}

	payload := make([]byte, 8<<20)
	rand.Read(payload)
	phone.send(msg{Type: "offer", To: laptop.me.ID, Files: []fileInfo{{Name: "../../evil/pic.jpg", Size: int64(len(payload))}}})
	offer := laptop.wait("offer")
	if offer.Files[0].Name != "pic.jpg" {
		t.Fatalf("file name not sanitised: %q", offer.Files[0].Name)
	}

	// Someone else can't grab the bytes, even with the transfer id.
	if resp, _ := e.req("GET", "/api/drop/"+offer.Transfer+"/0", "mallory@example.com", nil); resp.StatusCode != 404 {
		t.Fatal("transfer reachable by another user before accept:", resp.StatusCode)
	}
	laptop.send(msg{Type: "answer", Transfer: offer.Transfer, Accept: true})
	phone.wait("answer")
	if resp, _ := e.req("GET", "/api/drop/"+offer.Transfer+"/0", "mallory@example.com", nil); resp.StatusCode != 404 {
		t.Fatal("transfer reachable by another user:", resp.StatusCode)
	}

	got := make(chan []byte)
	go func() {
		resp, err := e.req("GET", "/api/drop/"+offer.Transfer+"/0", "alice@example.com", nil)
		if err != nil {
			got <- nil
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got <- b
	}()
	resp, err := e.req("POST", "/api/drop/"+offer.Transfer+"/0", "alice@example.com", bytes.NewReader(payload))
	if err != nil || resp.StatusCode != 204 {
		t.Fatal("send failed", err, resp.StatusCode)
	}
	if b := <-got; !bytes.Equal(b, payload) {
		t.Fatalf("received %d bytes, want %d identical", len(b), len(payload))
	}
	phone.wait("done")
}

func TestSaveToInbox(t *testing.T) {
	e := newEnv(t)
	a := e.open(t, "alice@example.com")
	b := e.open(t, "bob@example.com")
	a.send(msg{Type: "offer", To: b.me.ID, Files: []fileInfo{{Name: "note.txt", Size: 5}}})
	offer := b.wait("offer")
	b.send(msg{Type: "answer", Transfer: offer.Transfer, Accept: true, Save: true})
	a.wait("answer")
	resp, err := e.req("POST", "/api/drop/"+offer.Transfer+"/0", "alice@example.com", strings.NewReader("hello"))
	if err != nil || resp.StatusCode != 204 {
		t.Fatal("send", err, resp.StatusCode)
	}
	a.wait("done")
	if got, _ := os.ReadFile(filepath.Join(e.data, InboxDir, "note.txt")); string(got) != "hello" {
		t.Fatalf("inbox file = %q", got)
	}
}

func TestDeclineAndCrossSiteSocket(t *testing.T) {
	e := newEnv(t)
	a := e.open(t, "alice@example.com")
	b := e.open(t, "alice@example.com")
	a.send(msg{Type: "offer", To: b.me.ID, Files: []fileInfo{{Name: "x", Size: 1}}})
	offer := b.wait("offer")
	b.send(msg{Type: "answer", Transfer: offer.Transfer, Accept: false})
	if m := a.wait("cancel"); m.Error != "declined" {
		t.Fatalf("cancel: %+v", m)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{"Tailscale-User-Login": {"alice@example.com"}, "Origin": {"https://evil.example"}}
	if _, _, err := websocket.Dial(ctx, strings.Replace(e.srv.URL, "http", "ws", 1)+"/api/drop/ws", &websocket.DialOptions{HTTPHeader: h}); err == nil {
		t.Fatal("cross-site WebSocket accepted")
	}
}
