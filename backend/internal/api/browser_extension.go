package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"ghosthand/backend/internal/agents"
	"ghosthand/backend/internal/auth"
	"ghosthand/backend/internal/workbench"
	"ghosthand/proto"
)

func (s *Server) browserExtensionDevice(ctx context.Context, id string) (string, string, error) {
	var desktop, helper string
	err := s.store.QueryRowContext(ctx, `SELECT desktop_state,helper_state FROM devices
		WHERE id=? AND platform='windows' AND is_demo=0`, id).Scan(&desktop, &helper)
	if errors.Is(err, sql.ErrNoRows) {
		err = agents.ErrMissing
	}
	return desktop, helper, err
}

func desktopError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agents.ErrDesktopOffline):
		fail(w, http.StatusConflict, "设备当前离线。")
	case errors.Is(err, agents.ErrDesktopUnsupported), errors.Is(err, workbench.ErrUnsupported):
		fail(w, http.StatusConflict, "设备无法执行此操作，请确认 Windows Core 已更新并重新接入。")
	case errors.Is(err, workbench.ErrTaskControl):
		fail(w, http.StatusConflict, "设备当前正在执行其他任务，请稍后再试。")
	case errors.Is(err, agents.ErrConflict):
		fail(w, http.StatusConflict, "设备当前已有进行中的桌面操作。")
	case errors.Is(err, agents.ErrBrowserStatus):
		fail(w, http.StatusBadGateway, "设备无法读取 Chrome 扩展状态，请稍后刷新。")
	case errors.Is(err, context.DeadlineExceeded):
		fail(w, http.StatusGatewayTimeout, "等待设备操作超时，请刷新状态。")
	default:
		agentError(w, r, err)
	}
}

func (s *Server) getBrowserExtension(w http.ResponseWriter, r *http.Request, _ auth.Session, _ string) {
	if browser := r.URL.Query().Get("browser"); browser != "" && browser != "chrome" {
		fail(w, http.StatusBadRequest, "仅支持 Chrome。")
		return
	}
	device := r.PathValue("device")
	if _, _, err := s.browserExtensionDevice(r.Context(), device); err != nil {
		desktopError(w, r, err)
		return
	}
	status, err := s.agents.BrowserExtensionStatus(r.Context(), device)
	if err != nil {
		desktopError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) changeBrowserExtension(w http.ResponseWriter, r *http.Request, session auth.Session, _ string) {
	var input struct {
		Browser string `json:"browser"`
		Action  string `json:"action"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Browser != "chrome" || (input.Action != "install" && input.Action != "uninstall") {
		fail(w, http.StatusBadRequest, "请选择 Chrome 插件的安装或卸载操作。")
		return
	}
	ctx, device := r.Context(), r.PathValue("device")
	if _, _, err := s.browserExtensionDevice(ctx, device); err != nil {
		desktopError(w, r, err)
		return
	}
	status, err := s.agents.BrowserExtensionStatus(ctx, device)
	if err != nil {
		desktopError(w, r, err)
		return
	}
	type response struct {
		proto.BrowserExtensionStatus
		Skipped bool   `json:"skipped,omitempty"`
		Message string `json:"message"`
	}
	install := input.Action == "install"
	if install && (status.Connected || status.Installed) {
		writeJSON(w, http.StatusOK, response{status, true, "该设备已安装 GhostHand Chrome 插件，无需重复安装。"})
		return
	}
	if !install && !status.Connected && !status.Installed && !status.Registered && !status.BundlePresent {
		writeJSON(w, http.StatusOK, response{status, true, "该设备未安装 Chrome 插件，无需卸载。"})
		return
	}
	if install {
		desktop, helper, err := s.browserExtensionDevice(ctx, device)
		if err != nil {
			desktopError(w, r, err)
			return
		}
		if desktop != "unlocked" || helper != "ready" {
			message := "Windows 执行器尚未就绪。"
			if desktop != "unlocked" {
				message = "设备当前未解锁，请先解锁 Windows 桌面后再安装 Chrome 插件。"
			}
			fail(w, http.StatusConflict, message)
			return
		}
	}
	timeout := 20 * time.Second
	if install {
		timeout = 35 * time.Second
	}
	task, cancel := context.WithTimeout(ctx, timeout)
	result, err := s.workbench.CallBrowserExtension(task, session.UserID, device, input.Action)
	cancel()
	if err != nil {
		desktopError(w, r, err)
		return
	}
	if result.Error != "" {
		if result.Error == string(proto.ErrDesktopLocked) {
			result.Error = "设备当前未解锁，请先解锁 Windows 桌面后再安装 Chrome 插件。"
		}
		fail(w, http.StatusConflict, result.Error)
		return
	}
	if json.Unmarshal(result.Data, &status) != nil {
		fail(w, http.StatusBadGateway, "设备返回了无效的插件状态。")
		return
	}
	reply := response{BrowserExtensionStatus: status, Skipped: status.AlreadyInstalled || status.AlreadyUninstalled, Message: "已安装 Chrome 插件。"}
	switch {
	case status.AlreadyInstalled:
		reply.Message = "该设备已安装 GhostHand Chrome 插件，无需重复安装。"
	case status.AlreadyUninstalled:
		reply.Message = "该设备未安装 Chrome 插件，无需卸载。"
	case !install && status.Installed:
		reply.Message = "已清理可用的插件文件和注册项。请在桌面可用时再次卸载 Chrome 中的扩展。"
	case !install:
		reply.Message = "已卸载 Chrome 插件。"
	}
	writeJSON(w, http.StatusOK, reply)
}
