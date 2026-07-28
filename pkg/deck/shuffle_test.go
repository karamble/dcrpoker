package deck

import (
	"flag"
	"testing"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/util/random"
)

// The tests that matter.
//
// kyber's own shuffle tests are happy-path: they shuffle honestly and check the
// proof verifies. That establishes the protocol works when nobody is attacking
// it, which is not the question. Both reference implementations read before
// this one passed their own test suites while being trivially cheatable - one
// of them had a shuffle test that a deck of fifty-two identical aces would have
// passed, because it only checked that every peer agreed.
//
// So everything here is a malicious prover. If any of these ever starts
// passing, the deck is not a deck.

func testJoint(t *testing.T) kyber.Point {
	t.Helper()
	return suite.Point().Mul(suite.Scalar().Pick(random.New()), nil)
}

func testContext() Context {
	return Context{
		Match:  "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9",
		Hand:   7,
		Round:  1,
		Prover: nil, // filled per test
	}
}

func withProver(t *testing.T, c Context) Context {
	t.Helper()
	c.Prover = testJoint(t)
	return c
}

// The honest case, so the failures below mean something.
func TestAnHonestShuffleVerifies(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	if err := VerifyShuffle(c, joint, in, out, prf); err != nil {
		t.Fatalf("an honest shuffle did not verify: %v", err)
	}
	if len(out) != Size {
		t.Fatalf("shuffled %d cards into %d", Size, len(out))
	}

	// It really did move things: a shuffle that returned its input would
	// verify too, and would be useless.
	same := 0
	for i := range in {
		if in[i].C2.Equal(out[i].C2) {
			same++
		}
	}
	if same == Size {
		t.Fatal("the shuffle returned the deck unchanged")
	}
}

// Swapping two cards after the proof was made.
func TestASwappedCardIsRejected(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	out[0], out[1] = out[1], out[0]

	if err := VerifyShuffle(c, joint, in, out, prf); err == nil {
		t.Fatal("a deck with two cards swapped after proving was accepted")
	}
}

// A deck where one card appears twice - the attack that makes a shuffle proof
// worth having at all, and the one both reference implementations allow.
func TestADuplicatedCardIsRejected(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	out[5] = Masked{C1: out[9].C1.Clone(), C2: out[9].C2.Clone()}

	if err := VerifyShuffle(c, joint, in, out, prf); err == nil {
		t.Fatal("a deck holding the same card twice was accepted")
	}
}

// A point that is not a masking of anything in the input deck.
func TestACardFromNowhereIsRejected(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	out[13] = Masked{
		C1: suite.Point().Mul(suite.Scalar().Pick(random.New()), nil),
		C2: suite.Point().Mul(suite.Scalar().Pick(random.New()), nil),
	}

	if err := VerifyShuffle(c, joint, in, out, prf); err == nil {
		t.Fatal("a deck containing a point from outside the input was accepted")
	}
}

