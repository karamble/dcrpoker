package schema

import (
	"encoding/json"
	"testing"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
)

// disputed builds a real two-seat shuffle worth disputing: the input, the
// output, the proof and the secret behind it.
func disputed(t *testing.T) (joint kyber.Point, in, out deck.Deck, prf []byte, sec *deck.ShuffleSecret) {
	t.Helper()
	kp, other := deck.NewKeyPair(), deck.NewKeyPair()
	j, err := deck.JointKey([]kyber.Point{kp.Public, other.Public})
	if err != nil {
		t.Fatalf("joint key: %v", err)
	}
	fresh := deck.Fresh(j)
	ctx := deck.Context{Match: dealMatch, Hand: 3, Round: 0, Prover: kp.Public}
	o, p, s, err := deck.Shuffle(ctx, j, fresh)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	return j, fresh, o, p, s
}

// A complaint carries the refused frame whole, so a peer that lost the
// original still holds everything the verdict needs.
func TestAShuffleComplaintSurvivesTheWire(t *testing.T) {
	_, in, out, prf, _ := disputed(t)

	body, err := ShuffleComplaintFrom(0, 1, 3, 1, in, out, prf, []byte{0xaa}, []byte{0xbb})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	blob, err := Encode(KindShuffleComplaint, dealMatch, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var wire ShuffleComplaint
	if err := msg.Into(&wire); err != nil {
		t.Fatalf("into: %v", err)
	}
	gotIn, gotOut, gotPrf, refusedSig, sig, err := wire.Into()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !deck.SameDeck(gotIn, in) || !deck.SameDeck(gotOut, out) {
		t.Fatal("a deck came back as a different masking")
	}
	if len(gotPrf) != len(prf) || len(refusedSig) != 1 || len(sig) != 1 {
		t.Fatal("the frame's parts did not survive")
	}
}

// The stored dispute round-trips exactly - it answers and judges after a
// restart, so what is read back must be what was written down.
func TestAComplaintViewRoundTrips(t *testing.T) {
	joint, in, out, prf, _ := disputed(t)
	_ = joint

	c, err := ShuffleComplaintFrom(0, 1, 3, 1, in, out, prf, []byte{0xaa}, []byte{0xbb})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	db, err := deckBytes(in)
	if err != nil {
		t.Fatalf("deck: %v", err)
	}
	v := &ComplaintView{
		Match: dealMatch, Hand: 3, Round: 1, By: 0,
		Pubs:      []string{"aa", "bb"},
		Steps:     []StepView{{Deck: b64(db), Proof: b64(prf)}},
		Complaint: c,
		Verdict:   "",
	}
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ComplaintView
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Hand != 3 || back.Round != 1 || back.By != 0 || len(back.Steps) != 1 ||
		back.Complaint.Input != c.Input {
		t.Fatal("the stored dispute did not round-trip")
	}
}
