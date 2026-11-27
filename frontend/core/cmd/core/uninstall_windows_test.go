//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLauncherBinaryPath(t *testing.T) {
	got, err := parseLauncherBinaryPath(`"C:\Program Files\GhostHand\ghosthand-launcher.exe" --server "http://127.0.0.1:8080" --key "ghk_test"`)
	if err != nil {
		t.Fatalf("parseLauncherBinaryPath with args: %v", err)
	}
	if got != `C:\Program Files\GhostHand\ghosthand-launcher.exe` {
		t.Fatalf("got %q", got)
	}

	gotSingle, err := parseLauncherBinaryPath(`"C:\Program Files\GhostHand\ghosthand-launcher.exe"`)
	if err != nil || gotSingle != `C:\Program Files\GhostHand\ghosthand-launcher.exe` {
		t.Fatalf("parseLauncherBinaryPath single arg: got=%q err=%v", gotSingle, err)
	}

	if _, err := parseLauncherBinaryPath(`"C:\Windows\System32\cmd.exe" /c echo hi`); err == nil {
		t.Fatal("expected error for non-launcher executable")
	}
}

func TestBrowserPreferencesContain(t *testing.T) {
	dir := t.TempDir()
	extDir := filepath.Join(dir, "1.0.0")
	if err := os.MkdirAll(extDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(extDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"manifest_version":3}`), 0644); err != nil {
		t.Fatal(err)
	}

	prefsFile := filepath.Join(dir, "Secure Preferences")

	// Empty object {} left by Chrome after uninstallSelf / UIA removal must not count as installed.
	if err := os.WriteFile(prefsFile, []byte(`{"extensions":{"settings":{"`+browserExtensionID+`":{}}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if installed, err := browserPreferencesContain(prefsFile); err != nil || installed {
		t.Fatalf("empty extension settings object should not be installed: installed=%v err=%v", installed, err)
	}

	// Active unpacked extension pointing to existing bundle directory must count as installed.
	escapedPath := filepath.ToSlash(extDir)
	if err := os.WriteFile(prefsFile, []byte(`{"extensions":{"settings":{"`+browserExtensionID+`":{"location":4,"path":"`+escapedPath+`"}}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if installed, err := browserPreferencesContain(prefsFile); err != nil || !installed {
		t.Fatalf("valid unpacked extension should be installed: installed=%v err=%v", installed, err)
	}

	// Stale unpacked extension entry pointing to a removed bundle directory must not count as installed.
	if err := os.RemoveAll(extDir); err != nil {
		t.Fatal(err)
	}
	if installed, err := browserPreferencesContain(prefsFile); err != nil || installed {
		t.Fatalf("removed unpacked extension path should not be installed: installed=%v err=%v", installed, err)
	}
}

func TestExtractFreeRDPArchive(t *testing.T) {
	bundlePath := filepath.Clean(`..\..\..\..\backend\internal\agents\FreeRDP.zip`)
	file, err := os.Open(bundlePath)
	if err != nil {
		t.Fatalf("open embedded FreeRDP.zip: %v", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := extractFreeRDPArchive(file, info.Size(), stage); err != nil {
		t.Fatalf("extractFreeRDPArchive: %v", err)
	}
	client := filepath.Join(stage, "wfreerdp.exe")
	if stat, err := os.Stat(client); err != nil || !stat.Mode().IsRegular() {
		t.Fatalf("expected extracted wfreerdp.exe: stat=%v err=%v", stat, err)
	}
	if !isSDLFreeRDP(client) {
		t.Fatal("expected bundled wfreerdp.exe to be detected as SDL FreeRDP")
	}
}
