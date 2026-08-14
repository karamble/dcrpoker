// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// Package config parses dcrpoker's CLI flags and INI configuration file.
// The layout intentionally mirrors dcrd / dcrwallet / dcrlnd conventions:
// flags via go-flags, INI config via go-flags' IniParser, defaults rooted at
// the platform-appropriate application data directory.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/decred/dcrd/dcrutil/v4"
	flags "github.com/jessevdk/go-flags"

	"github.com/vctt94/dcrpoker/sampleconfig"
)

const (
	AppName               = "dcrpoker"
	DefaultConfigFilename = AppName + ".conf"
	DefaultLogFilename    = AppName + ".log"
	DefaultDebugLevel     = "info"
	DefaultLogSize        = "10M"

	// DefaultListen is loopback and stays loopback. What listens here is this
	// game's own interface, guarded by a token printed to the terminal that
	// started it, which is a session key rather than a network credential.
	DefaultListen = "127.0.0.1:8790"
)

var DefaultAppDataDir = dcrutil.AppDataDir(AppName, false)

// BridgeOptions name a dcrpulse bridge and the credential to reach it with
// (config section `[bridge]`). Set all four to configure a machine from a file
// alone; leave them empty to use what the first-run wizard wrote down.
type BridgeOptions struct {
	Addr       string `long:"addr" description:"Address of the dcrpulse gaming bridge (host:port)"`
	ClientCert string `long:"clientcert" description:"Path to this game's client certificate"`
	ClientKey  string `long:"clientkey" description:"Path to this game's client key"`
	BridgeCert string `long:"cert" description:"Path to the bridge's certificate"`
}

// Configured reports whether the bridge was named in full. A partial answer is
// a mistake worth refusing rather than half-using.
func (b BridgeOptions) Configured() bool {
	return b.Addr != "" && b.ClientCert != "" && b.ClientKey != "" && b.BridgeCert != ""
}

func (b BridgeOptions) partial() bool {
	return !b.Configured() &&
		(b.Addr != "" || b.ClientCert != "" || b.ClientKey != "" || b.BridgeCert != "")
}

// Config is the parsed runtime configuration.
type Config struct {
	ShowVersion bool   `short:"V" long:"version" description:"Display version information and exit"`
	AppDataDir  string `long:"appdata" description:"Top-level application data directory"`
	ConfigFile  string `short:"C" long:"configfile" description:"Path to configuration file"`
	DataDir     string `long:"datadir" description:"Directory holding this game's identity, tables and transcripts"`
	LogDir      string `long:"logdir" description:"Directory to log output"`
	LogSize     string `long:"logsize" description:"Size at which the log file rolls, e.g. 10M"`
	DebugLevel  string `long:"debuglevel" description:"Logging level {trace, debug, info, warn, error, critical}, or a list like info,TABL=debug"`
	Listen      string `long:"listen" description:"Address this game serves its own interface on (loopback only)"`
	TestNet     bool   `long:"testnet" description:"Use the test network"`
	SimNet      bool   `long:"simnet" description:"Use the simulation network"`

	Bridge BridgeOptions `group:"bridge" namespace:"bridge"`

	// RollSizeKB is the parsed form of LogSize, in kilobytes.
	RollSizeKB int64
}

// Network reports the active network under the name chaincfg uses, which is
// also the name of the log directory's subdirectory.
func (c *Config) Network() string {
	switch {
	case c.SimNet:
		return "simnet"
	case c.TestNet:
		return "testnet3"
	default:
		return "mainnet"
	}
}

// LogFile returns the absolute path to the rotating log file.
func (c *Config) LogFile() string {
	return filepath.Join(c.LogDir, DefaultLogFilename)
}

