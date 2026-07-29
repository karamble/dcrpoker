package main

import (
	"net/http"
	"strings"
	"testing"

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
