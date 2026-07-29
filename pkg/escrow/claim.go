package escrow

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

// Spending a table bond, when more than one person has to agree.
//
// The backstop needs one signature and goes through BuildTimelockedSpend like
// any other timelocked reclaim. The other two branches need several, and that
// changes the problem: signatures are made over the transaction, so every
// co-signer has to build *byte for byte the same transaction* before signing
// anything. A builder that produced almost the same bytes on two machines would
// collect signatures that cannot be combined, and the failure would look like a
// disagreement about the facts rather than a rounding difference.
//
// So nothing here takes a number the caller could compute differently. Outputs
// are ordered by the bond's own canonical member order, the split is exact
// division with the remainder placed by rule, and the whole thing is run
// through the real script engine before it is handed back.

// MinShareAtoms is the least a claim may pay one seat.
//
// A share below the network's dust threshold makes the whole transaction
// unrelayable, so a bond small enough to be split into dust cannot be claimed at
// all - which would quietly turn "abandonment is punished" into "abandonment is
// punished at some table sizes". Better to refuse to build it and say so.
const MinShareAtoms int64 = 20_000

// Shares divides a claimed bond among the seats taking it.
//
// Exact division, with the remainder given one atom at a time to the earliest
// seats in canonical order. Any rule would do so long as every peer applies the
// same one; what would not do is dropping the remainder into the fee, which
// looks tidy and makes the amount depend on how the caller rounded.
func Shares(total int64, n int) ([]int64, error) {
	if n <= 0 {
		return nil, fmt.Errorf("nobody to divide %d atoms between", total)
	}
	if total <= 0 {
		return nil, fmt.Errorf("there is nothing to divide")
	}
	base, rem := total/int64(n), total%int64(n)
	out := make([]int64, n)
	for i := range out {
		out[i] = base
		if int64(i) < rem {
			out[i]++
		}
	}
	if out[n-1] < MinShareAtoms {
		return nil, fmt.Errorf("dividing %d atoms between %d seats leaves %d each, "+
			"under the %d minimum", total, n, out[n-1], MinShareAtoms)
	}
	return out, nil
}

// ClaimDraft is everything needed to build the transaction that takes an absent
// player's bond.
type ClaimDraft struct {
	// Bond is the table bond script being spent.
	Bond []byte
	// Prevout and ValueAtoms identify the deposit. The value is not
	// bookkeeping - Decred's signature hash commits to it, so a wrong one
	// produces signatures that verify against nothing.
	Prevout    wire.OutPoint
	ValueAtoms int64
	// PayScripts is where each claiming seat is paid, in the same canonical
	// order the bond script lists them: PayScripts[i] belongs to
	// ParseTableBond(Bond).Others[i].
	//
	// The order is not a convenience. It decides the transaction's bytes,
	// and therefore what everybody is signing, so it has to come from the
	// bond rather than from whoever assembled the message.
	PayScripts [][]byte
	FeeAtoms   int64
}

// BuildClaim builds the unsigned transaction that divides an absent player's
// bond among the other seats.
//
// Unsigned, because no one peer can finish it. Each seat signs the result and
// sends its signature back; FinishClaim puts them together. A peer that built
// this from a different draft gets different bytes and its signature simply
// will not fit, which is the intended outcome - two peers who disagree about
// what is being paid should fail to co-sign rather than agree by accident.
func BuildClaim(d ClaimDraft) (*wire.MsgTx, error) {
	terms, err := ParseTableBond(d.Bond)
	if err != nil {
		return nil, err
	}
	if len(d.PayScripts) != len(terms.Others) {
		return nil, fmt.Errorf("a claim on this bond pays %d seats, and this names %d",
			len(terms.Others), len(d.PayScripts))
	}
	for i, p := range d.PayScripts {
		if len(p) == 0 {
			return nil, fmt.Errorf("seat %d has nowhere to be paid", i)
		}
	}
	if d.ValueAtoms <= 0 {
		return nil, fmt.Errorf("the bond being claimed holds nothing")
	}
	if d.FeeAtoms < 0 {
		return nil, fmt.Errorf("a negative fee")
	}
	shares, err := Shares(d.ValueAtoms-d.FeeAtoms, len(terms.Others))
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: d.Prevout,
		ValueIn:          d.ValueAtoms,
		// The claim branch is timelocked, and this is the only thing that
		// satisfies it. A claim built without it fails at the engine
		// rather than at broadcast.
		Sequence: terms.ClaimBlocks,
	})
	for i, p := range d.PayScripts {
		tx.AddTxOut(wire.NewTxOut(shares[i], p))
	}
	return tx, nil
}

