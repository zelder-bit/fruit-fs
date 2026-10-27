// Package auth decides who is making a request.
//
// Two ways in:
//   - Through Tailscale Serve: requests arriving from a trusted proxy address carry a
//     Tailscale-User-Login header set by Tailscale itself. Headers from anywhere else
//     are ignored, so nobody on the LAN can claim to be a tailnet user.
//   - On the LAN: a password login gives a guest a short, signed session cookie.
//     When it expires the guest has to sign in again. Tailnet admins set how long
//     guest sessions last, or turn guest sign-in off, from the admin page.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zelder-bit/fruit-fs/internal/config"
	"github.com/zelder-bit/fruit-fs/internal/httpx"
)

const (
	cookieName = "fruitfs_session"
	lanUser    = "lan"
)

type Identity struct {
	User      string     `json:"user"`                // tailnet login, or "lan" for guests
	Name      string     `json:"name"`                // display name
	Via       string     `json:"via"`                 // "tailscale" or "guest"
	Pic       string     `json:"pic,omitempty"`       // Tailscale profile picture URL
	ExpiresAt *time.Time `json:"expiresAt,omitempty"` // when a guest session ends
	Admin     bool       `json:"admin,omitempty"`     // may change guest settings
}

func guestIdentity(expires time.Time) Identity {
	return Identity{User: lanUser, Name: "Guest", Via: "guest", ExpiresAt: &expires}
}

type ctxKey struct{}

// From returns the identity attached by Middleware.
func From(ctx context.Context) Identity {
	id, _ := ctx.Value(ctxKey{}).(Identity)
	return id
}

// WithIdentity attaches an identity to a context (used by tests in other packages).
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

type Auth struct {
	cfg      *config.Config
	settings *settingsStore
	key      []byte
	limiter  *limiter
	hashing  chan struct{} // at most 2 password checks at once (each uses 64 MiB)
	now      func() time.Time
}

// New loads (or creates) the session signing key and loads the guest settings
// in stateDir. Keep stateDir outside the served data directory so no file API
// can ever reach them.
func New(cfg *config.Config, stateDir string) (*Auth, error) {
	raw, err := loadKey(filepath.Join(stateDir, "session.key"))
	if err != nil {
		return nil, err
	}
	settings, err := loadSettings(stateDir)
	if err != nil {
		return nil, err
	}
	// Mixing the password hash into the key means changing the password signs
	// everyone out.
	m := hmac.New(sha256.New, raw)
	m.Write([]byte("session:" + cfg.PasswordHash))
	return &Auth{cfg: cfg, settings: settings, key: m.Sum(nil), limiter: newLimiter(10, 15*time.Minute),
		hashing: make(chan struct{}, 2), now: time.Now}, nil
}

func loadKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// remoteIP is the address of the direct peer (never a forwarded header).
func remoteIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

func (a *Auth) fromTrustedProxy(r *http.Request) bool {
	ip := remoteIP(r)
	for _, p := range a.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Identify works out who sent r, or reports false.
func (a *Auth) Identify(r *http.Request) (Identity, bool) {
	if a.fromTrustedProxy(r) {
		if login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login")); login != "" {
			if len(a.cfg.AllowedUsers) > 0 && !a.cfg.AllowedUsers[strings.ToLower(login)] {
				return Identity{}, false
			}
			name := r.Header.Get("Tailscale-User-Name")
			if name == "" {
				name = login
			}
			id := Identity{User: login, Name: name, Via: "tailscale", Pic: r.Header.Get("Tailscale-User-Profile-Pic")}
			id.Admin = a.IsAdmin(id)
			return id, true
		}
	}
	// Turning guest sign-in off also ends the guest sessions already running.
	if c, err := r.Cookie(cookieName); err == nil && a.guestSignIn() {
		if exp, ok := a.sessionExpiry(c.Value); ok {
			return guestIdentity(exp), true
		}
	}
	return Identity{}, false
}

