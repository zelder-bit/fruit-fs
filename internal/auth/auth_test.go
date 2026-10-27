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

// 192.0.2.0/24 and 198.51.100.0/24 are documentation ranges, used as stand-ins.
const proxyAddr = "192.0.2.10:41000"
const lanAddr = "198.51.100.7:52000"

func newAuth(t *testing.T, mutate func(*config.Config)) *Auth {
	t.Helper()
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")},
		PasswordHash:   hash,
		AllowedUsers:   map[string]bool{},
	}
	if mutate != nil {
		mutate(cfg)
	}
	a, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func req(method, remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, "http://files.example/api/ls", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestTailscaleHeaderOnlyTrustedFromProxy(t *testing.T) {
	a := newAuth(t, nil)
	h := map[string]string{"Tailscale-User-Login": "alice@example.com", "Tailscale-User-Name": "Alice"}

	if id, ok := a.Identify(req("GET", proxyAddr, h)); !ok || id.User != "alice@example.com" || id.Via != "tailscale" {
		t.Fatalf("from proxy: got %+v %v", id, ok)
	}
	if _, ok := a.Identify(req("GET", lanAddr, h)); ok {
		t.Fatal("spoofed Tailscale header from the LAN was accepted")
	}
	if _, ok := a.Identify(req("GET", proxyAddr, nil)); ok {
		t.Fatal("proxy request without identity header was accepted")
	}
}

func TestAllowedUsers(t *testing.T) {
	a := newAuth(t, func(c *config.Config) { c.AllowedUsers["alice@example.com"] = true })
	if _, ok := a.Identify(req("GET", proxyAddr, map[string]string{"Tailscale-User-Login": "Alice@Example.com"})); !ok {
		t.Fatal("allowed user rejected")
	}
	if _, ok := a.Identify(req("GET", proxyAddr, map[string]string{"Tailscale-User-Login": "mallory@example.com"})); ok {
		t.Fatal("user outside the allow list accepted")
	}
}

func login(a *Auth, pw string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://files.example/api/login", strings.NewReader(`{"password":"`+pw+`"}`))
	r.RemoteAddr = lanAddr
	r.Header.Set("X-Fruit", "1")
	r.Header.Set("Origin", "http://files.example")
	w := httptest.NewRecorder()
	a.Login(w, r)
	return w
}

func TestPasswordLoginAndSession(t *testing.T) {
	a := newAuth(t, nil)
	if w := login(a, "nope"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", w.Code)
	}
	w := login(a, "correct horse")
	if w.Code != http.StatusOK {
		t.Fatalf("right password: %d %s", w.Code, w.Body)
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: %+v", cookie)
	}
	r := req("GET", lanAddr, nil)
	r.AddCookie(cookie)
	if id, ok := a.Identify(r); !ok || id.Via != "guest" || id.ExpiresAt == nil {
		t.Fatalf("session not accepted: %+v", id)
	}

	// Tampered and expired tokens are refused.
	r = req("GET", lanAddr, nil)
	r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie.Value[:len(cookie.Value)-2] + "xx"})
	if _, ok := a.Identify(r); ok {
		t.Fatal("tampered session accepted")
	}
	a.now = func() time.Time { return time.Now().Add(DefaultGuestLength + time.Minute) }
	r = req("GET", lanAddr, nil)
	r.AddCookie(cookie)
	if _, ok := a.Identify(r); ok {
		t.Fatal("expired session accepted")
	}
}

func TestLoginRateLimit(t *testing.T) {
	a := newAuth(t, nil)
	for range 10 {
		login(a, "nope")
	}
	if w := login(a, "correct horse"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected lockout, got %d", w.Code)
	}
}

func TestMiddlewareCSRF(t *testing.T) {
	a := newAuth(t, nil)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	h := a.Middleware(ok)
	ts := map[string]string{"Tailscale-User-Login": "alice@example.com"}

	cases := []struct {
		name   string
		method string
		hdr    map[string]string
		want   int
	}{
		{"get needs no header", "GET", ts, 299},
		{"post without X-Fruit", "POST", ts, http.StatusForbidden},
		{"post from another site", "POST", map[string]string{"Tailscale-User-Login": "a@example.com", "X-Fruit": "1", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"post same origin", "POST", map[string]string{"Tailscale-User-Login": "a@example.com", "X-Fruit": "1", "Origin": "http://files.example"}, 299},
		{"anonymous", "GET", nil, http.StatusUnauthorized},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req(c.method, proxyAddr, c.hdr))
		if w.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, w.Code, c.want)
		}
	}
}

func TestPasswordHashFormat(t *testing.T) {
	h, _ := HashPassword("pw")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected hash %q", h)
	}
	if ok, _ := VerifyPassword("pw", h); !ok {
		t.Fatal("verify failed")
	}
	if _, err := VerifyPassword("pw", "$argon2id$garbage"); err == nil {
		t.Fatal("malformed hash accepted")
	}
}

