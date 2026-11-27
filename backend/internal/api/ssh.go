package api

import (
	"errors"
	"net/http"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
)

func (s *Server) sshRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agents/ssh", s.require(s.createSSHDevice))
	mux.HandleFunc("GET /api/devices/{device}/ssh", s.require(s.getSSHStatus))
	mux.HandleFunc("POST /api/devices/{device}/ssh/config", s.require(s.updateSSHConfig))
	mux.HandleFunc("POST /api/devices/{device}/ssh/reconnect", s.require(s.reconnectSSH))
	mux.HandleFunc("POST /api/devices/{device}/ssh/disconnect", s.require(s.disconnectSSH))
}

func (s *Server) createSSHDevice(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input agents.SSHCreateRequest
	if !readJSONLimited(w, r, &input, 64<<10) {
		return
	}
	result, err := s.agents.CreateSSHDevice(r.Context(), session.UserID, input)
	if errors.Is(err, agents.ErrInvalid) {
		fail(w, http.StatusBadRequest, "请检查 SSH 设备名称、主机地址、端口、用户名、认证凭据及所选宿主 Agent。")
		return
	}
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) getSSHStatus(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	status, err := s.agents.SSHStatus(r.Context(), r.PathValue("device"))
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) updateSSHConfig(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input agents.SSHUpdateRequest
	if !readJSONLimited(w, r, &input, 64<<10) {
		return
	}
	status, err := s.agents.UpdateSSHConfig(r.Context(), session.UserID, r.PathValue("device"), input)
	if errors.Is(err, agents.ErrInvalid) {
		fail(w, http.StatusBadRequest, "请检查 SSH 主机地址、端口、用户名、认证凭据及所选宿主 Agent。")
		return
	}
	if err != nil {
		agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) reconnectSSH(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if err := s.agents.ReconnectSSH(r.Context(), session.UserID, r.PathValue("device")); err != nil {
		if errors.Is(err, agents.ErrMissing) {
			agentError(w, r, err)
			return
		}
		fail(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) disconnectSSH(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if err := s.agents.DisconnectSSH(r.Context(), session.UserID, r.PathValue("device")); err != nil {
		agentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
