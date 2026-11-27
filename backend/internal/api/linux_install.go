package api

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
)

const linuxInstallPath = "/api/agent/install/linux/"

//go:embed install_linux.sh
var linuxInstallScript []byte

func (s *Server) linuxInstallerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pairing/linux/files", s.require(s.linuxInstallerStatus))
	mux.HandleFunc("POST /api/pairing/linux/files/{name}", s.require(s.uploadLinuxInstallerFile))
	mux.HandleFunc("GET "+linuxInstallPath+"install.sh", serveLinuxInstallScript)
	mux.HandleFunc("GET "+linuxInstallPath+"manifest", s.requireLinuxInstallKey(s.linuxInstallerManifest))
	mux.HandleFunc("GET "+linuxInstallPath+"files/{name}", s.requireLinuxInstallKey(s.downloadLinuxInstallerFile))
}

func shLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func linuxInstallCommand(origin, key, fingerprint string) string {
	if !windowsInstallOriginAllowed(origin) {
		return ""
	}
	sum := sha256.Sum256(linuxInstallScript)
	return fmt.Sprintf(
		`sh -c 'set -eu; SERVER=%s; KEY=%s; FP=%s; TMP="$(mktemp)"; trap "rm -f \"$TMP\"" EXIT; if command -v curl >/dev/null 2>&1; then curl -fsSL --max-redirs 0 --connect-timeout 15 --max-time 60 "$SERVER%sinstall.sh" -o "$TMP"; elif command -v wget >/dev/null 2>&1; then wget -q -T 30 -O "$TMP" "$SERVER%sinstall.sh"; else echo "Requires curl or wget" >&2; exit 1; fi; echo "%x  $TMP" | sha256sum -c - >/dev/null; sh "$TMP" "$SERVER" "$KEY" "$FP"'`,
		shLiteral(origin), shLiteral(key), shLiteral(fingerprint), linuxInstallPath, linuxInstallPath, sum,
	)
}

func (s *Server) linuxInstallerStatus(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	files, err := s.agents.LinuxInstallerFiles(false)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) uploadLinuxInstallerFile(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		fail(w, http.StatusUnsupportedMediaType, "请上传有效的 Linux 64 位 ELF 可执行文件 (ghosthand-core)。")
		return
	}
	const maxSize = agents.MaxLinuxInstallerFileSize
	if r.ContentLength > maxSize {
		fail(w, http.StatusRequestEntityTooLarge, "单个二进制文件不能超过 256 MiB。")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
	body := http.MaxBytesReader(w, r.Body, maxSize)
	defer body.Close()
	file, err := s.agents.SaveLinuxInstallerFile(r.PathValue("name"), body)
	var sizeError *http.MaxBytesError
	switch {
	case errors.As(err, &sizeError):
		fail(w, http.StatusRequestEntityTooLarge, "单个二进制文件不能超过 256 MiB。")
	case errors.Is(err, agents.ErrInvalid):
		fail(w, http.StatusBadRequest, "请上传有效的 Linux 64 位 ELF 可执行文件 (ghosthand-core)。")
	case err != nil:
		agentError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, file)
	}
}

func serveLinuxInstallScript(w http.ResponseWriter, r *http.Request) {
	if !windowsInstallOriginAllowed(requestOrigin(r)) {
		fail(w, http.StatusForbidden, "请通过 HTTPS 下载 Linux 安装脚本。")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="install.sh"`)
	_, _ = w.Write(linuxInstallScript)
}

func (s *Server) requireLinuxInstallKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !windowsInstallOriginAllowed(requestOrigin(r)) {
			fail(w, http.StatusForbidden, "请通过 HTTPS 下载 Linux 安装文件。")
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

func (s *Server) linuxInstallerManifest(w http.ResponseWriter, r *http.Request) {
	files, err := s.agents.LinuxInstallerFiles(true)
	if errors.Is(err, agents.ErrMissing) {
		fail(w, http.StatusConflict, "请先上传 Linux 安装文件 ghosthand-core。")
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) downloadLinuxInstallerFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	file, info, err := s.agents.OpenLinuxInstallerFile(name)
	if err != nil {
		agentError(w, r, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	http.ServeContent(w, r, name, info.ModTime(), file)
}
