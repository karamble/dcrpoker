package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
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

// The pre-agreed answers outlive the process.
//
// They can only be agreed once. The branch they spend needs every member's
// signature including the accusers', and an accuser will not sign once it has
// started claiming - so a seat that comes back without them cannot answer a claim
// at all, and presignRefreshes cannot make more because it needs a live hand.
func TestTheAnswersToAClaimSurviveARestart(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	// The answers are agreed on a tick, once the bonds are all posted.
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	if len(tbl.refresh) == 0 {
		t.Fatal("no answers were agreed while the table was live")
	}
	before := len(tbl.refresh)
	var sigs int
	for _, r := range tbl.refresh {
		sigs += len(r.sigs)
	}
	if sigs == 0 {
		t.Fatal("the answers carry no signatures, so there is nothing to lose")
	}

	dir := filepath.Dir(a.tables.store.dir)
	back := h.restart(t, dir, "tok-a").tables.m[terms.SID]
	if back == nil {
		t.Fatal("the table did not come back")
	}
	if got := len(back.refresh); got != before {
		t.Fatalf("came back with %d answers, agreed %d", got, before)
	}
	var kept int
	for _, r := range back.refresh {
		kept += len(r.sigs)
		if r.tx == nil || len(r.bond) == 0 {
			t.Fatal("an answer came back without its transaction or its bond script")
		}
	}
	if kept != sigs {
		t.Fatalf("came back with %d signatures, gathered %d", kept, sigs)
	}
}

// A claim waiting in the mempool is answered, without anybody saying so.
//
// answerClaim otherwise runs only on a claim frame, so an opponent who sends
// nothing accuses in silence. A confirmed output being spent by a mempool
// transaction is still in the confirmed set, so the confirmed-only view finds it
// while the mempool-aware one does not, and that disagreement is the claim.
func TestAClaimInTheMempoolIsAnswered(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	advance(t, h, 2, a, b)

	tbl := a.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	outpoint := tbl.bondedAt[seat]
	if outpoint == "" {
		outpoint = tbl.bonded[seat]
	}
	if outpoint == "" {
		t.Fatal("this seat has no bond, so there is nothing to claim")
	}
	if len(tbl.refresh) == 0 {
		t.Fatal("no answer was agreed, so answering cannot be observed")
	}

	h.mu.Lock()
	was := len(h.sent)
	h.pending[outpoint] = true
	h.mu.Unlock()

	a.watchOwnBond(context.Background())

	h.mu.Lock()
	now := len(h.sent)
	h.mu.Unlock()
	if now == was {
		t.Fatal("a claim spending our bond in the mempool went unanswered")
	}
}

// The first outpoint a seat announces is the one that counts.
//
// Each announcement is signed by the session key that holds the seat, so two of
// them naming different outpoints is that seat saying two different things about
// where its money is. Keeping the first makes it a fault every peer agrees on;
// taking the last let a seat make its own bond unclaimable, because a claim is
// built against whichever one the claimant happened to keep.
func TestASecondStakeFromOneSeatIsRefused(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	tbl := p.tables.m[terms.SID]
	theirs := theirSeat(t, tbl)
	dep, err := tbl.deposit(theirs, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}

	// Two payments to the same seat's own deposit script, so both announcements
	// verify against the chain and only their outpoints differ. That is what
	// equivocation on a stake looks like, and it is also what an honest seat
	// that paid twice looks like - which is why the rule is to keep one rather
	// than to judge.
	first := payTo(h, dep.PkScriptHex, "c1")
	second := payTo(h, dep.PkScriptHex, "c2")
	if first == second {
		t.Fatal("the chain gave one outpoint for two payments")
	}

	for _, outpoint := range []string{first, second} {
		fn, err := membership.SignFunding(terms, theirs, outpoint, other.Session)
		if err != nil {
			t.Fatalf("sign funding: %v", err)
		}
		deliverKind(t, p, terms, schema.KindFunded, schema.FundedFrom(fn))
	}

	if got := p.tables.m[terms.SID].funded[theirs]; got != first {
		t.Fatalf("the seat is funded at %s, want the first announcement %s", got, first)
	}
}
