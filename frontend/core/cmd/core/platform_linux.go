//go:build linux

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"ghosthand/proto"
)

const (
	maxShellOutputBytes = 180 << 10
	maxFSReadBytes      = 180 << 10
)

func osVersion() string {
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if val, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_NAME="); ok {
				val = strings.Trim(val, `"'`)
				if val != "" {
					if len(val) > 120 {
						val = val[:120]
					}
					return val
				}
			}
		}
	}
	if rel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if s := strings.TrimSpace(string(rel)); s != "" {
			return "Linux " + s
		}
	}
	return "Linux"
}

func powerStatus() (*int, *bool) {
	matches, err := filepath.Glob("/sys/class/power_supply/BAT*")
	if err != nil || len(matches) == 0 {
		return nil, nil
	}
	capRaw, err := os.ReadFile(filepath.Join(matches[0], "capacity"))
	if err != nil {
		return nil, nil
	}
	pct, err := strconv.Atoi(strings.TrimSpace(string(capRaw)))
	if err != nil || pct < 0 || pct > 100 {
		return nil, nil
	}
	var charging *bool
	if stRaw, err := os.ReadFile(filepath.Join(matches[0], "status")); err == nil {
		c := strings.EqualFold(strings.TrimSpace(string(stRaw)), "Charging")
		charging = &c
	}
	return &pct, charging
}

func keepAwake() func() { return func() {} }

func deviceMemory() *memoryStatus {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	var totalKB, availKB, freeKB uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		case "MemFree:":
			freeKB = v
		}
	}
	if totalKB == 0 {
		return nil
	}
	if availKB == 0 {
		availKB = freeKB
	}
	return &memoryStatus{Total: totalKB * 1024, Available: availKB * 1024}
}

func readSystemMetrics(ctx context.Context, args proto.GUIArgs) systemMetrics {
	var m systemMetrics
	if u, err := user.Current(); err == nil && u.Username != "" {
		m.CoreAccount = u.Username
	} else if os.Geteuid() == 0 {
		m.CoreAccount = "root"
	}
	if cpu, ok := sampleLinuxCPU(ctx); ok {
		m.CPU = &cpu
	}
	if args.IncludeDisks == nil || *args.IncludeDisks {
		m.Disks = readLinuxDisks()
	}
	if args.IncludeNetwork == nil || *args.IncludeNetwork {
		if ifaces, err := net.Interfaces(); err == nil {
			for _, iface := range ifaces {
				if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
					continue
				}
				addrs, err := iface.Addrs()
				if err != nil {
					continue
				}
				for _, addr := range addrs {
					if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP != nil {
						if ip4 := ipNet.IP.To4(); ip4 != nil {
							m.Addresses = append(m.Addresses, ip4.String())
						}
					}
				}
			}
		}
	}
	return m
}

func readProcCPUTicks() (idle, total uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	first, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(first)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for i := 1; i < len(fields); i++ {
		v, _ := strconv.ParseUint(fields[i], 10, 64)
		total += v
		if i == 4 || i == 5 { // idle + iowait
			idle += v
		}
	}
	return idle, total, true
}

func sampleLinuxCPU(ctx context.Context) (float64, bool) {
	idle1, total1, ok := readProcCPUTicks()
	if !ok {
		return 0, false
	}
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return 0, false
	case <-timer.C:
	}
	idle2, total2, ok := readProcCPUTicks()
	if !ok || total2 <= total1 {
		return 0, false
	}
	deltaTotal := float64(total2 - total1)
	deltaIdle := float64(idle2 - idle1)
	pct := (1.0 - deltaIdle/deltaTotal) * 100.0
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct, true
}

func readLinuxDisks() []diskStatus {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var disks []diskStatus
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/") {
			continue
		}
		mount := fields[1]
		if seen[mount] {
			continue
		}
		seen[mount] = true
		var stat syscall.Statfs_t
		if syscall.Statfs(mount, &stat) != nil || stat.Blocks == 0 {
			continue
		}
		bsize := uint64(stat.Bsize)
		disks = append(disks, diskStatus{
			Drive: mount,
			Total: stat.Blocks * bsize,
			Free:  stat.Bavail * bsize,
		})
	}
	return disks
}

