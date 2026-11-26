//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"ghosthand/launcher/state"
)

func supervise(ctx context.Context, bootstrap string, cfg launcherConfig, events <-chan state.ServiceEvent) error {
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := superviseChild(ctx, bootstrap, cfg, events)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			delay = time.Second
			continue
		}
		if time.Since(started) >= time.Minute {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		delay = min(delay*2, 15*time.Second)
	}
	return nil
}

func superviseChild(ctx context.Context, bootstrap string, cfg launcherConfig, events <-chan state.ServiceEvent) error {
	args := append([]string{"run"}, cfg.coreArgs()...)
	args = append(args, "--helper", filepath.Join(bootstrap, state.HelperName), "--service")
	cmd := exec.Command(filepath.Join(bootstrap, state.CoreName), args...)
	cmd.Dir = bootstrap
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	job, err := newJob()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	if err := cmd.Start(); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		windows.CloseHandle(process)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("assign Core process to job: %w", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	writeEvents := make(chan state.ServiceEvent, 64)
	finishWriter := make(chan struct{})
	defer close(finishWriter)
	go func() {
		encoder := json.NewEncoder(stdin)
		for {
			select {
			case <-finishWriter:
				return
			case event := <-writeEvents:
				if encoder.Encode(event) != nil {
					return
				}
			}
		}
	}()
	stop := func() {
		_ = stdin.Close()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-exited:
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	for {
		select {
		case <-ctx.Done():
			stop()
			return nil
		case err := <-exited:
			return err
		case event := <-events:
			notify(writeEvents, event)
		}
	}
}
