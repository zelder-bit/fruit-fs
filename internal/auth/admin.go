package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/zelder-bit/fruit-fs/internal/httpx"
)

// IsAdmin reports whether id may change guest settings: tailnet users only
// (never guests), narrowed to FRUITFS_ADMINS when that is set.
func (a *Auth) IsAdmin(id Identity) bool {
	if id.Via != "tailscale" {
		return false
	}
	return len(a.cfg.Admins) == 0 || a.cfg.Admins[strings.ToLower(id.User)]
}

// RegisterAdmin adds the admin API. Requests pass through Middleware first
// (signed in, X-Fruit and same-origin for changes), then the admin check.
func (a *Auth) RegisterAdmin(mux *http.ServeMux) {
	mux.Handle("GET /api/admin/settings", a.Middleware(a.adminOnly(http.HandlerFunc(a.getSettings))))
	mux.Handle("PUT /api/admin/settings", a.Middleware(a.adminOnly(http.HandlerFunc(a.putSettings))))
}

func (a *Auth) adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.IsAdmin(From(r.Context())) {
			httpx.Error(w, http.StatusForbidden, "only tailnet admins can change settings")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type settingsView struct {
	Settings
	MinMinutes         int  `json:"minMinutes"`
	MaxMinutes         int  `json:"maxMinutes"`
	PasswordConfigured bool `json:"passwordConfigured"` // FRUITFS_PASSWORD_HASH is set
}

func (a *Auth) view() settingsView {
	return settingsView{
		Settings:           a.settings.get(),
		MinMinutes:         int(MinGuestLength / time.Minute),
		MaxMinutes:         int(MaxGuestLength / time.Minute),
		PasswordConfigured: a.cfg.PasswordHash != "",
	}
}

// getSettings handles GET /api/admin/settings.
func (a *Auth) getSettings(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, a.view())
}

// putSettings handles PUT /api/admin/settings {"guestEnabled": bool, "guestMinutes": n}.
// Either field may be left out. A new length applies to guest sessions started
// from now on; sessions already running keep the expiry they were given.
func (a *Auth) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GuestEnabled *bool `json:"guestEnabled"`
		GuestMinutes *int  `json:"guestMinutes"`
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	s := a.settings.get()
	if body.GuestEnabled != nil {
		s.GuestEnabled = *body.GuestEnabled
	}
	if body.GuestMinutes != nil {
		s.GuestMinutes = *body.GuestMinutes
	}
	if err := s.Validate(); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.settings.set(s); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not save settings")
		return
	}
	httpx.JSON(w, a.view())
}