type linuxProcessInfo struct {
	PID    uint32   `json:"pid"`
	Name   string   `json:"name"`
	Memory *float64 `json:"memory_mb"`
}

func listProcesses(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("读取 /proc 失败: %w", err)
	}
	needle := strings.ToLower(strings.TrimSpace(args.Filter))
	processes := make([]linuxProcessInfo, 0)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !entry.IsDir() {
			continue
		}
		pid64, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil || pid64 == 0 {
			continue
		}
		statusBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		var name string
		var rssKB float64
		for _, line := range strings.Split(string(statusBytes), "\n") {
			if v, ok := strings.CutPrefix(line, "Name:"); ok {
				name = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				fields := strings.Fields(v)
				if len(fields) > 0 {
					rssKB, _ = strconv.ParseFloat(fields[0], 64)
				}
			}
		}
		if name == "" {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(name), needle) && entry.Name() != needle {
			continue
		}
		mb := rssKB / 1024.0
		processes = append(processes, linuxProcessInfo{
			PID:    uint32(pid64),
			Name:   name,
			Memory: &mb,
		})
	}
	slices.SortFunc(processes, func(a, b linuxProcessInfo) int {
		var order int
		switch args.SortBy {
		case "name":
			order = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		case "pid":
			return cmp.Compare(a.PID, b.PID)
		default:
			order = cmp.Compare(*b.Memory, *a.Memory)
		}
		if order == 0 {
			return cmp.Compare(a.PID, b.PID)
		}
		return order
	})
	limit := args.Limit
	if limit <= 0 {
		limit = 50
	}
	return json.Marshal(struct {
		Total     int                `json:"total_count"`
		Processes []linuxProcessInfo `json:"processes"`
	}{len(processes), processes[:min(limit, len(processes))]})
}

func manageProcess(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	switch args.Action {
	case "kill", "kill_tree":
		var targets []int
		if args.PID > 0 {
			targets = append(targets, args.PID)
		} else {
			targets = findLinuxPIDsByName(args.Name)
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("未找到目标进程: %s", args.Name)
		}
		for _, pid := range targets {
			if args.Action == "kill_tree" {
				killLinuxTree(pid)
				continue
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				if err2 := syscall.Kill(pid, syscall.SIGKILL); err2 != nil && !errors.Is(err2, syscall.ESRCH) {
					return nil, fmt.Errorf("结束进程 %d 失败: %w", pid, err2)
				}
			}
		}
		return json.Marshal(map[string]any{
			"status": "ok",
			"action": args.Action,
			"pids":   targets,
			"detail": fmt.Sprintf("已终止 %d 个进程", len(targets)),
		})
	case "service_status", "service_start", "service_stop", "service_restart":
		verb := strings.TrimPrefix(args.Action, "service_")
		var cmd *exec.Cmd
		if hasSystemd() {
			cmd = exec.CommandContext(ctx, "systemctl", verb, args.Name, "--no-pager")
		} else if _, err := exec.LookPath("rc-service"); err == nil {
			cmd = exec.CommandContext(ctx, "rc-service", args.Name, verb)
		} else if _, err := exec.LookPath("service"); err == nil {
			cmd = exec.CommandContext(ctx, "service", args.Name, verb)
		} else {
			return nil, errors.New("未检测到受支持的服务管理器 (systemd / OpenRC / service)")
		}
		out, err := cmd.CombinedOutput()
		if err != nil && args.Action != "service_status" {
			return nil, fmt.Errorf("服务操作失败: %s", strings.TrimSpace(string(out)))
		}
		state := "ok"
		if args.Action == "service_status" {
			if err == nil {
				state = "running"
			} else {
				state = "stopped"
			}
		}
		return json.Marshal(map[string]any{
			"status":  "ok",
			"action":  args.Action,
			"service": args.Name,
			"state":   state,
			"output":  strings.TrimSpace(string(out)),
		})
	default:
		return nil, errors.New("E_CAP_UNSUPPORTED")
	}
}

func hasSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

func findLinuxPIDsByName(name string) []int {
	needle := strings.ToLower(strings.TrimSpace(name))
	if needle == "" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(string(comm))) == needle {
			pids = append(pids, pid)
		}
	}
	return pids
}

