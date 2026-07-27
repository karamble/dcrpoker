package deck

import (
	"encoding/hex"
	"fmt"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/proof"
	"go.dedis.ch/kyber/v4/util/random"
)

// Revealing a card, and why it is not kyber's proof/dleq.
//
// A masked card is (C1, C2) = (rG, M + rH) where H is the joint key, the sum of
// every player's public key. Because H = sum(x_i)G, the mask rH equals
// sum(x_i * C1): each player can compute their own term x_i*C1 from the card
// alone, and the card opens once all of them are subtracted. A player's term is
// their *share*, and the whole construction rests on a share being provable:
// the sharer must show that the D they publish uses the same secret as the
// public key they committed to at the start of the hand, without revealing it.
//
// That is a discrete-log equality statement, log_G(pub) == log_C1(D), and kyber
// ships proof/dleq to prove exactly it. It cannot be used - dleq's verifier
// never re-derives the challenge, so anything at all verifies. See
// dleq_check_test.go, which forges one.
//
// So the statement is written as a Rep predicate instead and run through the
// same proof.HashProve path the shuffle uses, whose verifier does re-derive the
// challenge from the transcript. The predicate says "there is one secret x such
// that pub = x*G and D = x*C1" - a conjunction over a shared secret name is
// what makes it an equality proof rather than two unrelated ones.

// dleqPred is the statement: one x satisfying both pub = x*G and share = x*C1.
//
// The shared secret name "x" across both terms is the entire content of the
// proof. Two separate Rep proofs with different names would prove only that
// each point is *some* multiple, which is trivially true of every point.
func dleqPred() proof.Predicate {
	return proof.And(
		proof.Rep("pub", "x", "G"),
		proof.Rep("share", "x", "C1"),
	)
}

// KeyPair is a player's per-hand card key.
//
// Per hand, and never reused: the secret is published at the end of a hand under
// the reveal-and-recompute rule, so carrying one across hands would retroactively
// open the previous one.
type KeyPair struct {
	Secret kyber.Scalar
	Public kyber.Point
}

// NewKeyPair draws a fresh card key.
func NewKeyPair() *KeyPair {
	x := suite.Scalar().Pick(random.New())
	return &KeyPair{Secret: x, Public: suite.Point().Mul(x, nil)}
}

// JointKey is the key a deck is masked under: the sum of every player's public
// key, so that reading any card needs every player's share.
//
// No player knows the corresponding secret and no coalition short of the whole
// table can assemble it. That is the property that makes a dealer unnecessary,
// and it is also why an absent player freezes the deck - a liveness cost taken
// deliberately, answered with money rather than cryptography.
func JointKey(pubs []kyber.Point) (kyber.Point, error) {
	if len(pubs) == 0 {
		return nil, fmt.Errorf("a deck needs at least one key to mask to")
	}
	seen := make(map[string]bool, len(pubs))
	joint := suite.Point().Null()
	for i, p := range pubs {
		if p == nil {
			return nil, fmt.Errorf("player %d published no card key", i)
		}
		k, err := keyOf(p)
		if err != nil {
			return nil, err
		}
		// A duplicate key means one secret covers two seats, so the
		// "every player must help" guarantee is off by one.
		if seen[k] {
			return nil, fmt.Errorf("two players published the same card key")
		}
		seen[k] = true
		joint = joint.Add(joint, p)
	}
	return joint, nil
}

