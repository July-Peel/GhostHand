package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"ghosthand/proto"
)

func (a *agent) browserExtensionStatus() (proto.BrowserExtensionStatus, error) {
	if a.browser == nil {
		return proto.BrowserExtensionStatus{}, errors.New("Windows 浏览器扩展桥接不可用")
	}
	desktop, err := browserDesktopNow()
	return a.browser.status(browserSessionKey{desktop, "chrome"}, err)
}

func browserBundlePresent() (bool, error) {
	root, err := browserBundleRoot()
	if err != nil {
		return false, err
	}
	info, err := os.Stat(filepath.Join(root, browserBundleVersion, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil && info.Mode().IsRegular(), err
}

func browserInstalled(key browserSessionKey) (bool, error) {
	token := windows.GetCurrentProcessToken()
	if currentProcessSessionID() == 0 {
		if err := windows.WTSQueryUserToken(key.session, &token); err != nil {
			return false, err
		}
		defer token.Close()
	}
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	if user.User.Sid.String() != key.sid {
		return false, errors.New("Windows 桌面用户已切换，请重新检测")
	}
	local, err := token.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DONT_VERIFY)
	if err != nil {
		return false, err
	}
	dir := filepath.Join(local, "Google", "Chrome", "User Data")
	if key.browser == "edge" {
		dir = filepath.Join(local, "Microsoft", "Edge", "User Data")
	}
	profiles, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, profile := range profiles {
		if !profile.IsDir() || (profile.Name() != "Default" && !strings.HasPrefix(profile.Name(), "Profile ")) {
			continue
		}
		for _, name := range []string{"Preferences", "Secure Preferences"} {
			// Chrome can rewrite one profile while the others remain readable.
			installed, _ := browserPreferencesContain(filepath.Join(dir, profile.Name(), name))
			if installed {
				return true, nil
			}
		}
	}
	return false, nil
}

func browserPreferencesContain(name string) (bool, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return false, errors.New("Chrome Preferences changed")
	}
	var prefs struct {
		Extensions struct {
			Settings map[string]struct {
				Path     string          `json:"path"`
				Location int             `json:"location"`
				Manifest json.RawMessage `json:"manifest"`
			}
		}
	}
	if err := json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&prefs); err != nil {
		return false, err
	}
	setting, installed := prefs.Extensions.Settings[browserExtensionID]
	if !installed || (setting.Path == "" && len(setting.Manifest) == 0) {
		return false, nil
	}
	if setting.Path != "" {
		manifest, err := os.Stat(filepath.Join(setting.Path, "manifest.json"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err == nil && !manifest.Mode().IsRegular() {
			return false, nil
		}
	}
	return true, nil
}

func (a *agent) desktopOperation(ctx context.Context, kind proto.MessageType, req proto.DesktopRequest, progress func(string)) proto.DesktopResult {
	result := proto.DesktopResult{OperationID: req.OperationID, Kind: kind, Code: proto.ErrTimeout}
	if req.ExpiresAt <= time.Now().UnixMilli() || ctx.Err() != nil {
		return result
	}
	if kind != proto.BrowserExtensionStatusRequest {
		return a.rdpOperation(ctx, result, req, progress)
	}
	status, err := a.browserExtensionStatus()
	result.OK, result.Code = err == nil, ""
	if err != nil {
		result.Code = proto.ErrInternal
		status.Error = "无法读取 Chrome 扩展状态。"
	}
	result.Detail, _ = json.Marshal(status)
	return result
}