func killLinuxTree(root int) {
	children := make(map[int][]int)
	if entries, err := os.ReadDir("/proc"); err == nil {
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 0 {
				continue
			}
			statBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
			if err != nil {
				continue
			}
			// Field 4 (after the closing ')' of comm) is ppid.
			raw := string(statBytes)
			idx := strings.LastIndexByte(raw, ')')
			if idx < 0 || idx+2 >= len(raw) {
				continue
			}
			fields := strings.Fields(raw[idx+2:])
			if len(fields) >= 2 {
				if ppid, err := strconv.Atoi(fields[1]); err == nil {
					children[ppid] = append(children[ppid], pid)
				}
			}
		}
	}
	var walk func(int)
	walk = func(p int) {
		for _, c := range children[p] {
			walk(c)
		}
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	walk(root)
}

func resolveLinuxShell(requested string) (string, string) {
	candidates := []string{"/bin/bash", "/bin/ash", "/bin/sh"}
	switch requested {
	case "bash":
		candidates = []string{"/bin/bash", "bash", "/bin/ash", "/bin/sh"}
	case "ash":
		candidates = []string{"/bin/ash", "ash", "/bin/sh"}
	case "sh":
		candidates = []string{"/bin/sh", "sh"}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, filepath.Base(p)
		}
	}
	return "/bin/sh", "sh"
}

func expandLinuxPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if raw == "~" {
				return home
			}
			raw = filepath.Join(home, raw[2:])
		}
	}
	return os.ExpandEnv(raw)
}

type linuxProcResult struct {
	exitCode  int
	stdout    string
	stderr    string
	duration  int64
	truncated bool
	timedOut  bool
	runAs     string
	launchErr error
}

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rem := b.limit - b.buf.Len()
	if rem <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > rem {
		b.buf.Write(p[:rem])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) output() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(b.buf.String(), "\uFFFD"), b.truncated
}

