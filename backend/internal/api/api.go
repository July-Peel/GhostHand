package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/db"
	"ghosthand/backend/internal/settings"
	"ghosthand/backend/internal/workbench"
)

type Server struct {
	store       *db.Store
	auth        *auth.Service
	settings    *settings.Service
	workbench   *workbench.Service
	origin      string
	web         fs.FS
	agents      *agents.Service
	unlock      *agents.UnlockManager
	fingerprint string
}

func New(store *db.Store, authentication *auth.Service, config *settings.Service, desk *workbench.Service, fleet *agents.Service, unlock *agents.UnlockManager, origin, fingerprint string, web fs.FS) http.Handler {
	s := &Server{store: store, auth: authentication, settings: config, workbench: desk, origin: origin, web: web, agents: fleet, unlock: unlock, fingerprint: fingerprint}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/session", s.require(s.session))
	mux.HandleFunc("POST /api/logout", s.require(s.logout))
	mux.HandleFunc("GET /api/settings", s.require(s.getSettings))
	mux.HandleFunc("POST /api/settings/password", s.require(s.changePassword))
	mux.HandleFunc("POST /api/settings/webdav", s.require(s.saveWebDAV))
	mux.HandleFunc("POST /api/settings/llm", s.require(s.saveLLM))
	mux.HandleFunc("POST /api/settings/llm/models", s.require(s.listModels))
	mux.HandleFunc("GET /api/devices", s.require(s.devices))
	mux.HandleFunc("GET /api/devices/{device}/messages", s.require(s.messages))
	mux.HandleFunc("POST /api/devices/{device}/messages", s.require(s.send))
	mux.HandleFunc("POST /api/devices/{device}/messages/clear", s.require(s.clearMessages))
	mux.HandleFunc("GET /api/events", s.require(s.events))
	s.agentRoutes(mux)
	s.taskRoutes(mux)
	s.mcpRoutes(mux)
	mux.HandleFunc("GET /", s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
}

type sessionHandler func(http.ResponseWriter, *http.Request, auth.Session, string)

func (s *Server) require(next sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName(httpsRequest(r)))
		if err != nil {
			fail(w, http.StatusUnauthorized, "请先登录。")
			return
		}
		session, err := s.auth.Session(r.Context(), cookie.Value)
		if errors.Is(err, auth.ErrSession) {
			fail(w, http.StatusUnauthorized, "登录会话已过期。")
			return
		}
		if err != nil {
			internalError(w, r, err)
			return
		}
		if r.Method == http.MethodPost && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(auth.CSRF(cookie.Value))) != 1 {
			fail(w, http.StatusForbidden, "请求校验失败，请刷新页面。")
			return
		}
		next(w, r, session, cookie.Value)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	var value int
	if err := s.store.QueryRowContext(r.Context(), "SELECT 1 FROM users LIMIT 1").Scan(&value); err != nil {
		fail(w, http.StatusServiceUnavailable, "数据库未就绪。")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" || utf8.RuneCountInString(input.Username) > 64 || input.Password == "" || len(input.Password) > 256 {
		fail(w, http.StatusBadRequest, "请填写有效的用户名和密码。")
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	agent := r.UserAgent()
	if len(agent) > 512 {
		agent = agent[:512]
	}
	session, token, err := s.auth.Login(r.Context(), input.Username, input.Password, ip, agent)
	switch {
	case errors.Is(err, auth.ErrCredentials):
		fail(w, http.StatusUnauthorized, "用户名或密码错误。")
	case errors.Is(err, auth.ErrRateLimit):
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后再试。")
	case err != nil:
		internalError(w, r, err)
	default:
		s.setCookie(w, r, token, session.ExpiresAt, int(auth.SessionTTL.Seconds()))
		s.session(w, r, session, token)
	}
}

func (s *Server) session(w http.ResponseWriter, _ *http.Request, session auth.Session, token string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"username": session.Username, "csrf_token": auth.CSRF(token), "expires_at": session.ExpiresAt.UnixMilli(),
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, session auth.Session, token string) {
	if err := s.auth.Logout(r.Context(), session, token); err != nil {
		internalError(w, r, err)
		return
	}
	s.setCookie(w, r, "", time.Unix(1, 0), -1)
	w.WriteHeader(http.StatusNoContent)
}

func httpsRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func requestOrigin(r *http.Request) string {
	if httpsRequest(r) {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

func cookieName(secure bool) string {
	if secure {
		return "__Host-ghosthand"
	}
	return "ghosthand"
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time, maxAge int) {
	secure := httpsRequest(r)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(secure), Value: token, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge,
	})
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	devices, err := s.workbench.Devices(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	freeChat, err := s.workbench.FreeChatStatus(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices, "commands": workbench.Commands(), "free_chat": freeChat})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	var before int64
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		var err error
		before, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || before <= 0 {
			fail(w, http.StatusBadRequest, "消息游标无效。")
			return
		}
	}
	page, err := s.workbench.Messages(r.Context(), r.PathValue("device"), before)
	if err != nil {
		workbenchError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var text string
	var images []workbench.UploadImage
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType == "multipart/form-data" {
		var ok bool
		text, images, ok = readChatForm(w, r)
		if !ok {
			return
		}
	} else {
		var input struct {
			Text string `json:"text"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		text = strings.TrimSpace(input.Text)
		if text == "" || utf8.RuneCountInString(text) > 2000 {
			fail(w, http.StatusBadRequest, "消息长度须为 1–2000 个字符。")
			return
		}
	}
	if err := s.workbench.Send(r.Context(), session.UserID, r.PathValue("device"), text, images); err != nil {
		workbenchError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readChatForm(w http.ResponseWriter, r *http.Request) (string, []workbench.UploadImage, bool) {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(90 * time.Second))
	r.Body = http.MaxBytesReader(w, r.Body, workbench.MaxChatImages*workbench.MaxChatImageSize+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			fail(w, http.StatusRequestEntityTooLarge, "请求过大。")
		} else {
			fail(w, http.StatusBadRequest, "无法读取图片上传表单。")
		}
		return "", nil, false
	}
	defer r.MultipartForm.RemoveAll()
	text := strings.TrimSpace(r.FormValue("text"))
	files := r.MultipartForm.File["images"]
	switch {
	case len(files) > workbench.MaxChatImages:
		workbenchError(w, r, workbench.ErrChatImageCount)
		return "", nil, false
	case text == "" && len(files) == 0:
		fail(w, http.StatusBadRequest, "请输入消息内容或添加图片。")
		return "", nil, false
	case utf8.RuneCountInString(text) > 2000:
		fail(w, http.StatusBadRequest, "消息长度须为 1–2000 个字符。")
		return "", nil, false
	}
	images := make([]workbench.UploadImage, 0, len(files))
	for _, header := range files {
		if header.Size > workbench.MaxChatImageSize {
			workbenchError(w, r, workbench.ErrChatImageSize)
			return "", nil, false
		}
		file, err := header.Open()
		if err != nil {
			internalError(w, r, err)
			return "", nil, false
		}
		data, err := io.ReadAll(io.LimitReader(file, workbench.MaxChatImageSize+1))
		_ = file.Close()
		if err != nil {
			internalError(w, r, err)
			return "", nil, false
		}
		images = append(images, workbench.UploadImage{Name: header.Filename, Data: data})
	}
	return text, images, true
}

func (s *Server) clearMessages(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	if err := s.workbench.ClearMessages(r.Context(), session.UserID, r.PathValue("device")); err != nil {
		workbenchError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	pwa := path == "manifest.webmanifest" || path == "sw.js" || path == "icon.svg"
	if path != "" && !pwa && !strings.HasPrefix(path, "assets/") {
		http.NotFound(w, r)
		return
	}
	name := path
	if name == "" {
		name = "index.html"
	}
	info, err := fs.Stat(s.web, name)
	if err != nil || info.IsDir() {
		if path == "" {
			fail(w, http.StatusServiceUnavailable, "Web 产物尚未嵌入，请使用 Compose 构建。")
			return
		}
		http.NotFound(w, r)
		return
	}
	if path == "" || pwa {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if path == "manifest.webmanifest" {
		w.Header().Set("Content-Type", "application/manifest+json")
	}
	http.FileServerFS(s.web).ServeHTTP(w, r)
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	return readJSONLimited(w, r, target, 16<<10)
}

func readJSONLimited(w http.ResponseWriter, r *http.Request, target any, limit int64) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "请求内容必须为 JSON。")
		return false
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(target)
	if err == nil {
		err = decoder.Decode(new(json.RawMessage))
		if err == io.EOF {
			return true
		}
	}
	var oversized *http.MaxBytesError
	if errors.As(err, &oversized) {
		fail(w, http.StatusRequestEntityTooLarge, "请求过大。")
		return false
	}
	fail(w, http.StatusBadRequest, "请求内容无效。")
	return false
}

func workbenchError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, workbench.ErrDeviceNotFound):
		fail(w, http.StatusNotFound, "设备不存在。")
	case errors.Is(err, workbench.ErrTaskNotFound):
		fail(w, http.StatusNotFound, "任务不存在。")
	case errors.Is(err, workbench.ErrTaskControl):
		fail(w, http.StatusConflict, "任务当前无法执行此操作。")
	case errors.Is(err, workbench.ErrFreeChatDisabled), errors.Is(err, workbench.ErrFreeChatBusy):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, workbench.ErrChatImagesExpired):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, workbench.ErrUnknownCommand):
		fail(w, http.StatusBadRequest, "未知的快捷指令。")
	case errors.Is(err, workbench.ErrChatImageCount), errors.Is(err, workbench.ErrChatImageSize),
		errors.Is(err, workbench.ErrChatImageData), errors.Is(err, workbench.ErrCommandImages):
		fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, workbench.ErrUnsupported):
		fail(w, http.StatusConflict, "此设备不支持该快捷指令。")
	default:
		internalError(w, r, err)
	}
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		return
	}
	slog.Error("request failed", "method", r.Method, "route", r.Pattern, "error", err)
	fail(w, http.StatusInternalServerError, "服务暂时无法处理此请求。")
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
