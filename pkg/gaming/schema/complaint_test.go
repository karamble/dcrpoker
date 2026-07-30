package schema

import (
	"encoding/json"
	"testing"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/util/random"

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

// The answer pins every length, exactly as the challenge secrets do: a
// permutation that is not deck-sized and blinding factors that are not
// 52 scalars are refused by the decoder, before any verdict is asked.
func TestAShuffleAnswerPinsItsLengths(t *testing.T) {
	_, _, _, _, sec := disputed(t)

	body, err := ShuffleAnswerFrom(1, 3, 1, sec, []byte{0xcc})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got, _, err := body.Into(); err != nil || len(got.Pi) != deck.Size || len(got.Beta) != deck.Size {
		t.Fatalf("an honest answer did not round-trip: %v", err)
	}

	short := body
	short.Pi = body.Pi[:deck.Size-1]
	if _, _, err := short.Into(); err == nil {
		t.Fatal("a short permutation was read")
	}
	truncated := body
	truncated.Beta = body.Beta[:len(body.Beta)-8]
	if _, _, err := truncated.Into(); err == nil {
		t.Fatal("truncated blinding factors were read")
	}
	if _, err := ShuffleAnswerFrom(1, 3, 1, &deck.ShuffleSecret{
		Pi:   sec.Pi[:3],
		Beta: sec.Beta,
	}, nil); err == nil {
		t.Fatal("a short secret was rendered")
	}
	if _, err := ShuffleAnswerFrom(1, 3, 1, &deck.ShuffleSecret{
		Pi:   sec.Pi,
		Beta: []kyber.Scalar{deck.Suite().Scalar().Pick(random.New())},
	}, nil); err == nil {
		t.Fatal("short blinding factors were rendered")
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
		back.Complaint.Input != c.Input || back.Answer != nil {
		t.Fatal("the stored dispute did not round-trip")
	}
}
