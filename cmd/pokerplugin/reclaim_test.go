package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
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
	code, body := post(t, a, "/table/bond/sweep", map[string]any{"sid": terms.SID, "destAddr": dest})
	if code == http.StatusOK {
		t.Fatalf("swept a bond that is %d blocks short of its lock", membership.TableBondBlocks)
	}
	if !strings.Contains(body, "not spendable") {
		t.Fatalf("the refusal should say the lock has not matured: %s", body)
	}

	// Move the chain rather than the transaction.
	h.mu.Lock()
	h.confs = int64(membership.TableBondBlocks)
	h.mu.Unlock()

	before := len(h.relayed())
	code, body = post(t, a, "/table/bond/sweep", map[string]any{"sid": terms.SID, "destAddr": dest})
	if code != http.StatusOK {
		t.Fatalf("/table/bond/sweep returned %d: %s", code, body)
	}
	if got := h.relayed(); len(got) != before+1 {
		t.Fatalf("relayed %d transactions, want one more than %d", len(got), before)
	}
	if !h.isSpent(outpoint) {
		t.Fatalf("the bond at %s was not spent by its own sweep", outpoint)
	}

	// And the table stops citing coin it has spent, because a table that
	// announced an output that is gone would be telling its peers something
	// untrue about the chain.
	if _, _, still, err := a.tables.ourTableBond(terms.SID); err == nil {
		t.Fatalf("the table still says its bond is at %s after sweeping it", still)
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
	h.drop(schema.KindShuffle, 8)

	// A real result to fall back to, rather than the buy-ins.
	playHand(t, h, terms.SID, foldAt("preflop"), a, b)
	at, want := settledStacks(t, a, terms.SID)
	if at == 0 {
		t.Fatal("no hand was ever signed, so there is no boundary to fall back to")
	}
	if h.dropped(schema.KindShuffle) == 0 {
		t.Fatal("no shuffle was ever lost, so nothing is stranded")
	}
	if n := handNumber(t, a, terms.SID); n <= at {
		t.Fatalf("hand %d is the newest and %d is signed, so nothing is in progress to strand", n, at)
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