func runLinuxProcess(ctx context.Context, timeout time.Duration, exe string, argv []string, cwd string) linuxProcResult {
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command(exe, argv...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")

	var stdoutBuf, stderrBuf cappedBuffer
	stdoutBuf.limit = maxShellOutputBytes
	stderrBuf.limit = maxShellOutputBytes / 2
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := runCtx.Err(); err != nil {
		return linuxProcResult{launchErr: err}
	}
	if err := cmd.Start(); err != nil {
		return linuxProcResult{launchErr: fmt.Errorf("启动进程失败: %w", err)}
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	var timedOut bool
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
		}
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	runAs := "root"
	if u, err := user.Current(); err == nil && u.Username != "" {
		runAs = u.Username
	}
	stdout, stdoutTrunc := stdoutBuf.output()
	stderr, stderrTrunc := stderrBuf.output()
	return linuxProcResult{
		exitCode:  exitCode,
		stdout:    stdout,
		stderr:    stderr,
		duration:  time.Since(started).Milliseconds(),
		truncated: stdoutTrunc || stderrTrunc,
		timedOut:  timedOut,
		runAs:     runAs,
	}
}

func marshalLinuxShellResult(res linuxProcResult, shell, scriptPath string) (json.RawMessage, error) {
	payload := struct {
		ScriptPath string `json:"script_path,omitempty"`
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMS int64  `json:"duration_ms"`
		Truncated  bool   `json:"truncated"`
		TimedOut   bool   `json:"timed_out,omitempty"`
		Shell      string `json:"shell"`
		RunAs      string `json:"run_as"`
	}{
		ScriptPath: scriptPath,
		ExitCode:   res.exitCode,
		Stdout:     res.stdout,
		Stderr:     res.stderr,
		DurationMS: res.duration,
		Truncated:  res.truncated,
		TimedOut:   res.timedOut,
		Shell:      shell,
		RunAs:      res.runAs,
	}
	if res.timedOut && payload.Stderr == "" {
		payload.Stderr = "命令执行超时已被终止"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(data) > proto.MaxToolDataSize-1024 {
		excess := len(data) - (proto.MaxToolDataSize - 2048)
		if len(payload.Stdout) > excess {
			payload.Stdout = trimUTF8Bytes(payload.Stdout, len(payload.Stdout)-excess)
		} else {
			payload.Stdout = ""
		}
		payload.Truncated = true
		data, err = json.Marshal(payload)
	}
	return data, err
}

func trimUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	b := []byte(s)[:maxBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func execShell(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	timeout := 60 * time.Second
	if args.TimeoutSec > 0 {
		timeout = time.Duration(args.TimeoutSec) * time.Second
	}
	cwd := expandLinuxPath(args.Cwd)
	if cwd != "" {
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("工作目录不存在: %s", cwd)
		}
	}
	exe, shellName := resolveLinuxShell(args.Shell)
	res := runLinuxProcess(ctx, timeout, exe, []string{"-c", args.Command}, cwd)
	if res.launchErr != nil {
		return nil, res.launchErr
	}
	return marshalLinuxShellResult(res, shellName, "")
}

func runScript(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	timeout := 120 * time.Second
	if args.TimeoutSec > 0 {
		timeout = time.Duration(args.TimeoutSec) * time.Second
	}
	cwd := expandLinuxPath(args.Cwd)
	if cwd != "" {
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("工作目录不存在: %s", cwd)
		}
	}
	scriptPath := expandLinuxPath(args.Path)
	hasContent := args.Content != nil && strings.TrimSpace(*args.Content) != ""
	interp := args.Interpreter
	if interp == "" || interp == "powershell" || interp == "pwsh" || interp == "cmd" {
		interp = "auto"
	}

	var cleanupPath string
	if scriptPath == "" {
		if !hasContent {
			return nil, errors.New("未提供脚本路径或脚本内容")
		}
		tmp, err := os.CreateTemp("", "gh-script-*")
		if err != nil {
			return nil, fmt.Errorf("创建临时脚本失败: %w", err)
		}
		if _, err := tmp.WriteString(*args.Content); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("写入临时脚本失败: %w", err)
		}
		_ = tmp.Chmod(0700)
		_ = tmp.Close()
		scriptPath = tmp.Name()
		cleanupPath = scriptPath
		defer os.Remove(cleanupPath)
	} else if hasContent {
		if err := os.MkdirAll(filepath.Dir(scriptPath), 0755); err != nil {
			return nil, fmt.Errorf("创建脚本目录失败: %w", err)
		}
		if err := os.WriteFile(scriptPath, []byte(*args.Content), 0755); err != nil {
			return nil, fmt.Errorf("写入脚本失败: %w", err)
		}
		if args.KeepFile != nil && !*args.KeepFile {
			defer os.Remove(scriptPath)
		}
	} else {
		if _, err := os.Stat(scriptPath); err != nil {
			return nil, fmt.Errorf("脚本文件不存在: %s", scriptPath)
		}
	}

	if interp == "auto" {
		if strings.EqualFold(filepath.Ext(scriptPath), ".py") {
			interp = "python3"
		} else {
			interp = "sh"
		}
	}

	var exe string
	var label string
	switch interp {
	case "python", "python3":
		if p, err := exec.LookPath("python3"); err == nil {
			exe, label = p, "python3"
		} else if p, err := exec.LookPath("python"); err == nil {
			exe, label = p, "python"
		} else {
			return nil, errors.New("未找到 Python 解释器")
		}
	case "bash", "ash", "sh":
		exe, label = resolveLinuxShell(interp)
	default:
		exe, label = resolveLinuxShell("")
	}

	argv := append([]string{scriptPath}, args.ArgsList...)
	res := runLinuxProcess(ctx, timeout, exe, argv, cwd)
	if res.launchErr != nil {
		return nil, res.launchErr
	}
	shownPath := scriptPath
	if cleanupPath != "" {
		shownPath = ""
	}
	return marshalLinuxShellResult(res, label, shownPath)
}

