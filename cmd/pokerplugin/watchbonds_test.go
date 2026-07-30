package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
)

// ladderOf is the deterministic ladder for one seat, plus the pieces a test
// needs to lay its steps onto the stand-in chain.
func ladderOf(t *testing.T, p *plugin, sid string, seat uint32) (positions, claimed []string, claimedHex string) {
	t.Helper()
	tbl := p.tables.m[sid]
	r, err := tbl.bondLadder(seat)
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if len(r.accuse) < 2 {
		t.Fatalf("a ladder of %d cannot test movement", len(r.accuse))
	}
	// One more position than accusation, the last being where the ladder runs
	// out - see rungs.
	if len(r.positions) != len(r.claimed)+1 {
		t.Fatalf("%d positions for %d accusations, want one more", len(r.positions), len(r.claimed))
	}
	script, err := escrow.AccuseDraft{Bond: r.script}.ClaimedScript()
	if err != nil {
		t.Fatalf("claimed script: %v", err)
	}
	return r.positions, r.claimed, hex.EncodeToString(script)
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

// A bond that has run the whole ladder is found where it ends up, by every peer
// and not only by the one that answered.
//
// The last answer leaves the bond on a position no agreed accusation spends. The
// peer that answered records it; if the others could not walk to it they would go
// on believing the rung before, and every seat derives its release from that -
// so the release would be proposed against one outpoint and checked against
// another, and a bond that had been ground all the way down could only come back
// through its week-long backstop. Which is the one state a long disagreement
// actually reaches.
func TestABondAtTheEndOfItsLadderIsStillFound(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	sayWhereToPay(t, h, a, b)
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	theirs := theirSeat(t, tbl)
	positions, claimed, claimedHex := ladderOf(t, a, terms.SID, theirs)
	bondHex := h.bonds[positions[0]]
	last := len(claimed) - 1
	end := len(positions) - 1
	if end != last+1 {
		t.Fatalf("the ladder ends at position %d with %d accusations", end, len(claimed))
	}

	// Every accusation made and every one answered, so the bond has walked to
	// the end of its ladder with nobody saying anything.
	h.mu.Lock()
	for i := range claimed {
		h.spent[positions[i]] = true
		h.bonds[claimed[i]] = claimedHex
		h.spent[claimed[i]] = true
	}
	h.bonds[positions[end]] = bondHex
	h.mu.Unlock()

	a.watchBonds(context.Background())
	if at := tbl.bondedAt[theirs]; at != positions[end] {
		t.Fatalf("their ground down bond is believed at %q, and the chain has it at %s",
			at, positions[end])
	}

	// And a peer that already believes the end is not dropped for believing
	// something no accusation spends: it walks, finds the coin, and stays put.
	a.watchBonds(context.Background())
	if at := tbl.bondedAt[theirs]; at != positions[end] {
		t.Fatalf("a second walk moved a bond that had not moved, to %q", at)
	}

	// The release every seat derives now names the outpoint the coin is at,
	// which is what makes it something the others can co-sign. The amount
	// comes from the chain the way the real poll gets it, since moving the
	// bond is what forgets the old one.
	a.learnBondValues(context.Background())
	d, err := tbl.releaseDraft(theirs)
	if err != nil {
		t.Fatalf("release draft: %v", err)
	}
	if got := d.Prevout.String(); got != positions[end] {
		t.Fatalf("the release spends %s, and the bond sits at %s", got, positions[end])
	}
}

// A bond that is at no rung at all leaves the belief alone.
//
// Released, forfeited or swept: the walk runs out of ladder without finding the
// coin, and writing a position down then would be inventing one.
func TestABondThatIsGoneMovesNoBelief(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	theirs := theirSeat(t, tbl)
	positions, claimed, claimedHex := ladderOf(t, a, terms.SID, theirs)
	before := tbl.bondedAt[theirs]

	// Every rung spent and nothing at the end: the bond left the ladder.
	h.mu.Lock()
	for i := range claimed {
		h.spent[positions[i]] = true
		h.bonds[claimed[i]] = claimedHex
		h.spent[claimed[i]] = true
	}
	h.mu.Unlock()

	a.watchBonds(context.Background())
	if at := tbl.bondedAt[theirs]; at != before {
		t.Fatalf("a bond that is on no rung moved the belief to %q", at)
	}
}

// The accusation for a bond is the ladder's own entry for where it sits, at
// every rung, and there is none past the end.
//
// It used to be built a second time from a draft rather than looked up, which
// agreed with the ladder by construction and by nothing else: a fee or a draft
// field changing on one side would have given two peers different transactions
// to sign and nothing to say which was meant. Derived once, that cannot happen -
// and this says so, because "derived once" is only true while nobody adds a
// second derivation back.
func TestTheAccusationIsTheLadderEntryForWhereTheBondSits(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	theirs := theirSeat(t, tbl)
	r, err := tbl.bondLadder(theirs)
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}

	for i := range r.accuse {
		tbl.bondedAt[theirs] = r.positions[i]
		chain, script, err := tbl.accuseChain(theirs)
		if err != nil {
			t.Fatalf("rung %d: %v", i, err)
		}
		if len(chain) != 1 {
			t.Fatalf("rung %d gave %d accusations, want the one against this rung", i, len(chain))
		}
		if chain[0].TxHash() != r.accuse[i].TxHash() {
			t.Fatalf("rung %d builds a different accusation than the ladder holds", i)
		}
		if !bytes.Equal(script, r.script) {
			t.Fatalf("rung %d names a different bond script", i)
		}
	}

	// And the rung where the ladder runs out has no accusation at all, which
	// is what stops the grinding rather than a bond ground to dust.
	tbl.bondedAt[theirs] = r.positions[len(r.positions)-1]
	if _, _, err := tbl.accuseChain(theirs); err == nil {
		t.Fatal("the end of the ladder produced an accusation, so the grinding never stops")
	}
}
