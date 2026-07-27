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
	// Bound records that this key took an irrevocable position, and Roster
	// which one. Aborted records that the session ended.
	Bound   bool   `json:"bound"`
	Roster  string `json:"roster,omitempty"`
	Aborted bool   `json:"aborted,omitempty"`
	Reason  string `json:"reason,omitempty"`
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}