// Middleware rejects unauthenticated requests and, for anything that changes state,
// requests that did not come from this app's own pages.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := a.Identify(r)
		if !ok {
			httpx.Error(w, http.StatusUnauthorized, "sign in first")
			return
		}
		if !safeMethod(r.Method) && !a.SameOrigin(r, true) {
			httpx.Error(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// SameOrigin checks that a request was made by our own pages. Browsers will not
// send a custom header cross-site without a CORS preflight (which we never allow),
// so requiring X-Fruit blocks cross-site form posts. Origin, when present, must
// name this host. WebSockets cannot set headers, so they pass needHeader=false.
func (a *Auth) SameOrigin(r *http.Request, needHeader bool) bool {
	if needHeader && r.Header.Get("X-Fruit") != "1" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	hosts := []string{r.Host}
	if a.fromTrustedProxy(r) {
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			hosts = append(hosts, fh)
		}
	}
	for _, h := range hosts {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

// --- sessions -------------------------------------------------------------

// A session token is base64(expiry || nonce) "." base64(hmac). The expiry is
// signed, so a guest can't stretch their session by editing the cookie.
func (a *Auth) newSession(length time.Duration) (string, time.Time) {
	exp := a.now().Add(length).Truncate(time.Second)
	payload := make([]byte, 8+16)
	binary.BigEndian.PutUint64(payload, uint64(exp.Unix()))
	rand.Read(payload[8:])
	return enc(payload) + "." + enc(a.sign(payload)), exp
}

// sessionExpiry checks a token and returns when it ends, or false if it is
// forged, malformed or already over.
func (a *Auth) sessionExpiry(tok string) (time.Time, bool) {
	p, s, ok := strings.Cut(tok, ".")
	if !ok {
		return time.Time{}, false
	}
	payload, err1 := dec(p)
	sig, err2 := dec(s)
	if err1 != nil || err2 != nil || len(payload) != 24 {
		return time.Time{}, false
	}
	if !hmac.Equal(sig, a.sign(payload)) {
		return time.Time{}, false
	}
	exp := time.Unix(int64(binary.BigEndian.Uint64(payload)), 0)
	if !a.now().Before(exp) {
		return time.Time{}, false
	}
	// No guest session can last longer than MaxGuestLength, so a token that
	// ends later is a 30-day session from before guest sessions existed.
	if exp.After(a.now().Add(MaxGuestLength)) {
		return time.Time{}, false
	}
	return exp, true
}

func (a *Auth) sign(b []byte) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write(b)
	return m.Sum(nil)
}

func enc(b []byte) string          { return base64.RawURLEncoding.EncodeToString(b) }
func dec(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func (a *Auth) secure(r *http.Request) bool {
	return r.TLS != nil || (a.fromTrustedProxy(r) && r.Header.Get("X-Forwarded-Proto") == "https")
}

// --- handlers -------------------------------------------------------------

// guestSignIn reports whether LAN password sign-in is possible right now.
func (a *Auth) guestSignIn() bool {
	return a.cfg.PasswordHash != "" && a.settings.get().GuestEnabled
}

// Login handles POST /api/login {"password": "..."}.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	if !a.SameOrigin(r, true) {
		httpx.Error(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if a.cfg.PasswordHash == "" {
		httpx.Error(w, http.StatusForbidden, "password sign-in is turned off")
		return
	}
	if !a.settings.get().GuestEnabled {
		httpx.Error(w, http.StatusForbidden, "guest sign-in is turned off")
		return
	}
	ip := remoteIP(r).String()
	// Each attempt is counted before any work is done, so parallel requests
	// can't slip past the limit.
	if !a.limiter.take(ip) {
		httpx.Error(w, http.StatusTooManyRequests, "too many tries, wait a few minutes")
		return
	}
	var body struct{ Password string }
	if !httpx.Decode(w, r, &body) {
		return
	}
	select {
	case a.hashing <- struct{}{}:
	default:
		httpx.Error(w, http.StatusTooManyRequests, "busy, try again in a moment")
		return
	}
	ok, err := VerifyPassword(body.Password, a.cfg.PasswordHash)
	<-a.hashing
	if err != nil || !ok {
		httpx.Error(w, http.StatusUnauthorized, "wrong password")
		return
	}
	a.limiter.reset(ip)
	tok, exp := a.newSession(a.settings.get().GuestLength())
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/",
		MaxAge: int(exp.Sub(a.now()).Seconds()), HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: a.secure(r),
	})
	httpx.JSON(w, guestIdentity(exp))
}

// Logout handles POST /api/logout.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if !a.SameOrigin(r, true) {
		httpx.Error(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /api/me: the caller's identity, or 401 plus whether a password login is possible.
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	id, ok := a.Identify(r)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": "sign in first", "password": a.guestSignIn()})
		return
	}
	httpx.JSON(w, id)
}

// --- login rate limit ------------------------------------------------------

type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *limiter) prune(ip string, now time.Time) []time.Time {
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, ip)
		return nil
	}
	l.fails[ip] = kept
	return kept
}

// take records an attempt and reports whether it is within the limit.
// A successful login clears the record with reset.
func (l *limiter) take(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	kept := l.prune(ip, now)
	if len(kept) >= l.max {
		return false
	}
	l.fails[ip] = append(kept, now)
	return true
}

func (l *limiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// constantTimeEqual is used by the password check.
func constantTimeEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

var errBadHash = errors.New("malformed argon2id hash")
