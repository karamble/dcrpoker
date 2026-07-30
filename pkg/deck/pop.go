package deck

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/proof"
)

// Proving a card key is a key, and not an equation.
//
// A card key is announced as a bare point, and the joint key is the sum of
// every seat's. Nothing about a point says its announcer knows the secret
// behind it, and the last seat to announce can therefore choose
// P = r*G - sum(others), knowing r but not dlog(P), and steer the joint key to
// r*G - a key it alone controls. The construction is self-defeating today -
// without dlog(P) that seat can never produce a valid share, so the hand can
// never deal - but "the attack fails later for algebraic reasons" is an
// invariant nothing asserts, where "the key was proved at the door" is one
// something does.
//
// So the announcement carries a possession proof: a Schnorr proof of knowing x
// with pub = x*G, which is half of the reveal predicate, run through the same
// proof.HashProve path everything here uses - never kyber's proof/dleq, whose
// verifier does not re-derive the challenge and accepts anything (see
// dleq_check_test.go).
//
// Deliberately absent: any cofactor or small-order check on the point. The base
// point generates the prime-order subgroup, so every point with a Rep witness
// lies in it - nothing of small order can carry a possession proof at all. The
// one degenerate witness is x = 0 for the identity, and ValidKey refuses the
// identity before this proof is ever consulted. Possession plus that refusal is
// the whole question answered; a cofactor test on top would be a second copy of
// half of it.

// popPred is the statement: the prover knows x with pub = x*G.
func popPred() proof.Predicate {
	return proof.Rep("pub", "x", "G")
}

// popLabel binds a possession proof to its statement and its place in the game.
//
// The general label() cannot be used here: it binds the joint key and the
// decks, and a card key is announced before either exists - the joint key is
// made *from* these announcements. What does exist, and is bound: the table,
// the hand, the seat, and the point itself, so a proof cannot be lifted between
// tables, replayed into a later hand, or presented for another seat's key.
func popLabel(match string, hand uint64, seat int, pub kyber.Point) (string, error) {
	h := suite.Hash()
	h.Write([]byte(popBase))
	writeLen(h, len(match))
	h.Write([]byte(match))
	_ = binary.Write(h, binary.BigEndian, hand)
	_ = binary.Write(h, binary.BigEndian, uint64(seat))
	if err := writePoint(h, pub); err != nil {
		return "", fmt.Errorf("card key: %w", err)
	}
	return popBase + "/" + hex.EncodeToString(h.Sum(nil)), nil
}

func popPoints(pub kyber.Point) map[string]kyber.Point {
	return map[string]kyber.Point{
		"G":   suite.Point().Base(),
		"pub": pub,
	}
}

// popLen is the length of an honest possession proof, measured for the same
// reason proofLen and shareLen are: kyber's verifier stops reading when it has
// what it needs and ignores the rest, so without this the proof is malleable.
var popLenOnce struct {
	n    int
	err  error
	done bool
}

func popLen() (int, error) {
	proofLenMu.Lock()
	defer proofLenMu.Unlock()
	if popLenOnce.done {
		return popLenOnce.n, popLenOnce.err
	}
	popLenOnce.done = true
	x := suite.Scalar().SetInt64(2)
	prover := popPred().Prover(suite,
		map[string]kyber.Scalar{"x": x},
		popPoints(suite.Point().Mul(x, nil)), nil)
	b, err := proof.HashProve(suite, popBase+"/measure", prover)
	if err != nil {
		popLenOnce.err = fmt.Errorf("measure a possession proof: %w", err)
		return 0, popLenOnce.err
	}
	popLenOnce.n = len(b)
	return popLenOnce.n, nil
}

// ProvePossession proves this seat knows the secret behind its card key.
func ProvePossession(match string, hand uint64, seat int, kp *KeyPair) ([]byte, error) {
	if kp == nil || kp.Secret == nil || kp.Public == nil {
		return nil, fmt.Errorf("no card key to prove possession of")
	}
	lbl, err := popLabel(match, hand, seat, kp.Public)
	if err != nil {
		return nil, err
	}
	prover := popPred().Prover(suite,
		map[string]kyber.Scalar{"x": kp.Secret},
		popPoints(kp.Public), nil)
	prf, err := proof.HashProve(suite, lbl, prover)
	if err != nil {
		return nil, fmt.Errorf("prove possession: %w", err)
	}
	return prf, nil
}

// VerifyPossession checks a card key's possession proof before the key is
// admitted.
//
// pub is the point from the announcement being judged, and the proof stands or
// falls with it: a proof made for any other point, seat, hand or table fails
// here, because all of them are in the label.
func VerifyPossession(match string, hand uint64, seat int, pub kyber.Point, pop []byte) error {
	if err := ValidKey(pub); err != nil {
		return err
	}
	if len(pop) == 0 {
		return fmt.Errorf("the card key carries no possession proof")
	}
	want, err := popLen()
	if err != nil {
		return err
	}
	if len(pop) != want {
		return fmt.Errorf("a possession proof is %d bytes, and this is %d", want, len(pop))
	}
	lbl, err := popLabel(match, hand, seat, pub)
	if err != nil {
		return err
	}
	verifier := popPred().Verifier(suite, popPoints(pub))
	if err := proof.HashVerify(suite, lbl, verifier, pop); err != nil {
		return fmt.Errorf("possession proof: %w", err)
	}
	return nil
}
