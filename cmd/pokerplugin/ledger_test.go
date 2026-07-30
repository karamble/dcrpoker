package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// Two real peers, each holding only its own keys.
//
// Every other table in these tests is one peer with a stand-in for the other,
// which is enough to check what one peer does and no use at all for checking
// that two agree. A funded, bonded, dealing table needs both halves to be
// separately convinced by the chain, and the things this file is about - a
// bond that can be answered, a payout every seat has to name - are exactly
// the things a test holding both sets of keys would prove by construction.

// dealingTable brings two peers all the way to dealing: settled, seated, both
// stakes and both bonds on the chain, as each peer checked for itself.
func dealingTable(t *testing.T, h *hub) (*plugin, *plugin, membership.Terms) {
	t.Helper()
	inv := testInvite(2)
	terms := inviteTerms(inv)
	a, b := h.join(t, "tok-a"), h.join(t, "tok-b")

	acceptInvite(t, a, inv)
	acceptInvite(t, b, inv)
	waitFor(t, membership.Settled, a, b)

	// One block, drawn once, given to both. Two peers seating from different
	// blocks are seating different tables under one match id, so a helper
	// that let that happen would hide the fault it exists to avoid.
	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 11)
	}
	a.tables.seat(terms.SID, beacon)
	b.tables.seat(terms.SID, beacon)

	for i, p := range []*plugin{a, b} {
		payStake(t, h, p, terms, fmt.Sprintf("%02x", 0x40+i))
		payBond(t, h, p, terms, fmt.Sprintf("%02x", 0x60+i))
	}

	// A peer's own stake and bond are recorded when they are paid, not when
	// they confirm, so the poll that asks the chain about them has to run
	// before anybody deals. See confirmOurPayments.
	advance(t, h, 1, a, b)
	waitDealing(t, terms.SID, a, b)
	return a, b, terms
}

// payStake pays one peer's stake and tells the table, the way the funding
// handler does once a spend has been approved.
func payStake(t *testing.T, h *hub, p *plugin, terms membership.Terms, nonce string) {
	t.Helper()
	tbl := p.tables.m[terms.SID]
	seat, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("no seat")
	}
	dep, err := tbl.deposit(seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	outpoint := payTo(h, dep.PkScriptHex, nonce)

	out, err := p.recordOwnStake(terms.SID, seat, outpoint)
	if err != nil {
		t.Fatalf("record stake: %v", err)
	}
	p.publish(context.Background(), out)
}

// payBond posts one peer's forfeitable bond for the table.
func payBond(t *testing.T, h *hub, p *plugin, terms membership.Terms, nonce string) {
	t.Helper()
	seat, bond, _, err := p.tables.ourBond(terms.SID)
	if err != nil {
		t.Fatalf("our bond: %v", err)
	}
	outpoint := payTo(h, bond.PkScriptHex, nonce)

	out, err := p.recordOwnBond(terms.SID, seat, outpoint)
	if err != nil {
		t.Fatalf("record bond: %v", err)
	}
	p.publish(context.Background(), out)
}

// payTo puts an output on the hub's chain paying a script.
func payTo(h *hub, pkScriptHex, nonce string) string {
	outpoint := fmt.Sprintf("%s:0", strings.Repeat(nonce, 32))
	h.mu.Lock()
	h.bonds[outpoint] = pkScriptHex
	h.mu.Unlock()
	return outpoint
}

