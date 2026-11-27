package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ghosthand/proto"
)

var browserTabCommands = map[string]string{
	"list": "Chrome.ListTabs", "open": "Chrome.OpenUrl", "navigate": "Chrome.OpenUrl", "activate": "Chrome.SwitchTab",
	"close": "Chrome.CloseTab", "reload": "Chrome.Refresh", "back": "Chrome.GoBack", "forward": "Chrome.GoForward",
}

var browserDOMCommands = map[string]string{
	"inspect": "Chrome.InspectDOM", "list_frames": "Chrome.ListFrames",
	"click": "Chrome.Click", "click_link": "Chrome.ClickByLinkText", "fill": "Chrome.SetValue", "select": "Chrome.SetSelect",
	"check": "Chrome.setRadioCheckBox", "hover": "Chrome.Hover", "focus": "Chrome.Focus",
	"scroll": "Chrome.ScrollPage", "scroll_to_view": "Chrome.ScrollToView",
	"wait": "Chrome.Wait", "switch_frame": "Chrome.SwitchToFrame", "switch_top_frame": "Chrome.SwitchToTopFrame",
	"get_text": "Chrome.GetText", "get_html": "Chrome.GetinnerHTML", "get_attr": "Chrome.GetAttributes", "get_similar": "Chrome.GetSimilar",
	"exists": "Chrome.Exists", "get_value": "Chrome.GetValue", "get_select": "Chrome.GetSelect", "get_options": "Chrome.GetSelectOptions",
	"get_checked": "Chrome.getRadioCheckBoxStatus", "get_rect": "Chrome.GetRectangleByXpath", "get_parent": "Chrome.GetParentElement",
	"get_children": "Chrome.GetChildElement", "get_siblings": "Chrome.GetSiblingElement",
	"get_title": "Chrome.GetTitle", "get_url": "Chrome.GetUrl", "get_source": "Chrome.GetViewSource",
	"cookie_get": "Chrome.GetCookie", "cookie_set": "Chrome.SetCookie", "batch": "Chrome.BatchExecute",
	"screenshot_element": "Chrome.ScreenShotToBase64", "screenshot_tab": "Chrome.CaptureVisibleTab",
}

func (a *agent) executeBrowserTool(ctx context.Context, call proto.MCPCall, wake <-chan struct{}) proto.MCPResult {
	result := proto.MCPResult{CallID: call.CallID, TaskID: call.TaskID}
	args, err := proto.ValidateBrowserTool(call.Tool, call.Arguments)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if !call.Approved && browserNeedsApproval(call.Tool, args) {
		result.Error = "E_CONFIRM_REQUIRED"
		return result
	}
	browserCtx, cancel := context.WithTimeout(ctx, 58*time.Second)
	reply, err := a.browser.execute(browserCtx, call, args)
	cancel()
	if len(reply.Data) <= proto.MaxToolDataSize {
		result.Data = reply.Data
	} else if err == nil && !browserScreenshot(args.Action) {
		err = errors.New("浏览器结果超出大小上限")
	}
	if err != nil {
		result.Error = browserErrorText(err)
		return result
	}
	if call.Tool != proto.ToolBrowserAct || !browserScreenshot(args.Action) {
		return result
	}
	result.Data = nil
	var image struct {
		JPEG string `json:"jpeg"`
	}
	if json.Unmarshal(reply.Data, &image) != nil || !strings.HasPrefix(image.JPEG, "data:image/jpeg;base64,") {
		result.Error = "浏览器截图格式无效"
		return result
	}
	encoded := strings.TrimPrefix(image.JPEG, "data:image/jpeg;base64,")
	if len(encoded) > base64.StdEncoding.EncodedLen(proto.MaxScreenshotSize) {
		result.Error = "浏览器截图超出大小上限"
		return result
	}
	jpeg, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(jpeg) < 3 || len(jpeg) > proto.MaxScreenshotSize ||
		jpeg[0] != 0xff || jpeg[1] != 0xd8 || jpeg[2] != 0xff {
		result.Error = "浏览器截图格式无效或超出大小上限"
		return result
	}
	upload, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	display := call.Display
	display.StepName = "上传浏览器截图"
	a.platform.overlay(taskOverlay{TaskDisplay: display, Visible: true})
	attachment, err := a.uploadScreenshot(upload, call.TaskID, call.CallID, jpeg, wake)
	if err != nil {
		result.Error = "浏览器截图上传失败"
		return result
	}
	result.AttachmentID = attachment.AttachmentID
	result.Data, _ = json.Marshal(map[string]any{"tab_id": reply.TabID, "url": reply.URL, "title": reply.Title})
	return result
}

