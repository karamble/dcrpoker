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

func fundedIn(out []outgoing) []outgoing {
	var f []outgoing
	for _, o := range out {
		if o.kind == schema.KindFunded {
			f = append(f, o)
		}
	}
	return f
}

// A stake is announced the moment it is paid, and every other peer refuses it
// until the chain has confirmed it - which it has not, seconds after the
// payment was broadcast. Said once, that is a message guaranteed to be refused
// and never repeated, and the table would sit unfunded with the money gone.
func TestAPaidSeatKeepsSayingSo(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	ours, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("no seat")
	}
	at := int64(membership.BeaconHeight(terms)) + 1

	// Nothing paid, so nothing to say.
	if got := fundedIn(p.tables.tick(at)); len(got) != 0 {
		t.Fatalf("announced a stake %d times before paying one", len(got))
	}

	p.tables.mu.Lock()
	tbl.funded[ours] = strings.Repeat("5c", 32) + ":0"
	p.tables.mu.Unlock()

	if got := fundedIn(p.tables.tick(at + 1)); len(got) != 1 {
		t.Fatalf("a paid seat announced %d times, want once", len(got))
	}
	if got := fundedIn(p.tables.tick(at + 1)); len(got) != 0 {
		t.Fatal("announced twice at one height")
	}
	if got := fundedIn(p.tables.tick(at + 2)); len(got) != 1 {
		t.Fatal("a table still short of funded stopped saying where its stake is")
	}

	// Seeing every seat funded ourselves is NOT a reason to stop. The peer
	// that still needs to hear it is the one whose view differs from ours,
	// and we cannot see their view - stopping on our own count is how a live
	// table ended up half funded, with one side certain everybody was in.
	p.tables.mu.Lock()
	tbl.funded[theirSeat(t, tbl)] = strings.Repeat("6d", 32) + ":0"
	p.tables.mu.Unlock()
	if got := fundedIn(p.tables.tick(at + 3)); len(got) != 1 {
		t.Fatalf("stopped announcing because our own view said every seat was funded")
	}

	// The deadline is what stops it. Past there the stake no longer matters.
	if got := fundedIn(p.tables.tick(int64(membership.FundingDeadline(terms)))); len(got) != 0 {
		t.Fatal("still announcing past the funding deadline")
	}
}

// A table nobody finished paying for has to end, at a height every peer
// derives the same way.
func TestAnUnfundedTableIsGivenUpOn(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	deadline := int64(membership.FundingDeadline(terms))

	// A block early is still waiting, not still hoping.
	p.tables.tick(deadline - 1)
	if got := p.tables.snapshots()[0].State; got != membership.Settled.String() {
		t.Fatalf("gave up at %d when the deadline is %d (state %s)", deadline-1, deadline, got)
	}

	p.tables.tick(deadline)
	s := p.tables.snapshots()[0]
	if s.State != membership.Aborted.String() {
		t.Fatalf("state is %s at the funding deadline, want aborted", s.State)
	}
	if s.Reason == "" {
		t.Fatal("gave up on a table without saying why")
	}

	// The membership survives being given up on. Somebody who did pay needs
	// it to derive the script their refund spends.
	if _, ok := p.tables.m[terms.SID].form.MatchID(); !ok {
		t.Fatal("abandoning the table forgot the membership, and with it where the money is")
	}
}

// A table everyone paid for is not given up on, however long it sits.
func TestAFullyFundedTableSurvivesTheDeadline(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	ours, _ := tbl.form.OurSeat()
	theirs := theirSeat(t, tbl)

	dep, err := tbl.deposit(theirs, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	outpoint := strings.Repeat("9a", 32) + ":0"
	h.mu.Lock()
	h.bonds[outpoint] = dep.PkScriptHex
	h.mu.Unlock()
	fn, err := membership.SignFunding(terms, theirs, outpoint, other.Session)
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	deliverKind(t, p, terms, schema.KindFunded, schema.FundedFrom(fn))

	p.tables.mu.Lock()
	tbl.funded[ours] = strings.Repeat("7b", 32) + ":1"
	p.tables.mu.Unlock()

	p.tables.tick(int64(membership.FundingDeadline(terms)) + 5)
	if got := p.tables.snapshots()[0].State; got != membership.Settled.String() {
		t.Fatalf("a table with every seat funded was given up on (state %s)", got)
	}
}

// The host has to be able to show somebody where their money is going before
// it goes, and what the table has actually been paid.
func TestTheViewReportsWhereTheStakeGoes(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	s := p.tables.snapshots()[0]
	if s.Seat == nil {
		t.Fatal("a seated table reports no seat")
	}
	if s.DepositAddr == "" {
		t.Fatal("a settled table reports nowhere to pay")
	}
	if s.FundingDeadline != membership.FundingDeadline(terms) {
		t.Fatalf("reports a funding deadline of %d, want %d", s.FundingDeadline, membership.FundingDeadline(terms))
	}
	if s.Funded != 0 || s.Stake != "" {
		t.Fatalf("reports %d seats funded and a stake at %q before anybody paid", s.Funded, s.Stake)
	}

	// The address is the one this seat is actually required to pay.
	want, err := p.tables.m[terms.SID].deposit(*s.Seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if s.DepositAddr != want.DepositAddr {
		t.Fatalf("reports %s, but seat %d must pay %s", s.DepositAddr, *s.Seat, want.DepositAddr)
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