// waitDealing waits until every peer has started dealing, which each one
// decides for itself once it has seen every stake and every bond.
func waitDealing(t *testing.T, sid string, peers ...*plugin) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		done := true
		for _, p := range peers {
			p.tables.mu.Lock()
			tbl := p.tables.m[sid]
			if tbl == nil || tbl.play == nil {
				done = false
			}
			p.tables.mu.Unlock()
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			for i, p := range peers {
				s := p.tables.snapshots()[0]
				t.Logf("peer %d: state %s funded %d/%d bonded %d/%d dealing %v",
					i, s.State, s.Funded, s.Seats, s.Bonded, s.Seats, s.Dealing)
			}
			t.Fatal("the table never started dealing")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every seat gets a hand, not just the one that started first.
//
// The regression this pins is a deadlock that happened at every table. Each
// seat starts dealing when it has seen the last bond confirmed, and each sees
// that at a different moment; the first to start publishes its card key
// straight away, to a table where nobody else is dealing. Those frames used to
// be dropped on the grounds that "the sender repeats them", and nothing
// repeated them - so the first seat collected everybody's keys and everybody
// else was missing the first seat's, forever.
//
// It is invisible to a test with one real peer, because a stand-in does not
// start dealing at its own moment. That is the whole reason this file exists.
func TestEverySeatGetsTheHandAndNotJustTheFirst(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)

	deadline := time.Now().Add(30 * time.Second)
	for {
		dealt := 0
		var phases []string
		for _, p := range []*plugin{a, b} {
			code, body := get(t, p, "/table/hand?sid="+terms.SID)
			if code != http.StatusOK {
				continue
			}
			var v handView
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("decode hand: %v", err)
			}
			phases = append(phases, v.Phase)
			if v.Hand > 0 {
				dealt++
			}
		}
		if dealt == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 2 seats ever got a hand; phases %v", dealt, phases)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ledgerOf(t *testing.T, p *plugin, sid string) ledgerView {
	t.Helper()
	code, body := get(t, p, "/table/ledger?sid="+sid)
	if code != http.StatusOK {
		t.Fatalf("/table/ledger returned %d: %s", code, body)
	}
	var v ledgerView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode ledger: %v", err)
	}
	return v
}

// The money question has an answer long before the cards do.
//
// /table/hand refuses a table that is not dealing, and rightly: there is no
// hand. But there are stakes, a deposit address, a funding deadline and a
// roster, and somebody who has just paid into one wants to see exactly those.
// A ledger that also refused would leave the whole funding period blank.
func TestTheLedgerAnswersBeforeTheTableDeals(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	// The hand is not there, and says so.
	if code, _ := get(t, p, "/table/hand?sid="+terms.SID); code != http.StatusBadRequest {
		t.Fatalf("/table/hand returned %d for a table that is not dealing, want 400", code)
	}
	// The money is, and answers.
	v := ledgerOf(t, p, terms.SID)
	if v.SID != terms.SID {
		t.Fatalf("ledger is for %q, want %q", v.SID, terms.SID)
	}
	if len(v.Roster) != int(terms.Seats) {
		t.Fatalf("ledger names %d seats, want %d", len(v.Roster), terms.Seats)
	}
	if v.Settled != nil {
		t.Fatal("a table that never dealt reports a settled boundary")
	}
}

// Until every seat has said where to pay it, no claim at this table can be
// built at all - and before this the only sign of that was a claim that never
// appeared, or a co-signer that refused for reasons only its log recorded.
func TestTheLedgerNamesTheSeatsHoldingUpEveryClaim(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	v := ledgerOf(t, p, terms.SID)
	if len(v.PayoutsMissing) != int(terms.Seats) {
		t.Fatalf("%d seats are missing a payout address, want all %d: %v",
			len(v.PayoutsMissing), terms.Seats, v.PayoutsMissing)
	}

	// This player says where to pay it. That settles one seat and not the
	// other, because a payout is signed by the seat that owns it: nobody can
	// answer this on a neighbour's behalf.
	addr := payoutAddress(t, p)
	if code, body := post(t, p, "/payout/set", map[string]string{"address": addr}); code != http.StatusOK {
		t.Fatalf("/payout/set returned %d: %s", code, body)
	}

	ours, _ := p.tables.m[terms.SID].form.OurSeat()
	v = ledgerOf(t, p, terms.SID)
	if len(v.PayoutsMissing) != int(terms.Seats)-1 {
		t.Fatalf("after one seat answered, %d are still missing, want %d: %v",
			len(v.PayoutsMissing), terms.Seats-1, v.PayoutsMissing)
	}
	for _, seat := range v.PayoutsMissing {
		if seat == ours {
			t.Fatal("this seat set a payout address and is still reported as missing one")
		}
	}
	for _, s := range v.Roster {
		if s.Ours && s.Payout != addr {
			t.Fatalf("our seat reports payout %q, want %q", s.Payout, addr)
		}
	}
}