func browserErrorText(err error) string {
	text := err.Error()
	for index := range text {
		if index > 500 {
			return text[:index]
		}
	}
	return text
}

func browserNeedsApproval(tool string, args proto.BrowserArgs) bool {
	if tool == proto.ToolBrowserEval || args.Action == "cookie_get" || args.Action == "cookie_set" {
		return true
	}
	for _, raw := range args.Steps {
		var step struct {
			Action string `json:"action"`
		}
		if json.Unmarshal(raw, &step) == nil && (step.Action == "cookie_get" || step.Action == "cookie_set") {
			return true
		}
	}
	return false
}

func browserScreenshot(action string) bool {
	return action == "screenshot_tab" || action == "screenshot_element"
}

func (bridge *browserBridge) execute(ctx context.Context, call proto.MCPCall, args proto.BrowserArgs) (browserResponse, error) {
	if bridge == nil {
		return browserResponse{}, errors.New("Windows 浏览器扩展桥接不可用")
	}
	desktop, desktopErr := browserDesktopNow()
	name := args.Browser
	if name == "" && call.Tool != proto.ToolBrowserManage {
		bridge.mu.Lock()
		name = bridge.preferred[desktop]
		bridge.mu.Unlock()
	}
	if name == "" {
		name = "chrome"
	}
	key := browserSessionKey{desktop, name}
	if call.Tool == proto.ToolBrowserManage && args.Action == "status" {
		return browserStatusReply(bridge.status(key, desktopErr))
	}
	if call.Tool == proto.ToolBrowserManage {
		return bridge.manage(ctx, key, call.TaskID, args.Action)
	}
	if desktopErr != nil {
		return browserResponse{}, desktopErr
	}
	if bridge.startErr != nil {
		return browserResponse{}, bridge.startErr
	}
	if bridge.host.snapshot().DesktopState != "unlocked" {
		return browserResponse{}, errors.New("E_DESKTOP_LOCKED")
	}
	peer, err := bridge.ensure(ctx, key, call.TaskID)
	if err != nil {
		return browserResponse{}, err
	}
	bridge.mu.Lock()
	bridge.preferred[desktop] = name
	bridge.mu.Unlock()
	code := ""
	switch call.Tool {
	case proto.ToolBrowserTabs:
		code = browserTabCommands[args.Action]
	case proto.ToolBrowserInspect:
		code = "Chrome.InspectDOM"
	case proto.ToolBrowserAct:
		code = browserDOMCommands[args.Action]
		if args.Action == "scroll" && args.HasLocator() {
			code = "Chrome.ScrollToView"
		}
		if code == "" {
			code = browserTabCommands[args.Action]
		}
	case proto.ToolBrowserEval:
		code = "Chrome.EvaluateJS"
	}
	if code == "" {
		return browserResponse{}, proto.ErrToolArguments
	}
	reply, err := peer.call(ctx, browserRequest{Code: code, TabID: args.TabID, FrameID: args.FrameID, Data: call.Arguments})
	if err != nil {
		return reply, err
	}
	// Data contains action results. Keep document context available for scalar and list results too.
	if call.Tool == proto.ToolBrowserAct && !browserScreenshot(args.Action) && reply.TabID != nil {
		reply.Data, err = json.Marshal(struct {
			TabID *int            `json:"tab_id"`
			URL   string          `json:"url"`
			Title string          `json:"title"`
			Data  json.RawMessage `json:"data"`
		}{reply.TabID, reply.URL, reply.Title, reply.Data})
	}
	return reply, err
}

