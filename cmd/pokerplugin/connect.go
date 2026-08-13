package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
)

// How this game finds its bridge, and how it is told the first time.
//
// A bridge is somebody's own appliance, reached at an address only they know,
// and admitted to with a credential only they can issue. None of that can be
// discovered, and none of it should be typed again after the first time - so it
// is asked for once, checked by actually connecting, and written down.

// bridgeConfig is what was asked for on the first run.
//
// The certificate and key are stored as files beside it rather than inline, so
// the operator can replace a regenerated credential by dropping two files in
// without editing JSON.
type bridgeConfig struct {
	Addr string `json:"addr"`

	// Network is the chain this game was set up for. Checked against what
	// the bridge says at Hello, because a mismatch builds scripts nobody can
	// spend and pays real money into them.
	Network string `json:"network"`
}

const (
	bridgeConfigFile = "bridge.json"
	clientCertFile   = "client.cert"
	clientKeyFile    = "client.key"
	bridgeCertFile   = "bridge.cert"
)

// loadBridge reads the stored connection, or asks for one and stores it.
//
// in and out are the wizard's terminal. They are arguments rather than the real
// standard streams so the first-run path is testable, and so a caller with no
// terminal can be refused rather than left hanging on a read that never returns.
func loadBridge(dataDir string, in io.Reader, out io.Writer, interactive bool) (transport.BridgeConfig, error) {
	cfg, err := readBridgeConfig(dataDir)
	switch {
	case err == nil:
		return cfg, nil
	case !os.IsNotExist(err):
		return transport.BridgeConfig{}, err
	case !interactive:
		return transport.BridgeConfig{}, fmt.Errorf(
			"this game has not been connected to a bridge yet, and there is no terminal to ask on. "+
				"Run it once where you can answer, or write %s yourself",
			filepath.Join(dataDir, bridgeConfigFile))
	}
	return runWizard(dataDir, in, out)
}

// readBridgeConfig loads a stored connection and the credential beside it.
func readBridgeConfig(dataDir string) (transport.BridgeConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, bridgeConfigFile))
	if err != nil {
		return transport.BridgeConfig{}, err
	}
	var stored bridgeConfig
	if err := json.Unmarshal(raw, &stored); err != nil {
		return transport.BridgeConfig{}, fmt.Errorf("read %s: %w", bridgeConfigFile, err)
	}

	read := func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		return b, nil
	}
	clientCert, err := read(clientCertFile)
	if err != nil {
		return transport.BridgeConfig{}, err
	}
	clientKey, err := read(clientKeyFile)
	if err != nil {
		return transport.BridgeConfig{}, err
	}
	bridgeCert, err := read(bridgeCertFile)
	if err != nil {
		return transport.BridgeConfig{}, err
	}

	return transport.BridgeConfig{
		Addr:       stored.Addr,
		ClientCert: clientCert,
		ClientKey:  clientKey,
		BridgeCert: bridgeCert,
	}, nil
}

// storedNetwork reports which chain this game was configured for, if it has
// been configured. An empty answer means the flag decides.
func storedNetwork(dataDir string) string {
	raw, err := os.ReadFile(filepath.Join(dataDir, bridgeConfigFile))
	if err != nil {
		return ""
	}
	var stored bridgeConfig
	if json.Unmarshal(raw, &stored) != nil {
		return ""
	}
	return stored.Network
}

// runWizard asks for a bridge, proves it answers, and writes it down.
//
// It connects before it saves. A configuration that was written and then turned
// out not to work is worse than none: the next run reads it, fails somewhere
// further in, and the person has nothing to compare against.
func runWizard(dataDir string, in io.Reader, out io.Writer) (transport.BridgeConfig, error) {
	r := bufio.NewReader(in)
	fmt.Fprintf(out, `
This game has not been connected to a bridge yet.

In dcrpulse, open Bison Relay > Gaming, register a game called %q, and generate
its credential. That gives you three blocks of text and a port. Paste the paths
you saved them to below.

`, "poker")

	ask := func(prompt string) (string, error) {
		fmt.Fprintf(out, "%s: ", prompt)
		line, err := r.ReadString('\n')
		if err != nil && !(err == io.EOF && strings.TrimSpace(line) != "") {
			return "", fmt.Errorf("read %s: %w", prompt, err)
		}
		return strings.TrimSpace(line), nil
	}

	host, err := ask("Address of the machine running dcrpulse")
	if err != nil {
		return transport.BridgeConfig{}, err
	}
	port, err := ask("Gaming bridge port [8443]")
	if err != nil {
		return transport.BridgeConfig{}, err
	}
	if port == "" {
		port = "8443"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return transport.BridgeConfig{}, fmt.Errorf("%q is not a port number", port)
	}

	cfg := transport.BridgeConfig{Addr: host + ":" + port}
	for _, f := range []struct {
		prompt string
		into   *[]byte
	}{
		{"Path to this game's certificate", &cfg.ClientCert},
		{"Path to this game's private key", &cfg.ClientKey},
		{"Path to the bridge's certificate", &cfg.BridgeCert},
	} {
		path, err := ask(f.prompt)
		if err != nil {
			return transport.BridgeConfig{}, err
		}
		pem, err := os.ReadFile(expandHome(path))
		if err != nil {
			return transport.BridgeConfig{}, fmt.Errorf("read %s: %w", f.prompt, err)
		}
		*f.into = pem
	}

	network, err := ask("Chain [mainnet]")
	if err != nil {
		return transport.BridgeConfig{}, err
	}
	if network == "" {
		network = "mainnet"
	}

	fmt.Fprintf(out, "\nConnecting to %s...\n", cfg.Addr)
	if err := proveItWorks(cfg, network); err != nil {
		return transport.BridgeConfig{}, fmt.Errorf(
			"could not use that: %w\n\nNothing has been saved. Check the address and port, that the "+
				"bridge is switched on in dcrpulse, and that the credential has not been revoked", err)
	}

	if err := saveBridge(dataDir, cfg, network); err != nil {
		return transport.BridgeConfig{}, err
	}
	fmt.Fprintf(out, "Connected, and saved to %s. This will not be asked again.\n\n",
		filepath.Join(dataDir, bridgeConfigFile))
	return cfg, nil
}

// proveItWorks connects and introduces the game, so a wizard that says it
// worked has actually seen it work.
func proveItWorks(cfg transport.BridgeConfig, network string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	b, err := transport.Dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer b.Close()

	reply, err := b.Hello(ctx, network)
	if err != nil {
		return err
	}
	if reply.GetGame() == "" {
		return fmt.Errorf("the bridge did not say what this game is")
	}
	return nil
}

// saveBridge writes the connection and the credential beside the identity.
//
// The same discipline the seed already gets: 0600 on the files, 0700 on the
// directory, written to a temporary name and renamed, so a crash halfway
// through leaves the previous configuration rather than half of a new one.
func saveBridge(dataDir string, cfg transport.BridgeConfig, network string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(bridgeConfig{Addr: cfg.Addr, Network: network}, "", "  ")
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		body []byte
	}{
		{clientCertFile, cfg.ClientCert},
		{clientKeyFile, cfg.ClientKey},
		{bridgeCertFile, cfg.BridgeCert},
		{bridgeConfigFile, append(body, '\n')},
	} {
		path := filepath.Join(dataDir, f.name)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, f.body, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}

// expandHome makes ~ work, because a person typing a path will use it.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
