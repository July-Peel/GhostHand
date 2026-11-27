package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
	"ghosthand/proto"
)

func (s *Server) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pairing", s.require(s.getPairing))
	mux.HandleFunc("POST /api/pairing/rotate", s.require(s.rotatePairingKey))
	s.windowsInstallerRoutes(mux)
	mux.HandleFunc("POST /api/agent/pair", s.redeemPairing)
	mux.HandleFunc("GET "+proto.AgentSocketPath, s.agents.Connect)
	mux.HandleFunc("GET /api/devices/{device}/connection", s.require(s.connectionHistory))
	mux.HandleFunc("POST /api/devices/{device}/rename", s.require(s.renameDevice))
	mux.HandleFunc("GET /api/devices/{device}/browser-extension", s.require(s.getBrowserExtension))
	mux.HandleFunc("POST /api/devices/{device}/browser-extension", s.require(s.changeBrowserExtension))
	mux.HandleFunc("GET /api/devices/{device}/rdp", s.require(s.rdpStatus))
	mux.HandleFunc("POST /api/devices/{device}/rdp/config", s.require(s.rdpConfig))
	mux.HandleFunc("POST /api/devices/{device}/rdp/preflight", s.require(s.rdpOperation(proto.RDPPreflight)))
	mux.HandleFunc("POST /api/devices/{device}/rdp/unlock", s.require(s.rdpOperation(proto.RDPUnlock)))
	mux.HandleFunc("POST /api/devices/{device}/rdp/lock", s.require(s.rdpOperation(proto.RDPLock)))
	mux.HandleFunc("POST /api/agent/rdp/{operation}/credential", s.rdpCredential)
	mux.HandleFunc("POST /api/devices/{device}/operations", s.require(s.queueOperation))
	mux.HandleFunc("POST /api/devices/{device}/revoke", s.require(s.revokeDevice))
	mux.HandleFunc("POST /api/devices/{device}/restore", s.require(s.restoreDevice))
	mux.HandleFunc("POST /api/devices/{device}/delete", s.require(s.deleteDevice))
	mux.HandleFunc("GET /api/devices/{device}/logs/{operation}", s.require(s.readLog))
	mux.HandleFunc("GET /api/releases", s.require(s.listReleases))
	mux.HandleFunc("POST /api/releases", s.require(s.createRelease))
	mux.HandleFunc("POST /api/releases/{release}/rollout", s.require(s.rollout))
	mux.HandleFunc("GET /api/agent/releases/{release}", s.downloadRelease)
	mux.HandleFunc("POST /api/agent/logs/{operation}/ticket", s.logTicket)
	mux.HandleFunc("POST /api/agent/logs/{operation}/upload", s.uploadLog)
}

func (s *Server) renameDevice(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	id := r.PathValue("device")
	name, err := s.agents.RenameDevice(r.Context(), session.UserID, id, input.Name, s.workbench.DemoEnabled())
	if errors.Is(err, agents.ErrInvalid) {
		fail(w, http.StatusBadRequest, "设备名称须为 1 至 80 个字符，且不能包含换行或控制字符。")
		return
	}
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "name": name})
}

func (s *Server) pairingResponse(w http.ResponseWriter, r *http.Request, key string) {
	origin := s.origin
	if origin == "" {
		origin = requestOrigin(r)
	}
	fingerprint := ""
	if strings.HasPrefix(origin, "https://") {
		fingerprint = s.fingerprint
	}
	details := map[string]string{"server": origin, "key": key, "fingerprint": fingerprint}
	data, err := json.Marshal(details)
	if err != nil {
		internalError(w, r, err)
		return
	}
	png, err := qrcode.Encode(string(data), qrcode.Medium, 240)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server": origin, "key": key, "fingerprint": fingerprint,
		"managed_by_env":          s.agents.NodeKeyManagedByEnv(),
		"qr_png":                  base64.StdEncoding.EncodeToString(png),
		"windows_install_command": windowsInstallCommand(origin, key, fingerprint),
	})
}

func (s *Server) getPairing(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	s.pairingResponse(w, r, s.agents.NodeKey())
}

func (s *Server) rotatePairingKey(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	key, err := s.agents.RotateNodeKey(r.Context(), session.UserID)
	if err != nil {
		agentError(w, r, err)
		return
	}
	s.pairingResponse(w, r, key)
}

