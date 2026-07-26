package client

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
)

const (
	testPrivHex    = "0000000000000000000000000000000000000000000000000000000000000002"
	testPayoutAddr = "TsRnk22spGQJTpKFcRBc281rmfNFpywh337"
	testOtherAddr  = "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5"

	testInputAtoms  = 1_000_000
	testCSVBlocks   = uint32(64)
	testPayoutAtoms = testInputAtoms - int64(DefaultMaxSettlementFeeAtoms)
)

func testPolicy() PresignPolicy {
	return PresignPolicy{PayoutAddress: testPayoutAddr}
}

// testOwnPub is the session key matching the private scalar the tests presign
// with, so ownership checks resolve the fixture's single input to us.
func testOwnPub(t *testing.T) []byte {
	t.Helper()
	pub, err := pubFromPrivHex(testPrivHex)
	if err != nil {
		t.Fatalf("derive own pubkey: %v", err)
	}
	return pub
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
	if err := validateNeedPreSigs(need, testPolicy(), testOwnPub(t)); err == nil {
		t.Fatalf("expected rejection when our own branch pays another address")
	}
}

func TestValidateNeedPreSigsRejectsExcessiveFee(t *testing.T) {
	need := buildTestNeedPreSigsTo(t, testPayoutAddr, testPayoutAtoms-1, 0)
	if err := validateNeedPreSigs(need, testPolicy(), testOwnPub(t)); err == nil {
		t.Fatalf("expected rejection when the fee exceeds the cap")
	}
}

func TestValidateNeedPreSigsRejectsExtraOutputs(t *testing.T) {
	need := buildTestNeedPreSigsTo(t, testPayoutAddr, testPayoutAtoms, 1)
	if err := validateNeedPreSigs(need, testPolicy(), testOwnPub(t)); err == nil {
		t.Fatalf("expected rejection of a draft with more than one output")
	}
}

func TestValidateNeedPreSigsRequiresPayoutAddress(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	if err := validateNeedPreSigs(need, PresignPolicy{}, testOwnPub(t)); err == nil {
		t.Fatalf("expected refusal to presign our own branch with no payout address")
	}
}

