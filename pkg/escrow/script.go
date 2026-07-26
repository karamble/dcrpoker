// Package escrow builds the per-depositor escrow scripts used to hold a
// player's stake for a hand.
//
// The scripts are shared between the poker server and the client: both sides
// must derive byte-identical scripts from the same table, since the script hash
// is the deposit address and a one-byte disagreement sends money somewhere
// nobody can spend it. Writing the construction twice is how that happens, so
// it lives here once.
package escrow

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/decred/dcrd/dcrec"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

const (
	// MaxMembers caps a table at the referee's SNG/WTA seat limit. The
	// settlement branch carries one key per member, so this also bounds the
	// script size.
	MaxMembers = 6

	// PubKeyLen is the length of a compressed secp256k1 public key.
	PubKeyLen = 33

	// SigLen is the length of a Decred consensus Schnorr signature: 64 bytes
	// of [r,s] plus a trailing hash type byte.
	SigLen = 65

	// scriptVersion is the only script version these escrows use.
	scriptVersion = 0
)

// sigType selects secp256k1 Schnorr for OP_CHECKSIGALTVERIFY. The escrow has
// to be Schnorr because settlement runs on adaptor signatures, and Decred has
// no multisig opcode that accepts an alternative signature type — hence one
// OP_CHECKSIGALTVERIFY per member rather than an OP_CHECKMULTISIG.
const sigType = int64(dcrec.STSchnorrSecp256k1)

// RedeemScript builds a depositor's escrow redeem script.
//
//	OP_IF
//	  <member[0]> 2 OP_CHECKSIGALTVERIFY    one per member, canonical order
//	  ...
//	  OP_TRUE
//	OP_ELSE
//	  <csvBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP
//	  <owner> 2 OP_CHECKSIGALTVERIFY
//	  OP_TRUE
//	OP_ENDIF
//
// The settlement branch requires a signature from every table member, so a
// depositor cannot move their own stake once it is funded: the only spends that
// can ever satisfy it are transactions every member signed, which are the
// settlement drafts agreed before the hand. Without that a losing player could
// sweep their stake mid-hand and strand the settlement, since a signature over
// one transaction says nothing about any other.
//
// The refund branch stays unilateral. Recovering your own funds after the CSV
// timeout is the liveness backstop and must never depend on anyone else being
// online or willing.
//
// owner must be one of members. Members may be passed in any order; they are
// sorted canonically, and CanonicalMembers reports the order signatures must
// then be supplied in.
func RedeemScript(owner []byte, members [][]byte, csvBlocks uint32) ([]byte, error) {
	if err := checkPubKey(owner); err != nil {
		return nil, fmt.Errorf("owner key: %w", err)
	}
	if csvBlocks == 0 {
		return nil, fmt.Errorf("csv blocks must be non-zero")
	}
	sorted, err := CanonicalMembers(members)
	if err != nil {
		return nil, err
	}
	if !containsKey(sorted, owner) {
		return nil, fmt.Errorf("owner key is not a table member")
	}

	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF)
	for _, m := range sorted {
		b.AddData(m).
			AddInt64(sigType).
			AddOp(txscript.OP_CHECKSIGALTVERIFY)
	}
	b.AddOp(txscript.OP_TRUE).
		AddOp(txscript.OP_ELSE).
		AddInt64(int64(csvBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(owner).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_TRUE).
		AddOp(txscript.OP_ENDIF)

	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("redeem script is %d bytes, over the %d byte push limit",
			len(script), txscript.MaxScriptElementSize)
	}
	return script, nil
}

