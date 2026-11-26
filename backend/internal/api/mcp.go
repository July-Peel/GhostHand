package api

import (
	"errors"
	"net/http"

	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/mcpserver"
)

func (s *Server) mcpRoutes(mux *http.ServeMux) {
	gateway := mcpserver.New(s.workbench)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		mux.HandleFunc(method+" /api/mcp", s.requireMCP(gateway.HTTP))
	}
	mux.HandleFunc("GET /api/mcp/sse", s.requireMCP(gateway.SSE))
	mux.HandleFunc("POST /api/mcp/message", s.requireMCP(gateway.SSE))
	mux.HandleFunc("GET /api/settings/mcp", s.require(s.mcpTokenStatus))
	mux.HandleFunc("POST /api/settings/mcp", s.require(s.changeMCPToken))
}

func (s *Server) requireMCP(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		allowedOrigin := s.origin
		if allowedOrigin == "" {
			allowedOrigin = requestOrigin(r)
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != allowedOrigin {
			fail(w, http.StatusForbidden, "MCP 请求来源无效。")
			return
		}
		if r.Header.Get("Authorization") == "" {
			if _, err := r.Cookie(cookieName(httpsRequest(r))); err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="GhostHand MCP"`)
			}
			s.require(func(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
				next.ServeHTTP(w, mcpserver.WithUser(r, session.UserID))
			})(w, r)
			return
		}
		user, err := s.auth.MCPUser(r.Context(), bearer(r))
		switch {
		case errors.Is(err, auth.ErrSession):
			w.Header().Set("WWW-Authenticate", `Bearer realm="GhostHand MCP"`)
			fail(w, http.StatusUnauthorized, "MCP 访问令牌无效。")
		case err != nil:
			internalError(w, r, err)
		default:
			next.ServeHTTP(w, mcpserver.WithUser(r, user))
		}
	}
}

func (s *Server) mcpTokenStatus(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	created, err := s.auth.MCPTokenStatus(r.Context(), session.UserID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"created_at": created})
}

func (s *Server) changeMCPToken(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input struct {
		Action string `json:"action"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	switch input.Action {
	case "issue":
		token, created, err := s.auth.IssueMCPToken(r.Context(), session.UserID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"token": token, "created_at": created})
	case "revoke":
		if err := s.auth.RevokeMCPToken(r.Context(), session.UserID); err != nil {
			internalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"created_at": 0})
	default:
		fail(w, http.StatusBadRequest, "MCP 令牌操作无效。")
	}
}
