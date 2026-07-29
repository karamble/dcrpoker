package escrow

import (
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/wire"
)

// The race, run through the real consensus engine.
//
// Two things have to hold and they pull in opposite directions. A player who
// walks out must lose their bond to the rest of the table. A player who is still
// there must be able to keep it, against any number of people saying otherwise.
// Everything below is one or the other.

const (
	testClaimBlocks = ClaimBlocks
	testLockBlocks  = MinBondBlocks
)

type tableBond struct {
	privs  []*secp256k1.PrivateKey
	terms  *TableBondTerms
	script []byte
}

// postTableBond seats n players and posts the first one's bond.
func postTableBond(t *testing.T, n int) *tableBond {
	t.Helper()
	privs, pubs := memberKeys(t, n)
	script, err := TableBondScript(pubs[0], pubs, testClaimBlocks, testLockBlocks)
	if err != nil {
		t.Fatalf("build bond: %v", err)
	}
	terms, err := ParseTableBond(script)
	if err != nil {
		t.Fatalf("parse bond: %v", err)
	}
	return &tableBond{privs: privs, terms: terms, script: script}
}

// sign produces signatures over tx in the order the given keys are listed.
func (b *tableBond) sign(t *testing.T, tx *wire.MsgTx, keys [][]byte) [][]byte {
	t.Helper()
	sigs := make([][]byte, 0, len(keys))
	for _, k := range keys {
		sigs = append(sigs, signInput(t, privFor(t, b.privs, k), b.script, tx))
	}
	return sigs
}

