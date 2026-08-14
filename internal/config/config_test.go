// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vctt94/dcrpoker/sampleconfig"
)

// loadIn parses args against a private application directory, so a test never
// reads or writes the real one.
func loadIn(t *testing.T, dir string, args ...string) (*Config, error) {
	t.Helper()
	return Load(append([]string{"--appdata", dir}, args...), "test")
}

func TestTheDataDirectoryIsTheApplicationDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadIn(t, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Flat, and not under data/<network>. An existing game keeps identity.json
	// and sessions/ at the top level, and a deeper default would leave one
	// unreadable with its coin unfindable.
	if cfg.DataDir != dir {
		t.Fatalf("the data directory is %s, not the application directory %s", cfg.DataDir, dir)
	}
	// The log directory, unlike the data directory, is network-scoped.
	if want := filepath.Join(dir, "logs", "mainnet"); cfg.LogDir != want {
		t.Fatalf("the log directory is %s, want %s", cfg.LogDir, want)
	}
	if want := filepath.Join(dir, "logs", "mainnet", "dcrpoker.log"); cfg.LogFile() != want {
		t.Fatalf("the log file is %s, want %s", cfg.LogFile(), want)
	}
}

func TestADataDirectoryOfItsOwnIsKept(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(t.TempDir(), "elsewhere")
	cfg, err := loadIn(t, dir, "--datadir", data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DataDir != data {
		t.Fatalf("--datadir was overridden by --appdata: %s", cfg.DataDir)
	}
}

func TestTheNetworkNamesTheLogDirectory(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{
		{"--testnet", "testnet3"},
		{"--simnet", "simnet"},
	} {
		dir := t.TempDir()
		cfg, err := loadIn(t, dir, tc.flag)
		if err != nil {
			t.Fatalf("%s: %v", tc.flag, err)
		}
		if cfg.Network() != tc.want {
			t.Fatalf("%s gives network %s", tc.flag, cfg.Network())
		}
		if want := filepath.Join(dir, "logs", tc.want); cfg.LogDir != want {
			t.Fatalf("%s logs to %s, want %s", tc.flag, cfg.LogDir, want)
		}
	}

	if _, err := loadIn(t, t.TempDir(), "--testnet", "--simnet"); err == nil {
		t.Fatal("two networks at once were accepted")
	}
}

func TestWhatIsTypedBeatsWhatTheFileSays(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, DefaultConfigFilename)
	if err := os.WriteFile(conf, []byte("debuglevel=warn\nlisten=127.0.0.1:9999\n"), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}

	cfg, err := loadIn(t, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DebugLevel != "warn" || cfg.Listen != "127.0.0.1:9999" {
		t.Fatalf("the file was not read: %s %s", cfg.DebugLevel, cfg.Listen)
	}

	cfg, err = loadIn(t, dir, "--debuglevel", "trace")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DebugLevel != "trace" {
		t.Fatalf("the file beat the command line: %s", cfg.DebugLevel)
	}
	// The rest of the file still applies.
	if cfg.Listen != "127.0.0.1:9999" {
		t.Fatalf("the file stopped being read: %s", cfg.Listen)
	}
}

func TestAnExampleConfigIsLeftBehindOnTheFirstRun(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, DefaultConfigFilename)

	if _, err := loadIn(t, dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	info, err := os.Stat(conf)
	if err != nil {
		t.Fatalf("no example config was written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the example config is mode %v", info.Mode().Perm())
	}
	blob, err := os.ReadFile(conf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Every option commented, so the file that appears changes no behaviour.
	if !strings.Contains(string(blob), "; debuglevel=info") {
		t.Fatalf("the example does not document debuglevel:\n%s", blob)
	}

	// An existing file is never overwritten - it is the operator's.
	if err := os.WriteFile(conf, []byte("debuglevel=warn\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := loadIn(t, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DebugLevel != "warn" {
		t.Fatal("the example config overwrote one that was already there")
	}
}

func TestTheSampleWeShipParses(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, DefaultConfigFilename)
	if err := os.WriteFile(conf, []byte(sampleconfig.FileContents()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Every line is commented, so this both proves the file is syntactically
	// good and that it changes nothing. A section header naming a group that
	// does not exist is the way to get this wrong, and it is silent until an
	// operator uncomments a line.
	cfg, err := loadIn(t, dir)
	if err != nil {
		t.Fatalf("the sample config we ship does not parse: %v", err)
	}
	if cfg.DebugLevel != DefaultDebugLevel || cfg.Listen != DefaultListen {
		t.Fatalf("the sample config changed behaviour: %s %s", cfg.DebugLevel, cfg.Listen)
	}

	// And it parses with its options live, not just commented out.
	live := strings.ReplaceAll(sampleconfig.FileContents(), "\n; bridge.", "\nbridge.")
	live = strings.ReplaceAll(live, "\n; debuglevel=", "\ndebuglevel=")
	if err := os.WriteFile(conf, []byte(live), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err = loadIn(t, dir)
	if err != nil {
		t.Fatalf("the sample config does not parse once uncommented: %v", err)
	}
	if !cfg.Bridge.Configured() {
		t.Fatal("the sample's bridge lines do not reach the bridge options")
	}
}

func TestAHomePathIsExpanded(t *testing.T) {
	t.Setenv("DCRPOKER_TEST_DIR", t.TempDir())
	dir := t.TempDir()
	cfg, err := loadIn(t, dir, "--logdir", "$DCRPOKER_TEST_DIR/logs")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Contains(cfg.LogDir, "$") {
		t.Fatalf("the environment was not expanded: %s", cfg.LogDir)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to expand against")
	}
	if got := cleanAndExpandPath("~/x"); got != filepath.Join(home, "x") {
		t.Fatalf("~ expanded to %s", got)
	}
}

func TestALogSizeNeedsItsUnit(t *testing.T) {
	for spec, want := range map[string]int64{
		"10M": 10240, "1K": 1, "1G": 1048576, "10MiB": 10240, "512k": 512,
	} {
		got, err := parseLogSize(spec)
		if err != nil {
			t.Fatalf("%s: %v", spec, err)
		}
		if got != want {
			t.Fatalf("%s parsed to %d, want %d", spec, got, want)
		}
	}
	// A bare number is refused rather than guessed at.
	for _, spec := range []string{"10", "", "M", "10X", "-1M"} {
		if _, err := parseLogSize(spec); err == nil {
			t.Fatalf("%q was accepted as a log size", spec)
		}
	}
}

func TestAHalfNamedBridgeIsRefused(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, DefaultConfigFilename)
	if err := os.WriteFile(conf, []byte("[bridge]\nbridge.addr=127.0.0.1:8443\n"), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	if _, err := loadIn(t, dir); err == nil {
		t.Fatal("a bridge with an address and no credential was accepted")
	}

	full := "[bridge]\nbridge.addr=127.0.0.1:8443\nbridge.clientcert=/c\nbridge.clientkey=/k\nbridge.cert=/b\n"
	if err := os.WriteFile(conf, []byte(full), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	cfg, err := loadIn(t, dir)
	if err != nil {
		t.Fatalf("a fully named bridge was refused: %v", err)
	}
	if !cfg.Bridge.Configured() {
		t.Fatal("a fully named bridge does not report itself configured")
	}
}

func TestAskingTheVersionIsNotAFailure(t *testing.T) {
	_, err := loadIn(t, t.TempDir(), "--version")
	if err == nil {
		t.Fatal("--version kept going")
	}
	if !IsDone(err) {
		t.Fatalf("--version reads as a failure: %v", err)
	}
}

func TestAskingForHelpIsNotAFailure(t *testing.T) {
	// go-flags returns a *flags.Error that neither wraps nor equals
	// flags.ErrHelp. Comparing with errors.Is sends the help text to stderr
	// under a program prefix and exits 1.
	_, err := loadIn(t, t.TempDir(), "--help")
	if err == nil {
		t.Fatal("--help kept going")
	}
	if !IsDone(err) {
		t.Fatalf("--help reads as a failure: %v", err)
	}
}

func TestAnUnknownFlagIsAFailure(t *testing.T) {
	_, err := loadIn(t, t.TempDir(), "--nonsense")
	if err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	if IsDone(err) {
		t.Fatal("an unknown flag reads as a question that was answered")
	}
}
