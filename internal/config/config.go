// Package config reads fruit-fs settings from FRUITFS_* environment variables.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DataDir        string         // root of the files that are served
	StateDir       string         // private state (session key), outside DataDir
	Listen         string         // address for the HTTP server
	TrustedProxies []netip.Prefix // where Tailscale Serve connects from
	AllowedUsers   map[string]bool
	Admins         map[string]bool // tailnet logins allowed on the admin page; empty = every tailnet user
	PasswordHash   string          // argon2id hash for LAN logins; empty disables them
	MaxUpload      int64           // bytes per file, 0 = unlimited
}

func FromEnv() (*Config, error) {
	c := &Config{
		DataDir:      env("FRUITFS_DATA", "/data"),
		StateDir:     env("FRUITFS_STATE", "/state"),
		Listen:       env("FRUITFS_LISTEN", ":8080"),
		PasswordHash: unquote(os.Getenv("FRUITFS_PASSWORD_HASH")),
		AllowedUsers: map[string]bool{},
		Admins:       map[string]bool{},
	}
	for _, s := range split(os.Getenv("FRUITFS_TRUSTED_PROXIES")) {
		p, err := parsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("FRUITFS_TRUSTED_PROXIES: %w", err)
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	for _, u := range split(os.Getenv("FRUITFS_ALLOWED_USERS")) {
		c.AllowedUsers[strings.ToLower(u)] = true
	}
	for _, u := range split(os.Getenv("FRUITFS_ADMINS")) {
		c.Admins[strings.ToLower(u)] = true
	}
	if s := os.Getenv("FRUITFS_MAX_UPLOAD"); s != "" {
		n, err := parseSize(s)
		if err != nil {
			return nil, fmt.Errorf("FRUITFS_MAX_UPLOAD: %w", err)
		}
		c.MaxUpload = n
	}
	if c.PasswordHash == "" && len(c.TrustedProxies) == 0 {
		return nil, fmt.Errorf("nobody could sign in: set FRUITFS_PASSWORD_HASH and/or FRUITFS_TRUSTED_PROXIES")
	}
	if c.PasswordHash != "" && !strings.HasPrefix(c.PasswordHash, "$argon2id$") {
		return nil, fmt.Errorf("FRUITFS_PASSWORD_HASH must be an argon2id hash (run: fruit-fs hash-password)")
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// unquote drops one pair of surrounding quotes. The hash must be single-quoted
// in a compose .env file (its $ signs would be expanded otherwise), but
// `docker run --env-file` passes the quotes through literally.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return s
}

func split(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, strings.TrimSpace(f))
	}
	return out
}

// parsePrefix accepts a CIDR prefix or a bare address.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

// parseSize accepts plain bytes or a K/M/G/T suffix (powers of 1024).
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(s), "B"))
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		if mult > 1 {
			s = s[:n-1]
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}
