package sshrunner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"ghosthand/proto"
)

const (
	maxShellOutputBytes = 180 << 10
	maxFSReadBytes      = 180 << 10
)

// ExecuteTool runs one of the 8 core Linux MCP tools on the already-authenticated SSH client.
func (m *Manager) ExecuteTool(ctx context.Context, targetDeviceID string, call proto.MCPCall) proto.MCPResult {
	result := proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID}
	if ctx.Err() != nil {
		result.Error = string(proto.ErrCancelled)
		return result
	}
	args, err := proto.ValidateTool(call.Tool, call.Arguments)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	client, username, err := m.getClient(targetDeviceID)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	var data json.RawMessage
	switch call.Tool {
	case proto.ToolShellExec:
		data, err = sshExecShell(ctx, client, username, args)
	case proto.ToolScriptRun:
		data, err = sshRunScript(ctx, client, username, args)
	case proto.ToolFSList:
		data, err = sshListFS(ctx, client, args)
	case proto.ToolFSRead:
		data, err = sshReadFS(ctx, client, args)
	case proto.ToolFSWrite:
		data, err = sshWriteFS(ctx, client, args)
	case proto.ToolProcesses:
		data, err = sshListProcesses(ctx, client, args)
	case proto.ToolProcessManage:
		data, err = sshManageProcess(ctx, client, args)
	case proto.ToolStatus:
		data, err = sshDeviceStatus(ctx, client, username, args)
	default:
		err = errors.New("E_CAP_UNSUPPORTED")
	}
	if err != nil {
		result.Error = limitError(err.Error())
	} else {
		result.Data = data
	}
	return result
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type sshRunResult struct {
	exitCode  int
	stdout    string
	stderr    string
	duration  int64
	truncated bool
	timedOut  bool
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

func runSSHCommand(ctx context.Context, client *ssh.Client, timeout time.Duration, command string, stdinData []byte) (sshRunResult, error) {
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session, err := client.NewSession()
	if err != nil {
		return sshRunResult{}, fmt.Errorf("创建 SSH 会话失败: %w", err)
	}
	defer session.Close()

	var stdoutBuf, stderrBuf cappedBuffer
	stdoutBuf.limit = maxShellOutputBytes
	stderrBuf.limit = maxShellOutputBytes / 2
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf
	if len(stdinData) > 0 {
		session.Stdin = bytes.NewReader(stdinData)
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()

	var runErr error
	var timedOut bool
	select {
	case runErr = <-done:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		select {
		case runErr = <-done:
		case <-time.After(2 * time.Second):
		}
	}

	exitCode := 0
	if runErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitStatus()
		} else {
			exitCode = 1
		}
	}
	stdout, stdoutTrunc := stdoutBuf.output()
	stderr, stderrTrunc := stderrBuf.output()
	return sshRunResult{
		exitCode:  exitCode,
		stdout:    stdout,
		stderr:    stderr,
		duration:  time.Since(started).Milliseconds(),
		truncated: stdoutTrunc || stderrTrunc,
		timedOut:  timedOut,
	}, nil
}

func marshalShellResult(res sshRunResult, shell, scriptPath, runAs string) (json.RawMessage, error) {
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
		RunAs:      runAs,
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

func wrapCwdPrefix(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return ""
	}
	if cwd == "~" {
		return `cd "$HOME" && `
	}
	if strings.HasPrefix(cwd, "~/") {
		return fmt.Sprintf(`cd "$HOME"/%s && `, shQuote(strings.TrimPrefix(cwd, "~/")))
	}
	return fmt.Sprintf(`cd %s && `, shQuote(cwd))
}

func expandRemotePathExpr(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(raw, "~/") {
		return `"$HOME"/` + shQuote(strings.TrimPrefix(raw, "~/"))
	}
	return shQuote(raw)
}