func (s *Server) redeemPairing(w http.ResponseWriter, r *http.Request) {
	var input proto.Enrollment
	if !readJSON(w, r, &input) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	result, err := s.agents.Enroll(r.Context(), input, ip)
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) connectionHistory(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	result, err := s.agents.History(r.Context(), r.PathValue("device"))
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) queueOperation(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input agents.OperationRequest
	if !readJSON(w, r, &input) {
		return
	}
	id, err := s.agents.Queue(r.Context(), session.UserID, r.PathValue("device"), input)
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if err := s.agents.Revoke(r.Context(), session.UserID, r.PathValue("device")); err != nil {
		agentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restoreDevice(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if err := s.agents.Restore(r.Context(), session.UserID, r.PathValue("device")); err != nil {
		agentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	id := r.PathValue("device")
	localFiles, err := s.agents.DeleteDevice(r.Context(), session.UserID, id)
	if err != nil {
		agentError(w, r, err)
		return
	}
	s.workbench.CleanupDeviceState(r.Context(), id, localFiles)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	result, err := s.agents.Releases(r.Context())
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createRelease(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if r.Header.Get("Content-Type") != "application/zip" {
		fail(w, http.StatusUnsupportedMediaType, "请上传 ZIP 版本包。")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
	body := http.MaxBytesReader(w, r.Body, proto.MaxReleaseSize+1)
	defer body.Close()
	query := r.URL.Query()
	release, err := s.agents.SaveRelease(r.Context(), session.UserID, query.Get("version"), query.Get("component"), query.Get("sha256"), body)
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, release)
}

func (s *Server) rollout(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var scope agents.Rollout
	if !readJSON(w, r, &scope) {
		return
	}
	count, err := s.agents.Rollout(r.Context(), session.UserID, r.PathValue("release"), scope)
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"count": count})
}

func bearer(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return token
}

func (s *Server) downloadRelease(w http.ResponseWriter, r *http.Request) {
	path, err := s.agents.PackagePath(r.Context(), r.Header.Get("X-GhostHand-Device"), bearer(r), r.PathValue("release"))
	if err != nil {
		agentError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	http.ServeFile(w, r, path)
}

func (s *Server) logTicket(w http.ResponseWriter, r *http.Request) {
	ticket, err := s.agents.IssueLogTicket(r.Context(), r.Header.Get("X-GhostHand-Device"), bearer(r), r.PathValue("operation"))
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}

func (s *Server) uploadLog(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(90 * time.Second))
	body := http.MaxBytesReader(w, r.Body, proto.MaxLogSize+1)
	defer body.Close()
	if err := s.agents.SaveLog(r.Context(), r.PathValue("operation"), bearer(r), body); err != nil {
		agentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readLog(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	id := r.PathValue("operation")
	path, err := s.agents.LogPath(r.Context(), r.PathValue("device"), id)
	if err != nil {
		agentError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=ghosthand-"+id+".jsonl.gz")
	http.ServeFile(w, r, path)
}

func agentError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, agents.ErrDenied):
		fail(w, http.StatusUnauthorized, "设备凭证或配对码无效。")
	case errors.Is(err, agents.ErrLimited):
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "配对操作过于频繁，请稍后重试。")
	case errors.Is(err, agents.ErrMissing):
		fail(w, http.StatusNotFound, "设备、版本或操作不存在。")
	case errors.Is(err, agents.ErrUninstallUnsupported):
		fail(w, http.StatusConflict, "此设备尚未上报远程卸载能力，请先更新 Windows Core。")
	case errors.Is(err, agents.ErrDeviceOnline):
		fail(w, http.StatusConflict, "设备当前仍在线，请先断开连接或吊销凭证后再删除。")
	case errors.Is(err, agents.ErrConflict):
		fail(w, http.StatusConflict, "相同版本已存在，或目标设备已有冲突的待处理操作。")
	case errors.As(err, &tooLarge):
		fail(w, http.StatusRequestEntityTooLarge, "文件超出大小限制。")
	case errors.Is(err, agents.ErrInvalid):
		fail(w, http.StatusBadRequest, "请求参数、版本包内容或 SHA-256 无效。")
	default:
		internalError(w, r, err)
	}
}
