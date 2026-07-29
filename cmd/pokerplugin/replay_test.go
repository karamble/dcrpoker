package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// A restart must not open a hand this seat has already opened.
//
// Hand numbers are signing positions. Nothing about a played hand survives in the
// record - no deck, no card key - so opening hand one again would draw a fresh key
// and shuffle a different deck, and signing those at a position already used
// publishes this seat's log key. That is the key the bond's punishment branch pays
// on, so it would forfeit the restarting player's own bond.
//
// Two things stop it, and this pins both. receipt marks a table that had everything
// it needed to deal as finished, which startPlaying refuses - and it re-asserts that
// after resume, because resume sets finished from the record. The table also records
// that it dealt, as a fact rather than an inference from the funding, so the refusal
// does not rest on that inference alone.
func TestARestartWillNotOpenAHandTwice(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	tbl := a.tables.m[terms.SID]
	if tbl == nil || tbl.play == nil {
		t.Fatal("the table is not dealing, so this proves nothing")
	}
	if !tbl.dealt {
		t.Fatal("a table that dealt did not record that it had")
	}

	// The same directory is the same player coming back.
	dir := filepath.Dir(a.tables.store.dir)
	after := h.restart(t, dir, "tok-a")

	back := after.tables.m[terms.SID]
	if back == nil {
		t.Fatal("the table did not come back at all")
	}
	if !back.finished {
		t.Fatal("a table that had dealt came back live, so it would deal again")
	}
	if !back.dealt {
		t.Fatal("a table that had dealt came back not knowing it")
	}
	if got := back.startPlaying(); len(got) != 0 {
		t.Fatalf("a restart opened a hand again and sent %d messages", len(got))
	}
	if back.play != nil {
		t.Fatal("a restart built a driver for a table that had already dealt")
	}
}

// The signing book outlives the process, so a key read back at startup still
// refuses to sign a position it has used. Entries and checkpoints take the
// position nonce and are what this protects; the deck frames commit to their
// message instead and need no book.
func TestTheSigningBookSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	st := newStore(dir)

	rec := &record{Terms: schema.Terms{SID: "0123456789abcdef", Seats: 2}}
	rec.Signed = map[string]string{
		"entry/7": "aa" + strings.Repeat("00", 31),
	}
	if err := st.save(rec.Terms.SID, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	back, err := st.load(rec.Terms.SID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if back.Signed["entry/7"] != rec.Signed["entry/7"] {
		t.Fatalf("the book came back as %q", back.Signed["entry/7"])
	}
}
