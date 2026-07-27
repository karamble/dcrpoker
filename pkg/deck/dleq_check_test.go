package deck

import (
	"testing"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/proof/dleq"
	"go.dedis.ch/kyber/v4/util/random"
)

// Why the reveal proofs in this package are not kyber's proof/dleq.
//
// dleq is the obvious choice - a decryption share is exactly a statement of
// discrete-log equality, and the package exists to prove those. It is also the
// one the plan named. It is not used, and this test is the reason.
//
// dleq.NewDLEQProof derives the challenge by hashing (xG, xH, vG, vH), which is
// the right idea. dleq.Proof.Verify then never recomputes it. It reads the
// challenge out of the proof it was handed and checks only the two group
// equations:
//
//	vG == rG + c(xG)
//	vH == rH + c(xH)
//
// Those hold for *any* c and r if vG and vH are computed from them, which a
// forger is free to do because vG and vH are also fields of the proof. Nothing
// ties c to the statement at verification time, so nothing forces the prover to
// have known x. The transform is not merely weak here; the verifier does not
// perform it at all.
//
// This test forges a proof of equality between two discrete logs that are not
// equal, with no secret at all, and asserts it is accepted - i.e. it passes
// while kyber is broken. If it ever fails, dleq has been fixed and can be
// reconsidered.
func TestWhyThisPackageDoesNotUseKyberDLEQ(t *testing.T) {
	G := suite.Point().Base()
	H := suite.Point().Mul(suite.Scalar().Pick(random.New()), nil)

	// Two points with deliberately different discrete logs: there is no x
	// with xG = aG and xH = bH, so an honest prover could not exist.
	a := suite.Scalar().Pick(random.New())
	b := suite.Scalar().Pick(random.New())
	xG := suite.Point().Mul(a, G)
	xH := suite.Point().Mul(b, H)

	// Choose the challenge and response first, then solve for the
	// commitments. This is only possible because the challenge is not
	// re-derived from anything.
	c := suite.Scalar().Pick(random.New())
	r := suite.Scalar().Pick(random.New())
	forged := &dleq.Proof{
		C:  c,
		R:  r,
		VG: suite.Point().Add(suite.Point().Mul(r, G), suite.Point().Mul(c, xG)),
		VH: suite.Point().Add(suite.Point().Mul(r, H), suite.Point().Mul(c, xH)),
	}

	if err := forged.Verify(suite, G, H, xG, xH); err != nil {
		t.Fatalf("kyber's dleq rejected a forgery, so it may have been fixed: %v", err)
	}
	t.Log("kyber's dleq accepted a proof of equality between unequal discrete logs")

	// And the same forgery against the shape a poker reveal would actually
	// use: claim a share on someone else's public key.
	var (
		theirSecret = suite.Scalar().Pick(random.New())
		theirPub    = suite.Point().Mul(theirSecret, nil)
		c1          = suite.Point().Mul(suite.Scalar().Pick(random.New()), nil)
		lie         = suite.Point().Mul(suite.Scalar().Pick(random.New()), nil) // any point
	)
	c2 := suite.Scalar().Pick(random.New())
	r2 := suite.Scalar().Pick(random.New())
	share := &dleq.Proof{
		C:  c2,
		R:  r2,
		VG: sum(suite.Point().Mul(r2, nil), suite.Point().Mul(c2, theirPub)),
		VH: sum(suite.Point().Mul(r2, c1), suite.Point().Mul(c2, lie)),
	}
	if err := share.Verify(suite, suite.Point().Base(), c1, theirPub, lie); err != nil {
		t.Fatalf("dleq rejected a forged decryption share: %v", err)
	}
	t.Log("kyber's dleq accepted an arbitrary decryption share against a key the forger does not hold")
}

func sum(a, b kyber.Point) kyber.Point { return suite.Point().Add(a, b) }
