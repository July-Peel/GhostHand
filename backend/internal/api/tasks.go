package api

import (
	"errors"
	"io"
	"mime"
	"net/http"

	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

func (s *Server) taskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/devices/{device}/tasks/{task}/{action}", s.require(s.taskControl))
	mux.HandleFunc("POST /api/devices/{device}/confirmations/{confirmation}/decision", s.require(s.confirmDecision))
	mux.HandleFunc("POST /api/devices/{device}/policy", s.require(s.devicePolicy))
	mux.HandleFunc("POST /api/upload", s.uploadScreenshot)
	mux.HandleFunc("GET /api/attachments/{id}/content", s.require(s.attachment))
	mux.HandleFunc("GET /api/attachments/{id}/thumb", s.require(s.attachment))
}

func (s *Server) devicePolicy(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input struct {
		AutoApprove *bool `json:"auto_approve"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.AutoApprove == nil {
		fail(w, http.StatusBadRequest, "请指定免确认策略开关状态。")
		return
	}
	if err := s.workbench.SetAutoApprove(r.Context(), session.UserID, r.PathValue("device"), *input.AutoApprove); err != nil {
		workbenchError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) confirmDecision(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input struct {
		Approve *bool `json:"approve"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Approve == nil {
		fail(w, http.StatusBadRequest, "请选择确认或拒绝。")
		return
	}
	if err := s.workbench.Decide(r.Context(), session.UserID, r.PathValue("device"), r.PathValue("confirmation"), *input.Approve); err != nil {
		workbenchError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) taskControl(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if err := s.workbench.Control(r.Context(), session.UserID, r.PathValue("device"), r.PathValue("task"), r.PathValue("action")); err != nil {
		workbenchError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) uploadScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "image/jpeg" {
		fail(w, http.StatusUnsupportedMediaType, "仅接受 JPEG 截图。")
		return
	}
	if r.ContentLength > proto.MaxScreenshotSize {
		fail(w, http.StatusRequestEntityTooLarge, "截图超出大小限制。")
		return
	}
	body := http.MaxBytesReader(w, r.Body, proto.MaxScreenshotSize)
	defer body.Close()
	result, err := s.workbench.SaveScreenshot(r.Context(), r.Header.Get("X-GhostHand-Task"),
		r.Header.Get("X-GhostHand-Upload"), bearer(r), body)
	var oversized *http.MaxBytesError
	switch {
	case errors.Is(err, workbench.ErrUploadDenied):
		fail(w, http.StatusUnauthorized, "截图上传票据无效或已使用。")
	case errors.As(err, &oversized):
		fail(w, http.StatusRequestEntityTooLarge, "截图超出大小限制。")
	case errors.Is(err, workbench.ErrUploadData):
		fail(w, http.StatusBadRequest, "截图格式或尺寸无效。")
	case err != nil:
		internalError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, result)
	}
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	thumb := r.Pattern == "GET /api/attachments/{id}/thumb"
	body, name, err := s.workbench.Attachment(r.Context(), r.PathValue("id"), thumb)
	if err != nil {
		workbenchError(w, r, err)
		return
	}
	defer body.Close()
	disposition := "inline"
	if !thumb && r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = io.Copy(w, io.LimitReader(body, proto.MaxScreenshotSize))
}