// AliveDraft is the transaction that answers a claim, and the same one that
// releases a bond when a table ends properly.
//
// One type for both because they are one transaction: every member signs, there
// is no timelock, and the bond goes wherever the draft says. What differs is
// only where that is. At a table's end it is the owner's own address. Answering
// a claim it is a fresh table bond on the same terms - because spending the
// output is what kills the claim, and the game cannot carry on without a bond
// behind the seat. Build that address with Address(newBondScript, params).
type AliveDraft struct {
	Bond       []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
	PayScript  []byte
	FeeAtoms   int64
}

// BuildAlive builds the unsigned transaction that answers a claim or releases a
// bond.
//
// It carries no timelock, and that is the mechanism rather than an omission: an
// answer confirms while a claim on the same output is still waiting out its
// delay, so coming back always beats being accused.
func BuildAlive(d AliveDraft) (*wire.MsgTx, error) {
	if _, err := ParseTableBond(d.Bond); err != nil {
		return nil, err
	}
	if len(d.PayScript) == 0 {
		return nil, fmt.Errorf("nowhere to pay the bond")
	}
	if d.ValueAtoms <= 0 {
		return nil, fmt.Errorf("the bond holds nothing")
	}
	if d.FeeAtoms < 0 {
		return nil, fmt.Errorf("a negative fee")
	}
	payout := d.ValueAtoms - d.FeeAtoms
	if payout < MinShareAtoms {
		return nil, fmt.Errorf("a fee of %d leaves %d of %d, under the %d minimum",
			d.FeeAtoms, payout, d.ValueAtoms, MinShareAtoms)
	}

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: d.Prevout,
		ValueIn:          d.ValueAtoms,
		Sequence:         0,
	})
	tx.AddTxOut(wire.NewTxOut(payout, d.PayScript))
	return tx, nil
}

// SignBondSpend produces one member's signature over a bond spend.
//
// The same call serves both branches. What a signature commits to is the
// transaction and the script, not which branch will be selected, so the
// selection happens when the witness is assembled and not here.
func SignBondSpend(tx *wire.MsgTx, bond []byte, key *secp256k1.PrivateKey) ([]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("no transaction to sign")
	}
	if key == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if _, err := ParseTableBond(bond); err != nil {
		return nil, err
	}
	sighash, err := txscript.CalcSignatureHash(bond, txscript.SigHashAll, tx, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("signature hash: %w", err)
	}
	sig, err := schnorr.Sign(key, sighash)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return append(sig.Serialize(), byte(txscript.SigHashAll)), nil
}

// FinishClaim attaches the collected signatures and checks the result against
// the real script engine.
//
// Signatures are in the bond's canonical order of everyone except the owner,
// which is the order the script checks them in. Running the engine here is what
// turns a missing or misplaced signature into an error somebody can read,
// instead of a transaction the network rejects for a reason nothing in it
// points at.
func FinishClaim(tx *wire.MsgTx, bond []byte, sigs [][]byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	sigScript, err := ClaimSigScript(bond, sigs)
	if err != nil {
		return nil, err
	}
	return finishBondSpend(tx, bond, sigScript, params)
}

// FinishAlive attaches every member's signature to an answer or a release.
func FinishAlive(tx *wire.MsgTx, bond []byte, sigs [][]byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	sigScript, err := AliveSigScript(bond, sigs)
	if err != nil {
		return nil, err
	}
	return finishBondSpend(tx, bond, sigScript, params)
}

func finishBondSpend(tx *wire.MsgTx, bond, sigScript []byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	if tx == nil || len(tx.TxIn) != 1 {
		return nil, fmt.Errorf("a bond spend has exactly one input")
	}
	out := tx.Copy()
	out.TxIn[0].SignatureScript = sigScript

	_, pkScript, err := Address(bond, params)
	if err != nil {
		return nil, fmt.Errorf("derive the bond's address: %w", err)
	}
	vm, err := txscript.NewEngine(pkScript, out, 0,
		txscript.ScriptVerifyCheckSequenceVerify, scriptVersion, nil)
	if err != nil {
		return nil, fmt.Errorf("build the script engine: %w", err)
	}
	if err := vm.Execute(); err != nil {
		return nil, fmt.Errorf("the spend does not satisfy the bond script: %w", err)
	}
	return out, nil
}

