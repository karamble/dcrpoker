package client

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
)

func TestSignActionProducesAVerifiableEntry(t *testing.T) {
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pc := &PokerClient{}
	if err := pc.SetActionSigner(3, hex.EncodeToString(priv.Serialize())); err != nil {
		t.Fatalf("set signer: %v", err)
	}

	head := make([]byte, 32)
	head[0] = 0xab
	pc.rememberGameUpdate(&pokerrpc.GameUpdate{
		LogHead:   head,
		LogSeq:    7,
		LogHand:   2,
		LogStreet: uint32(gamelog.StreetTurn),
	})

	signed, err := pc.signAction(gamelog.ActionBet, 500)
	if err != nil {
		t.Fatalf("sign action: %v", err)
	}
	if signed == nil {
		t.Fatal("expected a signed action")
	}

	// It must chain to where the table said it was, and be signed by us.
	if signed.GetSeq() != 8 {
		t.Fatalf("seq is %d, want one past the head", signed.GetSeq())
	}
	if !bytesEqual(signed.GetPrevHash(), head) {
		t.Fatal("action does not chain to the head we were given")
	}
	if signed.GetSeat() != 3 || signed.GetHand() != 2 ||
		signed.GetStreet() != uint32(gamelog.StreetTurn) {
		t.Fatal("action does not carry the position we were given")
	}
	if !bytesEqual(signed.GetSigner(), priv.PubKey().SerializeCompressed()) {
		t.Fatal("action is not attributed to our key")
	}

	// The signature has to survive the round trip through the wire shape,
	// since that is the only form the server ever sees.
	e := &gamelog.Entry{
		Version: uint16(signed.GetVersion()),
		Seq:     signed.GetSeq(),
		Hand:    signed.GetHand(),
		Street:  gamelog.Street(signed.GetStreet()),
		Seat:    signed.GetSeat(),
		Signer:  signed.GetSigner(),
		Action:  gamelog.Action(signed.GetAction()),
		Amount:  signed.GetAmount(),
		Sig:     signed.GetSig(),
	}
	copy(e.PrevHash[:], signed.GetPrevHash())
	if err := e.Verify(); err != nil {
		t.Fatalf("signature did not survive conversion to the wire shape: %v", err)
	}
}

// A table that keeps no log, or one we have not heard from yet, leaves nothing
// to chain to - and playing there must still work.
func TestSignActionIsSilentWithNothingToChainTo(t *testing.T) {
	priv, _ := secp256k1.GeneratePrivateKey()

	pc := &PokerClient{}
	if signed, err := pc.signAction(gamelog.ActionFold, 0); err != nil || signed != nil {
		t.Fatalf("no signer should mean no signature, got %v %v", signed, err)
	}

	if err := pc.SetActionSigner(0, hex.EncodeToString(priv.Serialize())); err != nil {
		t.Fatalf("set signer: %v", err)
	}
	if signed, err := pc.signAction(gamelog.ActionFold, 0); err != nil || signed != nil {
		t.Fatalf("no game update should mean no signature, got %v %v", signed, err)
	}

	// A table with no log reports an empty head.
	pc.rememberGameUpdate(&pokerrpc.GameUpdate{})
	if signed, err := pc.signAction(gamelog.ActionFold, 0); err != nil || signed != nil {
		t.Fatalf("an empty head should mean no signature, got %v %v", signed, err)
	}

	pc.ClearActionSigner()
	pc.rememberGameUpdate(&pokerrpc.GameUpdate{LogHead: make([]byte, 32)})
	if signed, err := pc.signAction(gamelog.ActionFold, 0); err != nil || signed != nil {
		t.Fatalf("a cleared signer should mean no signature, got %v %v", signed, err)
	}
}

func TestSetActionSignerRejectsBadKeys(t *testing.T) {
	pc := &PokerClient{}
	for _, k := range []string{"", "zz", "  "} {
		if err := pc.SetActionSigner(0, k); err == nil {
			t.Fatalf("expected %q to be refused as a signing key", k)
		}
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
