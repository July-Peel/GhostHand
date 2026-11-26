//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"ghosthand/launcher/state"
)

const serviceName = "GhostHandSvc"

type launcherConfig struct {
	Server      string
	Key         string
	Fingerprint string
	Name        string
	Group       string
}

func (c launcherConfig) coreArgs() []string {
	args := []string{"--server", c.Server, "--key", c.Key}
	if c.Fingerprint != "" {
		args = append(args, "--fingerprint", c.Fingerprint)
	}
	if c.Name != "" {
		args = append(args, "--name", c.Name)
	}
	if c.Group != "" {
		args = append(args, "--group", c.Group)
	}
	return args
}

func parseLauncherFlags(args []string) (launcherConfig, error) {
	flags := flag.NewFlagSet("ghosthand-launcher", flag.ContinueOnError)
	server := flags.String("server", os.Getenv("GH_SERVER"), "controller HTTP/HTTPS origin")
	key := flags.String("key", os.Getenv("GH_KEY"), "controller fixed node access key")
	pin := flags.String("fingerprint", os.Getenv("GH_FINGERPRINT"), "optional controller TLS leaf SHA-256 fingerprint")
	name := flags.String("name", os.Getenv("GH_NAME"), "optional device name")
	group := flags.String("group", os.Getenv("GH_GROUP"), "optional device group")
	if err := flags.Parse(args); err != nil {
		return launcherConfig{}, err
	}
	cfg := launcherConfig{
		Server:      strings.TrimRight(strings.TrimSpace(*server), "/"),
		Key:         strings.TrimSpace(*key),
		Fingerprint: strings.TrimSpace(*pin),
		Name:        strings.TrimSpace(*name),
		Group:       strings.TrimSpace(*group),
	}
	if cfg.Server == "" || cfg.Key == "" {
		return launcherConfig{}, errors.New("server and key are required: ghosthand-launcher install --server <URL> --key <KEY>")
	}
	return cfg, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	bootstrap := filepath.Dir(exe)
	if len(os.Args) >= 2 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "install":
			cfg, err := parseLauncherFlags(os.Args[2:])
			if err != nil {
				return err
			}
			return install(exe, bootstrap, cfg)
		case "uninstall":
			return uninstall()
		default:
			return errors.New("usage: ghosthand-launcher [install --server URL --key KEY | uninstall]")
		}
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isService {
		return errors.New("install and start GhostHandSvc from an elevated terminal: ghosthand-launcher install --server <URL> --key <KEY>")
	}
	cfg, err := parseLauncherFlags(os.Args[1:])
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return svc.Run(serviceName, service{bootstrap: bootstrap, cfg: cfg})
}

func install(exe, bootstrap string, cfg launcherConfig) error {
	for _, name := range []string{state.CoreName, state.HelperName} {
		if info, err := os.Stat(filepath.Join(bootstrap, name)); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("missing %s beside the launcher", name)
		}
	}
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "GhostHand", Description: "GhostHand stateless device connection and helper supervisor",
		StartType: mgr.StartAutomatic, DelayedAutoStart: true,
	}, cfg.coreArgs()...)
	if err != nil {
		return err
	}
	defer service.Close()
	recovery := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	err = configureNetworkTrigger(service.Handle)
	if err == nil {
		err = service.SetRecoveryActions([]mgr.RecoveryAction{recovery, recovery, recovery}, 86400)
	}
	if err == nil {
		err = service.SetRecoveryActionsOnNonCrashFailures(true)
	}
	if err == nil {
		err = service.Start()
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			err = nil
		}
	}
	if err != nil {
		_ = service.Delete()
	}
	return err
}

func uninstall() error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(serviceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer service.Close()
	if _, err = service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return service.Delete()
		}
	}
	return errors.New("GhostHandSvc did not stop")
}

type service struct {
	bootstrap string
	cfg       launcherConfig
}

func (s service) Execute(_ []string, changes <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan state.ServiceEvent, 64)
	done := make(chan error, 1)
	go func() { done <- supervise(ctx, s.bootstrap, s.cfg, events) }()
	running := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptSessionChange | svc.AcceptPowerEvent}
	status <- running
	unsubscribe := subscribePower()
	defer unsubscribe()
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case req, open := <-changes:
			if !open || req.Cmd == svc.Stop || req.Cmd == svc.Shutdown {
				status <- svc.Status{State: svc.StopPending, WaitHint: 10000}
				cancel()
				<-done
				return false, 0
			}
			switch req.Cmd {
			case svc.Interrogate:
				status <- running
			case svc.SessionChange:
				notify(events, state.ServiceEvent{Kind: "session", Code: req.EventType})
			case svc.PowerEvent:
				notify(events, state.ServiceEvent{Kind: "power", Code: req.EventType})
			}
		}
	}
}

func notify(events chan state.ServiceEvent, event state.ServiceEvent) {
	select {
	case events <- event:
	default:
	}
}

func newJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}
