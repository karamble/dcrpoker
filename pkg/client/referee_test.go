package client

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
)

const (
	testPayoutAddr = "TsRnk22spGQJTpKFcRBc281rmfNFpywh337"
	testOtherAddr  = "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5"

	testInputAtoms  = 1_000_000
	testPayoutAtoms = testInputAtoms - int64(DefaultMaxSettlementFeeAtoms)
)

func testPolicy() PresignPolicy {
	return PresignPolicy{PayoutAddress: testPayoutAddr}
}

func buildTestNeedPreSigs(t *testing.T) *pokerrpc.NeedPreSigs {
	t.Helper()
	return buildTestNeedPreSigsTo(t, testPayoutAddr, testPayoutAtoms, 0)
}

// buildTestNeedPreSigsTo builds a single-input winner-take-all draft paying
// payoutAddr, shaped the way the server's buildWTADrafts shapes one. extraOuts
// appends further outputs before the sighash is taken, so a draft with the
// wrong shape still carries a self-consistent sighash.
func buildTestNeedPreSigsTo(t *testing.T, payoutAddr string, payout int64, extraOuts int) *pokerrpc.NeedPreSigs {
	t.Helper()

	tx := wire.NewMsgTx()
	tx.Version = 3

	var h chainhash.Hash
	copy(h[:], bytes.Repeat([]byte{0x01}, chainhash.HashSize))
	outpoint := wire.NewOutPoint(&h, 0, wire.TxTreeRegular)
	tx.AddTxIn(wire.NewTxIn(outpoint, testInputAtoms, nil))

	script, err := txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
	if err != nil {
		t.Fatalf("build script: %v", err)
	}
	payScript, err := paymentScriptForAddress(payoutAddr)
	if err != nil {
		t.Fatalf("payout script: %v", err)
	}
	tx.AddTxOut(wire.NewTxOut(payout, payScript))
	for i := 0; i < extraOuts; i++ {
		tx.AddTxOut(wire.NewTxOut(1, script))
	}

	sighash, err := txscript.CalcSignatureHash(script, txscript.SigHashAll, tx, 0, nil)
	if err != nil {
		t.Fatalf("calc sighash: %v", err)
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize tx: %v", err)
	}

	priv := secp256k1.PrivKeyFromBytes([]byte{0x02})
	adaptorHex := hex.EncodeToString(priv.PubKey().SerializeCompressed())

	return &pokerrpc.NeedPreSigs{
		MatchId:    "m1",
		Branch:     0,
		DraftTxHex: hex.EncodeToString(buf.Bytes()),
		Inputs: []*pokerrpc.NeedPreSigsInput{
			{
				InputId:         h.String() + ":0",
				RedeemScriptHex: hex.EncodeToString(script),
				SighashHex:      hex.EncodeToString(sighash),
				AdaptorPointHex: adaptorHex,
				InputIndex:      0,
				AmountAtoms:     testInputAtoms,
			},
		},
	}
}

func TestValidateNeedPreSigsRejectsRedirectedPayout(t *testing.T) {
	need := buildTestNeedPreSigsTo(t, testOtherAddr, testPayoutAtoms, 0)
	if err := validateNeedPreSigs(need, testPolicy()); err == nil {
		t.Fatalf("expected rejection when our own branch pays another address")
	}
}

func TestValidateNeedPreSigsRejectsExcessiveFee(t *testing.T) {
	need := buildTestNeedPreSigsTo(t, testPayoutAddr, testPayoutAtoms-1, 0)
	if err := validateNeedPreSigs(need, testPolicy()); err == nil {
		t.Fatalf("expected rejection when the fee exceeds the cap")
	}
}

func TestValidateNeedPreSigsRejectsExtraOutputs(t *testing.T) {
	need := buildTestNeedPreSigsTo(t, testPayoutAddr, testPayoutAtoms, 1)
	if err := validateNeedPreSigs(need, testPolicy()); err == nil {
		t.Fatalf("expected rejection of a draft with more than one output")
	}
}

func TestValidateNeedPreSigsRequiresPayoutAddress(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	if err := validateNeedPreSigs(need, PresignPolicy{}); err == nil {
		t.Fatalf("expected refusal to presign our own branch with no payout address")
	}
}

// A branch that pays another seat carries an address this client cannot know,
// so only its shape and fee are checkable. The beneficiary checks the rest.
func TestValidateNeedPreSigsAllowsForeignBranch(t *testing.T) {
	need := buildTestNeedPreSigsTo(t, testOtherAddr, testPayoutAtoms, 0)
	need.Branch = 1 // our only input sits at index 0
	if err := validateNeedPreSigs(need, testPolicy()); err != nil {
		t.Fatalf("expected a foreign branch to validate on shape alone, got %v", err)
	}
}

func TestDraftBranchCountTracksInputs(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	n, err := draftBranchCount(need)
	if err != nil {
		t.Fatalf("branch count: %v", err)
	}
	if n != 1 {
		t.Fatalf("branch count = %d, want 1", n)
	}

	need.DraftTxHex = "zz"
	if _, err := draftBranchCount(need); err == nil {
		t.Fatalf("expected error on undecodable draft")
	}
}

func TestValidateNeedPreSigsOK(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	if err := validateNeedPreSigs(need, testPolicy()); err != nil {
		t.Fatalf("expected valid need presigs, got %v", err)
	}
}

func TestValidateNeedPreSigsRejectsTamper(t *testing.T) {
	need := buildTestNeedPreSigs(t)

	need.Inputs[0].SighashHex = "00" + need.Inputs[0].SighashHex[2:]
	if err := validateNeedPreSigs(need, testPolicy()); err == nil {
		t.Fatalf("expected mismatch error after sighash tamper")
	}

	need = buildTestNeedPreSigs(t)
	need.Inputs[0].InputId = "00" + need.Inputs[0].InputId[2:]
	if err := validateNeedPreSigs(need, testPolicy()); err == nil {
		t.Fatalf("expected input mismatch after txid tamper")
	}
}

func TestBuildVerifyOkUsesValidation(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	if _, err := BuildVerifyOk("02", need, testPolicy()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Tamper sighash to trigger same validation failure.
	need.Inputs[0].SighashHex = "00" + need.Inputs[0].SighashHex[2:]
	if _, err := BuildVerifyOk("02", need, testPolicy()); err == nil {
		t.Fatalf("expected validation error on tampered sighash")
	}
}

func TestVerifyPreSigRejectsAlteredPresig(t *testing.T) {
	need := buildTestNeedPreSigs(t)

	x := bytes.Repeat([]byte{0x03}, 32)
	xPrivHex := hex.EncodeToString(x)
	compPub := secp256k1.PrivKeyFromBytes(x).PubKey().SerializeCompressed()

	presigs, err := BuildVerifyOk(xPrivHex, need, testPolicy())
	if err != nil {
		t.Fatalf("build presigs: %v", err)
	}
	if len(presigs) != 1 {
		t.Fatalf("unexpected presig count %d", len(presigs))
	}

	// Happy path verify.
	if err := VerifyPreSig(need, compPub, presigs[0]); err != nil {
		t.Fatalf("verify presig failed: %v", err)
	}

	// Tamper s' to simulate server altering stored presig.
	presigs[0].SPrimeHex = "00" + presigs[0].SPrimeHex[2:]
	if err := VerifyPreSig(need, compPub, presigs[0]); err == nil {
		t.Fatalf("expected verification failure on altered presig")
	}
}
