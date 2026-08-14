// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package log

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/decred/slog"
)

// reset puts the package back to a closed rotator, so tests do not inherit
// each other's file handle.
func reset(t *testing.T) {
	t.Helper()
	CloseRotator()
	t.Cleanup(CloseRotator)
}

func TestALineReachesTheFile(t *testing.T) {
	reset(t)
	path := filepath.Join(t.TempDir(), "logs", "mainnet", "dcrpoker.log")

	if err := InitRotator(path); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := SetDebugLevel("info"); err != nil {
		t.Fatalf("level: %v", err)
	}
	POKR.Infof("rotator test line %d", 42)
	CloseRotator()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	// The exact shape an operator greps and the Logs tab colours: a bracketed
	// three-letter level, then the subsystem tag.
	if !strings.Contains(string(blob), "[INF] POKR: rotator test line 42") {
		t.Fatalf("the file does not carry the line in wire format:\n%s", blob)
	}
}

func TestOpeningTheFileTwiceKeepsOneHandle(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	first := filepath.Join(dir, "first.log")
	second := filepath.Join(dir, "second.log")

	if err := InitRotator(first); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := InitRotator(second); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if err := SetDebugLevel("info"); err != nil {
		t.Fatalf("level: %v", err)
	}
	POKR.Info("after the second open")
	CloseRotator()

	// The second call must not have swapped the handle, which would leave the
	// first file open with nothing left pointing at it.
	if _, err := os.Stat(second); err == nil {
		t.Fatal("a second InitRotator opened another file")
	}
	blob, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if !strings.Contains(string(blob), "after the second open") {
		t.Fatalf("the first file stopped receiving:\n%s", blob)
	}
}

func TestADebugLevelSpecNamesItsSubsystems(t *testing.T) {
	reset(t)

	if err := SetDebugLevel("info,TABL=debug,BRDG=trace"); err != nil {
		t.Fatalf("a valid spec was refused: %v", err)
	}
	if got := TABL.Level(); got != slog.LevelDebug {
		t.Fatalf("TABL is at %s", got)
	}
	if got := BRDG.Level(); got != slog.LevelTrace {
		t.Fatalf("BRDG is at %s", got)
	}
	if got := SETL.Level(); got != slog.LevelInfo {
		t.Fatalf("a subsystem the spec did not name is at %s", got)
	}

	err := SetDebugLevel("info,NOPE=debug")
	if err == nil {
		t.Fatal("an unknown subsystem was accepted")
	}
	// The message has to name the alternatives, or a typo is a guessing game.
	if !strings.Contains(err.Error(), "TABL") {
		t.Fatalf("the refusal does not list the subsystems: %v", err)
	}

	if err := SetDebugLevel("TABL=shouty"); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	if err := SetDebugLevel("shouty"); err == nil {
		t.Fatal("an unknown bare level was accepted")
	}
}

func TestEverySubsystemIsReachableByName(t *testing.T) {
	reset(t)
	for _, tag := range SubsystemTags() {
		if err := SetDebugLevel(tag + "=debug"); err != nil {
			t.Fatalf("tag %s is listed but not settable: %v", tag, err)
		}
	}
	if len(SubsystemTags()) != len(subsystems) {
		t.Fatal("a subsystem exists that SubsystemTags does not report")
	}
}

func TestWritingWhileTheFileIsOpenedAndClosed(t *testing.T) {
	reset(t)
	path := filepath.Join(t.TempDir(), "race.log")
	if err := SetDebugLevel("info"); err != nil {
		t.Fatalf("level: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			POKR.Infof("line %d", n)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = InitRotator(path)
	}()
	wg.Wait()
	CloseRotator()
}
