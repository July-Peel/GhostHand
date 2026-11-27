package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/brain"
	"ghosthand/backend/internal/settings"
)

type webDAVSettings struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type llmSettings struct {
	BaseURL             string `json:"base_url"`
	Model               string `json:"model"`
	APIKey              string `json:"api_key"`
	Instructions        string `json:"instructions"`
	DefaultInstructions string `json:"default_instructions"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	items, err := s.settings.Read(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	dav := items[settings.WebDAV]
	llm := items[settings.LLM]
	writeJSON(w, http.StatusOK, struct {
		WebDAV webDAVSettings `json:"webdav"`
		LLM    llmSettings    `json:"llm"`
	}{
		WebDAV: webDAVSettings{URL: dav.Endpoint, Username: dav.Label, Password: dav.Secret},
		LLM: llmSettings{
			BaseURL: llm.Endpoint, Model: llm.Label, APIKey: llm.Secret,
			Instructions: brain.ResolveInstructions(llm.Instructions), DefaultInstructions: brain.DefaultInstructions,
		},
	})
}

func (s *Server) saveWebDAV(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input struct {
		URL      string  `json:"url"`
		Username string  `json:"username"`
		Password *string `json:"password"`
	}
	if readJSON(w, r, &input) {
		s.saveSettings(w, r, session.UserID, settings.WebDAV, settings.Update{
			Endpoint: input.URL, Label: input.Username, Secret: input.Password,
		})
	}
}

func (s *Server) saveLLM(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input struct {
		BaseURL      string  `json:"base_url"`
		Model        string  `json:"model"`
		APIKey       *string `json:"api_key"`
		Instructions *string `json:"instructions"`
	}
	if readJSONLimited(w, r, &input, 256<<10) {
		s.saveSettings(w, r, session.UserID, settings.LLM, settings.Update{
			Endpoint: input.BaseURL, Label: input.Model, Secret: input.APIKey, Instructions: input.Instructions,
		})
	}
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	var input struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	models, err := brain.ListModels(r.Context(), input.BaseURL, input.APIKey)
	switch {
	case errors.Is(err, brain.ErrModelURL), errors.Is(err, brain.ErrAPIKey):
		fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		fail(w, http.StatusGatewayTimeout, "获取模型列表超时。")
	case errors.Is(err, context.Canceled):
		return
	case err != nil:
		fail(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string][]string{"models": models})
	}
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request, userID int64, kind settings.Kind, input settings.Update) {
	err := s.settings.Save(r.Context(), userID, kind, input)
	var validation settings.ValidationError
	var setup settings.WebDAVSetupError
	switch {
	case errors.As(err, &validation):
		fail(w, http.StatusBadRequest, validation.Error())
	case errors.As(err, &setup):
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			fail(w, http.StatusGatewayTimeout, "准备 WebDAV 目录超时。")
		case !errors.Is(err, context.Canceled):
			fail(w, http.StatusBadGateway, setup.Error())
		}
	case err != nil:
		internalError(w, r, err)
	default:
		if kind == settings.LLM {
			s.workbench.MarkRefresh()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, session auth.Session, token string) {
	var input struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	err := s.auth.ChangePassword(r.Context(), session, token, input.CurrentPassword, input.NewPassword)
	switch {
	case errors.Is(err, auth.ErrPasswordPolicy):
		fail(w, http.StatusBadRequest, "新密码至少 12 个字符，且不能超过 256 字节。")
	case errors.Is(err, auth.ErrSamePassword):
		fail(w, http.StatusBadRequest, "新密码须与当前密码不同。")
	case errors.Is(err, auth.ErrCredentials):
		fail(w, http.StatusBadRequest, "当前密码错误。")
	case errors.Is(err, auth.ErrRateLimit):
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "密码验证尝试过于频繁，请稍后再试。")
	case errors.Is(err, auth.ErrSession):
		fail(w, http.StatusUnauthorized, "登录会话已过期。")
	case err != nil:
		internalError(w, r, err)
	default:
		s.setCookie(w, r, "", time.Unix(1, 0), -1)
		w.WriteHeader(http.StatusNoContent)
	}
}
