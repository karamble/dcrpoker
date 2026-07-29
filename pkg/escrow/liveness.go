package escrow

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/txscript/v4"
)

// ClaimBlocks is how long a player has to prove they are still at the table
// before the others take their bond.
//
// Three blocks, about a quarter of an hour. This is a live game: a seat that has
// not attached its turn to the log for fifteen minutes has left, and the rest of
// the table should not be held there. The window is not sized for somebody
// asleep - it is sized for a reconnect, and anyone who wanted longer wanted a
// correspondence game.
//
// It is also the whole grace period. A player who comes back inside it plays
// their turn, the claim dies, and the game carries on as if nothing happened.
const ClaimBlocks uint32 = 3

// TableBondScript builds a bond that a table can take from a player who walks
// out of a hand.
//
//	OP_IF        <every member> 2 OP_CHECKSIGALTVERIFY ...      # alive
//	OP_ELSE OP_IF <claim> CSV DROP <every other member> ...      # claim
//	OP_ELSE       <lock> CSV DROP <owner> 2 OP_CHECKSIGALTVERIFY # backstop
//
// Three ways out, and the shape of them is the design.
//
// **Alive** needs every member's signature, the owner included, and has no
// timelock. It does two jobs: it releases the bond when a table ends normally,
// and it is how an accused player answers a claim. Because it needs everyone, the
// owner cannot use it to walk off with their own bond mid-table.
//
// It does *not* currently beat an accusation, and this is the gap to close. The
// claim's timelock is relative to the bond output, which is confirmed before the
// table deals - so by the time anybody abandons, the lock is long satisfied and
// both branches are spendable at once. Whoever broadcasts first wins, and the
// claimant chooses when to start. For the answer to have the priority described
// below, the delay has to run from the accusation rather than from the bond: the
// claim would spend into an intermediate output carrying the lock, which its
// owner can take at once with an answer and the claimant only after the window.
//
// **Claim** needs every member except the owner, after ClaimBlocks. That is the
// forfeiture: the others take the bond and divide it. Requiring all of them is
// what stops one player opening claims for sport.
//
// **Backstop** is the owner alone after a long lock, so a table that simply
// dissolves does not strand anybody's coin forever.
//
// # Why abandonment is decided by a race and not by a proof
//
// Nothing can prove a player left. A seat that goes quiet and a seat whose
// messages are being dropped look identical from outside, and heads-up the only
// witness is the person who benefits from the answer. Every design that asks
// somebody to adjudicate hands that somebody a way to rob an honest player.
//
// So it is not adjudicated. Abandonment is *defined* as failing to answer on
// chain inside the window, which is a fact rather than a claim: the accused
// answers to Decred, not to their opponent. Collusion buys nothing for the same
// reason - a table full of liars can open a claim, and one uncensorable answer
// kills it.
//
// Two things have to be true for that, and one is not yet. The window has to give
// the accused time, which needs the intermediate output described above. And the
// accused has to learn it is accused: answerClaim runs only when a claim frame
// arrives over the group chat, so an opponent who withholds every message does in
// fact withhold the accusation. Watching the chain instead does not close it,
// since the host reports an outpoint as absent whether it is spent, unconfirmed
// or never existed, and reveals nothing about a spend waiting in the mempool - by
// the time a claim is visible it has confirmed.
//
// # Why this is not the forfeiture in forfeit.go
//
// The two punishments need opposite properties and cannot share a branch.
// Equivocation is punished by a branch nobody can contest, because the cheat is
// online by definition and would contest it. Abandonment is punished by a branch
// that *must* be contestable, because the accusation might be false. One is
// keyed to a secret the cheat published themselves; this one is keyed to
// silence.
func TableBondScript(owner []byte, members [][]byte, claimBlocks, lockBlocks uint32) ([]byte, error) {
	if err := checkPubKey(owner); err != nil {
		return nil, fmt.Errorf("owner key: %w", err)
	}
	sorted, err := CanonicalMembers(members)
	if err != nil {
		return nil, err
	}
	if !containsKey(sorted, owner) {
		return nil, fmt.Errorf("the bond's owner is not at this table")
	}
	if len(sorted) < 2 {
		return nil, fmt.Errorf("a table bond needs at least 2 members, not %d", len(sorted))
	}
	if claimBlocks == 0 {
		return nil, fmt.Errorf("a claim with no delay could not be answered")
	}
	if claimBlocks > MaxCSVBlocks {
		return nil, fmt.Errorf("a claim delay of %d blocks is beyond %d", claimBlocks, MaxCSVBlocks)
	}
	if lockBlocks < MinBondBlocks {
		return nil, fmt.Errorf("bond lock of %d blocks is under the %d block minimum",
			lockBlocks, MinBondBlocks)
	}
	if lockBlocks > MaxCSVBlocks {
		return nil, fmt.Errorf("bond lock of %d blocks is beyond %d, so it could never be reclaimed",
			lockBlocks, MaxCSVBlocks)
	}
	if claimBlocks >= lockBlocks {
		return nil, fmt.Errorf("a claim delay of %d is not shorter than the %d block lock, "+
			"so the owner could always outrun a claim", claimBlocks, lockBlocks)
	}
	others := withoutKey(sorted, owner)

	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF)
	for _, m := range sorted {
		b.AddData(m).AddInt64(sigType).AddOp(txscript.OP_CHECKSIGALTVERIFY)
	}
	b.AddOp(txscript.OP_ELSE).
		AddOp(txscript.OP_IF).
		AddInt64(int64(claimBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP)
	for _, m := range others {
		b.AddData(m).AddInt64(sigType).AddOp(txscript.OP_CHECKSIGALTVERIFY)
	}
	b.AddOp(txscript.OP_ELSE).
		AddInt64(int64(lockBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(owner).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_ENDIF).
		AddOp(txscript.OP_ENDIF).
		AddOp(txscript.OP_TRUE)

	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("a table bond for %d members is %d bytes, over the %d byte push limit",
			len(sorted), len(script), txscript.MaxScriptElementSize)
	}
	return script, nil
}

