package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// The tests here are all one lesson, learned by paying a real 0.01 DCR stake
// twice: money that was asked for and never answered is not money that was not
// spent. The host survives its own restart and the answer is still there
// afterwards, so the only thing that must not happen is asking again.

func TestAPaymentNobodyCouldAskAboutStaysOpen(t *testing.T) {
	st := newStore(t.TempDir())
	s := newSpends(st)

	s.put(&pendingSpend{ID: "a", Purpose: purposeStake, SID: "s1", State: "pending"})
	s.update("a", func(p *pendingSpend) {
		p.State = "unknown"
		p.Unreachable = "dial tcp: lookup dashboard: server misbehaving"
	})

	got, _ := s.get("a")
	if !got.open() {
		t.Fatal("a payment this process could not ask about was written off; " +
			"the host may well have paid it")
	}

	// And a restart has to pick it up, because the goroutine watching it is
	// gone with the process.
	back := newSpends(st).resume()
	if len(back) != 1 || back[0].ID != "a" {
		t.Fatalf("resume returned %v, want the unanswered payment", back)
	}
}

func TestARefusedPaymentIsDoneWith(t *testing.T) {
	st := newStore(t.TempDir())
	s := newSpends(st)

	s.put(&pendingSpend{ID: "b", Purpose: purposeStake, SID: "s1", State: "pending"})
	s.update("b", func(p *pendingSpend) {
		p.State = "denied"
		p.Error = "the person said no"
		p.Settled = true
	})

	if got, _ := s.get("b"); got.open() {
		t.Fatal("a payment the host actually refused is still being waited on")
	}
	if back := newSpends(st).resume(); len(back) != 0 {
		t.Fatalf("resume picked up %d refused payments, want none", len(back))
	}
}

func TestTheSameSeatIsNotPaidForTwice(t *testing.T) {
	s := newSpends(newStore(t.TempDir()))
	s.put(&pendingSpend{ID: "c", Purpose: purposeStake, SID: "s1", Seat: 1, State: "pending"})

	if _, ok := s.openFor(purposeStake, "s1", 1); !ok {
		t.Fatal("a stake already asked for did not stop a second request")
	}

	// A different seat, table, or purpose is a different payment.
	for _, c := range []struct {
		what    string
		purpose spendPurpose
		sid     string
		seat    uint32
	}{
		{"another seat", purposeStake, "s1", 0},
		{"another table", purposeStake, "s2", 1},
		{"the table bond", purposeTableBond, "s1", 1},
	} {
		if _, ok := s.openFor(c.purpose, c.sid, c.seat); ok {
			t.Errorf("%s was refused as a duplicate", c.what)
		}
	}

	// Once it is accounted for, the seat is answerable again - which is
	// what the already-paid checks at each call site are for.
	s.update("c", func(p *pendingSpend) { p.Recorded = true })
	if _, ok := s.openFor(purposeStake, "s1", 1); ok {
		t.Fatal("a recorded payment is still blocking its own seat")
	}
}

// A deposit address can be paid twice, and the second payment is nobody's
// stake: it is not in the record, so the ordinary refund cannot reach it. The
// route takes a named outpoint for exactly that, and the naming is the part
// that has to be checked - otherwise "take this back" becomes "sign whatever I
// am pointed at".
func TestOnlyOurOwnDepositCanBeNamedForRefund(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	ours, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("no seat")
	}
	mine, err := tbl.deposit(ours, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	theirs, err := tbl.deposit(theirSeat(t, tbl), testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}

	dup := strings.Repeat("b4", 32) + ":0"
	notOurs := strings.Repeat("b5", 32) + ":0"
	h.mu.Lock()
	h.bonds[dup] = mine.PkScriptHex
	h.bonds[notOurs] = theirs.PkScriptHex
	h.mu.Unlock()

	if err := p.paysOurDeposit(context.Background(), dup, mine.PkScriptHex); err != nil {
		t.Fatalf("a second payment into our own deposit script was refused: %v", err)
	}
	if err := p.paysOurDeposit(context.Background(), notOurs, mine.PkScriptHex); err == nil {
		t.Fatal("another seat's deposit could be named for our refund")
	}
	if err := p.paysOurDeposit(context.Background(), strings.Repeat("b6", 32)+":0",
		mine.PkScriptHex); err == nil {
		t.Fatal("an outpoint holding no coin could be named for our refund")
	}
}

