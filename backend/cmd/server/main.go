package main

import (
	"context"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/api"
	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/workbench"
)

//go:embed all:web
var assets embed.FS

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("ghosthand", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := env("GH_ADDR", ":8080")
	if len(os.Args) == 2 && os.Args[1] == "health" {
		return health(addr)
	}
	if len(os.Args) != 1 {
		return errors.New("usage: ghosthand [health]")
	}
	origin, secure, err := publicOrigin(os.Getenv("GH_PUBLIC_URL"))
	if err != nil {
		return err
	}
	demo, err := strconv.ParseBool(env("GH_DEMO_DEVICES", "true"))
	if err != nil {
		return fmt.Errorf("GH_DEMO_DEVICES: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dataDir := env("GH_DATA_DIR", "data")
	store, err := db.Open(ctx, dataDir)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	config, err := settings.New(ctx, store, dataDir)
	if err != nil {
		return fmt.Errorf("open settings: %w", err)
	}
	authentication := auth.New(store)
	username := strings.TrimSpace(env("GH_ADMIN_USER", "admin"))
	generated, err := authentication.Bootstrap(ctx, username, os.Getenv("GH_ADMIN_PASSWORD"), !secure)
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	if generated != "" {
		slog.Info("generated local administrator credentials", "username", username, "password", generated)
	}
	fingerprint := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(os.Getenv("GH_TLS_FINGERPRINT")), ":", ""))
	if fingerprint != "" {
		if _, err := hex.DecodeString(fingerprint); err != nil || len(fingerprint) != 64 || (origin != "" && !secure) {
			return errors.New("GH_TLS_FINGERPRINT must be an HTTPS leaf certificate SHA-256 fingerprint")
		}
	}
	cloudflare, err := strconv.ParseBool(env("GH_BEHIND_CLOUDFLARE", "false"))
	if err != nil {
		return fmt.Errorf("GH_BEHIND_CLOUDFLARE: %w", err)
	}
	fleet, err := agents.New(ctx, store, dataDir, cloudflare)
	if err != nil {
		return fmt.Errorf("open device connections: %w", err)
	}
	desk, err := workbench.New(store, config, dataDir, demo)
	if err != nil {
		return fmt.Errorf("open workbench: %w", err)
	}
	if err := desk.Seed(ctx); err != nil {
		return fmt.Errorf("seed demo devices: %w", err)
	}
	demoDone := make(chan struct{})
	go func() {
		defer close(demoDone)
		desk.RunDemo(ctx)
	}()
	tasksDone := make(chan struct{})
	go func() {
		defer close(tasksDone)
		desk.RunTasks(ctx)
	}()
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		desk.RunMaintenance(ctx)
	}()
	brainDone := make(chan struct{})
	go func() {
		defer close(brainDone)
		desk.RunBrain(ctx)
	}()
	defer func() {
		stop()
		<-demoDone
		<-tasksDone
		<-maintenanceDone
		<-brainDone
	}()

	web, err := fs.Sub(assets, "web/dist")
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: addr, Handler: api.New(store, authentication, config, desk, fleet, origin, fingerprint, web),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 75 * time.Second,
		MaxHeaderBytes: 32 << 10,
	}
	slog.Info("controller starting", "listen", addr, "origin", origin, "demo_devices", demo)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	case err := <-serverDone:
		return err
	}
}

func publicOrigin(raw string) (string, bool, error) {
	if raw = strings.TrimSpace(raw); raw == "" {
		return "", false, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", false, errors.New("GH_PUBLIC_URL must be an origin")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false, errors.New("GH_PUBLIC_URL must use HTTP or HTTPS")
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	return u.Scheme + "://" + host, u.Scheme == "https", nil
}

func health(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/readyz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness endpoint returned %s", response.Status)
	}
	return nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
