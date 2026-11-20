// fruit-fs: a fruity file browser and device-to-device drop.
//
//	fruit-fs                 run the server (settings from FRUITFS_* env vars)
//	fruit-fs hash-password   read a password on stdin, print its argon2id hash
//	fruit-fs healthcheck     exit 0 if the local server answers /healthz
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zelder-bit/fruit-fs/internal/auth"
	"github.com/zelder-bit/fruit-fs/internal/config"
	"github.com/zelder-bit/fruit-fs/internal/drop"
	"github.com/zelder-bit/fruit-fs/internal/files"
	"github.com/zelder-bit/fruit-fs/internal/upload"
	"github.com/zelder-bit/fruit-fs/web"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "hash-password":
		err = hashPassword()
	case "healthcheck":
		err = healthcheck()
	default:
		err = fmt.Errorf("unknown command %q (serve, hash-password, healthcheck)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fruit-fs:", err)
		os.Exit(1)
	}
}

func serve() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	fsvc, err := files.New(cfg.DataDir, log)
	if err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	if inside(cfg.StateDir, cfg.DataDir) {
		return fmt.Errorf("FRUITFS_STATE must not be inside FRUITFS_DATA")
	}
	a, err := auth.New(cfg, cfg.StateDir)
	if err != nil {
		return fmt.Errorf("session key: %w", err)
	}
	usvc, err := upload.New(fsvc.Root(), cfg.MaxUpload, log)
	if err != nil {
		return fmt.Errorf("uploads: %w", err)
	}
	hub := drop.New(fsvc.Root(), a, cfg.MaxUpload, log)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/me", a.Me)
	mux.HandleFunc("POST /api/login", a.Login)
	mux.HandleFunc("POST /api/logout", a.Logout)
	a.RegisterAdmin(mux)
	fsvc.Register(mux, a.Middleware)
	usvc.Register(mux, a.Middleware)
	hub.Register(mux, a.Middleware)
	mux.Handle("/", web.Handler())

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No read/write timeouts: big uploads and downloads may take hours.
	}

	stop := make(chan struct{})
	go fsvc.Janitor(stop)
	go usvc.Janitor(stop)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("fruit-fs is ripe", "listen", cfg.Listen, "data", cfg.DataDir,
		"tailscale_proxies", len(cfg.TrustedProxies), "password_login", cfg.PasswordHash != "")

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	close(stop)
	log.Info("shutting down")
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// inside reports whether dir is base or below it.
func inside(dir, base string) bool {
	d, err1 := filepath.Abs(dir)
	b, err2 := filepath.Abs(base)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(b, d)
	return err == nil && (rel == "." || !strings.HasPrefix(rel, ".."))
}

// securityHeaders sets defaults for every response; file downloads tighten them further.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob: https:; media-src 'self' blob:; "+
			"connect-src 'self'; frame-src 'self'; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'self'")
		next.ServeHTTP(w, r)
	})
}

func hashPassword() error {
	fmt.Fprint(os.Stderr, "Password: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	pw := strings.TrimRight(line, "\r\n")
	if len(pw) < 8 {
		return errors.New("use at least 8 characters")
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

func healthcheck() error {
	addr := os.Getenv("FRUITFS_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
