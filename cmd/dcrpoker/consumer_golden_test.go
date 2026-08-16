package main

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// These pins stay in this module forever: go test never runs a dependency's
// tests, so the packages' own copies of these vectors do not run here.

const (
	goldenRedeemHex     = "632103413cd76706482cf339e15b57701b7a6843b459b41232f0d19207524bdbc8757a52bf2103548f21ec4e636b3704c56307da438ea1783c759932202c85220a1e6a3736c21252bf2103af5e04babf346972d67746c26dd2775c9dcd9605123d1ab9ed9ab2c341bf843f52bf51670140b2752103af5e04babf346972d67746c26dd2775c9dcd9605123d1ab9ed9ab2c341bf843f52bf5168"
	goldenDepositAddr   = "DcmtzgmXggCmtc5sjYcvDbWCjVSiFkddz2k"
	goldenDepositScript = "a9149e5fe1f87151c5cc9eb0a200084322d1972976a787"
	goldenTermsHash     = "398a281024e3fb3ae4991517f6db469e8c4cddfd2b63ac20dffa78e1590e94ad"
)

// goldenPub pins a compressed public key to a literal scalar, exactly as the
// package-side vectors do, so both sides derive from the same inputs.
func goldenPub(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("the literal %q is not a 32-byte scalar", s)
	}
	return secp256k1.PrivKeyFromBytes(b).PubKey().SerializeCompressed()
}

func TestTheConsumedEscrowScriptIsPinned(t *testing.T) {
	k1 := goldenPub(t, "2f5c8b1e4d7a09361c2e5b8f0a4d7c1963b2e5d8f14a7c0e3d6b9a2c5e8f1047")
	k2 := goldenPub(t, "4a7d0c3f6e9b2581d4f7a0c3e6b9d2f5081b4e7a0d3c6f9b2e5a8d1c4f70b3e6")
	k3 := goldenPub(t, "691e4b7d0a3c6f92e5b8d1f4a7c0e3961d4b7a0e3d6c9f2b5e8a1d4c7f0a3b6d")

	redeem, err := escrow.RedeemScript(k2, [][]byte{k1, k2, k3}, 64)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}
	if got := hex.EncodeToString(redeem); got != goldenRedeemHex {
		t.Fatalf("RedeemScript is %s, want the pinned %s", got, goldenRedeemHex)
	}

	a, pkScript, err := escrow.Address(redeem, chaincfg.MainNetParams())
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if got := a.String(); got != goldenDepositAddr {
		t.Fatalf("the deposit address is %s, want the pinned %s", got, goldenDepositAddr)
	}
	if got := hex.EncodeToString(pkScript); got != goldenDepositScript {
		t.Fatalf("the deposit pkScript is %s, want the pinned %s", got, goldenDepositScript)
	}
}

func TestTheConsumedTermsHashIsPinned(t *testing.T) {
	terms := membership.Terms{
		Game:       "poker",
		GameVer:    5,
		SID:        "1f2e3d4c5b6a7988",
		BuyInAtoms: 25_000_000,
		Seats:      2,
		CSVBlocks:  144,
		Until:      987654,
	}
	th, err := terms.Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	if got := hex.EncodeToString(th[:]); got != goldenTermsHash {
		t.Fatalf("Terms.Hash is %s, want the pinned %s", got, goldenTermsHash)
	}
}