func TestParallelLoginsCountedUpFront(t *testing.T) {
	a := newAuth(t, nil)
	done := make(chan int, 30)
	for range 30 {
		go func() { done <- login(a, "nope").Code }()
	}
	limited := 0
	for range 30 {
		if <-done == http.StatusTooManyRequests {
			limited++
		}
	}
	// Only 10 attempts may reach the password check; the rest are refused.
	if limited < 20 {
		t.Fatalf("only %d of 30 parallel attempts were limited", limited)
	}
}

func TestSessionsEndWhenPasswordChangesOrIsRemoved(t *testing.T) {
	dir := t.TempDir()
	hash, _ := HashPassword("correct horse")
	cfg := &config.Config{PasswordHash: hash, AllowedUsers: map[string]bool{}}
	a, _ := New(cfg, dir)
	cookie := login(a, "correct horse").Result().Cookies()[0]

	other, _ := HashPassword("battery staple")
	b, _ := New(&config.Config{PasswordHash: other, AllowedUsers: map[string]bool{}}, dir)
	r := req("GET", lanAddr, nil)
	r.AddCookie(cookie)
	if _, ok := b.Identify(r); ok {
		t.Fatal("session survived a password change")
	}
	c, _ := New(&config.Config{TrustedProxies: cfg.TrustedProxies, AllowedUsers: map[string]bool{}}, dir)
	if _, ok := c.Identify(r); ok {
		t.Fatal("session accepted with password login turned off")
	}
}

// guestSession signs in on the LAN at a fixed time and returns the cookie.
func guestSession(t *testing.T, a *Auth, at time.Time) *http.Cookie {
	t.Helper()
	a.now = func() time.Time { return at }
	w := login(a, "correct horse")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	return w.Result().Cookies()[0]
}

func identifyAt(a *Auth, at time.Time, c *http.Cookie) (Identity, bool) {
	a.now = func() time.Time { return at }
	r := req("GET", lanAddr, nil)
	r.AddCookie(c)
	return a.Identify(r)
}

func TestGuestSessionEndsAfterTenMinutes(t *testing.T) {
	a := newAuth(t, nil)
	start := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	cookie := guestSession(t, a, start)

	if cookie.MaxAge != 600 {
		t.Errorf("cookie MaxAge = %d, want 600", cookie.MaxAge)
	}
	id, ok := identifyAt(a, start.Add(9*time.Minute+59*time.Second), cookie)
	if !ok || id.Via != "guest" || id.User != lanUser {
		t.Fatalf("guest refused before expiry: %+v %v", id, ok)
	}
	if want := start.Add(10 * time.Minute); id.ExpiresAt == nil || !id.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", id.ExpiresAt, want)
	}
	if _, ok := identifyAt(a, start.Add(10*time.Minute), cookie); ok {
		t.Fatal("guest still signed in at 10 minutes")
	}

	// The middleware turns the expired guest away too.
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	r := req("GET", lanAddr, nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired guest through middleware: %d", w.Code)
	}
}

func TestGuestCannotExtendSessionByEditingCookie(t *testing.T) {
	a := newAuth(t, nil)
	start := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	cookie := guestSession(t, a, start)
	p, s, _ := strings.Cut(cookie.Value, ".")
	payload, _ := dec(p)
	payload[0]++ // push the expiry years into the future
	forged := &http.Cookie{Name: cookieName, Value: enc(payload) + "." + s}
	if _, ok := identifyAt(a, start, forged); ok {
		t.Fatal("edited expiry accepted")
	}
}

func TestLoginResponseCarriesExpiry(t *testing.T) {
	a := newAuth(t, nil)
	start := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return start }
	w := login(a, "correct horse")
	var id Identity
	if err := json.NewDecoder(w.Body).Decode(&id); err != nil {
		t.Fatal(err)
	}
	if id.Via != "guest" || id.ExpiresAt == nil || !id.ExpiresAt.Equal(start.Add(10*time.Minute)) {
		t.Fatalf("login response: %+v", id)
	}
}

func TestTailscaleUsersHaveNoExpiry(t *testing.T) {
	a := newAuth(t, nil)
	a.now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
	id, ok := a.Identify(req("GET", proxyAddr, map[string]string{"Tailscale-User-Login": "alice@example.com"}))
	if !ok || id.Via != "tailscale" || id.ExpiresAt != nil {
		t.Fatalf("tailscale identity: %+v %v", id, ok)
	}
	b, _ := json.Marshal(id)
	if strings.Contains(string(b), "expiresAt") {
		t.Fatalf("tailscale identity JSON mentions expiry: %s", b)
	}
}
