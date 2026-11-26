package main

import (
	"testing"
	"time"

	"ghosthand/proto"
)

func TestMemoryJournalAndMachineFingerprint(t *testing.T) {
	j := openMemoryJournal()
	if !j.isFresh() {
		t.Fatal("new journal should be fresh")
	}
	if err := j.acceptWelcome(0); err != nil {
		t.Fatalf("acceptWelcome: %v", err)
	}
	if j.isFresh() {
		t.Fatal("journal should not be fresh after acceptWelcome")
	}
	if err := j.enqueueCaps(proto.CapabilitiesReport{Hash: "h1"}); err != nil {
		t.Fatalf("enqueueCaps: %v", err)
	}
	frames, err := j.framesAfter(0)
	if err != nil || len(frames) != 1 || frames[0].Seq != 1 {
		t.Fatalf("framesAfter=%v err=%v", frames, err)
	}

	j.resetForFreshSession()
	if !j.isFresh() {
		t.Fatal("reset journal should be fresh")
	}

	buf := &memLogBuffer{}
	buf.append(time.Now(), []byte(`{"msg":"in-memory"}`))
	if lines := buf.snapshot(time.Now().Add(-time.Minute)); len(lines) != 1 {
		t.Fatalf("expected 1 in-memory log line, got %d", len(lines))
	}

	id1, err := machineFingerprint()
	if err != nil || len(id1) != 64 {
		t.Fatalf("machineFingerprint=%q err=%v", id1, err)
	}
	id2, err := machineFingerprint()
	if err != nil || id1 != id2 {
		t.Fatalf("machineFingerprint not deterministic: %q != %q", id1, id2)
	}
}