func sshExecShell(ctx context.Context, client *ssh.Client, username string, args proto.GUIArgs) (json.RawMessage, error) {
	timeout := 60 * time.Second
	if args.TimeoutSec > 0 {
		timeout = time.Duration(args.TimeoutSec) * time.Second
	}
	shell := args.Shell
	if shell == "" || shell == "powershell" || shell == "pwsh" || shell == "cmd" {
		shell = "sh"
	}
	var cmdLine string
	prefix := wrapCwdPrefix(args.Cwd)
	switch shell {
	case "bash":
		cmdLine = fmt.Sprintf(`%sif command -v bash >/dev/null 2>&1; then bash -c %s; elif command -v ash >/dev/null 2>&1; then ash -c %s; else sh -c %s; fi`,
			prefix, shQuote(args.Command), shQuote(args.Command), shQuote(args.Command))
	case "ash":
		cmdLine = fmt.Sprintf(`%sif command -v ash >/dev/null 2>&1; then ash -c %s; else sh -c %s; fi`,
			prefix, shQuote(args.Command), shQuote(args.Command))
	default:
		cmdLine = fmt.Sprintf(`%sif command -v bash >/dev/null 2>&1; then bash -c %s; elif command -v ash >/dev/null 2>&1; then ash -c %s; else sh -c %s; fi`,
			prefix, shQuote(args.Command), shQuote(args.Command), shQuote(args.Command))
	}
	res, err := runSSHCommand(ctx, client, timeout, cmdLine, nil)
	if err != nil {
		return nil, err
	}
	return marshalShellResult(res, shell, "", username)
}

func sshRunScript(ctx context.Context, client *ssh.Client, username string, args proto.GUIArgs) (json.RawMessage, error) {
	timeout := 120 * time.Second
	if args.TimeoutSec > 0 {
		timeout = time.Duration(args.TimeoutSec) * time.Second
	}
	interp := args.Interpreter
	if interp == "" || interp == "powershell" || interp == "pwsh" || interp == "cmd" {
		interp = "auto"
	}
	scriptPath := strings.TrimSpace(args.Path)
	hasContent := args.Content != nil && strings.TrimSpace(*args.Content) != ""
	if interp == "auto" {
		if strings.EqualFold(path.Ext(scriptPath), ".py") {
			interp = "python3"
		} else {
			interp = "sh"
		}
	}

	var quotedArgs strings.Builder
	for _, a := range args.ArgsList {
		quotedArgs.WriteByte(' ')
		quotedArgs.WriteString(shQuote(a))
	}

	interpCmd := func(fileExpr string) string {
		switch interp {
		case "python", "python3":
			return fmt.Sprintf(`if command -v python3 >/dev/null 2>&1; then python3 %s%s; elif command -v python >/dev/null 2>&1; then python %s%s; else echo "Python interpreter not found" >&2; exit 127; fi`,
				fileExpr, quotedArgs.String(), fileExpr, quotedArgs.String())
		case "bash":
			return fmt.Sprintf(`if command -v bash >/dev/null 2>&1; then bash %s%s; else sh %s%s; fi`,
				fileExpr, quotedArgs.String(), fileExpr, quotedArgs.String())
		case "ash":
			return fmt.Sprintf(`if command -v ash >/dev/null 2>&1; then ash %s%s; else sh %s%s; fi`,
				fileExpr, quotedArgs.String(), fileExpr, quotedArgs.String())
		default:
			return fmt.Sprintf(`if command -v bash >/dev/null 2>&1; then bash %s%s; elif command -v ash >/dev/null 2>&1; then ash %s%s; else sh %s%s; fi`,
				fileExpr, quotedArgs.String(), fileExpr, quotedArgs.String(), fileExpr, quotedArgs.String())
		}
	}

	prefix := wrapCwdPrefix(args.Cwd)
	if scriptPath == "" {
		if !hasContent {
			return nil, errors.New("未提供脚本路径或脚本内容")
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(*args.Content))
		cmdLine := fmt.Sprintf(`TMP="$(mktemp /tmp/gh-script-XXXXXX)"; trap 'rm -f "$TMP"' EXIT; printf '%%s' %s | base64 -d > "$TMP"; %s%s`,
			shQuote(b64), prefix, interpCmd(`"$TMP"`))
		res, err := runSSHCommand(ctx, client, timeout, cmdLine, nil)
		if err != nil {
			return nil, err
		}
		return marshalShellResult(res, interp, "", username)
	}

	pathExpr := expandRemotePathExpr(scriptPath)
	var b strings.Builder
	if hasContent {
		b64 := base64.StdEncoding.EncodeToString([]byte(*args.Content))
		fmt.Fprintf(&b, `TARGET=%s; mkdir -p "$(dirname "$TARGET")"; printf '%%s' %s | base64 -d > "$TARGET"; chmod +x "$TARGET" 2>/dev/null || true; `,
			pathExpr, shQuote(b64))
	} else {
		fmt.Fprintf(&b, `TARGET=%s; if [ ! -e "$TARGET" ]; then echo "script not found: $TARGET" >&2; exit 1; fi; `, pathExpr)
	}
	b.WriteString(prefix)
	b.WriteString(interpCmd(`"$TARGET"`))
	if args.KeepFile != nil && !*args.KeepFile && hasContent {
		b.WriteString(`; RC=$?; rm -f "$TARGET"; exit $RC`)
	}
	res, err := runSSHCommand(ctx, client, timeout, b.String(), nil)
	if err != nil {
		return nil, err
	}
	return marshalShellResult(res, interp, scriptPath, username)
}