// The one that would have made a bad day worse: a payment recovered late must
// not displace the stake the other seats have already checked and are playing
// against.
func TestARecoveredPaymentDoesNotDisplaceTheStake(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	payStake(t, h, p, terms, "01")
	tbl := p.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	real := tbl.funded[seat]
	if real == "" {
		t.Fatal("the seat was not funded")
	}

	dep, err := tbl.deposit(seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	// A second payment into the same script: real coin, ours, and not the
	// stake. This is what the abandoned request would have handed back.
	spare := payTo(h, dep.PkScriptHex, "02")

	req := &pendingSpend{ID: "d", Purpose: purposeStake, SID: terms.SID, Seat: seat,
		PkScript: dep.PkScriptHex}
	if err := p.recordSpend(req, spare); !errors.Is(err, errSurplus) {
		t.Fatalf("filing a second payment gave %v, want it refused as surplus", err)
	}
	if got := p.tables.m[terms.SID].funded[seat]; got != real {
		t.Fatalf("the seat's stake became %s, and the other seats are playing against %s", got, real)
	}
}

// Forming and funding a table takes as many blocks as the terms say, and this
// process can be restarted inside that window - by an upgrade, by a container
// coming back. It used to come back holding a table nobody could play and
// nobody had left: every funded table on a live box died to one restart.
func TestARestartDoesNotKillATableStillForming(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)
	other := h.lend(t, "dd")

	p := h.restart(t, dir, "tok")
	acceptInvite(t, p, inv)
	deliverJoin(t, p, terms, other)
	p.tables.tick(int64(terms.Until) + 1)

	bound := p.tables.snapshots()[0]
	c, err := membership.SignCommit(terms, rosterHashOf(t, bound.MatchID), other.Session)
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	deliverKind(t, p, terms, schema.KindCommit, schema.CommitFrom(c))
	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 3)
	}
	p.tables.seat(terms.SID, beacon)

	// Our stake is down. Nobody else's is, so no hand has been dealt and
	// there is no play state a restart could fail to rebuild.
	payStake(t, h, p, terms, "01")

	back := h.restart(t, dir, "tok")
	back.tables.resumeHeld(back.id)

	tbl := back.tables.m[terms.SID]
	if tbl == nil {
		t.Fatal("a funded table was not read back at all")
	}
	if tbl.finished {
		t.Fatal("a table still waiting for the other stakes came back finished, " +
			"so it will never deal and was never left")
	}
}

// The other half of the same rule: a table that had begun a hand cannot be put
// back into it, because no record holds a deck or a betting round.
func TestARestartDoesNotResumeATableMidHand(t *testing.T) {
	for _, c := range []struct {
		name string
		rec  *record
		want bool
	}{
		{"still funding", &record{Terms: schema.Terms{Seats: 2},
			Funded: map[uint32]string{0: "a:0"}}, false},
		{"funded, not all bonded", &record{Terms: schema.Terms{Seats: 2},
			Funded: map[uint32]string{0: "a:0", 1: "b:0"},
			Bonded: map[uint32]string{1: "c:0"}}, false},
		{"funded and bonded, so it dealt", &record{Terms: schema.Terms{Seats: 2},
			Funded: map[uint32]string{0: "a:0", 1: "b:0"},
			Bonded: map[uint32]string{0: "c:0", 1: "d:0"}}, true},
	} {
		if got := everDealt(c.rec); got != c.want {
			t.Errorf("%s: everDealt = %v, want %v", c.name, got, c.want)
		}
	}
}

// The one that stopped a live table with every stake and every bond already on
// the chain. A bond needs two confirmations and is announced the instant it is
// broadcast, so the first telling is always refused - and unlike the stake, it
// was never said again. Two peers sat waiting on a message nothing would send.
func TestABondIsSaidAgainUntilSomebodyTakesIt(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	seat, _ := p.tables.m[terms.SID].form.OurSeat()
	b, err := p.tables.m[terms.SID].bond(seat, testParams)
	if err != nil {
		t.Fatalf("bond: %v", err)
	}
	outpoint := payTo(h, b.PkScriptHex, "07")
	if _, err := p.recordOwnBond(terms.SID, seat, outpoint); err != nil {
		t.Fatalf("record bond: %v", err)
	}

	height := int64(terms.Until) + 2
	if got := p.tables.announceBondAgain(p.tables.m[terms.SID], height); len(got) == 0 {
		t.Fatal("a bond nobody has accepted was not said again")
	}
	// Once a block, not once a poll.
	if got := p.tables.announceBondAgain(p.tables.m[terms.SID], height); len(got) != 0 {
		t.Fatal("the bond was repeated twice in one block")
	}

	// Past the funding deadline it must still be repeated: a table short only
	// of a bond does not lapse, so giving up there is what left it stuck.
	late := int64(membership.FundingDeadline(terms)) + 10
	if got := p.tables.announceBondAgain(p.tables.m[terms.SID], late); len(got) == 0 {
		t.Fatal("the bond stopped being repeated while the table could still deal")
	}

	// And it must NOT stop merely because this peer has started dealing.
	// Dealing means our own view is complete, which is exactly the state in
	// which the other seat may still be waiting on us. Getting this wrong
	// left one box dealing and the other stuck at one bond of two.
	p.tables.m[terms.SID].play = &driver.Table{}
	if got := p.tables.announceBondAgain(p.tables.m[terms.SID], late+1); len(got) == 0 {
		t.Fatal("a dealing peer stopped repeating its bond, so a peer that " +
			"never got it has nothing left to wait for")
	}

	// It stops once this player is up from the table.
	p.tables.m[terms.SID].finished = true
	if got := p.tables.announceBondAgain(p.tables.m[terms.SID], late+2); len(got) != 0 {
		t.Fatal("a table this player left is still announcing its bond")
	}
}
