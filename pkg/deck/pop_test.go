package deck

import (
	"testing"

	"go.dedis.ch/kyber/v4/util/random"
)

const popMatch = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// An honest possession proof verifies for exactly the statement it was made
// for.
func TestAPossessionProofRoundTrips(t *testing.T) {
	kp := NewKeyPair()
	pop, err := ProvePossession(popMatch, 7, 1, kp)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if err := VerifyPossession(popMatch, 7, 1, kp.Public, pop); err != nil {
		t.Fatalf("an honest proof was refused: %v", err)
	}
}

// Corrupting a possession proof, and misshaping it.
func TestACorruptedPossessionProofIsRejected(t *testing.T) {
	kp := NewKeyPair()
	pop, err := ProvePossession(popMatch, 7, 1, kp)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	t.Logf("possession proof is %d bytes", len(pop))

	for i := range pop {
		bad := make([]byte, len(pop))
		copy(bad, pop)
		bad[i] ^= 0x01
		if err := VerifyPossession(popMatch, 7, 1, kp.Public, bad); err == nil {
			t.Fatalf("a possession proof with byte %d flipped was accepted", i)
		}
	}

	for _, bad := range [][]byte{
		nil,
		{},
		pop[:len(pop)-1],
		pop[1:],
		append(append([]byte{}, pop...), 0x00),
		append(append([]byte{}, pop...), pop...),
	} {
		if err := VerifyPossession(popMatch, 7, 1, kp.Public, bad); err == nil {
			t.Fatalf("a possession proof of length %d was accepted", len(bad))
		}
	}
}

// The proof is bound to every part of its statement: the table, the hand, the
// seat and the point. Change any one and the same bytes prove nothing.
func TestAPossessionProofIsBoundToItsStatement(t *testing.T) {
	kp := NewKeyPair()
	pop, err := ProvePossession(popMatch, 7, 1, kp)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}

	other := NewKeyPair()
	for name, check := range map[string]error{
		"another table": VerifyPossession(popMatch[1:]+"0", 7, 1, kp.Public, pop),
		"a later hand":  VerifyPossession(popMatch, 8, 1, kp.Public, pop),
		"another seat":  VerifyPossession(popMatch, 7, 0, kp.Public, pop),
		"another key":   VerifyPossession(popMatch, 7, 1, other.Public, pop),
	} {
		if check == nil {
			t.Fatalf("a possession proof was accepted for %s", name)
		}
	}
}

// The attack the proof exists for. The last seat to announce can compute
// P_att = r*G - sum(others) and steer the joint key to r*G - but it knows r,
// not dlog(P_att), and dlog(P_att) is what possession demands. The best it can
// do is prove possession of r*G, which is not the point it announced.
func TestARogueKeyCannotProvePossession(t *testing.T) {
	honest := NewKeyPair()

	r := suite.Scalar().Pick(random.New())
	rG := suite.Point().Mul(r, nil)
	att := suite.Point().Sub(rG, honest.Public)

	// The attacker's only provable statement is about r*G. Presented for the
	// rogue point, it must fail.
	rogue := &KeyPair{Secret: r, Public: rG}
	pop, err := ProvePossession(popMatch, 7, 1, rogue)
	if err != nil {
		t.Fatalf("prove: %v", err)
	}
	if err := VerifyPossession(popMatch, 7, 1, att, pop); err == nil {
		t.Fatal("a proof about r*G was accepted for r*G - sum(others)")
	}

	// Nor can it prove the rogue point directly: HashProve with a witness
	// that does not satisfy the predicate must refuse or produce garbage the
	// verifier refuses.
	lie := &KeyPair{Secret: r, Public: att}
	if pop, err := ProvePossession(popMatch, 7, 1, lie); err == nil {
		if err := VerifyPossession(popMatch, 7, 1, att, pop); err == nil {
			t.Fatal("possession of a point with an unknown discrete log was proven")
		}
	}
}

// The identity is refused before the proof is even consulted - it is the one
// point with a degenerate witness (x = 0), and ValidKey owns that refusal.
// There is deliberately no cofactor or small-order check here: the base point
// generates the prime-order subgroup, so nothing of small order has a Rep
// witness at all.
func TestTheIdentityCannotProvePossession(t *testing.T) {
	zero := &KeyPair{Secret: suite.Scalar().Zero(), Public: suite.Point().Null()}
	if pop, err := ProvePossession(popMatch, 7, 1, zero); err == nil {
		if err := VerifyPossession(popMatch, 7, 1, zero.Public, pop); err == nil {
			t.Fatal("the identity proved possession of itself")
		}
	}
}