func sshListFS(ctx context.Context, client *ssh.Client, args proto.GUIArgs) (json.RawMessage, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = 100
	}
	pattern := strings.ToLower(strings.TrimSpace(args.Pattern))
	rootExpr := expandRemotePathExpr(args.Path)
	depthFlag := "-maxdepth 1 "
	if args.Recursive {
		depthFlag = ""
	}
	// Compatible with both Debian GNU find/stat and Alpine BusyBox find/stat.
	script := fmt.Sprintf(`ROOT=%s
if [ ! -e "$ROOT" ]; then echo "path not found" >&2; exit 1; fi
printf 'ROOT\t%%s\n' "$ROOT"
find "$ROOT" %s-mindepth 1 2>/dev/null | head -n %d | while IFS= read -r p; do
  if [ -d "$p" ]; then t=d; else t=f; fi
  sz="$(wc -c < "$p" 2>/dev/null || echo 0)"
  if [ "$t" = "d" ]; then sz=0; fi
  mt="$(date -r "$p" -u +%%Y-%%m-%%dT%%H:%%M:%%SZ 2>/dev/null || echo '')"
  printf '%%s\t%%s\t%%s\t%%s\n' "$t" "$sz" "$mt" "$p"
done`, rootExpr, depthFlag, limit*3+10)

	res, err := runSSHCommand(ctx, client, 30*time.Second, script, nil)
	if err != nil {
		return nil, err
	}
	if res.exitCode != 0 && strings.TrimSpace(res.stdout) == "" {
		return nil, fmt.Errorf("读取目录失败: %s", strings.TrimSpace(res.stderr))
	}

	type entry struct {
		Name    string `json:"name"`
		Path    string `json:"path,omitempty"`
		IsDir   bool   `json:"is_dir"`
		Size    int64  `json:"size"`
		ModTime string `json:"mod_time"`
	}
	resolvedRoot := strings.TrimSpace(args.Path)
	var entries []entry
	truncated := false
	for _, line := range strings.Split(res.stdout, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "ROOT\t") {
			resolvedRoot = strings.TrimPrefix(line, "ROOT\t")
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) != 4 {
			continue
		}
		fullPath := parts[3]
		baseName := path.Base(fullPath)
		if pattern != "" {
			matched, _ := filepath.Match(pattern, strings.ToLower(baseName))
			if !matched {
				continue
			}
		}
		if len(entries) >= limit {
			truncated = true
			break
		}
		sz, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		name := baseName
		relPath := ""
		if args.Recursive {
			rel := strings.TrimPrefix(fullPath, strings.TrimRight(resolvedRoot, "/")+"/")
			name = rel
			relPath = fullPath
		}
		entries = append(entries, entry{
			Name:    name,
			Path:    relPath,
			IsDir:   parts[0] == "d",
			Size:    sz,
			ModTime: parts[2],
		})
	}
	if entries == nil {
		entries = []entry{}
	}
	return json.Marshal(map[string]any{
		"path":      resolvedRoot,
		"entries":   entries,
		"count":     len(entries),
		"truncated": truncated,
	})
}