// A proof from an earlier round, presented for a later one.
//
// This is what binding the statement into the transcript buys. kyber seeds the
// challenge with the protocol name alone and PairShuffle never absorbs the
// decks, so a constant label would leave a proof meaning "some shuffle happened"
// rather than "this shuffle happened".
func TestAProofFromAnotherRoundIsRejected(t *testing.T) {
	joint := testJoint(t)
	first := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(first, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	if err := VerifyShuffle(first, joint, in, out, prf); err != nil {
		t.Fatalf("honest shuffle did not verify: %v", err)
	}

	second := first
	second.Round++
	if err := VerifyShuffle(second, joint, in, out, prf); err == nil {
		t.Fatal("a proof made for one round verified in another")
	}

	later := first
	later.Hand++
	if err := VerifyShuffle(later, joint, in, out, prf); err == nil {
		t.Fatal("a proof made for one hand verified in another")
	}

	elsewhere := first
	elsewhere.Match = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := VerifyShuffle(elsewhere, joint, in, out, prf); err == nil {
		t.Fatal("a proof made at one table verified at another")
	}
}

// One player's shuffle presented as another player's.
func TestAnotherPlayersProofIsRejected(t *testing.T) {
	joint := testJoint(t)
	mine := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(mine, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}

	theirs := mine
	theirs.Prover = testJoint(t)
	if err := VerifyShuffle(theirs, joint, in, out, prf); err == nil {
		t.Fatal("one player's proof verified as another player's")
	}
}

// A shuffle checked against a different joint key than it was made under.
func TestAProofUnderAnotherKeyIsRejected(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	if err := VerifyShuffle(c, testJoint(t), in, out, prf); err == nil {
		t.Fatal("a shuffle verified under a key it was not masked to")
	}
}

// Corrupting the proof has to be caught. A proof that tolerates edits is a
// proof that is not being read.
//
// Sampled rather than exhaustive by default: a Neff proof for 52 cards is
// several kilobytes and each check is a full verification, so flipping every
// byte is thousands of them. The sample strides the whole proof deterministically
// so it covers every region, and `-exhaustive` does the complete sweep when it
// is worth the minutes.
var exhaustive = flag.Bool("exhaustive", false, "flip every byte of a proof rather than a sample")

func TestACorruptedProofIsRejected(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	t.Logf("proof is %d bytes", len(prf))

	stride := 1
	if !*exhaustive {
		if s := len(prf) / 64; s > 1 {
			stride = s
		}
	}
	checked := 0
	for i := 0; i < len(prf); i += stride {
		bad := make([]byte, len(prf))
		copy(bad, prf)
		bad[i] ^= 0x01
		if err := VerifyShuffle(c, joint, in, out, bad); err == nil {
			t.Fatalf("a proof with byte %d flipped was accepted", i)
		}
		checked++
	}
	t.Logf("rejected %d corruptions", checked)
}

// Truncation and extension, which are not covered by flipping bits.
func TestAMisshapenProofIsRejected(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}

	for _, bad := range [][]byte{
		nil,
		{},
		prf[:len(prf)-1],
		prf[:len(prf)/2],
		prf[1:],
		append(append([]byte{}, prf...), 0x00),
		append(append([]byte{}, prf...), prf...),
	} {
		if err := VerifyShuffle(c, joint, in, out, bad); err == nil {
			t.Fatalf("a proof of length %d was accepted", len(bad))
		}
	}
}

// The length check that makes the test above pass is only sound if an honest
// proof really is a fixed size. That is a property of kyber's encoding, not of
// this package, so assert it rather than assume it: if a future version ever
// makes proof length depend on the permutation or the randomness, the check
// would start rejecting honest shuffles and this is what says so.
func TestAnHonestProofIsAlwaysTheSameLength(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	want, err := proofLen(Size)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	for i := 0; i < 8; i++ {
		_, prf, _, err := Shuffle(c, joint, in)
		if err != nil {
			t.Fatalf("shuffle: %v", err)
		}
		if len(prf) != want {
			t.Fatalf("proof %d is %d bytes, but an honest proof is %d", i, len(prf), want)
		}
	}
}

// Proofs and decks cross a network, so they have to survive being written down
// and read back. This is the exact path that breaks Geometry's implementation -
// its proofs carry projective points into Fiat-Shamir and normalise to affine on
// serialization, so a proof that verifies in memory fails after a round trip,
// and its tests never serialize.
func TestAShuffleSurvivesASerializationRoundTrip(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, prf, _, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}

	if err := VerifyShuffle(reread(t, c), reread2(t, joint), rereadDeck(t, in),
		rereadDeck(t, out), append([]byte{}, prf...)); err != nil {
		t.Fatalf("a shuffle stopped verifying after a round trip: %v", err)
	}
}

func reread2(t *testing.T, p kyber.Point) kyber.Point {
	t.Helper()
	b, err := p.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := suite.Point()
	if err := out.UnmarshalBinary(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func reread(t *testing.T, c Context) Context {
	t.Helper()
	c.Prover = reread2(t, c.Prover)
	return c
}

func rereadDeck(t *testing.T, d Deck) Deck {
	t.Helper()
	out := make(Deck, len(d))
	for i, m := range d {
		out[i] = Masked{C1: reread2(t, m.C1), C2: reread2(t, m.C2)}
	}
	return out
}