// The reading is read back, so a caller can tell "not set" from "set to
// something I cannot see".
func TestThePayoutAddressCanBeReadBack(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok")

	code, body := get(t, p, "/payout")
	if code != http.StatusOK {
		t.Fatalf("GET /payout returned %d: %s", code, body)
	}
	var before struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal([]byte(body), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if before.Address != "" {
		t.Fatalf("a fresh player already has a payout address: %q", before.Address)
	}

	addr := payoutAddress(t, p)
	if code, body := post(t, p, "/payout/set", map[string]string{"address": addr}); code != http.StatusOK {
		t.Fatalf("/payout/set returned %d: %s", code, body)
	}
	code, body = get(t, p, "/payout")
	if code != http.StatusOK {
		t.Fatalf("GET /payout returned %d: %s", code, body)
	}
	var after struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal([]byte(body), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Address != addr {
		t.Fatalf("read back %q, want %q", after.Address, addr)
	}
}

// payoutAddress mints an address on the test network for a player to be paid at.
func payoutAddress(t *testing.T, p *plugin) string {
	t.Helper()
	key, err := p.id.sessionKey("payout-test")
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	addr, _, err := escrow.BondAddress(mustBondScript(t, key.PubKey().SerializeCompressed()), testParams)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	return addr.String()
}

func mustBondScript(t *testing.T, pub []byte) []byte {
	t.Helper()
	script, err := escrow.BondScript(pub, escrow.MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	return script
}

// A table that is dealing reports where the money is, and the difference
// between what is agreed and what is merely current.
func TestATableThatDealsReportsBothBalances(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	s := a.tables.snapshots()[0]
	if !s.Dealing {
		t.Fatal("a dealing table does not say it is dealing")
	}
	if s.Bonded != int(terms.Seats) {
		t.Fatalf("a dealing table reports %d bonds, want %d", s.Bonded, terms.Seats)
	}
	if s.Funded != int(terms.Seats) {
		t.Fatalf("a dealing table reports %d stakes, want %d", s.Funded, terms.Seats)
	}
	if len(s.Live) != int(terms.Seats) {
		t.Fatalf("live stacks name %d seats, want %d", len(s.Live), terms.Seats)
	}
	if s.Settled == nil {
		t.Fatal("a dealing table reports no settled boundary")
	}
	// Nothing has been agreed yet, which is not the same as nothing being
	// there. Hand zero means "this table would pay out its buy-ins".
	if s.Settled.Hand != 0 {
		t.Fatalf("a table on its first hand has settled hand %d, want 0", s.Settled.Hand)
	}

	// Every seat's stake and bond is an outpoint this peer found itself.
	for _, seat := range s.Roster {
		if seat.Stake == "" {
			t.Fatalf("seat %d is dealing with no stake this peer has seen", seat.Seat)
		}
		if seat.Bond == "" {
			t.Fatalf("seat %d is dealing with no bond this peer has seen", seat.Seat)
		}
		if seat.Key == "" {
			t.Fatalf("seat %d has no key", seat.Seat)
		}
	}
}

// The deck's provenance, hand by hand, and only what this process checked.
func TestTheHandReportsWhoShuffledIt(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	// Shuffling is the first thing a hand does, and it runs in seat order,
	// so give it a moment to get round the table.
	var v handView
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, body := get(t, a, "/table/hand?sid="+terms.SID)
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("decode hand: %v", err)
			}
			if len(v.Shuffles) == int(terms.Seats) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hand never reported %d shuffles: %+v", terms.Seats, v.Shuffles)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Our own turn with the deck is reported as ours, never merely as
	// verified: "we permuted it" is a stronger claim than "we checked their
	// proof" and the two must not read alike.
	var ours, verified, awaited int
	for _, s := range v.Shuffles {
		switch s.State {
		case shuffleOurs:
			ours++
			if s.Seat != uint32(v.Seat) {
				t.Fatalf("seat %d's shuffle is reported as ours; we are seat %d", s.Seat, v.Seat)
			}
		case shuffleVerified:
			verified++
			if s.Seat == uint32(v.Seat) {
				t.Fatal("our own shuffle is reported as merely verified")
			}
		case shuffleAwaited:
			awaited++
		default:
			t.Fatalf("seat %d's shuffle is in unknown state %q", s.Seat, s.State)
		}
	}
	if ours+verified+awaited != int(terms.Seats) {
		t.Fatalf("shuffles do not account for every seat: %+v", v.Shuffles)
	}
}

// The claim machinery reports what it did, rather than only writing it down.
//
// A peer that stops answering is claimed against once its obligation has stood
// long enough. Until now the whole of that - proposing it, co-signing it,
// sending it - happened inside log.Printf, in a container with no shell.
func TestAClaimIsReportedAndNotOnlyLogged(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)

	// Both seats say where to pay them, because a claim pays the seats that
	// are still there and cannot be built until each has said where.
	for _, p := range []*plugin{a, b} {
		addr := payoutAddress(t, p)
		if code, body := post(t, p, "/payout/set", map[string]string{"address": addr}); code != http.StatusOK {
			t.Fatalf("/payout/set returned %d: %s", code, body)
		}
	}
	waitPayouts(t, terms.SID, a, b)

	// Every seat answerable for first. Agreeing an accusation is an exchange
	// and exchanges cost blocks, and those blocks move the hand - so this has
	// to happen before the wait below rather than after it, or it would hand
	// this seat an obligation it has no way to discharge.
	waitAccusable(t, h, terms.SID, a, b)

	// Wait until this seat owes nothing before taking the other off the wire.
	//
	// Otherwise the hand can be at a point where *we* are the one holding it
	// up, and a peer that owes something is not entitled to accuse anybody -
	// so no claim is proposed and the test blames the claim machinery for its
	// own timing. The whole scenario is "we have done everything and the other
	// seat has not", and it has to be arranged rather than hoped for.
	waitOwesNothing(t, h, terms.SID, a, b)

	// One peer stops. Nothing announces that; it is only ever inferred from
	// an obligation that stands while the chain moves.
	h.silence(t, "tok-b")
	_ = b

	// Past the point where standing still stops looking like being slow.
	deadline := time.Now().Add(30 * time.Second)
	height := int64(terms.Until) + 2
	for {
		height += int64(claimAfter) + 1
		// Keep doing our part. A seat that owes something is not entitled to
		// accuse anybody, and with the other peer gone our turn comes round
		// repeatedly - so the scenario only holds if we keep playing.
		playOn(t, a, terms.SID)
		a.publish(context.Background(), a.tables.tick(height))

		v := ledgerOf(t, a, terms.SID)
		if len(v.Claims) > 0 && hasEvent(v.Events, eventProposed) {
			c := v.Claims[0]
			if c.Says == "" {
				t.Fatal("a claim is reported with no account of what it is for")
			}
			if c.Needs == 0 {
				t.Fatal("a claim reports needing no signatures")
			}
			if c.Bond == "" {
				t.Fatal("a claim names no bond to take")
			}
			return
		}
		if time.Now().After(deadline) {
			v := ledgerOf(t, a, terms.SID)
			t.Fatalf("no claim was ever reported; events %+v roster %+v", v.Events, v.Roster)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func hasEvent(events []chainEvent, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// waitPayouts waits until every peer has heard where every seat wants paying.
func waitPayouts(t *testing.T, sid string, peers ...*plugin) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		done := true
		for _, p := range peers {
			if len(ledgerOf(t, p, sid).PayoutsMissing) > 0 {
				done = false
			}
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the seats never agreed where to pay each other")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The JSON key set is pinned here, because a hand-written client reads these
// names and nothing else would notice one being renamed.
//
// Not a schema check. It is a reminder: changing this list is changing a
// published interface, and the test failing is the point at which somebody
// decides that on purpose rather than by refactoring.
func TestSnapshotNamesItsFields(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	// The chain has to have been read at least once, or the height is
	// genuinely absent rather than merely zero - which is the distinction
	// the omitted key is there to preserve.
	a.publish(context.Background(), a.tables.tick(int64(terms.Until)+2))

	blob, err := json.Marshal(a.tables.snapshots()[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{
		"sid", "gcid", "state", "seats", "buyinAtoms", "csvBlocks", "joined",
		"matchId", "waiting", "funded", "seat", "depositAddr", "stake",
		"fundingDeadline", "bonded", "dealing", "settled", "live", "height",
		"payoutSet", "roster",
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("a dealing table's snapshot no longer carries %q", key)
		}
	}

	seat, ok := got["roster"].([]any)
	if !ok || len(seat) == 0 {
		t.Fatal("a dealing table's snapshot has no roster")
	}
	first, _ := seat[0].(map[string]any)
	for _, key := range []string{"seat", "key", "stake", "bond"} {
		if _, ok := first[key]; !ok {
			t.Errorf("a seat no longer carries %q", key)
		}
	}
}

// A block number on its own is not information. A funding deadline of 1101193
// says nothing to somebody who cannot see how far away it is, and until the
// height was carried alongside it there was nothing to measure it against.
func TestTheHeightIsCarriedBesideTheDeadline(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	height := int64(terms.Until) + 2
	p.publish(context.Background(), p.tables.tick(height))

	s := p.tables.snapshots()[0]
	if s.Height != height {
		t.Fatalf("the table reports height %d, want %d", s.Height, height)
	}
	if s.FundingDeadline == 0 {
		t.Fatal("a seated table has no funding deadline to measure")
	}
	if ledgerOf(t, p, terms.SID).Height != height {
		t.Fatal("the ledger and the snapshot disagree about where the chain is")
	}
}

// A refusal to pre-agree an accusation is visible from outside the process.
//
// Nothing is co-signed at dispute time any more - an accusation is agreed while the
// table is still cooperating - so this is where a refusal happens now, and it is
// the more important place for it: a peer whose accusations are all refused has
// nothing to accuse with the day somebody stops, and should find out while there is
// still time to care.
func TestARefusalToPreAgreeIsReported(t *testing.T) {
	h := newHub(t)
	p, _, terms := dealingTable(t, h)

	tbl := p.tables.m[terms.SID]
	// An accusation this peer did not derive. Signed by nobody in particular
	// and against a transaction that is not in the chain it built.
	p.tables.mu.Lock()
	_ = tbl.adoptAccusation(schema.Accusation{
		Seat:   theirSeat(t, tbl),
		Tx:     strings.Repeat("00", 32),
		Signer: strings.Repeat("02", 33),
		Sig:    strings.Repeat("01", 64),
	})
	p.tables.mu.Unlock()

	v := ledgerOf(t, p, terms.SID)
	if !hasEvent(v.Events, eventRefused) {
		t.Fatalf("a refused claim left no account of itself: %+v", v.Events)
	}
}

func dutyAgainst(t *testing.T, tbl *table) driver.Duty {
	t.Helper()
	return driver.Duty{Seat: int(theirSeat(t, tbl)), Kind: driver.DutyAction, Hand: 1, At: 9}
}

// Leaving a table must not lose the only record of where the money is.
//
// A stake sits behind a timelock that outlives the table by hours, and the
// outpoint holding it is written nowhere a person can see except in the table
// that put it there. Forgetting the table at the moment the cards stop would
// leave coin on the chain with nothing pointing at it.
func TestLeavingKeepsATableThatStillHoldsOurMoney(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	// Pay this seat's stake, so the table holds something of ours.
	tbl := p.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	dep, err := tbl.deposit(seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	outpoint := payTo(h, dep.PkScriptHex, "5b")
	if _, err := p.recordOwnStake(terms.SID, seat, outpoint); err != nil {
		t.Fatalf("record stake: %v", err)
	}

	if code, body := post(t, p, "/table/leave", map[string]string{"sid": terms.SID}); code != http.StatusOK {
		t.Fatalf("/table/leave returned %d: %s", code, body)
	}

	snaps := p.tables.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("leaving forgot a table still holding a stake; %d tables remain", len(snaps))
	}
	if !snaps[0].Finished {
		t.Fatal("a table this player left is not reported as finished")
	}
	if snaps[0].Stake != outpoint {
		t.Fatalf("the kept table names stake %q, want %q", snaps[0].Stake, outpoint)
	}
	// And it is a receipt, not a table: nothing seats anybody at it again.
	p.tables.mu.Lock()
	out := p.tables.m[terms.SID].startPlaying()
	p.tables.mu.Unlock()
	if len(out) != 0 {
		t.Fatal("a finished table started dealing")
	}
}

// A table that holds nothing of ours is forgotten, as before.
func TestLeavingForgetsATableHoldingNothing(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	p, terms := seatedTable(t, h, inv, other)

	if code, body := post(t, p, "/table/leave", map[string]string{"sid": terms.SID}); code != http.StatusOK {
		t.Fatalf("/table/leave returned %d: %s", code, body)
	}
	if snaps := p.tables.snapshots(); len(snaps) != 0 {
		t.Fatalf("leaving kept a table with nothing in it: %+v", snaps)
	}
}

// Being finished has to survive a restart, or a table this player left comes
// back as a live one.
func TestAFinishedTableComesBackFinished(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	other := h.lend(t, "dd")
	dir := t.TempDir()

	p := h.restart(t, dir, "tok")
	acceptInvite(t, p, inv)
	terms := inviteTerms(inv)
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

	tbl := p.tables.m[terms.SID]
	seat, _ := tbl.form.OurSeat()
	dep, err := tbl.deposit(seat, testParams)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if _, err := p.recordOwnStake(terms.SID, seat, payTo(h, dep.PkScriptHex, "5c")); err != nil {
		t.Fatalf("record stake: %v", err)
	}
	post(t, p, "/table/leave", map[string]string{"sid": terms.SID})

	back := h.restart(t, dir, "tok2")
	snaps := back.tables.snapshots()
	if len(snaps) != 1 {
		t.Fatalf("a finished table did not come back; %d tables", len(snaps))
	}
	if !snaps[0].Finished {
		t.Fatal("a finished table came back as a live one")
	}
}

// The signed log is what a finished table is evidence of, and it has to
// outlive the process that played it.
//
// Every entry carries the signature of the seat that made it and the chain
// hashes forward, so a transcript can be checked by somebody who was not there
// and trusts nobody who was. It lives in memory while a table is played; a
// receipt with no evidence behind it would be worth much less.
//
// The specific thing pinned here is which chain gets read. A table writes to
// two at different times - the watcher folds entries in until it starts
// dealing, and after that the hand owns the log because it appends and folds
// each entry itself. Reading the watcher for a table that is dealing hands over
// an empty transcript for a table that has been playing for hours.
func TestAFinishedTableKeepsItsLog(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)
	waitBetting(t, a)

	code, body := get(t, a, "/table/log?sid="+terms.SID)
	if code != http.StatusOK {
		t.Fatalf("/table/log returned %d: %s", code, body)
	}
	var live struct {
		MatchID string            `json:"match_id"`
		Roster  map[string]string `json:"roster"`
	}
	if err := json.Unmarshal([]byte(body), &live); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if live.MatchID == "" {
		t.Fatal("the log names no match")
	}
	if len(live.Roster) == 0 {
		t.Fatal("the log carries no roster, so nothing in it can be checked")
	}

	// It is the hand's chain, not the watcher's. Compared rather than
	// described: the two are the same shape and differ only in what they
	// contain, so nothing short of the bytes distinguishes them.
	a.tables.mu.Lock()
	tbl := a.tables.m[terms.SID]
	want, err := tbl.play.Chain().Marshal()
	a.tables.mu.Unlock()
	if err != nil {
		t.Fatalf("marshal the hand's chain: %v", err)
	}
	if body != string(want) {
		t.Fatal("the log served is not the chain the hand is writing")
	}

	// And it is written down when the table stops being played, because the
	// entries live in memory and the coin behind them does not.
	a.tables.mu.Lock()
	a.tables.drop(terms.SID, tbl)
	a.tables.mu.Unlock()

	stored, err := a.store.loadTranscript(terms.SID)
	if err != nil {
		t.Fatalf("read back the log: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("a finished table kept no log on disk")
	}
	if string(stored) != string(want) {
		t.Fatal("the log written down is not the one the table was playing")
	}
}

// A table that never dealt has no log, and says so rather than handing over an
// empty one that looks like a played hand with nothing in it.
func TestATableThatNeverDealtHasNoLog(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok")
	acceptInvite(t, p, testInvite(2))

	code, body := get(t, p, "/table/log?sid="+inviteTerms(testInvite(2)).SID)
	if code != http.StatusNotFound {
		t.Fatalf("/table/log returned %d for a table that never dealt, want 404: %s", code, body)
	}
}

// The one the harness stopped one step short of. Everything above proves two
// peers can be dealt in; this proves they can then play, which is a different
// message and was the missing one.
//
// Three kinds carry the dealing and a fourth carries every bet. That fourth was
// routed to the hand and never decoded, so each peer dropped every action the
// other sent, in silence, and two fully funded seats sat waiting on each other
// forever. No test noticed, because no test bet.
func TestABetReachesTheOtherSeat(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)

	// Dealing is not yet betting: the deck has to be shuffled and the hole
	// cards handed out before anybody can act.
	waitBetting(t, a, b)

	actor, waiter := a, b
	if !toActIsOurs(t, a, terms.SID) {
		actor, waiter = b, a
	}

	before := logLen(t, waiter, terms.SID)
	out, err := actor.tables.Act(terms.SID, "call", 0)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	actor.publish(context.Background(), out)
	h.inflight.Wait()

	if got := logLen(t, waiter, terms.SID); got <= before {
		t.Fatalf("the other seat's log went %d -> %d, so the bet never reached it "+
			"and both seats are now waiting for each other", before, got)
	}
	// And it has to have moved the hand on, not merely been filed.
	if toActIsOurs(t, actor, terms.SID) {
		t.Fatal("the seat that acted is still the seat to act")
	}
}

// toActIsOurs reports whether this peer believes it is its own turn.
func toActIsOurs(t *testing.T, p *plugin, sid string) bool {
	t.Helper()
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()
	tbl := p.tables.m[sid]
	if tbl == nil || tbl.play == nil {
		t.Fatal("not dealing")
	}
	hand := tbl.play.Hand()
	if hand == nil {
		t.Fatal("no hand")
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		t.Fatal("no seat")
	}
	return hand.State().ToAct == int(seat)
}

// logLen is how many entries this peer has verified for itself.
func logLen(t *testing.T, p *plugin, sid string) uint64 {
	t.Helper()
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()
	tbl := p.tables.m[sid]
	if tbl == nil || tbl.play == nil {
		t.Fatal("not dealing")
	}
	_, seq := tbl.play.Chain().Head()
	return seq
}

// waitOwesNothing waits until this peer's own seat is not what the table is
// waiting on.
//
// The distinction a claim turns on. A seat that owes something is the reason the
// hand is not moving, and it cannot accuse anybody of stopping while it is
// itself the one stopped - so a test that wants to watch a claim has to reach
// that state first rather than assume it.
func waitOwesNothing(t *testing.T, h *hub, sid string, p *plugin, peers ...*plugin) {
	t.Helper()
	all := append([]*plugin{p}, peers...)
	deadline := time.Now().Add(30 * time.Second)
	for {
		clear := true
		for _, s := range p.tables.snapshots() {
			if s.SID != sid {
				continue
			}
			for _, seat := range s.Roster {
				if seat.Ours && seat.Owes != nil {
					clear = false
				}
			}
		}
		if clear {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("this seat never finished what the hand asked of it")
		}
		// Discharge it rather than wait for it to pass. The scenario is "we
		// have done everything and the other seat has not", and this seat's
		// turn arriving is part of everything - waiting it out would mean
		// waiting for somebody else to play our hand.
		playOn(t, p, sid)
		tickAll(all...)
		h.inflight.Wait()
		time.Sleep(10 * time.Millisecond)
	}
}

// waitAccusable ticks until every peer holds a complete accusation against every
// seat, which is what a table needs before anybody stopping can be answered.
func waitAccusable(t *testing.T, h *hub, sid string, peers ...*plugin) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ready := true
		for _, p := range peers {
			p.tables.mu.Lock()
			tbl := p.tables.m[sid]
			if tbl == nil || !tbl.accusationsReady() {
				ready = false
			}
			p.tables.mu.Unlock()
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the table never agreed an accusation against every seat")
		}
		tickAll(peers...)
		h.inflight.Wait()
		time.Sleep(10 * time.Millisecond)
	}
}

// playOn takes the cheapest legal action, if this seat has one to take.
//
// Check when checking is allowed and call when it is not, which pre-flop against
// a blind it is not. Refusals are ignored: this is asked on every pass and most
// of them are not this seat's turn at all.
func playOn(t *testing.T, p *plugin, sid string) {
	t.Helper()
	for _, action := range []string{"check", "call"} {
		if code, _ := post(t, p, "/table/act",
			map[string]any{"sid": sid, "action": action}); code == http.StatusOK {
			return
		}
	}
}