// CheckClaimDraft is what a seat runs before putting its signature to somebody
// else's claim.
//
// A claim arrives as a transaction to sign, and signing one without looking is
// how a seat ends up authorising a spend that pays somebody else entirely. The
// three things worth checking are all here: that it really spends the bond
// named, that this seat is paid what the division says it should be, and that
// nothing else has been slipped into the outputs.
//
// What it deliberately does not check is whether the accused player really
// left. Nothing can check that, and a seat that declines to sign because it
// believes otherwise simply does not sign.
func CheckClaimDraft(tx *wire.MsgTx, d ClaimDraft) error {
	want, err := BuildClaim(d)
	if err != nil {
		return err
	}
	if tx == nil {
		return fmt.Errorf("no transaction")
	}
	if len(tx.TxIn) != 1 || len(tx.TxOut) != len(want.TxOut) {
		return fmt.Errorf("a claim on this bond has 1 input and %d outputs; this has %d and %d",
			len(want.TxOut), len(tx.TxIn), len(tx.TxOut))
	}
	if tx.Version != want.Version {
		return fmt.Errorf("claim is version %d, want %d", tx.Version, want.Version)
	}
	if tx.TxIn[0].PreviousOutPoint != want.TxIn[0].PreviousOutPoint {
		return fmt.Errorf("claim spends a different output than the bond named")
	}
	if tx.TxIn[0].Sequence != want.TxIn[0].Sequence {
		return fmt.Errorf("claim's sequence is %d, which does not satisfy the claim delay",
			tx.TxIn[0].Sequence)
	}
	if tx.TxIn[0].ValueIn != want.TxIn[0].ValueIn {
		return fmt.Errorf("claim states the bond holds %d, not %d",
			tx.TxIn[0].ValueIn, want.TxIn[0].ValueIn)
	}
	for i := range want.TxOut {
		if tx.TxOut[i].Value != want.TxOut[i].Value {
			return fmt.Errorf("output %d pays %d, and the division says %d",
				i, tx.TxOut[i].Value, want.TxOut[i].Value)
		}
		if !bytes.Equal(tx.TxOut[i].PkScript, want.TxOut[i].PkScript) {
			return fmt.Errorf("output %d pays somewhere other than seat %d's address", i, i)
		}
		if tx.TxOut[i].Version != want.TxOut[i].Version {
			return fmt.Errorf("output %d is script version %d, want %d",
				i, tx.TxOut[i].Version, want.TxOut[i].Version)
		}
	}
	return nil
}

// RefreshDraft is a bond being respent into an identical bond.
//
// The answer to a claim, and the reason it has to be pre-signed. The branch that
// answers needs every member's signature - including the seats doing the
// claiming, who will not give it once they have started. So it is agreed while
// everybody is still cooperating, and the owner keeps it against the day
// somebody says they have gone.
//
// It pays back into the same script, which is what makes it safe to hand over in
// advance: broadcasting it early gains the owner nothing, because their coin
// lands straight back under the identical lock. All it does is spend the output
// a claim is against, and a claim whose input is gone is dead.
type RefreshDraft struct {
	Bond       []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
	FeeAtoms   int64
	Params     stdaddr.AddressParams
}

// BuildRefresh builds the unsigned transaction that answers a claim.
//
// Deterministic, like every other multi-signed draft here: every member has to
// build the same bytes or the signatures gathered in advance will not fit the
// transaction they were gathered for.
func BuildRefresh(d RefreshDraft) (*wire.MsgTx, error) {
	if _, err := ParseTableBond(d.Bond); err != nil {
		return nil, err
	}
	_, pkScript, err := Address(d.Bond, d.Params)
	if err != nil {
		return nil, fmt.Errorf("derive the bond's address: %w", err)
	}
	return BuildAlive(AliveDraft{
		Bond:       d.Bond,
		Prevout:    d.Prevout,
		ValueAtoms: d.ValueAtoms,
		PayScript:  pkScript,
		FeeAtoms:   d.FeeAtoms,
	})
}

// CheckRefreshDraft is what a member runs before pre-signing somebody else's
// answer.
//
// The one thing worth checking is that it really does pay back into the same
// bond. A signature on anything else would be a signature on that seat taking
// its bond out early, which is exactly what the lock exists to prevent - and it
// would be handed over months before anybody looked at it again.
func CheckRefreshDraft(tx *wire.MsgTx, d RefreshDraft) error {
	want, err := BuildRefresh(d)
	if err != nil {
		return err
	}
	if tx == nil || len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return fmt.Errorf("a refresh has one input and one output")
	}
	if tx.Version != want.Version {
		return fmt.Errorf("refresh is version %d, want %d", tx.Version, want.Version)
	}
	if tx.TxIn[0].PreviousOutPoint != want.TxIn[0].PreviousOutPoint {
		return fmt.Errorf("refresh spends a different output than the bond named")
	}
	if tx.TxIn[0].ValueIn != want.TxIn[0].ValueIn {
		return fmt.Errorf("refresh states the bond holds %d, not %d",
			tx.TxIn[0].ValueIn, want.TxIn[0].ValueIn)
	}
	if tx.TxIn[0].Sequence != want.TxIn[0].Sequence {
		return fmt.Errorf("refresh carries a sequence of %d, and the branch that answers has no timelock",
			tx.TxIn[0].Sequence)
	}
	if tx.TxOut[0].Value != want.TxOut[0].Value {
		return fmt.Errorf("refresh pays out %d, and the fee leaves %d",
			tx.TxOut[0].Value, want.TxOut[0].Value)
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, want.TxOut[0].PkScript) {
		return fmt.Errorf("refresh pays somewhere other than back into the same bond")
	}
	return nil
}