// Load parses CLI flags and the INI config file (if one exists). The CLI
// override semantics match dcrd: flags > INI > defaults.
func Load(args []string, version string) (*Config, error) {
	pre := defaults()
	preParser := flags.NewParser(pre, flags.HelpFlag|flags.PassDoubleDash|flags.IgnoreUnknown)
	if _, err := preParser.ParseArgs(args); err != nil {
		return nil, err
	}

	if pre.ShowVersion {
		fmt.Printf("%s version %s\n", AppName, version)
		return nil, ErrDone
	}

	if pre.AppDataDir != "" {
		pre.AppDataDir = cleanAndExpandPath(pre.AppDataDir)
	} else {
		pre.AppDataDir = DefaultAppDataDir
	}
	if pre.ConfigFile == "" {
		pre.ConfigFile = filepath.Join(pre.AppDataDir, DefaultConfigFilename)
	}
	pre.ConfigFile = cleanAndExpandPath(pre.ConfigFile)

	cfg := defaults()
	cfg.AppDataDir = pre.AppDataDir
	cfg.ConfigFile = pre.ConfigFile

	// Write a documented config file the first time this runs somewhere, so
	// there is always a file to edit rather than a manual to find. Unlike dcrd
	// this is not restricted to the default location: two games on one machine
	// each run from their own --appdata, and neither would ever get one.
	// A failure is non-fatal - it costs the example, not the run.
	if !fileExists(cfg.ConfigFile) {
		if err := createDefaultConfigFile(cfg.ConfigFile); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating a default config file: %v\n", err)
		}
	}

	parser := flags.NewParser(cfg, flags.HelpFlag|flags.PassDoubleDash)
	if _, err := os.Stat(cfg.ConfigFile); err == nil {
		if err := flags.NewIniParser(parser).ParseFile(cfg.ConfigFile); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", cfg.ConfigFile, err)
		}
	}

	// Second pass over the same parser, so anything typed on the line beats
	// what the file said.
	if _, err := parser.ParseArgs(args); err != nil {
		return nil, err
	}

	if cfg.TestNet && cfg.SimNet {
		return nil, errors.New("--testnet and --simnet cannot both be set")
	}

	if cfg.AppDataDir == "" {
		cfg.AppDataDir = DefaultAppDataDir
	}
	cfg.AppDataDir = cleanAndExpandPath(cfg.AppDataDir)

	// The data directory is the application directory itself, flat, because
	// that is the shape of every directory this program has already written:
	// identity.json and bridge.json sit at the top with sessions/ and the rest
	// beside them. Rooting it at a network subdirectory would leave an
	// existing one unreadable and its coin unfindable.
	if cfg.DataDir == "" {
		cfg.DataDir = cfg.AppDataDir
	}
	cfg.DataDir = cleanAndExpandPath(cfg.DataDir)

	// The log directory is network-scoped, unlike the data directory: the
	// network is known here from the flags, and two chains' logs in one file
	// is the sort of thing nobody notices until they are reading it.
	if cfg.LogDir == "" {
		cfg.LogDir = filepath.Join(cfg.AppDataDir, "logs", cfg.Network())
	}
	cfg.LogDir = cleanAndExpandPath(cfg.LogDir)

	rollKB, err := parseLogSize(cfg.LogSize)
	if err != nil {
		return nil, err
	}
	cfg.RollSizeKB = rollKB

	if cfg.Bridge.partial() {
		return nil, errors.New("the [bridge] section names some of addr, clientcert, clientkey " +
			"and cert but not all four; a half-named bridge cannot be reached")
	}
	for _, p := range []*string{
		&cfg.Bridge.ClientCert, &cfg.Bridge.ClientKey, &cfg.Bridge.BridgeCert,
	} {
		*p = cleanAndExpandPath(*p)
	}

	if err := ensureDir(cfg.AppDataDir); err != nil {
		return nil, err
	}
	if err := ensureDir(cfg.DataDir); err != nil {
		return nil, err
	}
	if err := ensureDir(cfg.LogDir); err != nil {
		return nil, err
	}

	return cfg, nil
}

// ErrDone says the program was asked something it has now answered, and should
// stop without being treated as having failed.
var ErrDone = errors.New("nothing left to do")

// IsDone reports whether loading ended in an answered question rather than a
// failure: --version, or go-flags having written the help text itself.
func IsDone(err error) bool {
	if errors.Is(err, ErrDone) {
		return true
	}
	// go-flags returns a *flags.Error that neither wraps nor compares equal to
	// flags.ErrHelp, so the type is the only thing worth testing. Getting this
	// wrong sends --help to stderr and exits 1.
	var fe *flags.Error
	return errors.As(err, &fe) && fe.Type == flags.ErrHelp
}

// defaults leaves every path empty rather than pre-filling it.
//
// An empty field is the only way to tell "not given" from "given, and happens
// to equal the default", and the difference decides whether --appdata moves
// the config file and the logs with it or leaves them in the home directory.
func defaults() *Config {
	return &Config{
		DebugLevel: DefaultDebugLevel,
		LogSize:    DefaultLogSize,
		Listen:     DefaultListen,
	}
}

// parseLogSize reads dcrd's log size grammar: digits then a unit, where the
// unit is required. Parsed at 32 bits because the multiplication that follows
// would otherwise be able to wrap.
func parseLogSize(s string) (int64, error) {
	units := 0
	for units < len(s) && s[units] >= '0' && s[units] <= '9' {
		units++
	}
	if units == 0 || units == len(s) {
		return 0, fmt.Errorf("invalid logsize %q: expected a number and a unit, e.g. 10M", s)
	}
	size, err := strconv.ParseInt(s[:units], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid logsize %q: %w", s, err)
	}
	switch s[units:] {
	case "k", "K", "KiB":
	case "m", "M", "MiB":
		size <<= 10
	case "g", "G", "GiB":
		size <<= 20
	default:
		return 0, fmt.Errorf("invalid logsize %q: unit must be one of K, M or G", s)
	}
	return size, nil
}

func cleanAndExpandPath(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return filepath.Clean(os.ExpandEnv(path))
}

// fileExists reports whether the named path exists.
func fileExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

// createDefaultConfigFile writes the documented sample configuration to
// destPath, creating parent directories as needed. There is nothing to inject
// into it - the bridge is reached with mutual TLS rather than a password - so
// the sample is written verbatim, with every option commented at its default
// so the resulting file changes no behaviour.
func createDefaultConfigFile(destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return err
	}
	dest, err := os.OpenFile(destPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer dest.Close()
	_, err = dest.WriteString(sampleconfig.FileContents())
	return err
}

func ensureDir(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists but is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return nil
}
