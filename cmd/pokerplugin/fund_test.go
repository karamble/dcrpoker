package main

import (
	"strings"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// seatedTable builds a table that has settled and been seated, which is the
// earliest point at which any deposit script exists.
func seatedTable(t *testing.T, h *hub, inv schema.Invite, other membership.Credentials) (*plugin, membership.Terms) {
	t.Helper()
	terms := inviteTerms(inv)

	p := h.restart(t, t.TempDir(), "tok")
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

	if got := p.tables.m[terms.SID].form.State(); got != membership.Settled {
		t.Fatalf("table is %s, want settled before funding", got)
	}
	return p, terms
}

// theirSeat reports the seat this peer does not hold.
func theirSeat(t *testing.T, tbl *table) uint32 {
	t.Helper()
	ours, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("this peer holds no seat")
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		t.Fatal("table is not seated")
	}
	for seat := range seats {
		if seat != ours {
			return seat
		}
	}
	t.Fatal("no other seat at this table")
	return 0
}

// A stake counts when the chain says it is there, not when somebody says so.
func TestASeatIsFundedWhenTheChainAgrees(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	seat := theirSeat(t, tbl)
	dep, err := tbl.deposit(seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// A payment to that seat's deposit script, as the chain would report it.
	outpoint := strings.Repeat("e1", 32) + ":0"
	h.mu.Lock()
	h.bonds[outpoint] = dep.PkScriptHex
	h.mu.Unlock()

	fn, err := membership.SignFunding(terms, seat, outpoint, other.Session)
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	deliverKind(t, p, terms, schema.KindFunded, schema.FundedFrom(fn))

	if got := p.tables.m[terms.SID].funded[seat]; got != outpoint {
		t.Fatalf("seat %d is funded at %q, want %q", seat, got, outpoint)
	}
}

// An announcement pointing somewhere the chain does not back is refused. This
// is the whole reason the announcement is not believed on its own.
func TestAStakeTheChainCannotSeeIsRefused(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	seat := theirSeat(t, p.tables.m[terms.SID])

	// Signed correctly, by the right key, for the right seat - and naming an
	// output that pays nothing.
	outpoint := strings.Repeat("ab", 32) + ":0"
	fn, err := membership.SignFunding(terms, seat, outpoint, other.Session)
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	deliverKind(t, p, terms, schema.KindFunded, schema.FundedFrom(fn))

	if got := p.tables.m[terms.SID].funded[seat]; got != "" {
		t.Fatalf("recorded seat %d as funded at %q on nothing but an announcement", seat, got)
	}
}

// A stake paying the wrong script is refused even though the coin is real -
// otherwise a member could point at somebody else's output, or at one paying a
// script the table never agreed.
func TestAStakePayingAnotherScriptIsRefused(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	seat := theirSeat(t, tbl)
	ours, _ := tbl.form.OurSeat()

	// Our own seat's deposit script: real coin, wrong seat.
	mine, err := tbl.deposit(ours, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	outpoint := strings.Repeat("cd", 32) + ":0"
	h.mu.Lock()
	h.bonds[outpoint] = mine.PkScriptHex
	h.mu.Unlock()

	fn, err := membership.SignFunding(terms, seat, outpoint, other.Session)
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	deliverKind(t, p, terms, schema.KindFunded, schema.FundedFrom(fn))

	if got := p.tables.m[terms.SID].funded[seat]; got != "" {
		t.Fatalf("seat %d was funded by an output paying another seat's script (%q)", seat, got)
	}
}

// Only the member who holds a seat can say where that seat's stake is.
func TestOnlyTheSeatHolderCanFundIt(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	stranger := h.lend(t, "ee")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	seat := theirSeat(t, tbl)
	dep, err := tbl.deposit(seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// Real coin, paying the right script - announced by the wrong key.
	outpoint := strings.Repeat("f0", 32) + ":0"
	h.mu.Lock()
	h.bonds[outpoint] = dep.PkScriptHex
	h.mu.Unlock()

	fn, err := membership.SignFunding(terms, seat, outpoint, stranger.Session)
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	deliverKind(t, p, terms, schema.KindFunded, schema.FundedFrom(fn))

	if got := p.tables.m[terms.SID].funded[seat]; got != "" {
		t.Fatalf("a key that does not hold seat %d funded it (%q)", seat, got)
	}
}

// Funding cannot start before there is a seat to fund.
func TestFundingWaitsForTheTableToSettle(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	other := h.lend(t, "dd")

	p := h.restart(t, t.TempDir(), "tok")
	acceptInvite(t, p, inv)

	if _, _, _, _, err := p.tables.ourDeposit(terms.SID); err == nil {
		t.Fatal("derived a deposit for a table that has not settled")
	}

	deliverJoin(t, p, terms, other)
	p.tables.tick(int64(terms.Until) + 1)

	// Bound, but nobody else has committed and no seating is drawn.
	if _, _, _, _, err := p.tables.ourDeposit(terms.SID); err == nil {
		t.Fatal("derived a deposit for a table that has not been seated")
	}
}
