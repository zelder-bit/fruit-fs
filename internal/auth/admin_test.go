package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/zelder-bit/fruit-fs/internal/config"
)

// adminReq sends a request to the admin API as the given identity source.
func adminReq(a *Auth, method, body, remote string, hdr map[string]string, c *http.Cookie) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	a.RegisterAdmin(mux)
	r := httptest.NewRequest(method, "http://files.example/api/admin/settings", strings.NewReader(body))
	r.RemoteAddr = remote
	r.Header.Set("X-Fruit", "1")
	r.Header.Set("Origin", "http://files.example")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if c != nil {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

var alice = map[string]string{"Tailscale-User-Login": "alice@example.com"}

func TestGuestsGet403OnAdminEndpoints(t *testing.T) {
	a := newAuth(t, nil)
	guest := login(a, "correct horse").Result().Cookies()[0]
	for _, m := range []string{"GET", "PUT"} {
		if w := adminReq(a, m, `{"guestMinutes":60}`, lanAddr, nil, guest); w.Code != http.StatusForbidden {
			t.Errorf("guest %s: got %d want 403", m, w.Code)
		}
	}
	if w := adminReq(a, "GET", "", lanAddr, nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: got %d want 401", w.Code)
	}
	if a.settings.get().GuestMinutes != 10 {
		t.Fatal("guest changed the settings")
	}
}

func TestTailnetAdminChangesGuestLength(t *testing.T) {
	a := newAuth(t, nil)
	w := adminReq(a, "PUT", `{"guestMinutes":45}`, proxyAddr, alice, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admin PUT: %d %s", w.Code, w.Body)
	}
	var v settingsView
	json.NewDecoder(w.Body).Decode(&v)
	if v.GuestMinutes != 45 || !v.GuestEnabled || v.MinMinutes != 5 || v.MaxMinutes != 1440 {
		t.Fatalf("response: %+v", v)
	}
	start := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	c := guestSession(t, a, start)
	if _, ok := identifyAt(a, start.Add(44*time.Minute), c); !ok {
		t.Fatal("new guest session shorter than the new length")
	}
	if _, ok := identifyAt(a, start.Add(45*time.Minute), c); ok {
		t.Fatal("new guest session outlived the new length")
	}
}

func TestNewLengthOnlyAffectsNewSessions(t *testing.T) {
	a := newAuth(t, nil)
	start := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	old := guestSession(t, a, start)
	a.now = time.Now
	if w := adminReq(a, "PUT", `{"guestMinutes":120}`, proxyAddr, alice, nil); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d", w.Code)
	}
	if _, ok := identifyAt(a, start.Add(11*time.Minute), old); ok {
		t.Fatal("running session was stretched by the new length")
	}
}

func TestSettingsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	hash, _ := HashPassword("correct horse")
	cfg := &config.Config{PasswordHash: hash, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")}, AllowedUsers: map[string]bool{}}
	a, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if w := adminReq(a, "PUT", `{"guestMinutes":90,"guestEnabled":false}`, proxyAddr, alice, nil); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	b, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := b.settings.get(); s.GuestMinutes != 90 || s.GuestEnabled {
		t.Fatalf("after restart: %+v", s)
	}
}

func TestAdminRejectsOutOfRangeAndCrossSite(t *testing.T) {
	a := newAuth(t, nil)
	for _, body := range []string{`{"guestMinutes":4}`, `{"guestMinutes":1441}`, `{"guestMinutes":-1}`, `nope`} {
		if w := adminReq(a, "PUT", body, proxyAddr, alice, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d want 400", body, w.Code)
		}
	}
	evil := map[string]string{"Tailscale-User-Login": "alice@example.com", "Origin": "https://evil.example"}
	if w := adminReq(a, "PUT", `{"guestMinutes":60}`, proxyAddr, evil, nil); w.Code != http.StatusForbidden {
		t.Errorf("cross-site PUT: got %d want 403", w.Code)
	}
	if a.settings.get().GuestMinutes != 10 {
		t.Fatal("settings changed by a refused request")
	}
}

func TestAdminsListNarrowsAdminAccess(t *testing.T) {
	a := newAuth(t, func(c *config.Config) { c.Admins = map[string]bool{"alice@example.com": true} })
	if w := adminReq(a, "GET", "", proxyAddr, map[string]string{"Tailscale-User-Login": "Alice@Example.com"}, nil); w.Code != http.StatusOK {
		t.Errorf("listed admin: got %d", w.Code)
	}
	bob := map[string]string{"Tailscale-User-Login": "bob@example.com"}
	if w := adminReq(a, "GET", "", proxyAddr, bob, nil); w.Code != http.StatusForbidden {
		t.Errorf("unlisted tailnet user: got %d want 403", w.Code)
	}
	if id, _ := a.Identify(req("GET", proxyAddr, bob)); id.Admin {
		t.Error("unlisted user marked admin")
	}
}

func TestTurningGuestsOffEndsGuestSessions(t *testing.T) {
	a := newAuth(t, nil)
	c := login(a, "correct horse").Result().Cookies()[0]
	if w := adminReq(a, "PUT", `{"guestEnabled":false}`, proxyAddr, alice, nil); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d", w.Code)
	}
	r := req("GET", lanAddr, nil)
	r.AddCookie(c)
	if _, ok := a.Identify(r); ok {
		t.Fatal("guest session survived guest sign-in being turned off")
	}
	if w := login(a, "correct horse"); w.Code != http.StatusForbidden {
		t.Fatalf("login with guests off: %d", w.Code)
	}
	// Tailscale users are unaffected.
	if _, ok := a.Identify(req("GET", proxyAddr, alice)); !ok {
		t.Fatal("tailnet user refused with guests off")
	}
}
