package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
)

// ladderOf is the deterministic ladder for one seat, plus the pieces a test
// needs to lay its steps onto the stand-in chain.
func ladderOf(t *testing.T, p *plugin, sid string, seat uint32) (positions, claimed []string, claimedHex string) {
	t.Helper()
	tbl := p.tables.m[sid]
	ladder, bond, err := tbl.bondLadder(seat)
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if len(ladder) < 2 {
		t.Fatalf("a ladder of %d cannot test movement", len(ladder))
	}
	for _, a := range ladder {
		positions = append(positions, a.TxIn[0].PreviousOutPoint.String())
		claimed = append(claimed, fmt.Sprintf("%s:0", a.TxHash()))
	}
	script, err := escrow.AccuseDraft{Bond: bond}.ClaimedScript()
	if err != nil {
		t.Fatalf("claimed script: %v", err)
	}
	return positions, claimed, hex.EncodeToString(script)
}

// A mined accusation is the window opening, not the window missed.
//
// The claimed bond's delay counts from the output the accusation creates, so an
// accusation seen only once confirmed still leaves the whole window to answer
// in. The bond outpoint leaving the confirmed set is therefore not "over" - the
// claimed output existing is what it looks like, and the answer spends it.
func TestAMinedAccusationIsAnswered(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	positions, claimed, claimedHex := ladderOf(t, a, terms.SID, seat)

	h.mu.Lock()
	h.spent[positions[0]] = true
	h.bonds[claimed[0]] = claimedHex
	was := len(h.sent)
	h.mu.Unlock()

	a.watchBonds(context.Background())

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) == was {
		t.Fatal("a mined accusation went unanswered")
	}
	got := h.sent[len(h.sent)-1]
	if spent := got.TxIn[0].PreviousOutPoint.String(); spent != claimed[0] {
		t.Fatalf("the answer spends %s, and the claimed bond is %s", spent, claimed[0])
	}
	if at := tbl.bondedAt[seat]; at != positions[1] {
		t.Fatalf("after answering the bond is believed at %q, and the answer put it at %s",
			at, positions[1])
	}
}

// A bond that moved while this peer was not running is found again, and the
// accusation against its new position is answered.
//
// bondedAt is not persisted, deliberately: the chain is re-asked rather than a
// note trusted. So a restarted peer believes its bond sits where it was first
// posted, which is spent - and everything that derives from the bond's position
// would build against a dead output until the walk moves it.
func TestTheSecondAnswerSurvivesARestart(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	positions, claimed, claimedHex := ladderOf(t, a, terms.SID, seat)
	bondHex := h.bonds[positions[0]]
	if bondHex == "" {
		t.Fatal("the bond's own script is not on the stand-in chain")
	}

	// The first accusation and its answer are on the chain, and nobody said so.
	h.mu.Lock()
	h.spent[positions[0]] = true
	h.bonds[claimed[0]] = claimedHex
	h.spent[claimed[0]] = true
	h.bonds[positions[1]] = bondHex
	h.mu.Unlock()

	dir := filepath.Dir(a.tables.store.dir)
	back := h.restart(t, dir, "tok-a")
	btbl := back.tables.m[terms.SID]
	if btbl == nil {
		t.Fatal("the table did not come back")
	}
	if at := btbl.bondedAt[seat]; at != "" {
		t.Fatalf("bondedAt came back as %q from disk; the walk is supposed to be the only writer", at)
	}

	back.watchBonds(context.Background())
	if at := btbl.bondedAt[seat]; at != positions[1] {
		t.Fatalf("after the walk the bond is believed at %q, and the chain has it at %s",
			at, positions[1])
	}

	// A second accusation, against the moved bond, mined while nobody said so.
	h.mu.Lock()
	h.spent[positions[1]] = true
	h.bonds[claimed[1]] = claimedHex
	was := len(h.sent)
	h.mu.Unlock()

	back.watchBonds(context.Background())

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sent) == was {
		t.Fatal("the second accusation went unanswered after a restart")
	}
	got := h.sent[len(h.sent)-1]
	if spent := got.TxIn[0].PreviousOutPoint.String(); spent != claimed[1] {
		t.Fatalf("the second answer spends %s, and the claimed bond is %s", spent, claimed[1])
	}
}

// A peer learns another seat's bond moved, and builds against where it is.
//
// Only the owner answers, so the answer's broadcast is the one thing the other
// seats never do themselves - but every later accusation and the release both
// build against the bond's position, and a peer that still names the original
// outpoint holds transactions nothing can spend.
func TestAPeerFindsAnotherSeatsMovedBond(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	ours, _ := tbl.form.OurSeat()
	theirs := theirSeat(t, tbl)
	positions, claimed, claimedHex := ladderOf(t, a, terms.SID, theirs)
	bondHex := h.bonds[positions[0]]

	h.mu.Lock()
	h.spent[positions[0]] = true
	h.bonds[claimed[0]] = claimedHex
	h.spent[claimed[0]] = true
	h.bonds[positions[1]] = bondHex
	was := len(h.sent)
	h.mu.Unlock()

	a.watchBonds(context.Background())

	h.mu.Lock()
	sent := len(h.sent)
	h.mu.Unlock()
	if sent != was {
		t.Fatal("answered a claim against somebody else's bond, which only its owner can do")
	}
	if at := tbl.bondedAt[theirs]; at != positions[1] {
		t.Fatalf("their bond is believed at %q, and the chain has it at %s", at, positions[1])
	}

	// What is derived from the position follows it: the next accusation spends
	// the moved bond and names what that output actually holds.
	d, _, err := tbl.accuseDraft(theirs)
	if err != nil {
		t.Fatalf("accuseDraft: %v", err)
	}
	if got := d.Prevout.String(); got != positions[1] {
		t.Fatalf("the next accusation spends %s, and the bond sits at %s", got, positions[1])
	}
	if want := int64(escrow.MinBondAtoms) - 2*claimFee; d.ValueAtoms != want {
		t.Fatalf("the next accusation names %d atoms, and the answered bond holds %d",
			d.ValueAtoms, want)
	}
	_ = ours
}