func sshReadFS(ctx context.Context, client *ssh.Client, args proto.GUIArgs) (json.RawMessage, error) {
	offset := args.OffsetLine
	if offset <= 0 {
		offset = 1
	}
	maxLines := args.MaxLines
	if maxLines <= 0 {
		maxLines = 500
	}
	targetExpr := expandRemotePathExpr(args.Path)
	script := fmt.Sprintf(`TARGET=%s
if [ ! -r "$TARGET" ]; then echo "cannot read file: $TARGET" >&2; exit 1; fi
TOTAL="$(wc -l < "$TARGET" 2>/dev/null | tr -d ' ')"
printf 'TOTAL=%%s\nPATH=%%s\n---DATA---\n' "${TOTAL:-0}" "$TARGET"
awk -v s=%d -v n=%d 'NR>=s && NR<s+n { print }' "$TARGET"`, targetExpr, offset, maxLines)

	res, err := runSSHCommand(ctx, client, 30*time.Second, script, nil)
	if err != nil {
		return nil, err
	}
	if res.exitCode != 0 && !strings.Contains(res.stdout, "\n---DATA---\n") {
		return nil, fmt.Errorf("读取文件失败: %s", strings.TrimSpace(res.stderr))
	}
	parts := strings.SplitN(res.stdout, "\n---DATA---\n", 2)
	resolvedPath := strings.TrimSpace(args.Path)
	totalLines := 0
	if len(parts) >= 1 {
		for _, hdr := range strings.Split(parts[0], "\n") {
			if v, ok := strings.CutPrefix(hdr, "TOTAL="); ok {
				totalLines, _ = strconv.Atoi(strings.TrimSpace(v))
			} else if v, ok := strings.CutPrefix(hdr, "PATH="); ok {
				resolvedPath = v
			}
		}
	}
	content := ""
	if len(parts) == 2 {
		content = parts[1]
	}
	truncated := res.truncated
	if len(content) > maxFSReadBytes {
		content = trimUTF8Bytes(content, maxFSReadBytes)
		truncated = true
	}
	linesReturned := 0
	if content != "" {
		linesReturned = strings.Count(content, "\n")
		if !strings.HasSuffix(content, "\n") {
			linesReturned++
		}
	}
	if totalLines == 0 && linesReturned > 0 {
		totalLines = offset - 1 + linesReturned
	}
	if offset-1+linesReturned < totalLines {
		truncated = true
	}
	return json.Marshal(map[string]any{
		"path":           resolvedPath,
		"content":        content,
		"offset_line":    offset,
		"lines_returned": linesReturned,
		"total_lines":    totalLines,
		"truncated":      truncated,
	})
}

func sshWriteFS(ctx context.Context, client *ssh.Client, args proto.GUIArgs) (json.RawMessage, error) {
	mode := args.Mode
	if mode == "" {
		mode = "overwrite"
	}
	op := ">"
	if mode == "append" {
		op = ">>"
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(*args.Content))
	targetExpr := expandRemotePathExpr(args.Path)
	script := fmt.Sprintf(`TARGET=%s; mkdir -p "$(dirname "$TARGET")" && base64 -d %s "$TARGET" && printf '%%s' "$TARGET"`, targetExpr, op)
	res, err := runSSHCommand(ctx, client, 30*time.Second, script, []byte(b64))
	if err != nil {
		return nil, err
	}
	if res.exitCode != 0 {
		return nil, fmt.Errorf("写入文件失败: %s", strings.TrimSpace(res.stderr))
	}
	resolvedPath := strings.TrimSpace(res.stdout)
	if resolvedPath == "" {
		resolvedPath = strings.TrimSpace(args.Path)
	}
	return json.Marshal(map[string]any{
		"status":        "ok",
		"path":          resolvedPath,
		"bytes_written": len(*args.Content),
		"mode":          mode,
	})
}

func sshListProcesses(ctx context.Context, client *ssh.Client, args proto.GUIArgs) (json.RawMessage, error) {
	// Directly parse /proc/[0-9]*/status so it works identically on Debian and Alpine BusyBox.
	script := `for f in /proc/[0-9]*/status; do
  [ -r "$f" ] || continue
  awk '/^Pid:/{p=$2} /^Name:/{n=$2} /^VmRSS:/{m=$2} END{if(p!="") printf "%s\t%s\t%s\n", p, n, (m==""?0:m)}' "$f" 2>/dev/null
done`
	res, err := runSSHCommand(ctx, client, 20*time.Second, script, nil)
	if err != nil {
		return nil, err
	}
	type processInfo struct {
		PID    uint32   `json:"pid"`
		Name   string   `json:"name"`
		Memory *float64 `json:"memory_mb"`
	}
	needle := strings.ToLower(strings.TrimSpace(args.Filter))
	processes := make([]processInfo, 0)
	for _, line := range strings.Split(res.stdout, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		pid64, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 32)
		if err != nil || pid64 == 0 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		if needle != "" && !strings.Contains(strings.ToLower(name), needle) && strconv.FormatUint(pid64, 10) != needle {
			continue
		}
		kb, _ := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		mb := kb / 1024.0
		processes = append(processes, processInfo{
			PID:    uint32(pid64),
			Name:   name,
			Memory: &mb,
		})
	}
	slices.SortFunc(processes, func(a, b processInfo) int {
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
		Total     int           `json:"total_count"`
		Processes []processInfo `json:"processes"`
	}{len(processes), processes[:min(limit, len(processes))]})
}

