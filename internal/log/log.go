// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// Package log wires dcrpoker's subsystem loggers. Pattern mirrors dcrd's
// log.go: a single slog backend writes to both stdout and a rotating file,
// and each subsystem holds its own named logger. Tags follow what the program
// is doing rather than which Go file it is in, because all of it lives in one
// command directory and tagging by file would tell an operator nothing.
package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/decred/slog"
	"github.com/jrick/logrotate/rotator"
)

// logMaxRolls is how many gzipped rolls are kept.
const logMaxRolls = 3

// logWriter tees every backend write to stdout and, once opened, to the
// rotating file. slog serialises its own writes, but InitRotator and
// CloseRotator run outside that lock, so the mutex covers the swap.
type logWriter struct {
	mtx sync.Mutex
	rot *rotator.Rotator
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	w.mtx.Lock()
	defer w.mtx.Unlock()
	os.Stdout.Write(p)
	if w.rot != nil {
		w.rot.Write(p)
	}
	return len(p), nil
}

var (
	writer     = &logWriter{}
	backendLog = slog.NewBackend(writer)

	BRDG = backendLog.Logger("BRDG")
	CHAN = backendLog.Logger("CHAN")
	COIN = backendLog.Logger("COIN")
	DSPT = backendLog.Logger("DSPT")
	HTTP = backendLog.Logger("HTTP")
	PLAY = backendLog.Logger("PLAY")
	POKR = backendLog.Logger("POKR")
	SETL = backendLog.Logger("SETL")
	TABL = backendLog.Logger("TABL")
)

// subsystems is fixed at build time. Registering loggers on demand would need
// a lock on every lookup and would turn a typo in a debuglevel spec into a
// silent new subsystem instead of a startup error.
var subsystems = map[string]slog.Logger{
	"BRDG": BRDG,
	"CHAN": CHAN,
	"COIN": COIN,
	"DSPT": DSPT,
	"HTTP": HTTP,
	"PLAY": PLAY,
	"POKR": POKR,
	"SETL": SETL,
	"TABL": TABL,
}

// InitRotator opens the log file at logFile, rolling it at rollSizeKB
// kilobytes, and wires the rotating writer. Parent directories are created as
// needed. Subsequent calls are no-ops so a second call cannot orphan the open
// file handle.
func InitRotator(logFile string, rollSizeKB int64) error {
	writer.mtx.Lock()
	defer writer.mtx.Unlock()
	if writer.rot != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	r, err := rotator.New(logFile, rollSizeKB, false, logMaxRolls)
	if err != nil {
		return fmt.Errorf("open log rotator: %w", err)
	}
	writer.rot = r
	return nil
}

// CloseRotator flushes the rotator and releases the underlying file. Safe to
// call multiple times.
func CloseRotator() {
	writer.mtx.Lock()
	defer writer.mtx.Unlock()
	if writer.rot != nil {
		writer.rot.Close()
		writer.rot = nil
	}
}

// SubsystemTags returns the registered subsystem names in sorted order.
func SubsystemTags() []string {
	out := make([]string, 0, len(subsystems))
	for k := range subsystems {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetDebugLevel applies a dcrwallet-style debuglevel spec: either a bare level
// applied to every subsystem ("debug"), or a comma-separated list of
// subsystem=level pairs optionally led by a bare level
// ("info,TABL=debug,BRDG=trace").
func SetDebugLevel(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	for _, field := range strings.Split(spec, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		subsys, level, isPair := strings.Cut(field, "=")
		if !isPair {
			if err := setLevels(field); err != nil {
				return err
			}
			continue
		}
		subsys = strings.TrimSpace(subsys)
		logger, ok := subsystems[subsys]
		if !ok {
			return fmt.Errorf("unknown log subsystem %q (have %s)", subsys,
				strings.Join(SubsystemTags(), ", "))
		}
		lvl, err := parseLevel(level)
		if err != nil {
			return err
		}
		logger.SetLevel(lvl)
	}
	return nil
}

// setLevels sets the same level across every subsystem logger.
func setLevels(level string) error {
	lvl, err := parseLevel(level)
	if err != nil {
		return err
	}
	for _, l := range subsystems {
		l.SetLevel(lvl)
	}
	return nil
}

func parseLevel(level string) (slog.Level, error) {
	lvl, ok := slog.LevelFromString(strings.TrimSpace(level))
	if !ok {
		return slog.LevelInfo, fmt.Errorf("unknown log level %q (have trace, "+
			"debug, info, warn, error, critical, off)", level)
	}
	return lvl, nil
}