func listFS(ctx context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	root := expandLinuxPath(args.Path)
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	pattern := strings.ToLower(strings.TrimSpace(args.Pattern))
	type entry struct {
		Name    string `json:"name"`
		Path    string `json:"path,omitempty"`
		IsDir   bool   `json:"is_dir"`
		Size    int64  `json:"size"`
		ModTime string `json:"mod_time"`
	}
	entries := make([]entry, 0)
	truncated := false

	if !args.Recursive {
		items, err := os.ReadDir(root)
		if err != nil {
			return nil, fmt.Errorf("读取目录失败: %w", err)
		}
		for _, item := range items {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if pattern != "" {
				matched, _ := filepath.Match(pattern, strings.ToLower(item.Name()))
				if !matched {
					continue
				}
			}
			if len(entries) >= limit {
				truncated = true
				break
			}
			var size int64
			var modTime string
			if info, err := item.Info(); err == nil {
				size = info.Size()
				modTime = info.ModTime().Format(time.RFC3339)
			}
			entries = append(entries, entry{
				Name:    item.Name(),
				IsDir:   item.IsDir(),
				Size:    size,
				ModTime: modTime,
			})
		}
	} else {
		errStop := errors.New("limit reached")
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil || p == root {
				return nil
			}
			if pattern != "" {
				matched, _ := filepath.Match(pattern, strings.ToLower(d.Name()))
				if !matched {
					return nil
				}
			}
			if len(entries) >= limit {
				truncated = true
				return errStop
			}
			rel, _ := filepath.Rel(root, p)
			var size int64
			var modTime string
			if info, err := d.Info(); err == nil {
				size = info.Size()
				modTime = info.ModTime().Format(time.RFC3339)
			}
			entries = append(entries, entry{
				Name:    rel,
				Path:    p,
				IsDir:   d.IsDir(),
				Size:    size,
				ModTime: modTime,
			})
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return nil, fmt.Errorf("遍历目录失败: %w", err)
		}
	}

	return json.Marshal(map[string]any{
		"path":      root,
		"entries":   entries,
		"count":     len(entries),
		"truncated": truncated,
	})
}

func readFS(_ context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	target := expandLinuxPath(args.Path)
	f, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	offset := args.OffsetLine
	if offset <= 0 {
		offset = 1
	}
	maxLines := args.MaxLines
	if maxLines <= 0 {
		maxLines = 500
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	var out strings.Builder
	totalLines := 0
	linesReturned := 0
	truncated := false
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			totalLines++
			if totalLines >= offset && linesReturned < maxLines {
				if out.Len()+len(line) > maxFSReadBytes {
					truncated = true
				} else {
					out.WriteString(strings.ToValidUTF8(line, "\uFFFD"))
					linesReturned++
				}
			} else if totalLines >= offset+maxLines {
				truncated = true
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("读取文件失败: %w", readErr)
		}
	}

	return json.Marshal(map[string]any{
		"path":           target,
		"content":        out.String(),
		"offset_line":    offset,
		"lines_returned": linesReturned,
		"total_lines":    totalLines,
		"truncated":      truncated,
	})
}

func writeFS(_ context.Context, args proto.GUIArgs) (json.RawMessage, error) {
	target := expandLinuxPath(args.Path)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return nil, fmt.Errorf("创建父目录失败: %w", err)
	}
	mode := args.Mode
	if mode == "" {
		mode = "overwrite"
	}
	flags := os.O_CREATE | os.O_WRONLY
	if mode == "append" {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(target, flags, 0644)
	if err != nil {
		return nil, fmt.Errorf("打开写入文件失败: %w", err)
	}
	defer f.Close()
	n, err := f.WriteString(*args.Content)
	if err != nil {
		return nil, fmt.Errorf("写入文件失败: %w", err)
	}
	return json.Marshal(map[string]any{
		"status":        "ok",
		"path":          target,
		"bytes_written": n,
		"mode":          mode,
	})
}

func corePowerScreen(context.Context, proto.GUIArgs) (json.RawMessage, bool, error) {
	return nil, false, errors.New("E_CAP_UNSUPPORTED")
}

