package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	osSignal "os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"ghosthand/launcher/state"
	"ghosthand/proto"
	"ghosthand/proto/sshrunner"
)

var version = "0.4.0"

func main() {
	if handled, err := runBrowserProcess(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "run", "pair", "connect":
			args = args[1:]
		default:
			return errors.New("usage: ghosthand-core [--server URL --key KEY] [options]")
		}
	}

	flags := flag.NewFlagSet("ghosthand-core", flag.ContinueOnError)
	server := flags.String("server", os.Getenv("GH_SERVER"), "controller HTTP/HTTPS origin")
	key := flags.String("key", os.Getenv("GH_KEY"), "controller fixed node access key")
	pin := flags.String("fingerprint", os.Getenv("GH_FINGERPRINT"), "optional controller TLS leaf SHA-256 fingerprint")
	hostname, _ := os.Hostname()
	defaultName := os.Getenv("GH_NAME")
	if defaultName == "" {
		defaultName = hostname
	}
	name := flags.String("name", defaultName, "device name")
	group := flags.String("group", os.Getenv("GH_GROUP"), "device group")
	helper := flags.String("helper", "", "optional Helper executable path")
	service := flags.Bool("service", false, "receive launcher events over stdin")
	doInstall := flags.Bool("install", false, "install as a system service on Linux")
	doUninstall := flags.Bool("uninstall", false, "uninstall system service on Linux")
	_ = flags.String("data", "", "deprecated (ignored in stateless mode)")
	_ = flags.String("trial", "", "deprecated (ignored in stateless mode)")
	_ = flags.Bool("verify-helper", false, "deprecated (ignored in stateless mode)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if *doUninstall {
		return uninstallServiceFlag()
	}

	srv := strings.TrimRight(strings.TrimSpace(*server), "/")
	accessKey := strings.TrimSpace(*key)
	if srv == "" || accessKey == "" {
		return errors.New("server and key are required: ghosthand-core --server <URL> --key <KEY>")
	}
	if *doInstall {
		return installServiceFlag(configuration{
			Server: srv,
			Key:    accessKey,
			Pin:    strings.ToLower(strings.ReplaceAll(strings.TrimSpace(*pin), ":", "")),
		})
	}
	machineID, err := machineFingerprint()
	if err != nil {
		return fmt.Errorf("read machine identifier: %w", err)
	}
	helperPath := strings.TrimSpace(*helper)
	if helperPath == "" && runtime.GOOS == "windows" {
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), state.HelperName)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				helperPath = candidate
			}
		}
	}

	cfg := configuration{
		Server:    srv,
		Key:       accessKey,
		Pin:       strings.ToLower(strings.ReplaceAll(strings.TrimSpace(*pin), ":", "")),
		Name:      strings.TrimSpace(*name),
		Group:     strings.TrimSpace(*group),
		MachineID: machineID,
	}
	if cfg.Name == "" {
		cfg.Name = "node"
	}
	return start(cfg, helperPath, *service)
}

func start(cfg configuration, helper string, service bool) error {
	logBuf := initMemoryLogger(service)
	ctx, stop := osSignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := transport(cfg)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	p, platformDone := startPlatform(ctx, stop, helper, service)
	defer func() { stop(); <-platformDone }()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	a := &agent{
		client:       client,
		local:        openMemoryJournal(),
		logs:         logBuf,
		platform:     p,
		browser:      startBrowserBridge(ctx, p, exe),
		corePath:     exe,
		helperPath:   helper,
		welcomed:     make(chan struct{}),
		operations:   make(chan struct{}, 1),
		taskChanges:  make(chan struct{}, 1),
		background:   make(chan struct{}, 4),
		sshEvents:    make(chan sshOutbound, 64),
		canUninstall: service && (runtime.GOOS == "windows" || runtime.GOOS == "linux"),
	}
	a.ssh = sshrunner.NewManager(ctx, func(st proto.SSHStatePayload) {
		a.enqueueSSHOutbound(proto.SSHStateMessage, st)
	})
	defer a.ssh.Close()
	a.cfg.Store(&cfg)

	if err := a.enrollWithRetry(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer stop()
		a.runOperations(ctx, service)
	}()
	tasksDone := make(chan struct{})
	go func() {
		defer close(tasksDone)
		defer stop()
		a.runTasks(ctx)
	}()
	a.connect(ctx)
	stop()
	<-done
	<-tasksDone
	return nil
}

func (a *agent) enrollWithRetry(ctx context.Context) error {
	delay := time.Second
	for ctx.Err() == nil {
		err := a.enroll(ctx)
		if err == nil {
			return nil
		}
		var status statusError
		if errors.As(err, &status) && (status == 400 || status == 401 || status == 403) {
			slog.Warn("device enrollment rejected, retrying", "status", int(status))
		} else {
			slog.Warn("device enrollment failed, retrying", "error", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 20*time.Second)
	}
	return ctx.Err()
}

func (a *agent) enroll(ctx context.Context) error {
	cfg := *a.cfg.Load()
	token, err := proto.NewToken()
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	input := proto.Enrollment{
		Key:       cfg.Key,
		MachineID: cfg.MachineID,
		Token:     token,
		Name:      cfg.Name,
		Group:     cfg.Group,
		Platform:  proto.Platform(runtime.GOOS),
		Arch:      runtime.GOARCH,
		OSVersion: osVersion(),
	}
	var reply proto.EnrollmentResult
	if err := postJSON(reqCtx, a.client, cfg, "/api/agent/pair", input, &reply, false); err != nil {
		return err
	}
	key, err := base64.RawURLEncoding.DecodeString(reply.UpdateKey)
	if err != nil || len(key) != 32 || !state.IDPattern.MatchString(reply.DeviceID) {
		return errors.New("invalid enrollment response")
	}
	cfg.Token = token
	cfg.DeviceID = reply.DeviceID
	cfg.UpdateKey = reply.UpdateKey
	a.cfg.Store(&cfg)
	slog.Info("device enrolled", "device_id", cfg.DeviceID)
	return nil
}
