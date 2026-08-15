package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vctt94/dcrpoker/pkg/escrow"
	"github.com/vctt94/dcrpoker/pkg/membership"
)

// seatedPair brings two peers to a drawn seating, which is as far as every test
// here needs to get: bonds are placed by hand from there.
func seatedPair(t *testing.T) (*hub, *plugin, *plugin, membership.Terms) {
	t.Helper()
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	a, b := h.join(t, "tok-a"), h.join(t, "tok-b")

	acceptInvite(t, a, inv)
	acceptInvite(t, b, inv)
	waitFor(t, membership.Settled, a, b)

	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 11)
	}
	a.tables.seat(terms.SID, beacon)
	b.tables.seat(terms.SID, beacon)
	return h, a, b, terms
}

// A bond one confirmation deep is on its way, not missing.
//
// checkTableBond answered five different situations with one error, so every
// caller had to treat "arriving" as "never posted". Seen live: eight minutes of
// ERR and WRN against a peer whose only fault was a bond in the previous block.
func TestAConfirmingBondIsNotAMissingOne(t *testing.T) {
	h, a, _, terms := seatedPair(t)
	ctx := context.Background()

	_, bond, _, err := a.tables.ourBond(terms.SID)
	if err != nil {
		t.Fatalf("our bond: %v", err)
	}
	outpoint := payTo(h, bond.PkScriptHex, "70")

	h.mu.Lock()
	h.shallow[outpoint] = 1
	h.mu.Unlock()

	_, verdict, err := checkTableBond(ctx, a.tables.chain, outpoint, bond.PkScriptHex)
	if err == nil {
		t.Fatal("a bond one confirmation deep was accepted")
	}
	if verdict != bondConfirming {
		t.Fatalf("a bond one confirmation deep is %v, and it should be confirming", verdict)
	}

	// Nothing was ever paid into this one, which is the situation the old
	// message described. The two must not collapse into each other.
	absent := fmt.Sprintf("%s:0", strings.Repeat("71", 32))
	if _, v, err := checkTableBond(ctx, a.tables.chain, absent, bond.PkScriptHex); err == nil {
		t.Fatal("an outpoint nothing paid into was accepted")
	} else if v != bondAbsent {
		t.Fatalf("an outpoint nothing paid into is %v, and it should be absent", v)
	}

	// And the same outpoint is good once the chain catches up, so what is
	// being told apart is the depth and not the script.
	h.mu.Lock()
	delete(h.shallow, outpoint)
	h.mu.Unlock()

	if _, v, err := checkTableBond(ctx, a.tables.chain, outpoint, bond.PkScriptHex); err != nil {
		t.Fatalf("a bond deep enough was refused: %v", err)
	} else if v != bondGood {
		t.Fatalf("a bond deep enough is %v, and it should be good", v)
	}
}

