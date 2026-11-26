//go:build windows

package main

import "testing"

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
