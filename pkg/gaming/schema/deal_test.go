package schema

import (
	"testing"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
)

const dealMatch = "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9"

// A shuffle that verifies in memory and fails after crossing a wire is the
// exact fault that makes Geometry's implementation unusable peer to peer, and
// the only path this protocol ever uses is the wire. So the round trip is
// tested against a real shuffle and a real proof rather than against a struct
// that happens to have the same fields.
func TestAShuffleSurvivesTheWireAndStillVerifies(t *testing.T) {
	kp := deck.NewKeyPair()
	other := deck.NewKeyPair()
	joint, err := deck.JointKey([]kyber.Point{kp.Public, other.Public})
	if err != nil {
		t.Fatalf("joint key: %v", err)
	}
	in := deck.Fresh(joint)
	ctx := deck.Context{Match: dealMatch, Hand: 3, Round: 0, Prover: kp.Public}

	out, prf, _, err := deck.Shuffle(ctx, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	if err := deck.VerifyShuffle(ctx, joint, in, out, prf); err != nil {
		t.Fatalf("the shuffle did not verify before it was sent: %v", err)
	}

	body, err := ShuffleFrom(driver.OutShuffle{Seat: 0, Deck: out, Proof: prf}, 3)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	blob, err := Encode(KindShuffle, dealMatch, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	t.Logf("a shuffle is %d bytes on the wire", len(blob))

	msg, err := Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Kind != KindShuffle {
		t.Fatalf("decoded kind %q", msg.Kind)
	}
	var wire Shuffle
	if err := msg.Into(&wire); err != nil {
		t.Fatalf("into: %v", err)
	}
	got, err := wire.Into()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := deck.VerifyShuffle(ctx, joint, in, got.Deck, got.Proof); err != nil {
		t.Fatalf("a shuffle stopped verifying after crossing the wire: %v", err)
	}

	// And a deck bent on the way is caught, rather than being read as some
	// other deck that happens to parse.
	bent := body
	raw := []byte(bent.Deck)
	raw[10] ^= 0x01
	bent.Deck = string(raw)
	if bad, err := bent.Into(); err == nil {
		if deck.VerifyShuffle(ctx, joint, in, bad.Deck, bad.Proof) == nil {
			t.Fatal("a deck altered in flight still verified")
		}
	}
}

// The same for a share, whose proof binds it to the card it opens.
func TestAShareSurvivesTheWireAndStillVerifies(t *testing.T) {
	kp := deck.NewKeyPair()
	other := deck.NewKeyPair()
	pubs := []kyber.Point{kp.Public, other.Public}
	joint, err := deck.JointKey(pubs)
	if err != nil {
		t.Fatalf("joint key: %v", err)
	}
	d := deck.Fresh(joint)
	ctx := deck.Context{Match: dealMatch, Hand: 3}

	s, err := deck.Reveal(ctx, joint, kp, d[7])
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}

	body, err := ShareFrom(driver.OutShare{Seat: 0, Slot: 7, Share: s}, 3)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	blob, err := Encode(KindShare, dealMatch, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	t.Logf("a share is %d bytes on the wire", len(blob))

	msg, err := Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var wire Share
	if err := msg.Into(&wire); err != nil {
		t.Fatalf("into: %v", err)
	}
	got, err := wire.Into()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Slot != 7 || got.Seat != 0 {
		t.Fatalf("a share came back for seat %d slot %d", got.Seat, got.Slot)
	}

	// The real check: it still opens the card it was made for.
	o, err := deck.NewOpening(ctx, joint, d[7], pubs)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := o.Add(kp.Public, got.Share); err != nil {
		t.Fatalf("a share stopped verifying after crossing the wire: %v", err)
	}
}

// A card key is a group element and has to come back as the same one, or every
// peer computes a different joint key and nothing verifies anywhere.
func TestACardKeySurvivesTheWire(t *testing.T) {
	kp := deck.NewKeyPair()
	pop, err := deck.ProvePossession(dealMatch, 5, 2, kp)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	body, err := CardKeyFrom(driver.OutCardKey{Seat: 2, Hand: 5, Key: kp.Public, Pop: pop})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	blob, err := Encode(KindCardKey, dealMatch, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var wire CardKey
	if err := msg.Into(&wire); err != nil {
		t.Fatalf("into: %v", err)
	}
	got, err := wire.Into()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Seat != 2 || got.Hand != 5 {
		t.Fatalf("a card key came back for seat %d hand %d", got.Seat, got.Hand)
	}
	if !got.Key.Equal(kp.Public) {
		t.Fatal("a card key came back as a different point")
	}
	if err := deck.VerifyPossession(dealMatch, 5, 2, got.Key, got.Pop); err != nil {
		t.Fatalf("the possession proof did not survive the wire: %v", err)
	}
}

// Malformed dealing messages are refused rather than read as something else.
func TestMalformedDealingMessagesAreRefused(t *testing.T) {
	if _, err := (CardKey{Key: "not base64!"}).Into(); err == nil {
		t.Fatal("a card key that is not a point was accepted")
	}
	if _, err := (Share{D: "", Proof: ""}).Into(); err == nil {
		t.Fatal("an empty share was accepted")
	}
	for _, s := range []Shuffle{
		{Deck: "", Proof: ""},
		{Deck: b64([]byte{1, 2, 3}), Proof: ""},
		{Deck: b64(make([]byte, 64*10)), Proof: ""}, // ten cards, not fifty-two
	} {
		if _, err := s.Into(); err == nil {
			t.Fatalf("a shuffle carrying %d bytes of deck was accepted", len(s.Deck))
		}
	}
}