func sshManageProcess(ctx context.Context, client *ssh.Client, args proto.GUIArgs) (json.RawMessage, error) {
	switch args.Action {
	case "kill", "kill_tree":
		var script string
		if args.PID > 0 {
			if args.Action == "kill_tree" {
				script = fmt.Sprintf(`kill_tree() {
  for c in $(awk -v p="$1" '$4==p {print $1}' /proc/[0-9]*/stat 2>/dev/null); do kill_tree "$c"; done
  kill -9 "$1" 2>/dev/null || true
}
kill_tree %d`, args.PID)
			} else {
				script = fmt.Sprintf(`kill -TERM %d 2>/dev/null || kill -9 %d`, args.PID, args.PID)
			}
		} else {
			name := shQuote(strings.TrimSpace(args.Name))
			script = fmt.Sprintf(`FOUND=0
for f in /proc/[0-9]*/status; do
  [ -r "$f" ] || continue
  P="$(awk -v target=%s '/^Pid:/{p=$2} /^Name:/{n=$2} END{if(tolower(n)==tolower(target)) print p}' "$f" 2>/dev/null)"
  if [ -n "$P" ]; then
    FOUND=1
    kill -9 "$P" 2>/dev/null || true
  fi
done
if [ "$FOUND" -eq 0 ]; then echo "process not found" >&2; exit 1; fi`, name)
		}
		res, err := runSSHCommand(ctx, client, 15*time.Second, script, nil)
		if err != nil {
			return nil, err
		}
		if res.exitCode != 0 {
			return nil, fmt.Errorf("终止进程失败: %s", strings.TrimSpace(res.stderr))
		}
		return json.Marshal(map[string]any{
			"status": "ok",
			"action": args.Action,
			"pid":    args.PID,
			"name":   args.Name,
			"detail": "进程已终止",
		})
	case "service_status", "service_start", "service_stop", "service_restart":
		svc := shQuote(strings.TrimSpace(args.Name))
		verb := strings.TrimPrefix(args.Action, "service_")
		script := fmt.Sprintf(`SVC=%s
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
  systemctl %s "$SVC" --no-pager
elif command -v rc-service >/dev/null 2>&1; then
  rc-service "$SVC" %s
elif command -v service >/dev/null 2>&1; then
  service "$SVC" %s
else
  echo "No supported service manager (systemd / OpenRC / service) found" >&2
  exit 1
fi`, svc, verb, verb, verb)
		res, err := runSSHCommand(ctx, client, 30*time.Second, script, nil)
		if err != nil {
			return nil, err
		}
		if res.exitCode != 0 && args.Action != "service_status" {
			return nil, fmt.Errorf("服务操作失败: %s", strings.TrimSpace(res.stderr+res.stdout))
		}
		state := "unknown"
		if args.Action == "service_status" {
			if res.exitCode == 0 {
				state = "running"
			} else {
				state = "stopped"
			}
		} else {
			state = "ok"
		}
		return json.Marshal(map[string]any{
			"status":  "ok",
			"action":  args.Action,
			"service": args.Name,
			"state":   state,
			"output":  strings.TrimSpace(res.stdout + "\n" + res.stderr),
		})
	default:
		return nil, errors.New("E_CAP_UNSUPPORTED")
	}
}

