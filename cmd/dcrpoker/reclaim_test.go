package main

import (
	"strings"
	"testing"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// The way out of a table that dissolved rather than ended.
//
// A table bond is released cooperatively when a table finishes, which needs
// every member's signature - and the case this exists for is precisely the one
// where those are not available: a table that never dealt, or whose hand could
// not finish, or whose peers are simply gone. Without this the coin is
// unreachable by any code at all, which is what it was until now.
func TestATableBondComesBackOnItsOwnOnceTheLockMatures(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	seat, bond, outpoint, err := a.tables.ourTableBond(terms.SID)
	if err != nil {
		t.Fatalf("this seat has no bond to reclaim: %v", err)
	}
	if outpoint == "" || bond.ScriptHex == "" {
		t.Fatalf("seat %d has bond %q under script %q", seat, outpoint, bond.ScriptHex)
	}

	dest := payoutAddress(t, a)

	// The chain says two confirmations, and the lock is a week. Maturity is
	// the one thing a script engine cannot see, so a build that skipped this
	// check would produce a transaction that verifies perfectly and that the
	// network then refuses.
	_, err = a.doReclaim(&gamingpb.Reclaim{
		Kind: gamingpb.Reclaim_TABLE_BOND, Sid: terms.SID, DestAddr: dest,
	})
	if err == nil {
		t.Fatalf("swept a bond that is %d blocks short of its lock", membership.TableBondBlocks)
	}
	if !strings.Contains(err.Error(), "not spendable") {
		t.Fatalf("the refusal should say the lock has not matured: %v", err)
	}

	// Move the chain rather than the transaction.
	h.mu.Lock()
	h.confs = int64(membership.TableBondBlocks)
	h.mu.Unlock()

	before := len(h.relayed())
	if _, err := a.doReclaim(&gamingpb.Reclaim{
		Kind: gamingpb.Reclaim_TABLE_BOND, Sid: terms.SID, DestAddr: dest,
	}); err != nil {
		t.Fatalf("sweep the table bond: %v", err)
	}
	if got := h.relayed(); len(got) != before+1 {
		t.Fatalf("relayed %d transactions, want one more than %d", len(got), before)
	}
	if !h.isSpent(outpoint) {
		t.Fatalf("the bond at %s was not spent by its own sweep", outpoint)
	}

	// And the table goes on citing it until the chain agrees it is gone.
	// watchBonds clears it then. Forgetting it here would leave a sweep that
	// never confirmed with nothing pointing at its output.
	if _, _, still, err := a.tables.ourTableBond(terms.SID); err != nil {
		t.Fatalf("the table forgot its bond before the chain confirmed the sweep: %v", err)
	} else if still != outpoint {
		t.Fatalf("the table cites %s after sweeping %s", still, outpoint)
	}
}

// A stake comes back from a table that has dealt.
//
// The accessor the refund reads its script through used to carry the funding
// guards, so this exact call was answered with "has already dealt, so it takes
// no more stake" - a sentence about paying in, to a request to take out. The
// set it refused was the set that needs refunding.
//
// Kills: putting dealt/finished or Settled onto ourDepositScript; pointing the
// Reclaim_STAKE branch back at whereToStake; dropping the maturity check.
func TestAStakeComesBackFromATableThatHasDealt(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	seat, dep, _, stake, err := a.tables.ourDepositScript(terms.SID)
	if err != nil {
		t.Fatalf("a dealt table would not say where its stake is: %v", err)
	}
	if stake == "" || dep.RedeemScriptHex == "" {
		t.Fatalf("seat %d has stake %q under script %q", seat, stake, dep.RedeemScriptHex)
	}
	// The funding side must still refuse the same table, or this is a test of
	// a deleted guard rather than of a split one.
	if _, _, _, _, err := a.tables.whereToStake(terms.SID); err == nil {
		t.Fatal("the funding accessor answered for a table that has dealt")
	}

	dest := payoutAddress(t, a)

	_, err = a.doReclaim(&gamingpb.Reclaim{
		Kind: gamingpb.Reclaim_STAKE, Sid: terms.SID, DestAddr: dest,
	})
	if err == nil {
		t.Fatalf("took back a stake that is short of its %d block lock", terms.CSVBlocks)
	}
	if !strings.Contains(err.Error(), "not spendable") {
		t.Fatalf("the refusal should say the lock has not matured: %v", err)
	}

	// Move the chain rather than the transaction.
	h.mu.Lock()
	h.confs = int64(terms.CSVBlocks)
	h.mu.Unlock()

	before := len(h.relayed())
	if _, err := a.doReclaim(&gamingpb.Reclaim{
		Kind: gamingpb.Reclaim_STAKE, Sid: terms.SID, DestAddr: dest,
	}); err != nil {
		t.Fatalf("take the stake back: %v", err)
	}
	if got := h.relayed(); len(got) != before+1 {
		t.Fatalf("relayed %d transactions, want one more than %d", len(got), before)
	}
	if !h.isSpent(stake) {
		t.Fatalf("the stake at %s was not spent by its own refund", stake)
	}
}

// A script that is not the one the output was paid into is caught here, before
// anything is signed.
//
// The engine check inside BuildTimelockedSpend derives its pkScript from the
// script it was handed, so it passes for a script that satisfies itself and is
// simply the wrong one. Without this the transaction is broadcast and dcrd
// answers "false stack entry at end of script execution", which reads as a
// signing bug rather than as a derivation that drifted.
//
// Kills: deleting the comparison in reclaim, or making it advisory - the
// relayed count catches a version that logs and broadcasts anyway.
func TestARefundRefusesAnOutputItDoesNotDerive(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)

	_, bond, outpoint, err := a.tables.ourTableBond(terms.SID)
	if err != nil {
		t.Fatalf("this seat has no bond to reclaim: %v", err)
	}
	dest := payoutAddress(t, a)

	h.mu.Lock()
	h.confs = int64(membership.TableBondBlocks)
	right := h.bonds[outpoint]
	// A different P2SH, so the output pays something this key does not derive.
	h.bonds[outpoint] = "a914" + strings.Repeat("11", 20) + "87"
	h.mu.Unlock()

	if right == "" || bond.ScriptHex == "" {
		t.Fatalf("fixture has no bond script: outpoint pays %q, bond script %q", right, bond.ScriptHex)
	}

	before := len(h.relayed())
	_, err = a.doReclaim(&gamingpb.Reclaim{
		Kind: gamingpb.Reclaim_TABLE_BOND, Sid: terms.SID, DestAddr: dest,
	})
	if err == nil {
		t.Fatal("signed a spend of an output paying a script this key does not derive")
	}
	if !strings.Contains(err.Error(), "not the script that output was paid into") {
		t.Fatalf("the refusal should name the mismatch: %v", err)
	}
	if strings.Contains(err.Error(), "false stack entry") {
		t.Fatalf("the refusal came from the engine rather than from the check: %v", err)
	}
	if got := h.relayed(); len(got) != before {
		t.Fatalf("relayed %d transactions, want the %d it started with", len(got), before)
	}

	// The same call succeeds once the output pays what this key derives, so
	// the refusal above came from the comparison and not from the fixture.
	h.mu.Lock()
	h.bonds[outpoint] = right
	h.mu.Unlock()

	if _, err := a.doReclaim(&gamingpb.Reclaim{
		Kind: gamingpb.Reclaim_TABLE_BOND, Sid: terms.SID, DestAddr: dest,
	}); err != nil {
		t.Fatalf("sweep the table bond: %v", err)
	}
	if got := h.relayed(); len(got) != before+1 {
		t.Fatalf("relayed %d transactions, want one more than %d", len(got), before)
	}
}