func withoutKey(members [][]byte, key []byte) [][]byte {
	out := make([][]byte, 0, len(members))
	for _, m := range members {
		if !bytes.Equal(m, key) {
			out = append(out, m)
		}
	}
	return out
}

// AliveSigScript spends the branch that answers a claim, and the same branch
// that releases every bond when a table ends properly.
//
// Signatures are in canonical member order - the same order the script checks
// them in, and the same order CanonicalMembers puts them in, so every player
// derives it from the roster rather than being told.
func AliveSigScript(bond []byte, sigs [][]byte) ([]byte, error) {
	terms, err := ParseTableBond(bond)
	if err != nil {
		return nil, err
	}
	if len(sigs) != len(terms.Members) {
		return nil, fmt.Errorf("answering a claim needs all %d members' signatures, not %d",
			len(terms.Members), len(sigs))
	}
	return branchSigScript(bond, sigs, txscript.OP_1)
}

// ClaimSigScript spends the branch that takes an absent player's bond.
//
// Every member except the owner has to sign, and the spending input's sequence
// has to satisfy ClaimBlocks.
func ClaimSigScript(bond []byte, sigs [][]byte) ([]byte, error) {
	terms, err := ParseTableBond(bond)
	if err != nil {
		return nil, err
	}
	if len(sigs) != len(terms.Others) {
		return nil, fmt.Errorf("a claim needs all %d other members' signatures, not %d",
			len(terms.Others), len(sigs))
	}
	// Decline the alive branch, take the claim branch.
	return branchSigScript(bond, sigs, txscript.OP_0, txscript.OP_1)
}