// A bond in the mempool is on its way, not missing.
//
// The window has two halves and this is the first and longer one. Outpoint
// answers about confirmed coin only, so a broadcast bond is not found there at
// all - identical, to that lookup, to one nobody ever posted. Seen live: the
// confirmations half was handled and this half went on producing every ERR and
// WRN the fix was written to stop.
func TestABondInTheMempoolIsNotAMissingOne(t *testing.T) {
	h, a, b, terms := seatedPair(t)

	seat, bond, _, err := b.tables.ourBond(terms.SID)
	if err != nil {
		t.Fatalf("our bond: %v", err)
	}

	// Broadcast and not mined: the confirmed lookup misses it, the mempool
	// lookup finds it.
	outpoint := fmt.Sprintf("%s:0", strings.Repeat("73", 32))
	h.mu.Lock()
	h.unmined[outpoint] = bond.PkScriptHex
	h.mu.Unlock()

	out, err := b.recordOwnBond(terms.SID, seat, outpoint)
	if err != nil {
		t.Fatalf("record bond: %v", err)
	}
	b.publish(context.Background(), out)

	deadline := time.Now().Add(20 * time.Second)
	var got bondVerdict
	for time.Now().Before(deadline) {
		a.tables.mu.Lock()
		if tbl := a.tables.m[terms.SID]; tbl != nil {
			got = tbl.bondVerdicts[seat]
		}
		a.tables.mu.Unlock()
		if got != bondUnknown {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got == bondUnknown {
		t.Fatal("the announcement never reached the other seat, so nothing here was tested")
	}
	if got == bondAbsent {
		t.Fatal("a bond sitting in the mempool reads as one nobody posted")
	}
	if !got.arriving() {
		t.Fatalf("a bond in the mempool is %v, and it should be on its way", got)
	}

	// And the accusation path says so rather than calling the seat unbonded.
	a.tables.mu.Lock()
	defer a.tables.mu.Unlock()
	tbl := a.tables.m[terms.SID]
	if _, err := tbl.bondLadder(seat); !errors.Is(err, errBondConfirming) {
		t.Fatalf("a bond in the mempool reads as %q", err)
	}
}

// A seat nobody has asked about does not read as bonded.
//
// bondVerdicts is a map, so an unrecorded seat answers with the zero value. If
// that were bondGood, a caller would be told a bond is good on the strength of
// nothing ever having looked.
func TestTheZeroBondVerdictIsNotAGoodOne(t *testing.T) {
	var unasked bondVerdict
	if unasked == bondGood {
		t.Fatal("a seat nobody asked about reads as bonded")
	}
	if unasked != bondUnknown {
		t.Fatalf("the zero verdict is %v, and it should be unknown", unasked)
	}
}

// A seat's own accusations reach the table before its bond confirms.
//
// A seat writes its bond down when it broadcasts and accuses on it from the
// next tick, while every other peer waits two confirmations before writing
// anything down. So the accusation lands here first, and it is expected.
func TestAnAccusationArrivingBeforeItsBondIsNotAnError(t *testing.T) {
	_, a, _, terms := seatedPair(t)

	a.tables.mu.Lock()
	defer a.tables.mu.Unlock()
	tbl := a.tables.m[terms.SID]
	if tbl == nil {
		t.Fatal("no table")
	}
	ours, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("no seat")
	}
	other := 1 - ours

	// The state during the window: the peer has announced, the chain has been
	// asked, and it is not deep enough to write down.
	delete(tbl.bonded, other)
	tbl.bondVerdicts[other] = bondConfirming

	_, err := tbl.bondLadder(other)
	if err == nil {
		t.Fatal("a ladder was built for a bond that has not confirmed")
	}
	if !errors.Is(err, errBondConfirming) {
		t.Fatalf("a confirming bond reads as %q, and it should name the wait", err)
	}
}

// The case the warning was written for still refuses, and says so plainly.
//
// A table with no accusations agreed has no answer to somebody who stops, so a
// seat that never bonded has to stay loud. Over-broad suppression is the
// regression this fix invites.
func TestAMissingBondIsStillRefusedLoudly(t *testing.T) {
	_, a, _, terms := seatedPair(t)

	a.tables.mu.Lock()
	defer a.tables.mu.Unlock()
	tbl := a.tables.m[terms.SID]
	ours, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("no seat")
	}
	other := 1 - ours

	for _, v := range []bondVerdict{bondUnknown, bondAbsent, bondWrongScript, bondUnderfunded} {
		delete(tbl.bonded, other)
		tbl.bondVerdicts[other] = v

		_, err := tbl.bondLadder(other)
		if err == nil {
			t.Fatalf("%v: a ladder was built for a seat with no bond", v)
		}
		if errors.Is(err, errBondConfirming) {
			t.Fatalf("%v: reads as a bond that is merely confirming", v)
		}
		if !strings.Contains(err.Error(), "no bond on the chain") {
			t.Fatalf("%v: reads as %q, and it should say there is no bond", v, err)
		}
	}
}

// A confirming bond is not a good one, and must never stand in for it.
//
// The verdict exists to soften what is said, not what is required. Two
// confirmations still gate dealing, which is what
// TestASeatWillNotDealOnItsOwnUnconfirmedBond holds from the other side.
func TestAConfirmingBondStillCannotDeal(t *testing.T) {
	h, a, _, terms := seatedPair(t)
	ctx := context.Background()

	_, bond, _, err := a.tables.ourBond(terms.SID)
	if err != nil {
		t.Fatalf("our bond: %v", err)
	}
	outpoint := payTo(h, bond.PkScriptHex, "72")

	for deep := int64(0); deep < int64(escrow.BondConfirmations); deep++ {
		h.mu.Lock()
		h.shallow[outpoint] = deep
		h.mu.Unlock()

		value, v, err := checkTableBond(ctx, a.tables.chain, outpoint, bond.PkScriptHex)
		if err == nil {
			t.Fatalf("%d confirmations was accepted", deep)
		}
		if value != 0 {
			t.Fatalf("%d confirmations reported a value of %d", deep, value)
		}
		if v == bondGood {
			t.Fatalf("%d confirmations reads as good", deep)
		}
	}
}