// A table that ends properly hands every bond back, with no delay and no chain
// argument. This is the path that should happen every time.
func TestATableThatEndsProperlyReleasesTheBond(t *testing.T) {
	for _, n := range []int{2, 3, 6} {
		b := postTableBond(t, n)
		tx := spendTx(t, 0)
		sig, err := AliveSigScript(b.script, b.sign(t, tx, b.terms.Members))
		if err != nil {
			t.Fatalf("%d seats: alive sigscript: %v", n, err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, b.script, tx, csvFlags); err != nil {
			t.Fatalf("%d seats: a table that ended properly could not release the bond: %v", n, err)
		}
	}
}

// A player who walks out loses the bond to the rest of the table, once the
// window has passed.
func TestAnAbsentPlayerLosesTheBond(t *testing.T) {
	for _, n := range []int{2, 3, 6} {
		b := postTableBond(t, n)
		tx := spendTx(t, uint32(testClaimBlocks))
		sig, err := ClaimSigScript(b.script, b.sign(t, tx, b.terms.Others))
		if err != nil {
			t.Fatalf("%d seats: claim sigscript: %v", n, err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, b.script, tx, csvFlags); err != nil {
			t.Fatalf("%d seats: the table could not take an absent player's bond: %v", n, err)
		}
	}
}

// And not before the window has passed, so there is always time to answer.
func TestAClaimCannotBeTakenBeforeTheWindow(t *testing.T) {
	b := postTableBond(t, 3)
	tx := spendTx(t, uint32(testClaimBlocks)-1)
	sig, err := ClaimSigScript(b.script, b.sign(t, tx, b.terms.Others))
	if err != nil {
		t.Fatalf("claim sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err == nil {
		t.Fatal("a bond was claimed a block before the window closed")
	}
}

// The direction that protects the honest player, and the reason the window
// exists: an answer has no timelock, so it confirms while a claim is still
// waiting. Coming back and playing your turn beats an accusation every time.
func TestAnAnswerBeatsAClaim(t *testing.T) {
	b := postTableBond(t, 3)

	// The claim cannot confirm yet.
	claimTx := spendTx(t, uint32(testClaimBlocks))
	claimSig, err := ClaimSigScript(b.script, b.sign(t, claimTx, b.terms.Others))
	if err != nil {
		t.Fatalf("claim sigscript: %v", err)
	}
	claimTx.TxIn[0].SignatureScript = claimSig
	early := spendTx(t, 0)
	early.TxIn[0].SignatureScript = claimSig
	if err := execute(t, b.script, early, csvFlags); err == nil {
		t.Fatal("a claim confirmed with no delay at all")
	}

	// The answer can, immediately - it spends the same output, so the claim
	// is dead rather than merely outvoted.
	answerTx := spendTx(t, 0)
	answerSig, err := AliveSigScript(b.script, b.sign(t, answerTx, b.terms.Members))
	if err != nil {
		t.Fatalf("alive sigscript: %v", err)
	}
	answerTx.TxIn[0].SignatureScript = answerSig
	if err := execute(t, b.script, answerTx, csvFlags); err != nil {
		t.Fatalf("a player who came back could not answer the claim: %v", err)
	}
}

// A table full of liars cannot take an honest player's bond, because the answer
// is on chain and they have no way to stop it. This is what makes the race safe
// where an adjudicated version would not be.
func TestACollusionOfEverybodyElseStillCannotTakeTheBond(t *testing.T) {
	b := postTableBond(t, 6)

	// They can open a claim - nothing stops that, and nothing needs to.
	claimTx := spendTx(t, uint32(testClaimBlocks))
	claimSig, err := ClaimSigScript(b.script, b.sign(t, claimTx, b.terms.Others))
	if err != nil {
		t.Fatalf("claim sigscript: %v", err)
	}
	claimTx.TxIn[0].SignatureScript = claimSig
	if err := execute(t, b.script, claimTx, csvFlags); err != nil {
		t.Fatalf("a claim by every other member did not stand up on its own: %v", err)
	}

	// What they cannot do is stop the answer, which needs no delay.
	answerTx := spendTx(t, 0)
	answerSig, err := AliveSigScript(b.script, b.sign(t, answerTx, b.terms.Members))
	if err != nil {
		t.Fatalf("alive sigscript: %v", err)
	}
	answerTx.TxIn[0].SignatureScript = answerSig
	if err := execute(t, b.script, answerTx, csvFlags); err != nil {
		t.Fatalf("the accused could not answer a claim the whole table had signed: %v", err)
	}
}

// One player must not be able to open a claim alone, or a claim becomes a way
// to harass whoever you like.
func TestOnePlayerCannotClaimAlone(t *testing.T) {
	b := postTableBond(t, 6)
	tx := spendTx(t, uint32(testClaimBlocks))

	for i := range b.terms.Others {
		// Everyone else signs; one abstains.
		partial := make([][]byte, 0, len(b.terms.Others))
		for j, m := range b.terms.Others {
			if j == i {
				continue
			}
			partial = append(partial, m)
		}
		sigs := b.sign(t, tx, partial)
		// Pad so the sig script builds; the missing member's slot is filled
		// with somebody else's signature, which must not satisfy the check.
		sigs = append(sigs, sigs[0])
		sig, err := ClaimSigScript(b.script, sigs)
		if err != nil {
			t.Fatalf("claim sigscript: %v", err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, b.script, tx, csvFlags); err == nil {
			t.Fatalf("a claim stood up without member %d agreeing", i)
		}
	}
}

// The owner must not be able to take their own bond back mid-table by claiming
// to be alive, or the bond is not locked up at all.
func TestTheOwnerCannotReleaseTheBondAlone(t *testing.T) {
	b := postTableBond(t, 3)
	tx := spendTx(t, 0)

	// Their own signature, repeated into every slot.
	own := signInput(t, b.privs[0], b.script, tx)
	sigs := make([][]byte, len(b.terms.Members))
	for i := range sigs {
		sigs[i] = own
	}
	sig, err := AliveSigScript(b.script, sigs)
	if err != nil {
		t.Fatalf("alive sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err == nil {
		t.Fatal("the owner released their own bond without the table")
	}

	// Nor through the backstop before its lock matures.
	early := spendTx(t, testLockBlocks-1)
	back, err := BackstopSigScript(b.script, signInput(t, b.privs[0], b.script, early))
	if err != nil {
		t.Fatalf("backstop sigscript: %v", err)
	}
	early.TxIn[0].SignatureScript = back
	if err := execute(t, b.script, early, csvFlags); err == nil {
		t.Fatal("the owner reclaimed their bond before the backstop matured")
	}
}

// A table that simply dissolves must not strand the coin forever.
func TestTheBackstopEventuallyReturnsTheBond(t *testing.T) {
	b := postTableBond(t, 3)
	tx := spendTx(t, testLockBlocks)
	sig, err := BackstopSigScript(b.script, signInput(t, b.privs[0], b.script, tx))
	if err != nil {
		t.Fatalf("backstop sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err != nil {
		t.Fatalf("the owner could not reclaim a stranded bond: %v", err)
	}
}

// Reading a bond back has to be exact: what matters is not that coin is locked
// but which roster can take it.
func TestATableBondIsReadBackExactly(t *testing.T) {
	b := postTableBond(t, 4)

	if string(b.terms.Owner) != string(b.privs[0].PubKey().SerializeCompressed()) {
		t.Fatal("parsed the wrong owner")
	}
	if len(b.terms.Members) != 4 || len(b.terms.Others) != 3 {
		t.Fatalf("parsed %d members and %d others, want 4 and 3",
			len(b.terms.Members), len(b.terms.Others))
	}
	if b.terms.ClaimBlocks != testClaimBlocks || b.terms.LockBlocks != testLockBlocks {
		t.Fatalf("parsed %d/%d blocks, want %d/%d",
			b.terms.ClaimBlocks, b.terms.LockBlocks, testClaimBlocks, testLockBlocks)
	}
	if _, err := MemberIndex(b.terms, b.terms.Owner); err != nil {
		t.Fatalf("the owner is not among the members: %v", err)
	}
	strangers, _ := memberKeys(t, 1)
	if _, err := MemberIndex(b.terms, strangers[0].PubKey().SerializeCompressed()); err == nil {
		t.Fatal("a key not at the table was reported as a member")
	}

	// Other kinds of bond must not read as this one.
	plain, err := BondScript(b.terms.Owner, testLockBlocks)
	if err != nil {
		t.Fatalf("plain bond: %v", err)
	}
	if _, err := ParseTableBond(plain); err == nil {
		t.Fatal("a plain bond parsed as a table bond")
	}
}

// The builder refuses what it cannot make safe.
func TestTheTableBondBuilderRefusesUnsafeTerms(t *testing.T) {
	_, pubs := memberKeys(t, 3)
	strangers, _ := memberKeys(t, 1)
	stranger := strangers[0].PubKey().SerializeCompressed()

	for _, tc := range []struct {
		name    string
		owner   []byte
		members [][]byte
		claim   uint32
		lock    uint32
	}{
		{"an owner who is not at the table", stranger, pubs, testClaimBlocks, testLockBlocks},
		{"a table of one", pubs[0], pubs[:1], testClaimBlocks, testLockBlocks},
		{"a claim nobody could answer", pubs[0], pubs, 0, testLockBlocks},
		{"a lock under the minimum", pubs[0], pubs, testClaimBlocks, MinBondBlocks - 1},
		{"a lock nothing could satisfy", pubs[0], pubs, testClaimBlocks, MaxCSVBlocks + 1},
		// The one that would quietly undo the whole mechanism: if the owner's
		// own way out matures no later than a claim, they simply outrun it.
		{"a backstop no later than the claim", pubs[0], pubs, MinBondBlocks, MinBondBlocks},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TableBondScript(tc.owner, tc.members, tc.claim, tc.lock); err == nil {
				t.Fatal("built a bond that should have been refused")
			}
		})
	}
}

// A peer must never sign a bond release it did not derive itself.
//
// The release needs every member's signature, so a seat that signed whatever
// arrived would hand the sender a transaction paying wherever they liked with
// everybody's names on it. This is the only thing standing between a
// co-operative release and a way to rob the table politely.
func TestABondReleaseIsRefusedUnlessItIsTheOneWeWouldBuild(t *testing.T) {
	b := postTableBond(t, 2)
	spent := spendTx(t, 0)
	d := AliveDraft{
		Bond:       b.script,
		Prevout:    spent.TxIn[0].PreviousOutPoint,
		ValueAtoms: spent.TxIn[0].ValueIn,
		PayScript:  spent.TxOut[0].PkScript,
		FeeAtoms:   2000,
	}

	honest, err := BuildAlive(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := CheckAliveDraft(honest, d); err != nil {
		t.Fatalf("the transaction we build ourselves was refused: %v", err)
	}

	// Paying somewhere else, which is the attack.
	elsewhere := honest.Copy()
	elsewhere.TxOut[0].PkScript = append(
		append([]byte(nil), honest.TxOut[0].PkScript...), 0x51)
	if err := CheckAliveDraft(elsewhere, d); err == nil {
		t.Fatal("a release paying somebody else was accepted")
	}

	// Keeping more than the fee.
	greedy := honest.Copy()
	greedy.TxOut[0].Value -= 1000
	if err := CheckAliveDraft(greedy, d); err == nil {
		t.Fatal("a release skimming the difference was accepted")
	}

	// Spending some other output.
	elsewhereIn := honest.Copy()
	elsewhereIn.TxIn[0].PreviousOutPoint.Index++
	if err := CheckAliveDraft(elsewhereIn, d); err == nil {
		t.Fatal("a release spending a different output was accepted")
	}

	// And a timelocked sequence, which would be the backstop branch wearing
	// the alive branch's clothes.
	locked := honest.Copy()
	locked.TxIn[0].Sequence = 42
	if err := CheckAliveDraft(locked, d); err == nil {
		t.Fatal("a release carrying a timelock was accepted")
	}
}