// The table that could not be paid out, which is what this is really about.
//
// A hand that cannot complete never reaches a boundary, and a table that never
// reaches a boundary never ends - so it can never build a settlement, because
// one is only proposed once the table is over. Every seat asking to leave did
// not help: leaving set a flag and waited for the boundary that was not coming.
//
// It happened for real, on the first table these two boxes ever played: two
// peers held different decks for hand 2, each waiting on the other, both seats
// asked to leave, and nothing moved. The 0.001 DCR a side was then reachable
// only by each player waiting out their own refund timelock - the slow unilateral
// path this whole design exists so that nobody has to use.
//
// So: every seat leaving pays the table out at the last result they all signed,
// whatever the hand in progress was doing.
func TestATableNobodyCanFinishStillPaysOut(t *testing.T) {
	h := newHub(t)
	a, b, terms := dealingTable(t, h)
	sayWhereToPay(t, h, a, b)

	// Lose every shuffle from here on. The hand already being dealt is past
	// shuffling and plays out normally; the one that opens behind its boundary
	// never starts. Dropping them before that hand rather than after is the
	// whole trick, because a boundary opens its successor immediately - wait
	// until a hand has visibly finished and the next has already shuffled.
	// Generously, so the strand holds: republication now repeats on a timer,
	// so a small budget would be eaten by the repairs and let a later copy
	// through - which would un-strand the hand this test needs stranded.
	h.drop(schema.KindShuffle, 500)

	// A real result to fall back to, rather than the buy-ins.
	playHand(t, h, terms.SID, foldAt("preflop"), a, b)
	at, want := settledStacks(t, a, terms.SID)
	if at == 0 {
		t.Fatal("no hand was ever signed, so there is no boundary to fall back to")
	}

	// Wait for the next hand to actually reach for a shuffle, rather than
	// assuming it has. A boundary opens its successor, but not instantly.
	stranded := false
	for range 20 {
		if h.dropped(schema.KindShuffle) > 0 && handNumber(t, a, terms.SID) > at {
			stranded = true
			break
		}
		tickAll(a, b)
		h.inflight.Wait()
		time.Sleep(10 * time.Millisecond)
	}
	if !stranded {
		t.Fatalf("no hand was left stranded: %d shuffles lost, newest hand %d, signed %d",
			h.dropped(schema.KindShuffle), handNumber(t, a, terms.SID), at)
	}

	// One seat leaving is not enough, and must not be: it cannot fold its way
	// out either, because betting never began.
	getUp(t, h, terms.SID, a)
	if over(t, a, terms.SID) {
		t.Fatal("one seat leaving ended the table, so leaving can un-bet a hand")
	}

	// The second is, because unanimity means nobody is being stopped on.
	getUp(t, h, terms.SID, b)

	waitOver(t, h, terms.SID, a, b)
	paid := waitPaid(t, h, a, b)

	// And it pays the boundary, not a guess at the hand it voided.
	got, stacks := settledStacks(t, a, terms.SID)
	if got != at {
		t.Fatalf("paid out at hand %d, want the last signed boundary %d", got, at)
	}
	for i := range stacks {
		if stacks[i] != want[i] {
			t.Fatalf("paid out at %v, want the signed %v", stacks, want)
		}
	}
	if len(paid.TxOut) == 0 {
		t.Fatal("the settlement pays nobody")
	}
}
