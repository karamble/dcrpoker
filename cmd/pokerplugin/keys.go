package main

import (
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// identity is the only thing this process keeps between runs.
//
// It holds no Bison Relay identity and no wallet key - it is given a bearer
// token to reach the host with, and nothing else - so this is a seed of its
// own, generated here and never sent anywhere. What it produces are session
// keys: the keys a table's escrow scripts name and its actions are signed by.
type identity struct {
	seed []byte
}

type identityFile struct {
	SeedHex string `json:"seed_hex"`
}

// loadIdentity reads the seed under dir, creating one the first time.
func loadIdentity(dir string) (*identity, error) {
	path := filepath.Join(dir, "identity.json")

	blob, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f identityFile
		if err := json.Unmarshal(blob, &f); err != nil {
			return nil, fmt.Errorf("read identity: %w", err)
		}
		seed, err := hex.DecodeString(strings.TrimSpace(f.SeedHex))
		if err != nil || len(seed) != 32 {
			return nil, fmt.Errorf("identity seed is not 32 bytes of hex")
		}
		return &identity{seed: seed}, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("read identity: %w", err)
	}

	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate seed: %w", err)
	}
	seed := priv.Serialize()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	out, err := json.Marshal(identityFile{SeedHex: hex.EncodeToString(seed)})
	if err != nil {
		return nil, err
	}
	// Write and rename, so a crash leaves either no identity or a whole
	// one. A half-written seed would read as a different player.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write identity: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("write identity: %w", err)
	}
	return &identity{seed: seed}, nil
}

// sessionKeyTag domain-separates session keys from anything else this seed
// might one day be used for.
var sessionKeyTag = []byte("poker/table-session/v1")

// sessionKey derives the key this player sits at one table under.
//
// It is derived from the session rather than stored, so restarting reproduces
// the same key for the same table without keeping per-table state - which
// matters because losing it would mean losing the ability to sign for a seat
// the escrow scripts already name.
//
// A key per table rather than one for everything: a table's members learn each
// other's session keys by design, so reusing one across tables would link them
// and hand each table's members a way to watch the others.
func (id *identity) sessionKey(sid string) (*secp256k1.PrivateKey, error) {
	if strings.TrimSpace(sid) == "" {
		return nil, fmt.Errorf("no session to derive a key for")
	}
	mac := hmac.New(blake256.New, id.seed)
	mac.Write(sessionKeyTag)
	mac.Write([]byte(sid))
	sum := mac.Sum(nil)
	if len(sum) != 32 {
		return nil, fmt.Errorf("derived %d bytes, want 32", len(sum))
	}
	return secp256k1.PrivKeyFromBytes(sum), nil
}