// RefreshDepth is how many times a seat can answer a claim on signatures it
// already holds.
//
// Answering spends the bond, so it lands at a new outpoint and the answer just
// used is gone. A second claim needs a second answer, against an output that did
// not exist when the first was agreed - and asking the table to sign one then
// puts the accused back where it started, needing help from the people accusing
// it.
//
// It does not have to. Decred hashes a transaction's prefix and its witness
// separately, and the txid is the prefix alone, so the identity of an answer is
// fixed before anybody signs it. Every outpoint in the chain is therefore known
// in advance and the whole chain can be agreed at once.
//
// Eight is far past what a real table should ever see: a claim means a seat has
// been gone half an hour, and answering eight of them in one session is not a
// game anybody is playing. Each step costs a fee, so a deeper chain is cheap but
// not free.
const RefreshDepth = 8

// BuildRefreshChain builds every answer a seat may need, in order.
//
// The first spends the bond where it is now; each one after it spends the output
// the one before produces. A seat answers with the entry whose input is where
// its bond actually sits, so a peer that restarts and has forgotten how many it
// has used finds its place by looking at the chain rather than by counting.
func BuildRefreshChain(d RefreshDraft, depth int) ([]*wire.MsgTx, error) {
	if depth < 1 {
		return nil, fmt.Errorf("a chain of %d answers is no chain", depth)
	}
	if depth > RefreshDepth {
		return nil, fmt.Errorf("a chain of %d answers is beyond the %d agreed", depth, RefreshDepth)
	}
	out := make([]*wire.MsgTx, 0, depth)
	next := d
	for i := range depth {
		tx, err := BuildRefresh(next)
		if err != nil {
			return nil, fmt.Errorf("answer %d: %w", i, err)
		}
		out = append(out, tx)

		// The next one spends what this produces. Its outpoint is known
		// because the txid does not depend on the signatures - which is
		// the property the whole chain rests on.
		next.Prevout = wire.OutPoint{Hash: tx.TxHash(), Index: 0, Tree: wire.TxTreeRegular}
		next.ValueAtoms = tx.TxOut[0].Value
	}
	return out, nil
}

// CheckAliveDraft rebuilds the transaction a draft describes and refuses
// anything that is not it.
//
// The rule every co-signing step here follows: a peer signs what it derived
// itself, never what it was handed. A bond release is the one where that matters
// most - it needs every member's signature, so a seat that signed whatever
// arrived would be handing the sender a transaction paying wherever they liked,
// with everybody's names on it. Rebuilding is the whole defence, and there is
// deliberately no field-by-field trust in the incoming transaction beyond
// comparing it with our own.
func CheckAliveDraft(tx *wire.MsgTx, d AliveDraft) error {
	want, err := BuildAlive(d)
	if err != nil {
		return err
	}
	if tx == nil || len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return fmt.Errorf("a bond release has one input and one output")
	}
	if tx.Version != want.Version {
		return fmt.Errorf("release is version %d, want %d", tx.Version, want.Version)
	}
	if tx.TxIn[0].PreviousOutPoint != want.TxIn[0].PreviousOutPoint {
		return fmt.Errorf("release spends a different output than the bond named")
	}
	if tx.TxIn[0].ValueIn != want.TxIn[0].ValueIn {
		return fmt.Errorf("release states the bond holds %d, not %d",
			tx.TxIn[0].ValueIn, want.TxIn[0].ValueIn)
	}
	if tx.TxIn[0].Sequence != want.TxIn[0].Sequence {
		return fmt.Errorf("release carries a sequence of %d, and the branch every member signs has no timelock",
			tx.TxIn[0].Sequence)
	}
	if tx.TxOut[0].Value != want.TxOut[0].Value {
		return fmt.Errorf("release pays out %d, and the fee leaves %d",
			tx.TxOut[0].Value, want.TxOut[0].Value)
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, want.TxOut[0].PkScript) {
		return fmt.Errorf("release pays somewhere other than where this seat asked to be paid")
	}
	return nil
}