// A branch that pays another seat carries an address this client cannot know,
// so only its shape and fee are checkable. The beneficiary checks the rest.
func TestValidateNeedPreSigsAllowsForeignBranch(t *testing.T) {
	need := buildTestNeedPreSigsTo(t, testOtherAddr, testPayoutAtoms, 0)
	need.Branch = 1 // our only input sits at index 0
	if err := validateNeedPreSigs(need, testPolicy(), testOwnPub(t)); err != nil {
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
	if err := validateNeedPreSigs(need, testPolicy(), testOwnPub(t)); err != nil {
		t.Fatalf("expected valid need presigs, got %v", err)
	}
}

func TestValidateNeedPreSigsRejectsTamper(t *testing.T) {
	need := buildTestNeedPreSigs(t)

	need.Inputs[0].SighashHex = "00" + need.Inputs[0].SighashHex[2:]
	if err := validateNeedPreSigs(need, testPolicy(), testOwnPub(t)); err == nil {
		t.Fatalf("expected mismatch error after sighash tamper")
	}

	need = buildTestNeedPreSigs(t)
	need.Inputs[0].InputId = "00" + need.Inputs[0].InputId[2:]
	if err := validateNeedPreSigs(need, testPolicy(), testOwnPub(t)); err == nil {
		t.Fatalf("expected input mismatch after txid tamper")
	}
}

func TestBuildVerifyOkUsesValidation(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	if _, err := BuildVerifyOk(testPrivHex, need, testPolicy()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Tamper sighash to trigger same validation failure.
	need.Inputs[0].SighashHex = "00" + need.Inputs[0].SighashHex[2:]
	if _, err := BuildVerifyOk(testPrivHex, need, testPolicy()); err == nil {
		t.Fatalf("expected validation error on tampered sighash")
	}
}

func TestVerifyPreSigRejectsAlteredPresig(t *testing.T) {
	need := buildTestNeedPreSigs(t)

	x := bytes.Repeat([]byte{0x03}, 32)
	xPrivHex := hex.EncodeToString(x)
	compPub := secp256k1.PrivKeyFromBytes(x).PubKey().SerializeCompressed()

	signed, err := BuildVerifyOk(xPrivHex, need, testPolicy())
	if err != nil {
		t.Fatalf("build presigs: %v", err)
	}
	if len(signed.PreSigs) != 1 {
		t.Fatalf("unexpected presig count %d", len(signed.PreSigs))
	}

	// Happy path verify.
	if err := VerifyPreSig(need, compPub, signed.PreSigs[0]); err != nil {
		t.Fatalf("verify presig failed: %v", err)
	}

	// Tamper s' to simulate server altering stored presig.
	signed.PreSigs[0].SPrimeHex = "00" + signed.PreSigs[0].SPrimeHex[2:]
	if err := VerifyPreSig(need, compPub, signed.PreSigs[0]); err == nil {
		t.Fatalf("expected verification failure on altered presig")
	}
}

// With every input of every branch now arriving, a client adaptor-presigns only
// the input it owns and plainly co-signs the rest.
func TestBuildPresigsSplitsOwnedAndForeignInputs(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	ownPub := testOwnPub(t)

	// Mark the existing input as ours and add one owned by somebody else.
	need.Inputs[0].OwnerPubkey = ownPub
	otherPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	own := need.Inputs[0]
	foreign := &pokerrpc.NeedPreSigsInput{
		InputId:         "ff" + own.InputId[2:],
		RedeemScriptHex: own.RedeemScriptHex,
		SighashHex:      own.SighashHex,
		AdaptorPointHex: own.AdaptorPointHex,
		InputIndex:      1,
		AmountAtoms:     own.AmountAtoms,
		OwnerPubkey:     otherPriv.PubKey().SerializeCompressed(),
	}
	need.Inputs = append(need.Inputs, foreign)

	signed, err := buildPresigs(testPrivHex, need, ownPub)
	if err != nil {
		t.Fatalf("build presigs: %v", err)
	}
	if len(signed.PreSigs) != 1 || signed.PreSigs[0].InputId != need.Inputs[0].InputId {
		t.Fatalf("expected one adaptor presig over our own input, got %d", len(signed.PreSigs))
	}
	if len(signed.CoSigs) != 1 || signed.CoSigs[0].InputId != foreign.InputId {
		t.Fatalf("expected one co-signature over the foreign input, got %d", len(signed.CoSigs))
	}
	if !bytes.Equal(signed.CoSigs[0].SignerPubkey, ownPub) {
		t.Fatalf("co-signature is not attributed to us")
	}
	// 65 bytes: 64 of [r,s] plus the hash type byte.
	if len(signed.CoSigs[0].SigHex) != 130 {
		t.Fatalf("co-signature is %d hex chars, want 130", len(signed.CoSigs[0].SigHex))
	}
}

// A draft carrying no input of ours is not something we should be signing.
func TestBuildPresigsRejectsDraftWeDoNotOwn(t *testing.T) {
	need := buildTestNeedPreSigs(t)
	otherPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	need.Inputs[0].OwnerPubkey = otherPriv.PubKey().SerializeCompressed()

	if _, err := buildPresigs(testPrivHex, need, testOwnPub(t)); err == nil {
		t.Fatalf("expected refusal to sign a draft with no input of ours")
	}
}

// buildRosterResponse builds the OpenEscrowResponse an honest referee returns
// for a two-seat roster, along with the caller's own session key.
func buildRosterResponse(t *testing.T) (*pokerrpc.OpenEscrowResponse, []byte) {
	t.Helper()

	ownPub := testOwnPub(t)
	otherPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	members := [][]byte{ownPub, otherPriv.PubKey().SerializeCompressed()}

	redeem, err := escrow.RedeemScript(ownPub, members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}
	canonical, err := escrow.CanonicalMembers(members)
	if err != nil {
		t.Fatalf("canonical members: %v", err)
	}
	addr, err := stdaddr.NewAddressScriptHash(0, redeem, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("script hash address: %v", err)
	}
	_, pkScript := addr.PaymentScript()

	return &pokerrpc.OpenEscrowResponse{
		EscrowId:        "esc1",
		DepositAddr:     addr.String(),
		RedeemScriptHex: hex.EncodeToString(redeem),
		PkScriptHex:     hex.EncodeToString(pkScript),
		RosterReady:     true,
		MemberPubkeys:   canonical,
	}, ownPub
}

func TestVerifyEscrowRosterAcceptsHonestResponse(t *testing.T) {
	resp, ownPub := buildRosterResponse(t)
	if err := VerifyEscrowRoster(resp, ownPub, testCSVBlocks); err != nil {
		t.Fatalf("expected an honest roster response to verify, got %v", err)
	}
}

// The referee is the one party that gains from a script the table did not
// agree to, so each way it could hand back a bad one has to be caught before
// the client funds it.
func TestVerifyEscrowRosterRejectsTamperedResponses(t *testing.T) {
	strangerPriv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	stranger := strangerPriv.PubKey().SerializeCompressed()

	cases := []struct {
		name   string
		tamper func(*pokerrpc.OpenEscrowResponse, []byte)
	}{{
		name: "member swapped for a referee key",
		tamper: func(r *pokerrpc.OpenEscrowResponse, own []byte) {
			for i, m := range r.MemberPubkeys {
				if !bytes.Equal(m, own) {
					r.MemberPubkeys[i] = stranger
					break
				}
			}
		},
	}, {
		name: "roster reduced to the caller alone",
		tamper: func(r *pokerrpc.OpenEscrowResponse, own []byte) {
			r.MemberPubkeys = [][]byte{own}
		},
	}, {
		name: "no members reported",
		tamper: func(r *pokerrpc.OpenEscrowResponse, _ []byte) {
			r.MemberPubkeys = nil
		},
	}, {
		name: "redeem script does not match the roster",
		tamper: func(r *pokerrpc.OpenEscrowResponse, _ []byte) {
			raw, _ := hex.DecodeString(r.RedeemScriptHex)
			raw[len(raw)-2] ^= 0xff
			r.RedeemScriptHex = hex.EncodeToString(raw)
		},
	}, {
		name: "pk script pays a different script",
		tamper: func(r *pokerrpc.OpenEscrowResponse, _ []byte) {
			raw, _ := hex.DecodeString(r.PkScriptHex)
			raw[3] ^= 0xff
			r.PkScriptHex = hex.EncodeToString(raw)
		},
	}, {
		name: "deposit address points elsewhere",
		tamper: func(r *pokerrpc.OpenEscrowResponse, _ []byte) {
			r.DepositAddr = testPayoutAddr
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, ownPub := buildRosterResponse(t)
			tc.tamper(resp, ownPub)
			if err := VerifyEscrowRoster(resp, ownPub, testCSVBlocks); err == nil {
				t.Fatalf("expected rejection: %s", tc.name)
			}
		})
	}
}

// A client that accepted a script built for a different timelock would be
// funding an address whose refund branch it cannot spend on the schedule it
// asked for.
func TestVerifyEscrowRosterRejectsForeignCSV(t *testing.T) {
	resp, ownPub := buildRosterResponse(t)
	if err := VerifyEscrowRoster(resp, ownPub, testCSVBlocks+1); err == nil {
		t.Fatalf("expected rejection when the script commits to another csv delay")
	}
}
