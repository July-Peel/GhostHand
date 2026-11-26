package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	chromeextension "ghosthand/chrome-extension"
	"ghosthand/launcher/state"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const browserBundleVersion = "1.0.0"

var deleteBrowserRegistryKey = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegDeleteKeyExW")

func browserBundleRoot() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "GhostHand", "chrome-extension"), nil
}

func unpackBrowserBundle(core string) (dir, manifestPath string, err error) {
	root, err := browserBundleRoot()
	if err != nil {
		return "", "", err
	}
	if _, err := state.PrepareBrowserExtensionDirectory(filepath.Dir(root)); err != nil {
		return "", "", err
	}
	var embedded struct{ Version, Key string }
	manifest, err := chromeextension.Files.ReadFile("manifest.json")
	if err != nil {
		return "", "", err
	}
	if err := json.Unmarshal(manifest, &embedded); err != nil {
		return "", "", err
	}
	key, err := base64.StdEncoding.DecodeString(embedded.Key)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(key)
	var extensionID strings.Builder
	for _, item := range digest[:16] {
		extensionID.WriteByte('a' + (item >> 4))
		extensionID.WriteByte('a' + (item & 15))
	}
	if extensionID.String() != browserExtensionID || embedded.Version != browserBundleVersion {
		return "", "", errors.New("embedded browser manifest ID or version mismatch")
	}
	dir = filepath.Join(root, browserBundleVersion)
	if err := ordinaryBrowserDirectory(dir, true); err != nil {
		return "", "", err
	}
	if err := fs.WalkDir(chromeextension.Files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		dest := filepath.Join(dir, filepath.FromSlash(path))
		if entry.IsDir() {
			return ordinaryBrowserDirectory(dest, true)
		}
		content, err := chromeextension.Files.ReadFile(path)
		if err != nil {
			return err
		}
		return writeBrowserBundleFile(dest, content)
	}); err != nil {
		return "", "", err
	}
	data, err := json.MarshalIndent(struct {
		Name           string   `json:"name"`
		Description    string   `json:"description"`
		Path           string   `json:"path"`
		Type           string   `json:"type"`
		AllowedOrigins []string `json:"allowed_origins"`
	}{browserHostName, "GhostHand Native Messaging Host", core, "stdio", []string{browserOrigin}}, "", "  ")
	if err != nil {
		return "", "", err
	}
	manifestPath = filepath.Join(root, browserHostName+".json")
	return dir, manifestPath, writeBrowserBundleFile(manifestPath, data)
}

func ordinaryBrowserDirectory(path string, create bool) error {
	if create {
		if err := os.Mkdir(path, 0755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("GhostHand browser extension path must be an ordinary directory")
	}
	return nil
}

func writeBrowserBundleFile(path string, content []byte) error {
	if info, err := os.Lstat(path); err == nil {
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("GhostHand browser bundle contains an invalid file")
		}
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".ghosthand-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	_, writeErr := file.Write(content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func browserRegistryPath(name string) string {
	browser := `Google\Chrome`
	if name == "edge" {
		browser = `Microsoft\Edge`
	}
	return `SOFTWARE\` + browser + `\NativeMessagingHosts\` + browserHostName
}

type browserRegistryLocation struct {
	key  registry.Key
	path string
	view uint32
}

func browserRegistryLocations(key browserSessionKey) []browserRegistryLocation {
	path := browserRegistryPath(key.browser)
	locations := []browserRegistryLocation{
		{registry.LOCAL_MACHINE, path, registry.WOW64_64KEY},
		{registry.LOCAL_MACHINE, path, registry.WOW64_32KEY},
	}
	if key.sid != "" {
		locations = append(locations, browserRegistryLocation{registry.USERS, key.sid + `\` + path, registry.WOW64_64KEY})
	}
	return locations
}

func registerBrowserHost(session browserSessionKey, manifest string) error {
	for _, location := range browserRegistryLocations(session) {
		key, _, err := registry.CreateKey(location.key, location.path, registry.SET_VALUE|location.view)
		if err != nil {
			return err
		}
		err = key.SetStringValue("", manifest)
		key.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func browserRegistered(session browserSessionKey) (bool, error) {
	for _, location := range browserRegistryLocations(session) {
		key, err := registry.OpenKey(location.key, location.path, registry.QUERY_VALUE|location.view)
		if errors.Is(err, registry.ErrNotExist) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			continue
		}
		if err != nil {
			return false, err
		}
		value, _, err := key.GetStringValue("")
		key.Close()
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if value != "" {
			return true, nil
		}
	}
	return false, nil
}

func removeBrowserRegistration(session browserSessionKey) error {
	var removeErr error
	for _, location := range browserRegistryLocations(session) {
		name, err := windows.UTF16PtrFromString(location.path)
		if err != nil {
			return err
		}
		status, _, _ := deleteBrowserRegistryKey.Call(uintptr(location.key), uintptr(unsafe.Pointer(name)), uintptr(location.view), 0)
		if status != 0 && syscall.Errno(status) != windows.ERROR_FILE_NOT_FOUND && syscall.Errno(status) != windows.ERROR_PATH_NOT_FOUND {
			removeErr = errors.Join(removeErr, syscall.Errno(status))
		}
	}
	return removeErr
}

func removeBrowserBundleIfUnused(desktop browserDesktop) error {
	for _, name := range []string{"chrome", "edge"} {
		registered, err := browserRegistered(browserSessionKey{desktop, name})
		if err != nil || registered {
			return err
		}
	}
	root, err := browserBundleRoot()
	if err != nil {
		return err
	}
	for _, path := range []string{filepath.Dir(root), root} {
		if err := ordinaryBrowserDirectory(path, false); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return nil
			}
			return err
		}
	}
	if _, err := state.PrepareBrowserExtensionDirectory(filepath.Dir(root)); err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func browserCleanupElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
