package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Guest session length limits and default. Guests are people on the LAN who
// signed in with the password; tailnet users change these from the admin page.
const (
	DefaultGuestLength = 10 * time.Minute
	MinGuestLength     = 5 * time.Minute
	MaxGuestLength     = 24 * time.Hour
)

// Settings are the guest sign-in options an admin can change at runtime.
type Settings struct {
	GuestEnabled bool `json:"guestEnabled"` // LAN password sign-in allowed
	GuestMinutes int  `json:"guestMinutes"` // length of new guest sessions
}

func defaultSettings() Settings {
	return Settings{GuestEnabled: true, GuestMinutes: int(DefaultGuestLength / time.Minute)}
}

// GuestLength is the session length as a duration.
func (s Settings) GuestLength() time.Duration { return time.Duration(s.GuestMinutes) * time.Minute }

// Validate reports whether the settings are within the allowed range.
func (s Settings) Validate() error {
	if l := s.GuestLength(); l < MinGuestLength || l > MaxGuestLength {
		return fmt.Errorf("guest sessions must last between %d minutes and %d hours",
			int(MinGuestLength/time.Minute), int(MaxGuestLength/time.Hour))
	}
	return nil
}

// settingsStore keeps Settings in memory and in settings.json in the state
// directory (never the data folder, so no file API can read or change it).
type settingsStore struct {
	mu   sync.RWMutex
	path string
	cur  Settings
}

func loadSettings(stateDir string) (*settingsStore, error) {
	st := &settingsStore{path: filepath.Join(stateDir, "settings.json"), cur: defaultSettings()}
	b, err := os.ReadFile(st.path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	// Start from the defaults so a field missing from an older file keeps its default.
	s := defaultSettings()
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", st.path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", st.path, err)
	}
	st.cur = s
	return st, nil
}

func (st *settingsStore) get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.cur
}

// set validates s, writes it to disk atomically and then makes it current.
func (st *settingsStore) set(s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), st.path); err != nil {
		return err
	}
	st.cur = s
	return nil
}
