package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testSID = "0123456789abcdef"

func TestATranscriptIsKeptApartFromTheProgramsOwnLog(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)

	if err := s.saveTranscript(testSID, []byte(`{"entries":[]}`)); err != nil {
		t.Fatalf("save: %v", err)
	}

	want := filepath.Join(dir, "transcripts", testSID+".json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("a saved transcript is not at %s: %v", want, err)
	}
	// The old home. A transcript here would be read by nothing and would sit
	// where the rotating log is about to be written.
	if _, err := os.Stat(filepath.Join(dir, "logs", testSID+".json")); err == nil {
		t.Fatal("the transcript was written under logs/, where nothing reads it")
	}

	blob, err := s.loadTranscript(testSID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(blob) != `{"entries":[]}` {
		t.Fatalf("read back %q", blob)
	}
}

func TestATranscriptLeftInTheOldPlaceIsNoticed(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)

	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(filepath.Join(logs, "mainnet"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, blob := range map[string]string{
		filepath.Join(logs, testSID+".json"):     "{}",
		filepath.Join(logs, "dcrpoker.log"):      "not a transcript",
		filepath.Join(logs, "mainnet", "x.json"): "under the network dir",
		filepath.Join(logs, "notasession.json"):  "not a session id",
	} {
		if err := os.WriteFile(name, []byte(blob), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	found := s.strandedTranscripts()
	if len(found) != 1 || found[0] != testSID+".json" {
		t.Fatalf("expected only the stranded transcript, got %v", found)
	}
}

func TestNothingIsStrandedInAFreshDirectory(t *testing.T) {
	if found := newStore(t.TempDir()).strandedTranscripts(); found != nil {
		t.Fatalf("a directory with no logs/ reported %v", found)
	}
}
