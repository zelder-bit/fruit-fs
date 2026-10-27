package auth

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id settings: 64 MiB memory, 3 passes, 2 lanes (OWASP-recommended range).
const (
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 2
	argonKeyLen  = 32
)

// HashPassword returns a PHC-style string: $argon2id$v=19$m=65536,t=3,p=2$salt$hash
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64(salt), b64(sum)), nil
}

// VerifyPassword checks pw against a hash made by HashPassword.
func VerifyPassword(pw, hash string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var version int
	var mem uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil || mem == 0 || t == 0 || p == 0 || mem > 1<<20 {
		return false, errBadHash
	}
	salt, err1 := unb64(parts[4])
	want, err2 := unb64(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false, errBadHash
	}
	got := argon2.IDKey([]byte(pw), salt, t, mem, p, uint32(len(want)))
	return constantTimeEqual(got, want), nil
}

func b64(b []byte) string            { return base64.RawStdEncoding.EncodeToString(b) }
func unb64(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) }