// BackstopSigScript spends the owner's own way out, once the long lock matures.
func BackstopSigScript(bond, ownerSig []byte) ([]byte, error) {
	if _, err := ParseTableBond(bond); err != nil {
		return nil, err
	}
	return branchSigScript(bond, [][]byte{ownerSig}, txscript.OP_0, txscript.OP_0)
}

// branchSigScript assembles signatures and branch selectors.
//
// Both go on backwards, and for the same reason: the script consumes the top of
// the stack first, so the last thing pushed is the first thing read. The
// selectors are listed here in the order the nested OP_IFs consume them and
// reversed on the way out, because writing them in consumption order is the
// only way this stays readable.
func branchSigScript(bond []byte, sigs [][]byte, selectors ...byte) ([]byte, error) {
	for i, sig := range sigs {
		if len(sig) != SigLen {
			return nil, fmt.Errorf("signature %d is %d bytes, want %d", i, len(sig), SigLen)
		}
	}
	b := txscript.NewScriptBuilder()
	for i := len(sigs) - 1; i >= 0; i-- {
		b.AddData(sigs[i])
	}
	for i := len(selectors) - 1; i >= 0; i-- {
		b.AddOp(selectors[i])
	}
	return b.AddData(bond).Script()
}

// TableBondTerms is what a table bond commits to.
type TableBondTerms struct {
	Owner       []byte
	Members     [][]byte // canonical order, owner included
	Others      [][]byte // canonical order, owner excluded
	ClaimBlocks uint32
	LockBlocks  uint32
}

// ParseTableBond reads a table bond back.
//
// Every player runs this on every other player's bond before the first hand. A
// bond that does not name this table's exact roster is one whose claim branch
// these players could never satisfy - it looks like a bond, holds real coin, and
// could never be taken from anybody.
func ParseTableBond(bond []byte) (*TableBondTerms, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, bond)

	var (
		keys     [][]byte
		locks    []uint32
		last     uint32
		haveLast bool
	)
	for tokenizer.Next() {
		switch op := tokenizer.Opcode(); {
		case op == txscript.OP_CHECKSEQUENCEVERIFY:
			if !haveLast {
				return nil, fmt.Errorf("script has a timelock with no length")
			}
			locks = append(locks, last)
		case op == txscript.OP_DATA_33:
			keys = append(keys, append([]byte(nil), tokenizer.Data()...))
		default:
			if n, ok := smallIntOrData(tokenizer); ok {
				last, haveLast = n, true
			}
		}
	}
	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("parse table bond script: %w", err)
	}
	if len(locks) != 2 {
		return nil, fmt.Errorf("a table bond has a claim delay and a lock; this has %d timelocks", len(locks))
	}
	// Members, then everyone but the owner, then the owner: 2n keys for n
	// members, so an odd count is not this script at all.
	if len(keys) < 4 || len(keys)%2 != 0 {
		return nil, fmt.Errorf("script names %d keys, which is not a table bond's layout", len(keys))
	}
	n := len(keys) / 2

	terms := &TableBondTerms{
		Owner:       keys[len(keys)-1],
		Members:     keys[:n],
		Others:      keys[n : len(keys)-1],
		ClaimBlocks: locks[0],
		LockBlocks:  locks[1],
	}

	// Rebuilding is the real check - it is the only thing that rules out a
	// script carrying these same pushes alongside another path to the coin.
	want, err := TableBondScript(terms.Owner, terms.Members, terms.ClaimBlocks, terms.LockBlocks)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(want, bond) {
		return nil, fmt.Errorf("script is not a table bond on the terms it appears to carry")
	}
	return terms, nil
}

// MemberIndex reports where a key sits in a bond's canonical member order, so a
// player can find which signature slot is theirs.
func MemberIndex(terms *TableBondTerms, key []byte) (int, error) {
	if terms == nil {
		return 0, fmt.Errorf("no bond")
	}
	for i, m := range terms.Members {
		if bytes.Equal(m, key) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("that key is not a member of this table bond")
}
