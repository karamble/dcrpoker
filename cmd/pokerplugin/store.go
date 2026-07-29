package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// record is what has to survive this process restarting.
//
// Not for convenience: for correctness. A session key is derived from the seed
// and the session, so restarting re-derives the same key - and a peer that
// committed, restarted, and then bound to some other membership under the same
// session would have signed two contradictory commits with one key. That is
// exactly the self-contained proof of equivocation the protocol relies on, so
// an honest player would have framed themselves by rebooting.
//
// Keeping the joins as well means a restart can resume rather than merely
// refuse: rebinding the same membership reproduces the same commit, byte for
// byte, because signing is deterministic.
//
// Our own commit reproduces itself that way, but nobody else's does. Settling
// needs a commit from every member, and those arrived as messages that will not
// be sent again - so a peer that kept only its own came back holding one
// signature short of a table it had already agreed, and waited for it forever.
// The beacon is here for the same reason: it is drawn once, and a table that
// has been dealt must not be reseated by a later reading of that height.
type record struct {
	Terms   schema.Terms    `json:"terms"`
	Joins   []schema.Join   `json:"joins"`
	Commits []schema.Commit `json:"commits,omitempty"`
	Beacon  string          `json:"beacon,omitempty"` // hex of the block the seats were drawn from
	// Funded is the output each seat's stake sits in, once the chain has
	// been asked. Keeping it means a restart does not go back to the chain
	// for every seat, and - for our own seat - does not pay a second time
	// because it forgot it had already paid.
	Funded map[uint32]string `json:"funded,omitempty"`
	// Bound records that this key took an irrevocable position, and Roster
	// which one. Aborted records that the session ended.
	Bound   bool   `json:"bound"`
	Roster  string `json:"roster,omitempty"`
	Aborted bool   `json:"aborted,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Finished records that this player got up and the table is kept only
	// because it still holds their money. Without it a restart would bring
	// a table this player has left back as a live one.
	Finished bool `json:"finished,omitempty"`
	// GCID is the conversation this table's frames belong to. Kept so a
	// table read back at startup is complete rather than nearly so; a
	// finished one sends nothing, but a record that cannot say where it
	// came from is a worse record.
	GCID string `json:"gcid,omitempty"`
	// Bonded is where each seat's table bond sits, the counterpart to
	// Funded. Both are needed to answer "does this table still hold any of
	// my money", which is the question that decides whether it is kept.
	Bonded map[uint32]string `json:"bonded,omitempty"`
	// UIDs is which Bison Relay identity delivered each seat's own funding
	// announcement. Display only - it labels seats with names, and nothing
	// that moves money ever reads it - but it is kept because the message
	// that taught it arrives once and a restart should not cost the labels.
	UIDs map[uint32]string `json:"uids,omitempty"`
	// Dealt records that this table has opened a hand. A restart must not
	// open one again: hand numbers are signing positions, and re-signing one
	// with a different deck publishes this seat's log key.
	Dealt bool `json:"dealt,omitempty"`
	// Signed is which positions the log key has put its name to, so the
	// refusal in pkg/forfeit survives a restart. Keys are "domain/seq".
	Signed map[string]string `json:"signed,omitempty"`
	// Refreshes are the pre-agreed answers to a claim. They are agreed once,
	// while everybody is still talking, and can never be agreed again: the
	// branch needs the accusers' signatures and they will not sign once they
	// have started claiming. A restart without them is a seat that cannot
	// answer.
	Refreshes []recordedRefresh `json:"refreshes,omitempty"`
}

// recordedRefresh is one answer and the signatures gathered for it.
type recordedRefresh struct {
	Seat uint32 `json:"seat"`
	Tx   string `json:"tx"`
	Bond string `json:"bond"`
	// Sigs is signer pubkey hex to signature hex.
	Sigs map[string]string `json:"sigs"`
}

// sidRe is the shape a session id takes, repeated here because this builds a
// filename out of one. It must stay the shape the invite grammar accepts.
var sidRe = regexp.MustCompile(`^[0-9a-f]{1,32}$`)

// store keeps one record per session under the data directory.
type store struct{ dir string }

func newStore(dataDir string) *store {
	return &store{dir: filepath.Join(dataDir, "sessions")}
}

func (s *store) path(sid string) (string, error) {
	// The session id is checked against the invite grammar before it ever
	// reaches here, so it is lowercase hex - but this builds a filename
	// from it, and a path that trusts its input is worth refusing to
	// write at all.
	if !sidRe.MatchString(sid) {
		return "", fmt.Errorf("session id %q is not a name this can store", sid)
	}
	return filepath.Join(s.dir, sid+".json"), nil
}

// used reports whether this player has ever sat at a table.
//
// It answers one question: whether the seed underneath is safe to replace. A
// seed swapped under sessions that already exist would change the keys those
// tables were agreed with, so the records would name a player that no longer
// exists and the commits in them could never be reproduced.
func (s *store) used() (bool, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read sessions: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			return true, nil
		}
	}
	return false, nil
}

// sessions lists every session this player has a record for.
func (s *store) sessions() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if sid := strings.TrimSuffix(name, ".json"); sidRe.MatchString(sid) {
			out = append(out, sid)
		}
	}
	return out, nil
}

// saveTranscript keeps a finished table's signed log.
//
// The record says where the money is; this says how it got there. It is written
// when a table stops being played, because the log lives in memory and a
// restart would otherwise leave a receipt with no evidence behind it - and the
// entries are exactly what a dispute is argued from.
func (s *store) saveTranscript(sid string, blob []byte) error {
	path, err := s.logPath(sid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	return nil
}

// loadTranscript reads back a finished table's log, or nil when there is none.
func (s *store) loadTranscript(sid string) ([]byte, error) {
	path, err := s.logPath(sid)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	return blob, nil
}

func (s *store) logPath(sid string) (string, error) {
	if !sidRe.MatchString(sid) {
		return "", fmt.Errorf("session id %q is not a name this can store", sid)
	}
	return filepath.Join(s.dir, "..", "logs", sid+".json"), nil
}

// load reads a session's record, returning nil when there is none.
func (s *store) load(sid string) (*record, error) {
	path, err := s.path(sid)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	var rec record
	if err := json.Unmarshal(blob, &rec); err != nil {
		// A record that cannot be read is worse than none: it may say
		// this key already committed. Refusing beats guessing.
		return nil, fmt.Errorf("read session %s: %w", sid, err)
	}
	return &rec, nil
}

// spendPath is where one asked-for payment is written down.
func (s *store) spendPath(id string) (string, error) {
	// Built into a filename, so it is checked rather than trusted. Spend
	// ids come from the host, which is not the same as coming from us.
	if !spendIDRe.MatchString(id) {
		return "", fmt.Errorf("payment id %q is not a name this can store", id)
	}
	return filepath.Join(s.dir, "..", "spends", id+".json"), nil
}

var spendIDRe = regexp.MustCompile(`^[0-9a-zA-Z_-]{1,64}$`)

// saveSpend writes down a payment that has been asked for.
//
// Before the answer arrives, not after. A payment approved while this process
// was restarting used to leave money moved with nothing pointing at it,
// recoverable only by somebody who knew to call /table/deposit/set - which is
// tolerable when a person is watching a curl and not when a panel is showing
// "pending".
func (s *store) saveSpend(p *pendingSpend) error {
	path, err := s.spendPath(p.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create spend directory: %w", err)
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("write payment: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write payment: %w", err)
	}
	return nil
}

// loadSpends reads back every payment this process asked for.
func (s *store) loadSpends() ([]*pendingSpend, error) {
	dir := filepath.Join(s.dir, "..", "spends")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read payments: %w", err)
	}
	var out []*pendingSpend
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p pendingSpend
		if err := json.Unmarshal(blob, &p); err != nil || p.ID == "" {
			// One unreadable record is not a reason to forget the
			// others, and there is nothing here that a guess would
			// improve.
			continue
		}
		out = append(out, &p)
	}
	return out, nil
}

// save writes a session's record, whole or not at all.
func (s *store) save(sid string, rec *record) error {
	path, err := s.path(sid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeDurable(path, blob); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}

// writeDurable writes, flushes, renames and flushes the directory.
//
// A plain write-then-rename can leave a renamed empty file after a host crash,
// and what is being written here is the record of which positions this key has
// signed at. Losing that costs the bond, so it is worth the two syncs.
func writeDurable(path string, blob []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