func keyOf(p kyber.Point) (string, error) {
	b, err := p.MarshalBinary()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Share is one player's contribution to opening one card.
type Share struct {
	// D is x*C1 for the sharer's secret x.
	D kyber.Point
	// Proof binds D to the sharer's published public key.
	Proof []byte
}

func revealPoints(pub kyber.Point, m Masked, share kyber.Point) map[string]kyber.Point {
	return map[string]kyber.Point{
		"G":     suite.Point().Base(),
		"C1":    m.C1,
		"pub":   pub,
		"share": share,
	}
}

// revealLabel binds a share proof to its statement and its place in the game.
//
// Same reasoning as the shuffle label: the challenge must commit to every public
// input. Here that includes the card itself, so a share for one card cannot be
// presented as a share for another, and the Context, so a share cannot be
// replayed into a later hand or a different table.
func revealLabel(c Context, joint kyber.Point, m Masked, d kyber.Point) (string, error) {
	return label(revealBase, c, joint, Deck{m, {C1: d, C2: d}})
}

// Reveal computes this player's share of one card and proves it honest.
//
// c.Prover is set here from the key being revealed with, rather than trusted
// from the caller: the proof is bound to whoever made it, and a Context naming
// somebody else would produce a share nobody can verify.
func Reveal(c Context, joint kyber.Point, kp *KeyPair, m Masked) (*Share, error) {
	if kp == nil || kp.Secret == nil || kp.Public == nil {
		return nil, fmt.Errorf("no card key to reveal with")
	}
	if m.C1 == nil || m.C2 == nil {
		return nil, fmt.Errorf("that card is not masked")
	}
	c.Prover = kp.Public
	d := suite.Point().Mul(kp.Secret, m.C1)

	lbl, err := revealLabel(c, joint, m, d)
	if err != nil {
		return nil, err
	}
	prover := dleqPred().Prover(suite,
		map[string]kyber.Scalar{"x": kp.Secret},
		revealPoints(kp.Public, m, d), nil)
	prf, err := proof.HashProve(suite, lbl, prover)
	if err != nil {
		return nil, fmt.Errorf("prove reveal: %w", err)
	}
	return &Share{D: d, Proof: prf}, nil
}

// shareLen is the length of an honest share proof, measured for the same reason
// proofLen is: kyber's verifier stops reading when it has what it needs and
// ignores the rest, so without this a share proof is malleable.
var shareLenOnce struct {
	n    int
	err  error
	done bool
}

func shareLen() (int, error) {
	proofLenMu.Lock()
	defer proofLenMu.Unlock()
	if shareLenOnce.done {
		return shareLenOnce.n, shareLenOnce.err
	}
	shareLenOnce.done = true
	x := suite.Scalar().SetInt64(2)
	pub := suite.Point().Mul(x, nil)
	c1 := suite.Point().Base()
	prover := dleqPred().Prover(suite,
		map[string]kyber.Scalar{"x": x},
		revealPoints(pub, Masked{C1: c1, C2: c1}, suite.Point().Mul(x, c1)), nil)
	b, err := proof.HashProve(suite, revealBase+"/measure", prover)
	if err != nil {
		shareLenOnce.err = fmt.Errorf("measure a share proof: %w", err)
		return 0, shareLenOnce.err
	}
	shareLenOnce.n = len(b)
	return shareLenOnce.n, nil
}

// VerifyShare checks a share before letting it near a card.
//
// pub is the key the sharer committed to when the hand started, taken from the
// roster rather than from the message carrying the share - a share that
// certifies itself against a key of its author's choosing certifies nothing.
func VerifyShare(c Context, joint kyber.Point, pub kyber.Point, m Masked, s *Share) error {
	if s == nil || s.D == nil {
		return fmt.Errorf("no share")
	}
	if pub == nil {
		return fmt.Errorf("no key to check the share against")
	}
	if m.C1 == nil || m.C2 == nil {
		return fmt.Errorf("that card is not masked")
	}
	want, err := shareLen()
	if err != nil {
		return err
	}
	if len(s.Proof) != want {
		return fmt.Errorf("a share proof is %d bytes, and this is %d", want, len(s.Proof))
	}
	lbl, err := revealLabel(c, joint, m, s.D)
	if err != nil {
		return err
	}
	verifier := dleqPred().Verifier(suite, revealPoints(pub, m, s.D))
	if err := proof.HashVerify(suite, lbl, verifier, s.Proof); err != nil {
		return fmt.Errorf("share proof: %w", err)
	}
	return nil
}

// Opening accumulates the shares for one card.
//
// It exists as a type rather than a function over a slice because of a specific
// bug worth designing out: Geometry's unmask sums whatever shares it is handed
// and returns a group element regardless, so a card short of one share opens to
// a plausible-looking point that is not a card - or, worse, to a real card if a
// duplicate share stands in for a missing one. Here the shares needed are fixed
// when the Opening is made, each is verified as it arrives, each key may
// contribute once, and Card refuses to answer until every one of them has.
type Opening struct {
	ctx   Context
	joint kyber.Point
	card  Masked
	need  map[string]kyber.Point
	have  map[string]bool
	sum   kyber.Point
}

// NewOpening starts collecting the shares of one card from a known set of keys.
func NewOpening(c Context, joint kyber.Point, m Masked, pubs []kyber.Point) (*Opening, error) {
	if m.C1 == nil || m.C2 == nil {
		return nil, fmt.Errorf("that card is not masked")
	}
	if len(pubs) == 0 {
		return nil, fmt.Errorf("no keys to open with")
	}
	o := &Opening{
		ctx:   c,
		joint: joint,
		card:  m,
		need:  make(map[string]kyber.Point, len(pubs)),
		have:  make(map[string]bool, len(pubs)),
		sum:   suite.Point().Null(),
	}
	for i, p := range pubs {
		if p == nil {
			return nil, fmt.Errorf("player %d published no card key", i)
		}
		k, err := keyOf(p)
		if err != nil {
			return nil, err
		}
		o.need[k] = p
	}
	if len(o.need) != len(pubs) {
		return nil, fmt.Errorf("two players published the same card key")
	}
	return o, nil
}

// Add verifies a share and accumulates it.
//
// Verification happens here rather than being left to the caller, so that a
// share cannot reach the sum without having been checked.
func (o *Opening) Add(pub kyber.Point, s *Share) error {
	k, err := keyOf(pub)
	if err != nil {
		return err
	}
	if _, ok := o.need[k]; !ok {
		return fmt.Errorf("a share arrived from somebody not at this table")
	}
	if o.have[k] {
		return fmt.Errorf("that player already shared this card")
	}
	c := o.ctx
	c.Prover = pub
	if err := VerifyShare(c, o.joint, pub, o.card, s); err != nil {
		return err
	}
	o.have[k] = true
	o.sum = o.sum.Add(o.sum, s.D)
	return nil
}

// Missing reports how many shares are still outstanding.
func (o *Opening) Missing() int { return len(o.need) - len(o.have) }

// Card opens the card, or explains why it cannot.
//
// An incomplete Opening is an error and never a card. Subtracting a partial sum
// would yield a perfectly well-formed group element that simply is not any card,
// and the only thing standing between that and a silently wrong showdown is this
// check.
func (o *Opening) Card() (Card, error) {
	if n := o.Missing(); n != 0 {
		return 0, fmt.Errorf("that card needs %d more share(s) before it can be read", n)
	}
	return CardOf(suite.Point().Sub(o.card.C2, o.sum))
}