func (bridge *browserBridge) acquire(ctx context.Context) error {
	select {
	case bridge.lifecycle <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (bridge *browserBridge) release() { <-bridge.lifecycle }

func (bridge *browserBridge) manage(ctx context.Context, key browserSessionKey, task, action string) (browserResponse, error) {
	if err := bridge.acquire(ctx); err != nil {
		return browserResponse{}, err
	}
	defer bridge.release()
	desktop, desktopErr := browserDesktopNow()
	key.browserDesktop = desktop
	status, err := bridge.status(key, desktopErr)
	if err != nil {
		return browserResponse{}, err
	}
	if action == "install" {
		if status.Installed {
			status.AlreadyInstalled = true
			return browserStatusReply(status, nil)
		}
		if bridge.host.snapshot().DesktopState != "unlocked" {
			return browserResponse{}, errors.New("E_DESKTOP_LOCKED")
		}
		if err := errors.Join(bridge.startErr, desktopErr); err != nil {
			return browserResponse{}, err
		}
		if _, err := bridge.ensureLocked(ctx, key, task); err != nil {
			return browserResponse{}, err
		}
	} else {
		if !status.Installed && !status.Registered && !status.BundlePresent {
			status.AlreadyUninstalled = true
			return browserStatusReply(status, nil)
		}
		if err := bridge.uninstall(ctx, key, task); err != nil {
			return browserResponse{}, err
		}
	}
	return browserStatusReply(bridge.status(key, desktopErr))
}

func browserStatusReply(status proto.BrowserExtensionStatus, err error) (browserResponse, error) {
	if err != nil {
		return browserResponse{}, err
	}
	data, err := json.Marshal(status)
	return browserResponse{Data: data}, err
}

func (bridge *browserBridge) status(key browserSessionKey, desktopErr error) (proto.BrowserExtensionStatus, error) {
	peer, _ := bridge.peer(key)
	registered, err := browserRegistered(key)
	if err != nil {
		return proto.BrowserExtensionStatus{}, err
	}
	present, err := browserBundlePresent()
	if err != nil {
		return proto.BrowserExtensionStatus{}, err
	}
	installed := peer != nil
	if !installed && desktopErr == nil {
		installed, err = browserInstalled(key)
		if err != nil {
			return proto.BrowserExtensionStatus{}, err
		}
	}
	result := proto.BrowserExtensionStatus{
		Browser: key.browser, ExtensionID: browserExtensionID, Version: browserBundleVersion,
		Connected: peer != nil, Installed: installed, Registered: registered, BundlePresent: present,
	}
	if bridge.startErr != nil {
		result.Error = browserErrorText(bridge.startErr)
	} else if desktopErr != nil {
		result.Error = browserErrorText(desktopErr)
	}
	return result, nil
}

func (bridge *browserBridge) ensure(ctx context.Context, key browserSessionKey, task string) (*browserPeer, error) {
	if peer, _ := bridge.peer(key); peer != nil {
		return peer, nil
	}
	if err := bridge.acquire(ctx); err != nil {
		return nil, err
	}
	defer bridge.release()
	return bridge.ensureLocked(ctx, key, task)
}

func (bridge *browserBridge) ensureLocked(ctx context.Context, key browserSessionKey, task string) (*browserPeer, error) {
	if peer, _ := bridge.peer(key); peer != nil {
		return peer, nil
	}
	if bridge.host.helperLink() == nil {
		return nil, errors.New("GhostHand Helper 不可用")
	}
	dir, manifest, err := unpackBrowserBundle(bridge.core)
	if err != nil {
		return nil, err
	}
	if err := registerBrowserHost(key, manifest); err != nil {
		return nil, err
	}
	token, err := proto.NewID()
	if err != nil {
		return nil, err
	}
	install, cancel := context.WithTimeout(ctx, 17*time.Second)
	_, err = bridge.callInstaller(install, task, "browser.ext_install", key.browser, dir, manifest, token)
	cancel()
	if err != nil {
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 8*time.Second)
	peer, err := bridge.waitConnected(wait, key)
	cancel()
	if err != nil {
		// The Helper can still close its own management tab when the native host never connects.
		closeTab, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_, _ = bridge.callInstaller(closeTab, task, "browser.ext_close_uia", key.browser, "", "", token)
		return nil, err
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	data, _ := json.Marshal(map[string]string{"install_token": token})
	reply, err := peer.call(cleanup, browserRequest{Code: "Chrome.CloseExtensionsTabs", Data: data})
	cancel()
	var closeResult struct{ Closed int }
	if err != nil || json.Unmarshal(reply.Data, &closeResult) != nil || closeResult.Closed == 0 {
		closeTab, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = bridge.callInstaller(closeTab, task, "browser.ext_close_uia", key.browser, "", "", token)
	}
	return peer, nil
}

func (bridge *browserBridge) callInstaller(ctx context.Context, task, method, browser, dir, manifest, token string) (captureReply, error) {
	call := struct {
		Task      string `json:"task_id"`
		Browser   string `json:"browser"`
		Directory string `json:"extension_dir,omitempty"`
		Manifest  string `json:"manifest_path,omitempty"`
		Token     string `json:"install_token,omitempty"`
	}{task, browser, dir, manifest, token}
	return bridge.host.callHelper(ctx, task, method, call)
}

func (bridge *browserBridge) uninstall(ctx context.Context, key browserSessionKey, task string) error {
	var uiErr error
	peers := bridge.connectedPeers(key)
	removed := len(peers) > 0
	detach, cancel := context.WithTimeout(ctx, 3*time.Second)
	for _, peer := range peers {
		if _, err := peer.call(detach, browserRequest{Code: "Chrome.UninstallSelf"}); err == nil {
			select {
			case <-peer.done:
			case <-detach.Done():
				removed = false
			}
		} else {
			removed = false
		}
	}
	cancel()
	if !removed && key.session != 0 && bridge.host.snapshot().DesktopState == "unlocked" {
		var installed bool
		installed, uiErr = browserInstalled(key)
		if installed || len(peers) > 0 {
			token, err := proto.NewID()
			if err == nil {
				fallback, cancel := context.WithTimeout(ctx, 13*time.Second)
				_, uiErr = bridge.callInstaller(fallback, task, "browser.ext_remove_uia", key.browser, "", "", token)
				cancel()
			} else {
				uiErr = err
			}
		}
	}
	var removeErr, bundleErr error
	if uiErr == nil {
		removeErr = removeBrowserRegistration(key)
		bundleErr = removeBrowserBundleIfUnused(key.browserDesktop)
	}
	bridge.mu.Lock()
	if bridge.preferred[key.browserDesktop] == key.browser {
		delete(bridge.preferred, key.browserDesktop)
	}
	bridge.mu.Unlock()
	return errors.Join(uiErr, removeErr, bundleErr)
}

func (bridge *browserBridge) cleanup(ctx context.Context) error {
	if bridge == nil {
		return nil
	}
	if err := bridge.acquire(ctx); err != nil {
		return err
	}
	defer bridge.release()
	desktop, _ := browserDesktopNow()
	return errors.Join(bridge.uninstall(ctx, browserSessionKey{desktop, "chrome"}, "browser-cleanup"),
		bridge.uninstall(ctx, browserSessionKey{desktop, "edge"}, "browser-cleanup"))
}

func runLocalBrowserCleanup() error {
	if !browserCleanupElevated() {
		return errors.New("browser cleanup requires an elevated administrator")
	}
	connect, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	conn, err := dialBrowserPipe(connect)
	cancel()
	if err == nil {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(32 * time.Second))
		if err := writeBrowserFrame(conn, []byte(`{"Code":"Browser.cleanup","Status":true}`), 4096); err == nil {
			data, err := readBrowserFrame(conn, 4096)
			if err == nil {
				var reply browserResponse
				if json.Unmarshal(data, &reply) == nil && reply.Code == "Browser.cleanup" {
					if reply.Status {
						return nil
					}
					return errors.New(reply.Error)
				}
				err = errors.New("invalid browser cleanup response")
			}
			return fmt.Errorf("read browser cleanup response: %w", err)
		}
	}
	desktop, _ := browserDesktopNow()
	return errors.Join(removeBrowserRegistration(browserSessionKey{desktop, "chrome"}),
		removeBrowserRegistration(browserSessionKey{desktop, "edge"}), removeBrowserBundleIfUnused(desktop))
}