// CanonicalMembers returns members sorted by compressed key bytes. The order
// fixes both the script layout and the order signatures are supplied in, so
// every participant derives the same script and the same signing order from the
// same set of keys regardless of seat assignment.
func CanonicalMembers(members [][]byte) ([][]byte, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("no table members")
	}
	if len(members) > MaxMembers {
		return nil, fmt.Errorf("%d members exceeds the maximum of %d", len(members), MaxMembers)
	}

	out := make([][]byte, 0, len(members))
	for i, m := range members {
		if err := checkPubKey(m); err != nil {
			return nil, fmt.Errorf("member %d: %w", i, err)
		}
		out = append(out, append([]byte(nil), m...))
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	for i := 1; i < len(out); i++ {
		if bytes.Equal(out[i-1], out[i]) {
			return nil, fmt.Errorf("duplicate table member key")
		}
	}
	return out, nil
}

// MemberCount reports how many signatures a redeem script's settlement branch
// requires, by counting the checks before the refund branch begins.
func MemberCount(redeem []byte) (int, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, redeem)
	n := 0
	for tokenizer.Next() {
		switch tokenizer.Opcode() {
		case txscript.OP_CHECKSIGALTVERIFY:
			n++
		case txscript.OP_ELSE:
			if err := tokenizer.Err(); err != nil {
				return 0, err
			}
			if n == 0 {
				return 0, fmt.Errorf("settlement branch has no signature checks")
			}
			return n, nil
		}
	}
	if err := tokenizer.Err(); err != nil {
		return 0, fmt.Errorf("parse redeem script: %w", err)
	}
	return 0, fmt.Errorf("redeem script has no refund branch")
}

// SettlementSigScript builds the signature script that spends the settlement
// branch. sigs are indexed as CanonicalMembers reports, one per member.
//
// Each OP_CHECKSIGALTVERIFY consumes the signature on top of the stack, and the
// script checks members in ascending canonical order, so the signatures are
// pushed in reverse: the last member's goes on first and ends up deepest.
func SettlementSigScript(redeem []byte, sigs [][]byte) ([]byte, error) {
	want, err := MemberCount(redeem)
	if err != nil {
		return nil, err
	}
	if len(sigs) != want {
		return nil, fmt.Errorf("got %d signatures, redeem script requires %d", len(sigs), want)
	}
	for i, sig := range sigs {
		if len(sig) != SigLen {
			return nil, fmt.Errorf("signature %d is %d bytes, want %d", i, len(sig), SigLen)
		}
	}

	b := txscript.NewScriptBuilder()
	for i := len(sigs) - 1; i >= 0; i-- {
		b.AddData(sigs[i])
	}
	b.AddOp(txscript.OP_1) // select the settlement branch
	b.AddData(redeem)
	return b.Script()
}

// RefundSigScript builds the signature script that spends the refund branch
// once CSV has matured. The spending input's sequence must satisfy the
// script's relative timelock.
func RefundSigScript(redeem, ownerSig []byte) ([]byte, error) {
	if len(ownerSig) != SigLen {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(ownerSig), SigLen)
	}
	b := txscript.NewScriptBuilder()
	b.AddData(ownerSig)
	b.AddOp(txscript.OP_0) // select the refund branch
	b.AddData(redeem)
	return b.Script()
}

// Address returns the P2SH address a redeem script's deposits are paid to,
// along with the pkScript that funds it.
func Address(redeem []byte, params stdaddr.AddressParams) (stdaddr.Address, []byte, error) {
	a, err := stdaddr.NewAddressScriptHash(scriptVersion, redeem, params)
	if err != nil {
		return nil, nil, err
	}
	_, pkScript := a.PaymentScript()
	return a, pkScript, nil
}

func checkPubKey(key []byte) error {
	if len(key) != PubKeyLen {
		return fmt.Errorf("got %d bytes, want a %d byte compressed key", len(key), PubKeyLen)
	}
	if _, err := secp256k1.ParsePubKey(key); err != nil {
		return fmt.Errorf("not a valid public key: %w", err)
	}
	return nil
}

func containsKey(keys [][]byte, want []byte) bool {
	for _, k := range keys {
		if bytes.Equal(k, want) {
			return true
		}
	}
	return false
}
