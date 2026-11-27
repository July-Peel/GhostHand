package api

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
)

const windowsInstallPath = "/api/agent/install/windows/"

//go:embed install_windows.ps1
var windowsInstallScript []byte

func (s *Server) windowsInstallerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pairing/windows/files", s.require(s.windowsInstallerStatus))
	mux.HandleFunc("POST /api/pairing/windows/files/{name}", s.require(s.uploadWindowsInstallerFile))
	mux.HandleFunc("GET "+windowsInstallPath+"install.ps1", serveWindowsInstallScript)
	mux.HandleFunc("GET "+windowsInstallPath+"manifest", s.requireWindowsInstallKey(s.windowsInstallerManifest))
	mux.HandleFunc("GET "+windowsInstallPath+"files/{name}", s.requireWindowsInstallKey(s.downloadWindowsInstallerFile))
}

func windowsInstallOriginAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()))
}

func psLiteral(value string) string {
	return "'" + strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’").Replace(value) + "'"
}

func windowsInstallCommand(origin, key, fingerprint string) string {
	if !windowsInstallOriginAllowed(origin) {
		return ""
	}
	return strings.Join([]string{
		`& { $ErrorActionPreference = 'Stop'; $server = ` + psLiteral(origin) + `;`,
		`$tls = [Net.ServicePointManager]::SecurityProtocol; try {`,
		`if ($tls -ne 0) { [Net.ServicePointManager]::SecurityProtocol = $tls -bor [Net.SecurityProtocolType]::Tls12 };`,
		`$script = (Invoke-WebRequest -UseBasicParsing -MaximumRedirection 0 -TimeoutSec 30 -Uri ($server + '` + windowsInstallPath + `install.ps1')).RawContentStream.ToArray();`,
		`$sha = [Security.Cryptography.SHA256]::Create(); try { $hash = [BitConverter]::ToString($sha.ComputeHash($script)).Replace('-', '') } finally { $sha.Dispose() };`,
		fmt.Sprintf(`if ($hash -ne '%x') { throw 'GhostHand install script checksum mismatch. Copy a new command from the controller.' };`, sha256.Sum256(windowsInstallScript)),
		`& ([ScriptBlock]::Create([Text.Encoding]::UTF8.GetString($script))) -Server $server -NodeKey ` + psLiteral(key) + ` -Fingerprint ` + psLiteral(fingerprint),
		`} finally { [Net.ServicePointManager]::SecurityProtocol = $tls } }`,
	}, " ")
}

func (s *Server) windowsInstallerStatus(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	files, err := s.agents.WindowsInstallerFiles(false)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) uploadWindowsInstallerFile(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		fail(w, http.StatusUnsupportedMediaType, "请上传 Windows EXE 文件。")
		return
	}
	const maxSize = agents.MaxWindowsInstallerFileSize
	if r.ContentLength > maxSize {
		fail(w, http.StatusRequestEntityTooLarge, "单个 EXE 不能超过 256 MiB。")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
	body := http.MaxBytesReader(w, r.Body, maxSize)
	defer body.Close()
	file, err := s.agents.SaveWindowsInstallerFile(r.PathValue("name"), body)
	var sizeError *http.MaxBytesError
	switch {
	case errors.As(err, &sizeError):
		fail(w, http.StatusRequestEntityTooLarge, "单个 EXE 不能超过 256 MiB。")
	case errors.Is(err, agents.ErrInvalid):
		fail(w, http.StatusBadRequest, "请上传有效的 Windows x64 EXE 文件。")
	case err != nil:
		agentError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, file)
	}
}

func serveWindowsInstallScript(w http.ResponseWriter, r *http.Request) {
	if !windowsInstallOriginAllowed(requestOrigin(r)) {
		fail(w, http.StatusForbidden, "请通过 HTTPS 下载 Windows 安装脚本。")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="install.ps1"`)
	_, _ = w.Write(windowsInstallScript)
}

func (s *Server) requireWindowsInstallKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !windowsInstallOriginAllowed(requestOrigin(r)) {
			fail(w, http.StatusForbidden, "请通过 HTTPS 下载 Windows 安装文件。")
			return
		}
		expected := base64.StdEncoding.EncodeToString([]byte(s.agents.NodeKey()))
		if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(expected)) != 1 {
			fail(w, http.StatusUnauthorized, "设备接入密钥无效。")
			return
		}
		next(w, r)
	}
}

func (s *Server) windowsInstallerManifest(w http.ResponseWriter, r *http.Request) {
	files, err := s.agents.WindowsInstallerFiles(true)
	if errors.Is(err, agents.ErrMissing) {
		fail(w, http.StatusConflict, "请先上传全部三个 Windows 安装文件。")
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) downloadWindowsInstallerFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	file, info, err := s.agents.OpenWindowsInstallerFile(name)
	if err != nil {
		agentError(w, r, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	http.ServeContent(w, r, name, info.ModTime(), file)
}
