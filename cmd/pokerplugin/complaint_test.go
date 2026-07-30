package main

import (
	"path/filepath"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
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
// A table that is over cannot lapse. The chain-truth clearing empties the coin
// maps as the settlement and the releases confirm - correctly, and gated on
// the table being finished or over - and the lapse checks judge those same
// maps. A live table that played to its end was re-labelled "only 1 of 2 seats
// were funded" the moment its spent stake was forgotten; the lapse gate has to
// mirror the clearing gate, or every table anybody wins ends as a lie.
func TestATableThatIsOverCannotLapse(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	_ = b

	a.tables.mu.Lock()
	tbl := a.tables.m[terms.SID]
	tbl.play.VoidWedgedHand()
	// What the chain-truth clearing does once the payout and the releases
	// confirm.
	tbl.funded = map[uint32]string{}
	tbl.bonded = map[uint32]string{}
	a.tables.mu.Unlock()

	a.tables.tick(int64(membership.BondingDeadline(terms)) + 10)

	a.tables.mu.Lock()
	got := tbl.form.State()
	a.tables.mu.Unlock()
	if got != membership.Settled {
		t.Fatalf("a table that was over lapsed to %v with its maps emptied by the payout", got)
	}
}

// A table that has dealt takes no more money. Its funded and bonded entries
// are cleared once the chain pays the stake and releases the bonds, and
// without a guard the fund path would read the empty entry as "not yet paid"
// and ask for a second buy-in - recoverable only through the stake's own
// days-long refund. This is the live incident from table 0e1873cd, where a
// won, paid-out table sat live re-offering its deposit.
func TestADealtTableTakesNoMoreMoney(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	// The chain has paid out and released, so the maps are empty - exactly
	// what the outpoint watchers leave behind.
	a.tables.mu.Lock()
	tbl := a.tables.m[terms.SID]
	tbl.funded = map[uint32]string{}
	tbl.bonded = map[uint32]string{}
	a.tables.mu.Unlock()

	if _, _, _, _, err := a.tables.ourDeposit(terms.SID); err == nil {
		t.Fatal("a dealt table offered a second deposit")
	}
	if _, _, _, err := a.tables.ourBond(terms.SID); err == nil {
		t.Fatal("a dealt table offered a second bond")
	}
}

// A table whose game is over becomes a receipt on the next tick, whether or
// not anybody pressed leave. Before this, finished was set only by leave, so a
// table that played to its end and paid its winner sat live forever - the
// accusation warning never cleared, the rail walked back to "pay your stake",
// and the fund path re-offered a deposit once the chain emptied the maps. This
// is the tick that retires it.
func TestAnOverTableBecomesAReceiptOnTheNextTick(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)

	a.tables.mu.Lock()
	tbl := a.tables.m[terms.SID]
	if tbl.finished {
		t.Fatal("a dealing table is already finished")
	}
	tbl.play.VoidWedgedHand() // the game is over, nobody left
	a.tables.mu.Unlock()

	a.tables.tick(int64(terms.Until) + 100)

	a.tables.mu.Lock()
	fin := a.tables.m[terms.SID].finished
	a.tables.mu.Unlock()
	if !fin {
		t.Fatal("a table whose game ended did not become a receipt on the next tick")
	}
}

func complaintsIn(out []outgoing) int {
	n := 0
	for _, o := range out {
		if o.kind == schema.KindShuffleComplaint {
			n++
		}
	}
	return n
}

// A dispute is retransmitted once a block, not once a poll, and stops the
// moment the table settles. Dispute traffic is the one class exempt from the
// finished-tables-fall-silent rule, so without a bound it is the last place
// the unbounded per-poll repeat that buried a join can still happen.
func TestADisputeRepeatsOncePerBlockAndStopsAtSettlement(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	waitBetting(t, a, b)
	seat0, seat1, _ := wedgeHandTwo(t, h, a, b, terms.SID)
	_ = seat0

	// seat1 raised the complaint, so it is the one that repeats it.
	seat1.tables.mu.Lock()
	judged := seat1.tables.m[terms.SID].complJudged[2] != ""
	seat1.tables.mu.Unlock()
	if !judged {
		t.Fatal("the complaint was not judged")
	}

	at := int64(terms.Until) + 500
	if got := complaintsIn(seat1.tables.tick(at)); got != 1 {
		t.Fatalf("a fresh block produced %d complaint frames, want one", got)
	}
	if got := complaintsIn(seat1.tables.tick(at)); got != 0 {
		t.Fatalf("the same block produced %d more, want none", got)
	}
	if got := complaintsIn(seat1.tables.tick(at)); got != 0 {
		t.Fatalf("a third poll at one height produced %d, want none", got)
	}
	if got := complaintsIn(seat1.tables.tick(at + 1)); got != 1 {
		t.Fatalf("the next block produced %d complaint frames, want one", got)
	}

	// Once the boundary settlement co-signs, the complaint has done its job
	// and stops entirely.
	seat1.tables.mu.Lock()
	seat1.tables.m[terms.SID].settled = true
	seat1.tables.mu.Unlock()
	if got := complaintsIn(seat1.tables.tick(at + 2)); got != 0 {
		t.Fatalf("a settled table still repeated its complaint %d times", got)
	}
}
