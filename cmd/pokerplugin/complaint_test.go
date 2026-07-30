package main

import (
	"path/filepath"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// The dispute over a refused shuffle, end to end.
//
// The harness cannot fabricate a wedge over the wire, and that is itself the
// property: a middleman cannot corrupt a shuffle without breaking its
// signature. So these tests play the cheat the way impersonation_test does -
// signing hostile frames with keys the test holds - and assert the verdict,
// the naming, the void, and the money.

// bySeat sorts the two peers of a dealing table into seat order.
func bySeat(t *testing.T, sid string, a, b *plugin) (seat0, seat1 *plugin) {
	t.Helper()
	a.tables.mu.Lock()
	sa, ok := a.tables.m[sid].form.OurSeat()
	a.tables.mu.Unlock()
	if !ok {
		t.Fatal("peer a holds no seat")
	}
	if sa == 0 {
		return a, b
	}
	return b, a
}

// freshLogKey re-derives a seat's signing key with no sign book, which is what
// lets a test sign the equivocating frame an honest client never would.
func freshLogKey(t *testing.T, p *plugin, sid string) *forfeit.LogKey {
	t.Helper()
	p.tables.mu.Lock()
	tbl := p.tables.m[sid]
	match, ok := tbl.form.MatchID()
	priv := tbl.logPriv
	p.tables.mu.Unlock()
	if !ok {
		t.Fatal("no match")
	}
	key, err := forfeit.LogKeyFrom(priv, match)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	return key
}

// wedgeHandTwo plays hand one to a signed boundary, swallows every hand-two
// shuffle, and delivers to the seat-1 peer a copy of seat 0's real hand-two
// shuffle with its proof corrupted - signed, because the test holds seat 0's
// key. Returns the peers in seat order and the outgoing complaint.
func wedgeHandTwo(t *testing.T, h *hub, a, b *plugin, sid string) (seat0, seat1 *plugin, complaint schema.ShuffleComplaint) {
	t.Helper()
	h.drop(schema.KindShuffle, 64)
	playHand(t, h, sid, checkOrCall, a, b)
	waitSettled(t, sid, 1, a, b)
	seat0, seat1 = bySeat(t, sid, a, b)

	// Hand two opens on its card keys, which still flow; the shuffles do
	// not. Wait until seat 0 has produced its hand-two shuffle locally.
	var step deck.Step
	for range 200 {
		advance(t, h, 1, a, b)
		seat0.tables.mu.Lock()
		tbl := seat0.tables.m[sid]
		if tbl.playingHand() == 2 && tbl.play != nil {
			if hd := tbl.play.Hand(); hd != nil && len(hd.Steps()) > 0 {
				step = hd.Steps()[0]
			}
		}
		seat0.tables.mu.Unlock()
		if step.Deck != nil {
			break
		}
	}
	if step.Deck == nil {
		t.Fatal("seat 0 never produced its hand-two shuffle")
	}
	if h.dropped(schema.KindShuffle) == 0 {
		t.Fatal("no shuffle was lost, so the corrupted one would be a repeat")
	}

	seat1.tables.mu.Lock()
	match, _ := seat1.tables.m[sid].form.MatchID()
	terms := seat1.tables.m[sid].terms
	seat1.tables.mu.Unlock()

	bad := append([]byte(nil), step.Proof...)
	bad[len(bad)/2] ^= 0xff
	digest, err := driver.ShuffleFrameDigest(match, 2, 0, step.Deck, bad)
	if err != nil {
		t.Fatalf("frame digest: %v", err)
	}
	sig, err := freshLogKey(t, seat0, sid).SignCommitted(forfeit.DomainShuffle, 2, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, err := schema.ShuffleFrom(driver.OutShuffle{Seat: 0, Deck: step.Deck, Proof: bad, Sig: sig}, 2)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	out := deliverKind(t, seat1, terms, schema.KindShuffle, body)
	var c *schema.ShuffleComplaint
	for _, o := range out {
		if v, ok := o.body.(schema.ShuffleComplaint); ok {
			c = &v
		}
	}
	if c == nil {
		t.Fatalf("a signed shuffle with a bad proof produced %d frames and no complaint", len(out))
	}
	return seat0, seat1, *c
}

// A signed shuffle whose proof does not verify names its signer, on both
// sides, from the complaint alone - and the table pays the boundary it
// already signed, with the cheat's bond release withheld.
func TestADisputedBadProofNamesTheShuffler(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	sayWhereToPay(t, h, a, b)
	seat0, seat1, complaint := wedgeHandTwo(t, h, a, b, terms.SID)

	// The refuser judged on the spot.
	seat1.tables.mu.Lock()
	tbl1 := seat1.tables.m[terms.SID]
	v1, c1, over1 := tbl1.complJudged[2], tbl1.caughtCheating(0), tbl1.play.Over()
	cause1 := tbl1.play.VoidCause()
	seat1.tables.mu.Unlock()
	if v1 != complaintCheat || !c1 {
		t.Fatalf("the refuser judged %q with cheat=%v, want the shuffler named", v1, c1)
	}
	if !over1 || cause1 != driver.VoidWedge {
		t.Fatalf("the refuser's table is over=%v cause=%v, want a voided wedge", over1, cause1)
	}

	// The shuffler's own peer reaches the same verdict from the same bytes.
	deliverKind(t, seat0, terms, schema.KindShuffleComplaint, complaint)
	seat0.tables.mu.Lock()
	tbl0 := seat0.tables.m[terms.SID]
	v0, c0, over0 := tbl0.complJudged[2], tbl0.caughtCheating(0), tbl0.play.Over()
	seat0.tables.mu.Unlock()
	if v0 != complaintCheat || !c0 || !over0 {
		t.Fatalf("the shuffler's peer judged %q cheat=%v over=%v; the verdict did not travel", v0, c0, over0)
	}

	// The table settles at hand one - the boundary both signed - and only
	// the honest seat's bond comes back.
	for i, p := range []*plugin{seat0, seat1} {
		v := ledgerOf(t, p, terms.SID)
		if v.Settled == nil || v.Settled.Hand != 1 {
			t.Fatalf("peer %d settles at %+v, want the boundary at hand 1", i, v.Settled)
		}
	}
	waitPaid(t, h, a, b)
	if released := waitReleased(t, h, 1, a, b); len(released) != 1 {
		t.Fatalf("%d bonds came back, want only the honest seat's", len(released))
	}
}

// A complaint about a shuffle whose proof verifies names the complainer: the
// judge re-runs the proof from the complaint alone, and a refusal of a valid
// proof is a lie or a broken verifier, which the protocol does not need to
// tell apart.
func TestAFalseComplaintNamesTheComplainer(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	seat0, seat1 := bySeat(t, terms.SID, a, b)

	// Seat 0's real hand-one shuffle, from its own produced cache - deck,
	// proof and the signature it really made.
	var real *driver.OutShuffle
	seat0.tables.mu.Lock()
	tbl0 := seat0.tables.m[terms.SID]
	match, _ := tbl0.form.MatchID()
	for _, m := range tbl0.play.Republish() {
		if sh, ok := m.Out.(driver.OutShuffle); ok && m.Hand == 1 {
			real = &sh
		}
	}
	input, err := tbl0.play.Hand().PriorDeck(0)
	seat0.tables.mu.Unlock()
	if err != nil {
		t.Fatalf("prior deck: %v", err)
	}
	if real == nil {
		t.Fatal("seat 0 holds no produced shuffle to complain about")
	}

	// Seat 1 disputes it anyway, correctly signed.
	refusedDigest, err := driver.ShuffleFrameDigest(match, 1, 0, real.Deck, real.Proof)
	if err != nil {
		t.Fatalf("frame digest: %v", err)
	}
	digest, err := driver.ShuffleComplaintDigest(match, 1, 1, 0, input, refusedDigest)
	if err != nil {
		t.Fatalf("complaint digest: %v", err)
	}
	sig, err := freshLogKey(t, seat1, terms.SID).Sign(forfeit.DomainShuffleComplaint, 1, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, err := schema.ShuffleComplaintFrom(1, 0, 1, 0, input, real.Deck, real.Proof, real.Sig, sig)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	deliverKind(t, seat0, terms, schema.KindShuffleComplaint, body)
	seat0.tables.mu.Lock()
	v, named, over := tbl0.complJudged[1], tbl0.caughtCheating(1), tbl0.play.Over()
	cause := tbl0.play.VoidCause()
	seat0.tables.mu.Unlock()
	if v != complaintFalse || !named {
		t.Fatalf("judged %q named=%v, want the complainer named", v, named)
	}
	if !over || cause != driver.VoidWedge {
		t.Fatalf("over=%v cause=%v, want the hand voided on the proof", over, cause)
	}
}

// Settlement at hand zero - the stakes back - is reachable exactly when the
// table is over for a proven or unanimous reason, and never as an ordinary
// path.
func TestHandZeroSettlementNeedsAProvenVoid(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	sayWhereToPay(t, h, a, b)

	a.tables.mu.Lock()
	defer a.tables.mu.Unlock()
	tbl := a.tables.m[terms.SID]

	if _, err := tbl.settleDraft(); err == nil {
		t.Fatal("a live table mid-hand-one drafted a settlement at hand zero")
	}
	tbl.play.VoidWedgedHand()
	draft, err := tbl.settleDraft()
	if err != nil {
		t.Fatalf("a proven void could not draft the stakes back: %v", err)
	}
	for seat, amount := range draft.Amounts {
		if amount != int64(terms.BuyInAtoms) {
			t.Fatalf("seat %d's stakes-back is %d, want %d", seat, amount, terms.BuyInAtoms)
		}
	}
}

// The verdict, the naming and the evidence survive a restart: the judged
// dispute refuses to reopen, the cheat's release stays withheld, and the
// complainer keeps saying its complaint.
func TestAComplaintSurvivesARestart(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	_, seat1, _ := wedgeHandTwo(t, h, a, b, terms.SID)

	dir := filepath.Dir(seat1.tables.store.dir)
	tok := "tok-a"
	if seat1 == b {
		tok = "tok-b"
	}
	back := h.restart(t, dir, tok)
	tbl := back.tables.m[terms.SID]
	if tbl == nil {
		t.Fatal("the table did not come back")
	}
	if got := tbl.complJudged[2]; got != complaintCheat {
		t.Fatalf("the verdict came back as %q", got)
	}
	if !tbl.caughtCheating(0) {
		t.Fatal("the named cheat was laundered by the restart")
	}
	back.tables.mu.Lock()
	repeats := tbl.repeatComplaints()
	back.tables.mu.Unlock()
	if len(repeats) != 1 || repeats[0].kind != schema.KindShuffleComplaint {
		t.Fatalf("the restarted complainer repeats %d frames, want its complaint", len(repeats))
	}
}