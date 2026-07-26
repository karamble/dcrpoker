package escrow

import (
	"fmt"

	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

// MinBondBlocks is the shortest lock a bond may carry. A bond deters by being
// expensive to hold, so a lock short enough to roll over between tables deters
// nothing. At Decred's ~5 minute blocks this is about a week.
const MinBondBlocks uint32 = 2016

// BondScript builds a fidelity bond: a deposit its owner can reclaim only after
// a relative timelock, and that nobody else can ever spend.
//
//	<lockBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP <owner> 2 OP_CHECKSIGALTVERIFY OP_TRUE
//
// The bond is posted at registration, before a player takes a seat, and it is
// deliberately not forfeitable. That is a consequence of when it has to exist
// rather than a softening of the penalty: a bond forfeitable to the other
// players would have to name them in its script, and at registration there is
// no roster yet to name - that is the whole reason funding is roster-first. The
// only party known that early is the referee, and a bond forfeitable to the
// referee would hand it custody of exactly the funds this design takes away
// from it.
//
// So the cost is the lock-up, not a transfer. What it buys:
//
//   - Sybil resistance. zkidentity keys are free to generate, so anything keyed
//     to identity alone is worth nothing; a fresh identity needs a fresh bond
//     and a fresh week of locked coin.
//   - Something expensive for reputation to attach to, which is what makes a
//     record of past behaviour worth keeping at all.
//   - A standing deterrent against register-and-vanish, which roster-first
//     funding otherwise makes cheap: a seat held by someone who never funds
//     keeps every other stake waiting on its CSV.
//
// It does not compensate anyone for that wait. Compensation needs a forfeitable
// bond, which can only be posted once the roster is closed - alongside the
// stake, not at registration.
func BondScript(owner []byte, lockBlocks uint32) ([]byte, error) {
	if err := checkPubKey(owner); err != nil {
		return nil, fmt.Errorf("owner key: %w", err)
	}
	if lockBlocks < MinBondBlocks {
		return nil, fmt.Errorf("bond lock of %d blocks is under the %d block minimum",
			lockBlocks, MinBondBlocks)
	}

	b := txscript.NewScriptBuilder().
		AddInt64(int64(lockBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(owner).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_TRUE)

	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("bond script is %d bytes, over the %d byte push limit",
			len(script), txscript.MaxScriptElementSize)
	}
	return script, nil
}

// BondSigScript builds the signature script that reclaims a bond once its lock
// has matured. The spending input's sequence must satisfy the timelock.
func BondSigScript(bond, ownerSig []byte) ([]byte, error) {
	if len(ownerSig) != SigLen {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(ownerSig), SigLen)
	}
	return txscript.NewScriptBuilder().
		AddData(ownerSig).
		AddData(bond).
		Script()
}

// BondTerms is what a bond script commits to.
type BondTerms struct {
	Owner      []byte
	LockBlocks uint32
}

// ParseBond reads a bond script back, so a referee can check that a deposit a
// player points at really is a bond of theirs on the terms claimed rather than
// some other script that happens to hold coin.
func ParseBond(bond []byte) (*BondTerms, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, bond)

	var (
		terms   BondTerms
		ops     []byte
		haveCSV bool
	)
	for tokenizer.Next() {
		op := tokenizer.Opcode()
		ops = append(ops, op)
		switch {
		case op == txscript.OP_CHECKSEQUENCEVERIFY:
			// The lock is whatever was pushed immediately before.
			haveCSV = true
		case op == txscript.OP_DATA_33:
			terms.Owner = append([]byte(nil), tokenizer.Data()...)
		case !haveCSV && terms.LockBlocks == 0:
			if n, ok := smallIntOrData(tokenizer); ok {
				terms.LockBlocks = n
			}
		}
	}
	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("parse bond script: %w", err)
	}

	if !haveCSV {
		return nil, fmt.Errorf("script has no timelock; a bond without one is not locked up at all")
	}
	if err := checkPubKey(terms.Owner); err != nil {
		return nil, fmt.Errorf("bond owner key: %w", err)
	}
	if terms.LockBlocks == 0 {
		return nil, fmt.Errorf("bond script has no lock length")
	}

	// Rebuilding is the real check: it rejects anything with extra opcodes,
	// a second signature check, or any other path to the coin, which reading
	// opcode by opcode would let through.
	want, err := BondScript(terms.Owner, terms.LockBlocks)
	if err != nil {
		return nil, err
	}
	if len(want) != len(bond) {
		return nil, fmt.Errorf("script is not a bond: it has %d bytes, a bond on these terms has %d",
			len(bond), len(want))
	}
	for i := range want {
		if want[i] != bond[i] {
			return nil, fmt.Errorf("script is not a bond on the terms it appears to carry")
		}
	}
	return &terms, nil
}

// BondAddress returns the P2SH address a bond is posted to, and the pkScript
// that funds it.
func BondAddress(bond []byte, params stdaddr.AddressParams) (stdaddr.Address, []byte, error) {
	return Address(bond, params)
}

// smallIntOrData reads a pushed number, whether it arrived as a small-integer
// opcode or a data push.
func smallIntOrData(tok txscript.ScriptTokenizer) (uint32, bool) {
	op := tok.Opcode()
	if op == txscript.OP_0 {
		return 0, true
	}
	if op >= txscript.OP_1 && op <= txscript.OP_16 {
		return uint32(op-txscript.OP_1) + 1, true
	}
	data := tok.Data()
	if len(data) == 0 || len(data) > 4 {
		return 0, false
	}
	var n uint32
	for i := len(data) - 1; i >= 0; i-- {
		n = n<<8 | uint32(data[i])
	}
	return n, true
}