func startUninstallWorker(ctx context.Context) (uninstallWorker, error) {
	script := `read -r line || exit 1
[ "$line" = "uninstall" ] || exit 1
sleep 1
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    systemctl stop ghosthand-core.service 2>/dev/null || true
    systemctl disable ghosthand-core.service 2>/dev/null || true
    rm -f /etc/systemd/system/ghosthand-core.service
    systemctl daemon-reload 2>/dev/null || true
elif command -v rc-service >/dev/null 2>&1; then
    rc-service ghosthand-core stop 2>/dev/null || true
    rc-update del ghosthand-core default 2>/dev/null || true
    rm -f /etc/init.d/ghosthand-core
fi
rm -rf /opt/ghosthand /etc/ghosthand /var/log/ghosthand-core.log
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return uninstallWorker{}, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return uninstallWorker{}, err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	return uninstallWorker{input: stdin, done: done}, nil
}

func startPlatform(_ context.Context, _ context.CancelFunc, _ string, _ bool) (*host, <-chan struct{}) {
	done := make(chan struct{})
	close(done)
	return newHost("headless", "none"), done
}

func installServiceFlag(cfg configuration) error {
	if os.Geteuid() != 0 {
		return errors.New("--install requires root privileges")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll("/opt/ghosthand", 0755); err != nil {
		return err
	}
	if err := os.MkdirAll("/etc/ghosthand", 0700); err != nil {
		return err
	}
	targetBin := "/opt/ghosthand/ghosthand-core"
	if exe != targetBin {
		data, err := os.ReadFile(exe)
		if err != nil {
			return err
		}
		if err := os.WriteFile(targetBin+".new", data, 0755); err != nil {
			return err
		}
		if err := os.Rename(targetBin+".new", targetBin); err != nil {
			return err
		}
	}
	envContent := fmt.Sprintf("GH_SERVER=%s\nGH_KEY=%s\nGH_FINGERPRINT=%s\n", cfg.Server, cfg.Key, cfg.Pin)
	if err := os.WriteFile("/etc/ghosthand/agent.env", []byte(envContent), 0600); err != nil {
		return err
	}
	if hasSystemd() {
		unit := `[Unit]
Description=GhostHand Linux Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/ghosthand/agent.env
ExecStart=/opt/ghosthand/ghosthand-core --service
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`
		if err := os.WriteFile("/etc/systemd/system/ghosthand-core.service", []byte(unit), 0644); err != nil {
			return err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "ghosthand-core.service"}, {"restart", "ghosthand-core.service"}} {
			if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl %v: %s", args, strings.TrimSpace(string(out)))
			}
		}
		return nil
	}
	if _, err := exec.LookPath("rc-service"); err == nil {
		rcScript := `#!/sbin/openrc-run

name="ghosthand-core"
description="GhostHand Linux Agent"
command="/opt/ghosthand/ghosthand-core"
command_args="--service"
command_background="yes"
pidfile="/run/ghosthand-core.pid"
output_log="/var/log/ghosthand-core.log"
error_log="/var/log/ghosthand-core.log"

depend() {
    need net
    after firewall
}

start_pre() {
    if [ -r /etc/ghosthand/agent.env ]; then
        while IFS='=' read -r k v; do
            case "$k" in
                ''|'#'*) continue ;;
            esac
            export "$k=$v"
        done < /etc/ghosthand/agent.env
    fi
}
`
		if err := os.WriteFile("/etc/init.d/ghosthand-core", []byte(rcScript), 0755); err != nil {
			return err
		}
		_ = exec.Command("rc-update", "add", "ghosthand-core", "default").Run()
		if out, err := exec.Command("rc-service", "ghosthand-core", "restart").CombinedOutput(); err != nil {
			return fmt.Errorf("rc-service restart: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	return errors.New("neither systemd nor OpenRC was detected on this host")
}

func uninstallServiceFlag() error {
	if os.Geteuid() != 0 {
		return errors.New("--uninstall requires root privileges")
	}
	if hasSystemd() {
		_ = exec.Command("systemctl", "stop", "ghosthand-core.service").Run()
		_ = exec.Command("systemctl", "disable", "ghosthand-core.service").Run()
		_ = os.Remove("/etc/systemd/system/ghosthand-core.service")
		_ = exec.Command("systemctl", "daemon-reload").Run()
	} else if _, err := exec.LookPath("rc-service"); err == nil {
		_ = exec.Command("rc-service", "ghosthand-core", "stop").Run()
		_ = exec.Command("rc-update", "del", "ghosthand-core", "default").Run()
		_ = os.Remove("/etc/init.d/ghosthand-core")
	}
	_ = os.RemoveAll("/etc/ghosthand")
	_ = os.RemoveAll("/opt/ghosthand")
	return nil
}
