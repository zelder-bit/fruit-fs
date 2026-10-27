package config

import "testing"

func TestFromEnv(t *testing.T) {
	const hash = "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA"
	for _, raw := range []string{hash, "'" + hash + "'", `"` + hash + `"`} {
		t.Setenv("FRUITFS_PASSWORD_HASH", raw)
		t.Setenv("FRUITFS_TRUSTED_PROXIES", "192.0.2.10, 198.51.100.0/24")
		t.Setenv("FRUITFS_MAX_UPLOAD", "20G")
		c, err := FromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if c.PasswordHash != hash || len(c.TrustedProxies) != 2 || c.MaxUpload != 20<<30 {
			t.Fatalf("got %+v", c)
		}
	}
}

func TestRefusesToRunWideOpen(t *testing.T) {
	t.Setenv("FRUITFS_PASSWORD_HASH", "")
	t.Setenv("FRUITFS_TRUSTED_PROXIES", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("started with no way to sign in")
	}
	t.Setenv("FRUITFS_PASSWORD_HASH", "hunter2")
	if _, err := FromEnv(); err == nil {
		t.Fatal("accepted a plain-text password")
	}
}