func sshDeviceStatus(ctx context.Context, client *ssh.Client, username string, args proto.GUIArgs) (json.RawMessage, error) {
	includeDisks := args.IncludeDisks == nil || *args.IncludeDisks
	includeNet := args.IncludeNetwork == nil || *args.IncludeNetwork
	script := `printf 'HOST=%s\n' "$(hostname 2>/dev/null || echo unknown)"
printf 'USER=%s\n' "$(id -un 2>/dev/null || echo unknown)"
printf 'ARCH=%s\n' "$(uname -m 2>/dev/null || echo amd64)"
printf 'CPUS=%s\n' "$(nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo 2>/dev/null || echo 1)"
printf 'OS=%s\n' "$(if [ -r /etc/os-release ]; then sed -n 's/^PRETTY_NAME=//p' /etc/os-release | head -n1 | tr -d '"'; else uname -sr; fi)"
printf 'LOAD=%s\n' "$(cat /proc/loadavg 2>/dev/null | awk '{print $1,$2,$3}')"
awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} /^MemFree:/{f=$2} END{printf "MEM=%d %d\n", t*1024, (a>0?a:f)*1024}' /proc/meminfo 2>/dev/null
printf -- '---DISKS---\n'
df -kP 2>/dev/null | awk 'NR>1 && $1 ~ /^\// {printf "%s\t%s\t%s\n", $6, $2*1024, $4*1024}'
printf -- '---NET---\n'
if command -v ip >/dev/null 2>&1; then ip -o -4 addr show scope global 2>/dev/null | awk '{split($4,a,"/"); print a[1]}'; else hostname -i 2>/dev/null | tr ' ' '\n'; fi`

	res, err := runSSHCommand(ctx, client, 15*time.Second, script, nil)
	if err != nil {
		return nil, err
	}

	type diskStatus struct {
		Drive string `json:"drive"`
		Total uint64 `json:"total_bytes"`
		Free  uint64 `json:"free_bytes"`
	}
	type memoryStatus struct {
		Total     uint64 `json:"total_bytes"`
		Available uint64 `json:"available_bytes"`
	}
	payload := struct {
		HelperState  string        `json:"helper_state"`
		DesktopState string        `json:"desktop_state"`
		Platform     string        `json:"platform"`
		Arch         string        `json:"arch"`
		OS           string        `json:"os_version"`
		Hostname     string        `json:"hostname,omitempty"`
		CoreAccount  string        `json:"core_privilege,omitempty"`
		CPUs         int           `json:"logical_cpu_count"`
		LoadAvg      string        `json:"load_avg,omitempty"`
		Memory       *memoryStatus `json:"memory,omitempty"`
		Disks        []diskStatus  `json:"disks,omitempty"`
		Addresses    []string      `json:"local_ips,omitempty"`
	}{
		HelperState:  "none",
		DesktopState: "headless",
		Platform:     "linux",
		Arch:         "amd64",
		OS:           "Linux (SSH)",
		CoreAccount:  username,
		CPUs:         1,
	}

	section := "header"
	for _, line := range strings.Split(res.stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "---DISKS---" {
			section = "disks"
			continue
		}
		if line == "---NET---" {
			section = "net"
			continue
		}
		switch section {
		case "header":
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch k {
			case "HOST":
				payload.Hostname = v
			case "USER":
				if v != "" {
					payload.CoreAccount = v
				}
			case "ARCH":
				if v == "x86_64" {
					payload.Arch = "amd64"
				} else if v == "aarch64" {
					payload.Arch = "arm64"
				} else if v != "" {
					payload.Arch = v
				}
			case "CPUS":
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					payload.CPUs = n
				}
			case "OS":
				if v != "" {
					payload.OS = v
				}
			case "LOAD":
				payload.LoadAvg = v
			case "MEM":
				fields := strings.Fields(v)
				if len(fields) == 2 {
					total, _ := strconv.ParseUint(fields[0], 10, 64)
					avail, _ := strconv.ParseUint(fields[1], 10, 64)
					if total > 0 {
						payload.Memory = &memoryStatus{Total: total, Available: avail}
					}
				}
			}
		case "disks":
			if !includeDisks {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) == 3 {
				total, _ := strconv.ParseUint(parts[1], 10, 64)
				free, _ := strconv.ParseUint(parts[2], 10, 64)
				payload.Disks = append(payload.Disks, diskStatus{Drive: parts[0], Total: total, Free: free})
			}
		case "net":
			if !includeNet {
				continue
			}
			if line != "" && line != "127.0.0.1" {
				payload.Addresses = append(payload.Addresses, line)
			}
		}
	}
	return json.Marshal(payload)
}
