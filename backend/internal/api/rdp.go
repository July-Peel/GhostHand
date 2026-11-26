package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
	"ghosthand/proto"
)

func (s *Server) rdpStatus(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	status, err := s.unlock.Status(r.Context(), r.PathValue("device"))
	if err != nil {
		desktopError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) rdpConfig(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if !httpsRequest(r) {
		fail(w, http.StatusForbidden, "请通过 HTTPS 配置 Windows 解锁凭据。")
		return
	}
	var input agents.RDPConfig
	if !readJSON(w, r, &input) {
		return
	}
	if err := s.unlock.Save(r.Context(), session.UserID, r.PathValue("device"), input); err != nil {
		if errors.Is(err, agents.ErrInvalid) {
			fail(w, http.StatusBadRequest, "请检查 Windows 用户名、域、密码和 RDP 端口。")
		} else {
			rdpError(w, r, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rdpOperation(kind proto.MessageType) sessionHandler {
	return func(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
		var input struct {
			Confirm bool `json:"confirm_suspended"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		result, err := s.unlock.Operate(r.Context(), session.UserID, r.PathValue("device"), kind, "manual", input.Confirm)
		if err != nil {
			rdpError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func (s *Server) rdpCredential(w http.ResponseWriter, r *http.Request) {
	if !httpsRequest(r) || len(r.Header.Values("Origin")) != 0 {
		fail(w, http.StatusForbidden, "此接口仅接受节点的 HTTPS 请求。")
		return
	}
	credential, err := s.unlock.Redeem(r.Context(), r.Header.Get("X-GhostHand-Device"),
		r.Header.Get("X-GhostHand-Token"), r.PathValue("operation"), bearer(r))
	switch {
	case errors.Is(err, agents.ErrRDPTicket):
		writeJSON(w, http.StatusGone, map[string]string{"code": string(proto.ErrRDPTicketExpired)})
		return
	case errors.Is(err, agents.ErrDenied):
		fail(w, http.StatusForbidden, "设备凭据无效。")
		return
	case err != nil:
		internalError(w, r, err)
		return
	}
	defer clear(credential.Password)
	identity, _ := json.Marshal(map[string]string{"username": credential.Username, "domain": credential.Domain})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(identity[:len(identity)-1])
	_, _ = io.WriteString(w, `,"password":"`)
	if len(credential.Password) > 0 {
		encoded := base64.NewEncoder(base64.StdEncoding, w)
		_, _ = encoded.Write(credential.Password)
		_ = encoded.Close()
	}
	_, _ = io.WriteString(w, `"}`)
}

func rdpError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agents.ErrRDPNoCredential), errors.Is(err, agents.ErrRDPSuspended), errors.Is(err, agents.ErrRDPTLS):
		fail(w, http.StatusConflict, err.Error())
	default:
		desktopError(w, r, err)
	}
}
