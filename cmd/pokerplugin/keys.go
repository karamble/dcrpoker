package main

import (
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// identity is the only thing this process keeps between runs.
//
// It holds no Bison Relay identity and no wallet key - it is given a bearer
// token to reach the host with, and nothing else - so this is a seed of its
// own, generated here and never sent anywhere. What it produces are session
// keys: the keys a table's escrow scripts name and its actions are signed by.
type identity struct {
	mu   sync.Mutex
	dir  string
	seed []byte
	// bondOutpoint is the deposit backing every seat this player takes. It
	// is one per identity rather than one per table: a bond is the standing
	// cost of being somebody, and paying it again per table would price
	// playing rather than price existing.
	bondOutpoint string
}

type identityFile struct {
	SeedHex      string `json:"seed_hex"`
	BondOutpoint string `json:"bond_outpoint,omitempty"`
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
		return &identity{dir: dir, seed: seed, bondOutpoint: f.BondOutpoint}, nil
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
	return &identity{dir: dir, seed: seed}, nil
}

// bondKeyTag domain-separates the bond key from session keys. They are derived
// from one seed and must never be the same key: the bond is what a seat costs,
// and a session key is published to everyone at the table.
var bondKeyTag = []byte("poker/bond/v1")

// bondKey is the key this player's bond pays out to.
func (id *identity) bondKey() (*secp256k1.PrivateKey, error) {
	mac := hmac.New(blake256.New, id.seed)
	mac.Write(bondKeyTag)
	sum := mac.Sum(nil)
	if len(sum) != 32 {
		return nil, fmt.Errorf("derived %d bytes, want 32", len(sum))
	}
	return secp256k1.PrivKeyFromBytes(sum), nil
}

// bondScript is the timelocked script this player's bond must pay.
func (id *identity) bondScript() ([]byte, error) {
	priv, err := id.bondKey()
	if err != nil {
		return nil, err
	}
	return escrow.BondScript(priv.PubKey().SerializeCompressed(), escrow.MinBondBlocks)
}

// bondDeposit reports the outpoint backing this identity, if it has one.
func (id *identity) bondDeposit() string {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.bondOutpoint
}

// setBondDeposit records which deposit backs this identity.
func (id *identity) setBondDeposit(outpoint string) error {
	id.mu.Lock()
	defer id.mu.Unlock()

	if err := id.writeLocked(id.seed, outpoint); err != nil {
		return err
	}
	id.bondOutpoint = outpoint
	return nil
}

// writeLocked replaces the identity file. Write and rename, so a crash leaves
// either the old identity or the new one, never half of either.
func (id *identity) writeLocked(seed []byte, outpoint string) error {
	out, err := json.Marshal(identityFile{
		SeedHex:      hex.EncodeToString(seed),
		BondOutpoint: outpoint,
	})
	if err != nil {
		return err
	}
	path := filepath.Join(id.dir, "identity.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	return nil
}

// backup reports the secret everything this player owns is derived from.
//
// The bond key and every session key are HMACs of it. It is not derived from
// the wallet seed, it cannot be reconstructed, and it lives in one file in one
// volume - so a volume deleted without a copy of this makes the bond
// permanently unspendable, because the bond script names that key and no other,
// ever. Any stake still in escrow goes the same way: the refund branch is the
// depositor's own session key alone.
//
// Exporting it through the host is the right direction. The host is the trusted
// party in this arrangement; the game is the thing being confined.
func (id *identity) backup() (seedHex, bondOutpoint string) {
	id.mu.Lock()
	defer id.mu.Unlock()
	return hex.EncodeToString(id.seed), id.bondOutpoint
}

// restore puts back a seed saved from somewhere else.
//
// Only onto a player that has never sat down, which the caller establishes.
// Swapping the seed under an identity holding a bond would strand that bond
// where nothing can reach it, and under one with sessions would change the keys
// those tables were agreed with - leaving records naming a player that no
// longer exists, whose commits can never be reproduced.
func (id *identity) restore(seedHex, bondOutpoint string) error {
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != 32 {
		return fmt.Errorf("a seed is 32 bytes of hex")
	}
	outpoint := strings.TrimSpace(bondOutpoint)

	id.mu.Lock()
	defer id.mu.Unlock()
	if id.bondOutpoint != "" {
		return fmt.Errorf("this player already holds a bond, and restoring over it would strand that coin")
	}
	if err := id.writeLocked(seed, outpoint); err != nil {
		return err
	}
	id.seed, id.bondOutpoint = seed, outpoint
	return nil
}

// credentials are what this player sits down with.
func (id *identity) credentials(sid string) (membership.Credentials, error) {
	session, err := id.sessionKey(sid)
	if err != nil {
		return membership.Credentials{}, err
	}
	bondPriv, err := id.bondKey()
	if err != nil {
		return membership.Credentials{}, err
	}
	script, err := id.bondScript()
	if err != nil {
		return membership.Credentials{}, err
	}
	outpoint := id.bondDeposit()
	if outpoint == "" {
		return membership.Credentials{}, fmt.Errorf(
			"this player has no bond; fund one before taking a seat")
	}
	logKey, err := id.logKey(sid)
	if err != nil {
		return membership.Credentials{}, err
	}
	return membership.Credentials{
		Session:      session,
		Log:          logKey,
		Bond:         bondPriv,
		BondOutpoint: outpoint,
		BondScript:   script,
	}, nil
}

// sessionKeyTag domain-separates session keys from anything else this seed
// might one day be used for.
var sessionKeyTag = []byte("poker/table-session/v1")

// logKeyTag derives log keys from a different string than session keys, which
// is the point rather than tidiness: a log key is expected to become public if
// its owner equivocates, and one derivable from the other would take the stake
// with it.
var logKeyTag = []byte("poker/table-log/v1")

// logKey derives the key this player signs one table's log with.
//
// Derived rather than drawn at random, for the same reason the session key is:
// a client that restarted mid-table and came back signing with a different key
// would make every earlier entry unverifiable against the roster, which looks
// exactly like having cheated.
//
// Never the session key and never derivable from it. Log signatures take their
// nonce from the position in the log, so equivocating publishes this key - and
// a key that both did that and held the stake would mean a cheat hands out
// their own escrow instead of forfeiting a bond.
func (id *identity) logKey(sid string) (*secp256k1.PrivateKey, error) {
	if strings.TrimSpace(sid) == "" {
		return nil, fmt.Errorf("no session to derive a log key for")
	}
	mac := hmac.New(blake256.New, id.seed)
	mac.Write(logKeyTag)
	mac.Write([]byte(sid))
	sum := mac.Sum(nil)
	if len(sum) != 32 {
		return nil, fmt.Errorf("derived %d bytes, want 32", len(sum))
	}
	return secp256k1.PrivKeyFromBytes(sum), nil
}

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
