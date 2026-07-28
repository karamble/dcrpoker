package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
)

// Building a spend more than one person signs has a failure mode that a single
// signer does not: two peers can build almost the same transaction, and the
// signatures then do not fit. That looks like a disagreement about the facts and
// is really a rounding difference, so most of what follows is about byte
// equality rather than about poker.

const (
	testBondAtoms = int64(10_000_000) // 0.1 DCR
	testBondFee   = int64(10_000)
)

// payTo builds a plausible payout script for each of n seats.
func payTo(t *testing.T, n int) [][]byte {
	t.Helper()
	out := make([][]byte, 0, n)
	for i := range n {
		s, err := txscript.NewScriptBuilder().
			AddOp(txscript.OP_DUP).
			AddOp(txscript.OP_HASH160).
			AddData(bytes.Repeat([]byte{byte(i + 1)}, 20)).
			AddOp(txscript.OP_EQUALVERIFY).
			AddOp(txscript.OP_CHECKSIG).
			Script()
		if err != nil {
			t.Fatalf("pay script: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func testDraft(t *testing.T, b *tableBond) ClaimDraft {
	t.Helper()
	return ClaimDraft{
		Bond:       b.script,
		Prevout:    testPrevout(),
		ValueAtoms: testBondAtoms,
		PayScripts: payTo(t, len(b.terms.Others)),
		FeeAtoms:   testBondFee,
	}
}

// The whole flow: an absent player's bond is divided among the seats that
// stayed, and the result satisfies the real script engine.
func TestAClaimIsBuiltSignedAndSatisfiesTheScript(t *testing.T) {
	for _, n := range []int{2, 3, 6} {
		b := postTableBond(t, n)
		d := testDraft(t, b)

		tx, err := BuildClaim(d)
		if err != nil {
			t.Fatalf("%d seats: build: %v", n, err)
		}
		sigs := make([][]byte, 0, len(b.terms.Others))
		for _, m := range b.terms.Others {
			sig, err := SignBondSpend(tx, b.script, privFor(t, b.privs, m))
			if err != nil {
				t.Fatalf("%d seats: sign: %v", n, err)
			}
			sigs = append(sigs, sig)
		}
		done, err := FinishClaim(tx, b.script, sigs, chaincfg.TestNet3Params())
		if err != nil {
			t.Fatalf("%d seats: finish: %v", n, err)
		}

		// Every atom is accounted for: what went in is what came out, plus
		// the fee and nothing else.
		var paid int64
		for _, o := range done.TxOut {
			paid += o.Value
		}
		if paid != testBondAtoms-testBondFee {
			t.Fatalf("%d seats: paid out %d of %d less %d fee",
				n, paid, testBondAtoms, testBondFee)
		}
		if len(done.TxOut) != n-1 {
			t.Fatalf("%d seats: paid %d seats, want %d", n, len(done.TxOut), n-1)
		}
	}
}

// Every co-signer has to build the same bytes, or their signatures cannot be
// combined at all.
func TestEveryPeerBuildsTheSameClaim(t *testing.T) {
	b := postTableBond(t, 4)
	d := testDraft(t, b)

	first, err := BuildClaim(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want, err := first.Bytes()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	for i := range 20 {
		again, err := BuildClaim(d)
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		got, err := again.Bytes()
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("two builds of one draft produced different transactions")
		}
	}
}

// The remainder of an uneven division has to land somewhere by rule, not
// wherever the arithmetic happened to put it.
func TestTheSplitIsExactAndDeterministic(t *testing.T) {
	for _, tc := range []struct {
		total int64
		n     int
		want  []int64
	}{
		{300_000, 3, []int64{100_000, 100_000, 100_000}},
		{300_001, 3, []int64{100_001, 100_000, 100_000}},
		{300_002, 3, []int64{100_001, 100_001, 100_000}},
		{100_000, 1, []int64{100_000}},
	} {
		got, err := Shares(tc.total, tc.n)
		if err != nil {
			t.Fatalf("%d/%d: %v", tc.total, tc.n, err)
		}
		var sum int64
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%d/%d: share %d is %d, want %d", tc.total, tc.n, i, got[i], tc.want[i])
			}
			sum += got[i]
		}
		if sum != tc.total {
			t.Fatalf("%d/%d: shares total %d, so %d atoms went missing",
				tc.total, tc.n, sum, tc.total-sum)
		}
	}

	// A bond too small to divide is refused rather than split into dust that
	// would make the whole transaction unrelayable.
	if _, err := Shares(1000, 6); err == nil {
		t.Fatal("a bond was split into dust")
	}
	if _, err := Shares(0, 2); err == nil {
		t.Fatal("nothing was divided between two seats")
	}
}

// An answer carries no timelock, which is what lets it beat a claim on the same
// output.
func TestAnAnswerIsBuiltAndBeatsAClaimOnTheSameOutput(t *testing.T) {
	b := postTableBond(t, 3)

	claim, err := BuildClaim(testDraft(t, b))
	if err != nil {
		t.Fatalf("build claim: %v", err)
	}
	if claim.TxIn[0].Sequence != b.terms.ClaimBlocks {
		t.Fatalf("a claim's sequence is %d, want the claim delay of %d",
			claim.TxIn[0].Sequence, b.terms.ClaimBlocks)
	}

	// The answer re-posts the bond on the same terms, which is what lets the
	// game carry on: spending the output is what kills the claim, and the
	// seat still needs a bond behind it.
	_, pkScript, err := Address(b.script, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	answer, err := BuildAlive(AliveDraft{
		Bond:       b.script,
		Prevout:    testPrevout(),
		ValueAtoms: testBondAtoms,
		PayScript:  pkScript,
		FeeAtoms:   testBondFee,
	})
	if err != nil {
		t.Fatalf("build answer: %v", err)
	}
	if answer.TxIn[0].Sequence != 0 {
		t.Fatalf("an answer's sequence is %d, want 0 so it confirms immediately",
			answer.TxIn[0].Sequence)
	}

	sigs := make([][]byte, 0, len(b.terms.Members))
	for _, m := range b.terms.Members {
		sig, err := SignBondSpend(answer, b.script, privFor(t, b.privs, m))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		sigs = append(sigs, sig)
	}
	done, err := FinishAlive(answer, b.script, sigs, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if !bytes.Equal(done.TxOut[0].PkScript, pkScript) {
		t.Fatal("the answer did not re-post the bond")
	}
	// Both spend the same output, so only one of them can ever confirm - and
	// the one with no delay is the one that can confirm now.
	if done.TxIn[0].PreviousOutPoint != claim.TxIn[0].PreviousOutPoint {
		t.Fatal("the answer does not spend the output the claim is against")
	}
}

// A seat asked to co-sign has to be able to check what it is signing, because
// signing without looking is how you authorise a spend that pays somebody else.
func TestASeatChecksAClaimBeforeSigningIt(t *testing.T) {
	b := postTableBond(t, 4)
	d := testDraft(t, b)

	honest, err := BuildClaim(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := CheckClaimDraft(honest, d); err != nil {
		t.Fatalf("an honest claim did not check out: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(*wire.MsgTx)
	}{
		{"paying somewhere else", func(tx *wire.MsgTx) {
			tx.TxOut[0].PkScript = payTo(t, 9)[8]
		}},
		{"paying one seat more", func(tx *wire.MsgTx) {
			tx.TxOut[0].Value += 1000
		}},
		{"quietly taking a fee", func(tx *wire.MsgTx) {
			tx.TxOut[0].Value -= 5000
		}},
		{"an extra output", func(tx *wire.MsgTx) {
			tx.AddTxOut(wire.NewTxOut(1000, payTo(t, 9)[8]))
		}},
		{"a different deposit", func(tx *wire.MsgTx) {
			var h chainhash.Hash
			copy(h[:], bytes.Repeat([]byte{0x22}, chainhash.HashSize))
			tx.TxIn[0].PreviousOutPoint.Hash = h
		}},
		{"a sequence that dodges the delay", func(tx *wire.MsgTx) {
			tx.TxIn[0].Sequence = 0
		}},
		{"claiming the bond is bigger than it is", func(tx *wire.MsgTx) {
			tx.TxIn[0].ValueIn = testBondAtoms * 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad, err := BuildClaim(d)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			tc.bend(bad)
			if err := CheckClaimDraft(bad, d); err == nil {
				t.Fatal("a seat would have signed a claim it should have refused")
			}
		})
	}
}

// A claim missing one seat's agreement must not stand up, and neither must one
// signed by somebody who is not at the table.
func TestAClaimNeedsTheRightSignatures(t *testing.T) {
	b := postTableBond(t, 4)
	d := testDraft(t, b)
	tx, err := BuildClaim(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	good := make([][]byte, 0, len(b.terms.Others))
	for _, m := range b.terms.Others {
		sig, err := SignBondSpend(tx, b.script, privFor(t, b.privs, m))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		good = append(good, sig)
	}
	params := chaincfg.TestNet3Params()

	t.Run("one seat's signature repeated for another", func(t *testing.T) {
		bad := append([][]byte(nil), good...)
		bad[1] = bad[0]
		if _, err := FinishClaim(tx, b.script, bad, params); err == nil {
			t.Fatal("a claim stood up with one seat signing twice")
		}
	})
	t.Run("the owner's own signature", func(t *testing.T) {
		bad := append([][]byte(nil), good...)
		sig, err := SignBondSpend(tx, b.script, b.privs[0])
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		bad[0] = sig
		if _, err := FinishClaim(tx, b.script, bad, params); err == nil {
			t.Fatal("the bond's owner helped claim their own bond")
		}
	})
	t.Run("a stranger's signature", func(t *testing.T) {
		outsiders, _ := memberKeys(t, 1)
		bad := append([][]byte(nil), good...)
		sig, err := SignBondSpend(tx, b.script, outsiders[0])
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		bad[0] = sig
		if _, err := FinishClaim(tx, b.script, bad, params); err == nil {
			t.Fatal("a claim stood up on a stranger's signature")
		}
	})
	t.Run("signatures in the wrong order", func(t *testing.T) {
		if len(good) < 2 {
			t.Skip("needs at least two claimants")
		}
		bad := append([][]byte(nil), good...)
		bad[0], bad[1] = bad[1], bad[0]
		if _, err := FinishClaim(tx, b.script, bad, params); err == nil {
			t.Fatal("a claim stood up with its signatures out of order")
		}
	})
	t.Run("a signature over a different transaction", func(t *testing.T) {
		other, err := BuildClaim(ClaimDraft{
			Bond: b.script, Prevout: testPrevout(),
			ValueAtoms: testBondAtoms - 1, PayScripts: d.PayScripts, FeeAtoms: testBondFee,
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		bad := append([][]byte(nil), good...)
		sig, err := SignBondSpend(other, b.script, privFor(t, b.privs, b.terms.Others[0]))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		bad[0] = sig
		if _, err := FinishClaim(tx, b.script, bad, params); err == nil {
			t.Fatal("a signature over another transaction was accepted")
		}
	})
}

// The builder refuses drafts it cannot make safe.
func TestTheClaimBuilderRefusesUnsafeDrafts(t *testing.T) {
	b := postTableBond(t, 3)
	ok := testDraft(t, b)

	for _, tc := range []struct {
		name string
		bend func(*ClaimDraft)
	}{
		{"too few payout addresses", func(d *ClaimDraft) { d.PayScripts = d.PayScripts[:1] }},
		{"a seat with nowhere to be paid", func(d *ClaimDraft) { d.PayScripts[0] = nil }},
		{"an empty deposit", func(d *ClaimDraft) { d.ValueAtoms = 0 }},
		{"a negative fee", func(d *ClaimDraft) { d.FeeAtoms = -1 }},
		{"a fee that eats the bond", func(d *ClaimDraft) { d.FeeAtoms = testBondAtoms }},
		{"a bond too small to divide", func(d *ClaimDraft) { d.ValueAtoms = 20_000 }},
		{"not a table bond at all", func(d *ClaimDraft) {
			plain, err := BondScript(b.terms.Owner, MinBondBlocks)
			if err != nil {
				t.Fatalf("plain bond: %v", err)
			}
			d.Bond = plain
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ok
			d.PayScripts = append([][]byte(nil), ok.PayScripts...)
			tc.bend(&d)
			if _, err := BuildClaim(d); err == nil {
				t.Fatal("built a claim that should have been refused")
			}
		})
	}
}

// The backstop needs one signature and goes through the existing builder, so
// what matters is that it still works against a table bond.
func TestTheBackstopBuildsThroughTheOrdinarySpendBuilder(t *testing.T) {
	b := postTableBond(t, 3)
	pay := payTo(t, 1)[0]

	tx, err := BuildTimelockedSpend(Spend{
		Key:        b.privs[0],
		Script:     b.script,
		Prevout:    testPrevout(),
		ValueAtoms: testBondAtoms,
		CSVBlocks:  b.terms.LockBlocks,
		PayScript:  pay,
		FeeAtoms:   testBondFee,
		SigScript:  func(script, sig []byte) ([]byte, error) { return BackstopSigScript(script, sig) },
		Params:     chaincfg.TestNet3Params(),
	})
	if err != nil {
		t.Fatalf("the owner could not reclaim a stranded bond: %v", err)
	}
	if tx.TxOut[0].Value != testBondAtoms-testBondFee {
		t.Fatalf("backstop paid %d, want %d", tx.TxOut[0].Value, testBondAtoms-testBondFee)
	}

	// And not a block early.
	if _, err := BuildTimelockedSpend(Spend{
		Key: b.privs[0], Script: b.script, Prevout: testPrevout(),
		ValueAtoms: testBondAtoms, CSVBlocks: b.terms.LockBlocks - 1,
		PayScript: pay, FeeAtoms: testBondFee,
		SigScript: func(script, sig []byte) ([]byte, error) { return BackstopSigScript(script, sig) },
		Params:    chaincfg.TestNet3Params(),
	}); err == nil {
		t.Fatal("the backstop was spent a block before it matured")
	}
}

// The answer to a claim has to be one the accused can actually produce.
//
// This is the test the earlier one should have been. TestAnAnswerBeatsAClaim
// signs with every member's key because the test holds them all - and a real
// accused peer holds exactly one. The branch that answers needs every signature
// including the accusers', who will not give it once they have started claiming,
// so an answer assembled at the time is not something that can exist.
//
// Which is why it is agreed in advance. Every member signs the refresh while the
// table is still cooperating, the owner keeps it, and the day somebody says they
// have gone it is already in their hand.
func TestAnAccusedSeatCanAnswerWithSignaturesItAlreadyHolds(t *testing.T) {
	b := postTableBond(t, 3)
	params := chaincfg.TestNet3Params()
	draft := RefreshDraft{
		Bond:       b.script,
		Prevout:    testPrevout(),
		ValueAtoms: testBondAtoms,
		FeeAtoms:   testBondFee,
		Params:     params,
	}

	// Agreed while everybody is cooperating.
	tx, err := BuildRefresh(draft)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	held := make([][]byte, 0, len(b.terms.Members))
	for _, m := range b.terms.Members {
		sig, err := SignBondSpend(tx, b.script, privFor(t, b.privs, m))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		held = append(held, sig)
	}

	// Later, with the others refusing to help, the owner assembles what it
	// already has and it satisfies the script.
	done, err := FinishAlive(tx, b.script, held, params)
	if err != nil {
		t.Fatalf("an accused seat could not answer with the signatures it held: %v", err)
	}

	// It pays back into the same bond, so holding it early is worth nothing.
	_, pkScript, err := Address(b.script, params)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if !bytes.Equal(done.TxOut[0].PkScript, pkScript) {
		t.Fatal("a refresh paid somewhere other than back into the same bond")
	}
	if done.TxIn[0].Sequence != 0 {
		t.Fatalf("a refresh carries a sequence of %d, so it could not beat a delayed claim",
			done.TxIn[0].Sequence)
	}
	// And it spends the output a claim would be against, which is what kills it.
	claimTx, err := BuildClaim(testDraft(t, b))
	if err != nil {
		t.Fatalf("build claim: %v", err)
	}
	if done.TxIn[0].PreviousOutPoint != claimTx.TxIn[0].PreviousOutPoint {
		t.Fatal("the answer does not spend the output the claim is against")
	}
}

// A member asked to pre-sign somebody's answer is being asked months in advance,
// so what it signs has to be checked. Anything but a payment back into the same
// bond is that seat taking its bond out early.
func TestPreSigningAnAnswerRefusesAnythingButTheSameBond(t *testing.T) {
	b := postTableBond(t, 3)
	params := chaincfg.TestNet3Params()
	draft := RefreshDraft{
		Bond:       b.script,
		Prevout:    testPrevout(),
		ValueAtoms: testBondAtoms,
		FeeAtoms:   testBondFee,
		Params:     params,
	}
	honest, err := BuildRefresh(draft)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := CheckRefreshDraft(honest, draft); err != nil {
		t.Fatalf("an honest refresh did not check out: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(*wire.MsgTx)
	}{
		{"paying the owner instead", func(tx *wire.MsgTx) {
			tx.TxOut[0].PkScript = payTo(t, 1)[0]
		}},
		{"keeping some of it", func(tx *wire.MsgTx) {
			tx.TxOut[0].Value -= 100_000
		}},
		{"an extra output", func(tx *wire.MsgTx) {
			tx.AddTxOut(wire.NewTxOut(1000, payTo(t, 1)[0]))
		}},
		{"a different deposit", func(tx *wire.MsgTx) {
			var h chainhash.Hash
			copy(h[:], bytes.Repeat([]byte{0x33}, chainhash.HashSize))
			tx.TxIn[0].PreviousOutPoint.Hash = h
		}},
		{"a timelock that could not beat a claim", func(tx *wire.MsgTx) {
			tx.TxIn[0].Sequence = 10
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad, err := BuildRefresh(draft)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			tc.bend(bad)
			if err := CheckRefreshDraft(bad, draft); err == nil {
				t.Fatal("a member would have pre-signed a bond leaving its own lock")
			}
		})
	}
}

// The chain of answers rests on one property of Decred's serialization: a
// transaction's identity is its prefix, and signatures live in the witness. If
// that ever stopped being true, every answer after the first would be built
// against an outpoint that never existed - so it is asserted here rather than
// assumed from a dependency.
func TestATransactionsIdentityDoesNotDependOnItsSignatures(t *testing.T) {
	b := postTableBond(t, 3)
	tx, err := BuildRefresh(RefreshDraft{
		Bond: b.script, Prevout: testPrevout(), ValueAtoms: testBondAtoms,
		FeeAtoms: testBondFee, Params: chaincfg.TestNet3Params(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	before := tx.TxHash()

	sigs := make([][]byte, 0, len(b.terms.Members))
	for _, m := range b.terms.Members {
		sig, err := SignBondSpend(tx, b.script, privFor(t, b.privs, m))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		sigs = append(sigs, sig)
	}
	done, err := FinishAlive(tx, b.script, sigs, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if done.TxHash() != before {
		t.Fatal("signing changed a transaction's identity, so no answer after the first could be pre-agreed")
	}
}

// A seat can answer more than once without going back to the people accusing it.
func TestASeatCanAnswerMoreThanOneClaim(t *testing.T) {
	b := postTableBond(t, 3)
	params := chaincfg.TestNet3Params()
	draft := RefreshDraft{
		Bond: b.script, Prevout: testPrevout(), ValueAtoms: testBondAtoms,
		FeeAtoms: testBondFee, Params: params,
	}
	chain, err := BuildRefreshChain(draft, RefreshDepth)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(chain) != RefreshDepth {
		t.Fatalf("built %d answers, want %d", len(chain), RefreshDepth)
	}

	// Each one spends what the one before produces, so every outpoint in the
	// chain is known before any of them is signed.
	for i := 1; i < len(chain); i++ {
		want := wire.OutPoint{Hash: chain[i-1].TxHash(), Index: 0, Tree: wire.TxTreeRegular}
		if chain[i].TxIn[0].PreviousOutPoint != want {
			t.Fatalf("answer %d does not spend what answer %d produces", i, i-1)
		}
		if chain[i].TxIn[0].ValueIn != chain[i-1].TxOut[0].Value {
			t.Fatalf("answer %d states a value of %d, and answer %d produces %d",
				i, chain[i].TxIn[0].ValueIn, i-1, chain[i-1].TxOut[0].Value)
		}
	}

	// Every one of them is signable now and satisfies the bond, which is what
	// lets the accused use any of them later without asking anybody.
	for i, tx := range chain {
		sigs := make([][]byte, 0, len(b.terms.Members))
		for _, m := range b.terms.Members {
			sig, err := SignBondSpend(tx, b.script, privFor(t, b.privs, m))
			if err != nil {
				t.Fatalf("answer %d: sign: %v", i, err)
			}
			sigs = append(sigs, sig)
		}
		if _, err := FinishAlive(tx, b.script, sigs, params); err != nil {
			t.Fatalf("answer %d does not satisfy the bond: %v", i, err)
		}
	}

	// The bond shrinks by a fee each time and stays worth having.
	last := chain[len(chain)-1].TxOut[0].Value
	if last != testBondAtoms-int64(len(chain))*testBondFee {
		t.Fatalf("after %d answers the bond holds %d", len(chain), last)
	}
	if last < MinShareAtoms {
		t.Fatalf("after %d answers the bond is down to %d, under the %d minimum",
			len(chain), last, MinShareAtoms)
	}
	if _, err := BuildRefreshChain(draft, RefreshDepth+1); err == nil {
		t.Fatal("a chain longer than the agreed depth was built")
	}
}
