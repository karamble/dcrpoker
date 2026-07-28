// Package deck deals cards that nobody holds.
//
// A dealer is a process that knows the deck order. Remove it and the deck has
// to exist in a form no single player can read: every card encrypted under a
// key that is the sum of everybody's, so a card becomes legible only when the
// others help, and shuffled by each player in turn so that no one of them knows
// where anything went.
//
// The construction is Barnett-Smart in shape - ElGamal masking, sequential
// re-masking shuffles, per-player decryption shares - instantiated on kyber's
// primitives rather than written here. That is deliberate. Two reference
// implementations were read before this one: both got the protocol wiring right
// and the proofs wrong or absent, in ways their own tests did not catch, so
// everything here that could be a library call is one.
//
// What is proved, and what is therefore not possible:
//
//   - A shuffle is proved to be a permutation and a re-masking of exactly the
//     deck it was given (Neff). A player cannot insert a card, duplicate one,
//     or drop one.
//   - A decryption share is proved to come from the key its author published
//     (Chaum-Pedersen). A player cannot send a wrong share to make somebody
//     else misread a card, and cannot claim at showdown to hold a card they
//     were not dealt.
//   - Every card needs every player's share, so no coalition short of the whole
//     table can read a card early.
//
// What is not addressed here: a player who simply stops sending shares freezes
// the deck. That is a liveness problem with an economic answer, not a
// cryptographic one, and it lives elsewhere.
package deck

import (
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/group/edwards25519"
	"go.dedis.ch/kyber/v4/proof"
	"go.dedis.ch/kyber/v4/shuffle"
	"go.dedis.ch/kyber/v4/util/random"
)

// Size is the number of cards in a deck.
const Size = 52

// suite is the group everything here lives in.
//
// Not secp256k1, which is what the rest of this project signs with, and that is
// fine: these keys are per-hand and encrypt nothing but cards. A player is tied
// to their card key by signing it with the session key that holds their money,
// which binds the two without forcing one curve to do both jobs.
//
// The group is named directly rather than looked up through kyber's suites
// registry, because that registry imports every suite kyber has - including
// three separate BLS12-381 pairing implementations. None of that belongs in a
// sandboxed binary that needs one curve.
var suite = edwards25519.NewBlakeSHA256Ed25519()

// Suite reports the group in use, for callers that need to build points.
func Suite() proof.Suite { return suite }

// The transcript labels, and the reason they are computed rather than constant.
//
// kyber seeds a proof's challenge derivation with the protocol name and nothing
// else: newHashProver does `suite.XOF([]byte(protoName))`, and PairShuffle.Prove
// then feeds in only its own commitments. The statement - the deck before, the
// deck after, the joint key - is never absorbed. It appears in the verification
// equations and nowhere in the challenge.
//
// That is weak Fiat-Shamir, the bug class that has broken implementations with
// far more funding and review than this one. A challenge that does not commit to
// every public input leaves the prover free to go looking for a statement its
// proof happens to satisfy, and leaves a proof from one context verifiable in
// another - another round, another hand, another player's shuffle.
//
// So the label is the whole statement, hashed. Everything public goes in: which
// table, which hand, which round of shuffling, whose shuffle it is, the key it
// was masked under, and both decks in full. A proof is then valid for exactly
// one claim and is meaningless anywhere else, which is what the transform is
// supposed to give and does not give by default.
const (
	shuffleBase = "dcrpoker/deck/shuffle/v1"
	revealBase  = "dcrpoker/deck/reveal/v1"
)

// Context is where in the game a proof belongs. Every field binds into the
// challenge, so a proof cannot be lifted from one of them to another.
type Context struct {
	// Match is the table's match id - the roster hash, so a proof cannot
	// cross between tables even at the same hand and round.
	Match string
	// Hand counts from the table's first, Round counts shuffles within one
	// hand. Together they stop a proof being replayed later in the session.
	Hand  uint64
	Round uint32
	// Prover is the card key of whoever made the proof, so one player's
	// shuffle cannot be presented as another's.
	Prover kyber.Point
}

func writeLen(h io.Writer, n int) {
	_ = binary.Write(h, binary.BigEndian, uint32(n))
}

func writePoint(h io.Writer, p kyber.Point) error {
	if p == nil {
		return fmt.Errorf("a public input is missing")
	}
	b, err := p.MarshalBinary()
	if err != nil {
		return err
	}
	writeLen(h, len(b))
	_, err = h.Write(b)
	return err
}

// label binds a statement into a transcript seed.
func label(base string, c Context, joint kyber.Point, decks ...Deck) (string, error) {
	h := suite.Hash()
	h.Write([]byte(base))

	writeLen(h, len(c.Match))
	h.Write([]byte(c.Match))
	_ = binary.Write(h, binary.BigEndian, c.Hand)
	_ = binary.Write(h, binary.BigEndian, c.Round)
	if err := writePoint(h, c.Prover); err != nil {
		return "", fmt.Errorf("prover key: %w", err)
	}
	if err := writePoint(h, joint); err != nil {
		return "", fmt.Errorf("joint key: %w", err)
	}
	for _, d := range decks {
		writeLen(h, len(d))
		for _, m := range d {
			if err := writePoint(h, m.C1); err != nil {
				return "", err
			}
			if err := writePoint(h, m.C2); err != nil {
				return "", err
			}
		}
	}
	return base + "/" + hex.EncodeToString(h.Sum(nil)), nil
}

// Card is a card's identity: 0..51, suit-major.
type Card int

// point returns the group element standing for a card.
//
// A fixed multiple of the base point, so the map is public, identical for
// everyone, and needs no setup protocol to agree on. It must be invertible -
// revealing a card means recovering which card it was - which is why this is a
// small enumerable set and not a hash to the curve.
func (c Card) point() kyber.Point {
	return suite.Point().Mul(suite.Scalar().SetInt64(int64(c)+1), nil)
}

// CardOf recovers a card from its group element.
//
// A linear scan of 52, which is nothing, and the only way round: the encoding
// is one-way as arithmetic and invertible only because the domain is tiny.
func CardOf(p kyber.Point) (Card, error) {
	for c := Card(0); c < Size; c++ {
		if c.point().Equal(p) {
			return c, nil
		}
	}
	return 0, fmt.Errorf("that point is not a card")
}

// Masked is one card under ElGamal: (C1, C2) = (rG, M + rH).
type Masked struct {
	C1, C2 kyber.Point
}

// Deck is a masked deck, in dealing order.
type Deck []Masked

// Fresh builds the starting deck: every card masked under the joint key with no
// randomness at all.
//
// Deliberately deterministic. Everybody can recompute this from the joint key
// alone, so the deck that goes into the first shuffle is not something anyone
// has to be trusted to have built correctly - the secrecy comes entirely from
// the shuffles that follow.
func Fresh(joint kyber.Point) Deck {
	d := make(Deck, Size)
	zero := suite.Scalar().Zero()
	for c := Card(0); c < Size; c++ {
		d[c] = Masked{
			C1: suite.Point().Mul(zero, nil),
			C2: c.point(),
		}
	}
	return d
}

// split turns a deck into the parallel slices kyber's shuffle works on.
func (d Deck) split() (x, y []kyber.Point) {
	x = make([]kyber.Point, len(d))
	y = make([]kyber.Point, len(d))
	for i, m := range d {
		x[i], y[i] = m.C1, m.C2
	}
	return x, y
}

func join(x, y []kyber.Point) Deck {
	d := make(Deck, len(x))
	for i := range x {
		d[i] = Masked{C1: x[i], C2: y[i]}
	}
	return d
}

// ShuffleSecret is what a shuffler used, kept so it can be published when the
// hand is over.
//
// Secret during the hand and worthless after it: together with every player's
// card key it reproduces the shuffle exactly, which is the point. See audit.go
// for why a hand ends by giving all of this away.
type ShuffleSecret struct {
	// Pi is the permutation applied: output slot i took input slot Pi[i].
	Pi []int
	// Beta is the re-masking randomness, indexed by *input* slot.
	Beta []kyber.Scalar
}

// remask applies a permutation and re-masking to a deck.
//
// This is kyber's own formula, written out here rather than called, because the
// audit has to recompute a shuffle from published secrets without going through
// a prover. Getting it wrong in either direction would make honest hands look
// like cheating, so the two uses share this one function. Note Beta is indexed
// by Pi[i] and not by i - a detail worth exactly one bug if missed.
func remask(in Deck, joint kyber.Point, pi []int, beta []kyber.Scalar) Deck {
	out := make(Deck, len(in))
	for i := range in {
		j := pi[i]
		out[i] = Masked{
			C1: suite.Point().Add(in[j].C1, suite.Point().Mul(beta[j], nil)),
			C2: suite.Point().Add(in[j].C2, suite.Point().Mul(beta[j], joint)),
		}
	}
	return out
}

// Shuffle permutes and re-masks a deck, and proves it did nothing else.
//
// The proof is what separates this from a player simply asserting a deck. It
// establishes that the output is a permutation and re-masking of the input -
// so a shuffler cannot introduce a card, duplicate one or remove one - while
// revealing nothing about which card went where.
//
// The permutation and blinding factors are drawn here rather than left inside
// kyber's convenience wrapper, which discards them. They are returned because a
// hand ends by publishing them: the proof says the shuffle was honest, and the
// secrets let everyone check that claim directly rather than take the proof's
// word for it.
func Shuffle(c Context, joint kyber.Point, in Deck) (out Deck, proofBytes []byte, sec *ShuffleSecret, err error) {
	k := len(in)
	if k == 0 {
		return nil, nil, nil, fmt.Errorf("nothing to shuffle")
	}
	rand := random.New()

	pi := make([]int, k)
	for i := range pi {
		pi[i] = i
	}
	for i := k - 1; i > 0; i-- {
		j := int(randUint64(rand) % uint64(i+1))
		pi[i], pi[j] = pi[j], pi[i]
	}
	beta := make([]kyber.Scalar, k)
	for i := range beta {
		beta[i] = suite.Scalar().Pick(rand)
	}
	out = remask(in, joint, pi, beta)

	// The label covers both decks, so it can only be built once the shuffle
	// has produced the second one.
	lbl, err := label(shuffleBase, c, joint, in, out)
	if err != nil {
		return nil, nil, nil, err
	}
	x, y := in.split()
	ps := shuffle.PairShuffle{}
	ps.Init(suite, k)
	prover := func(ctx proof.ProverContext) error {
		return ps.Prove(pi, nil, joint, beta, x, y, rand, ctx)
	}
	proofBytes, err = proof.HashProve(suite, lbl, prover)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("prove shuffle: %w", err)
	}
	return out, proofBytes, &ShuffleSecret{Pi: pi, Beta: beta}, nil
}

// randUint64 draws a uniform 64-bit value, as kyber's own shuffle does.
func randUint64(rand cipher.Stream) uint64 {
	return binary.BigEndian.Uint64(random.Bits(64, false, rand))
}

// proofLen is how many bytes an honest shuffle proof over k cards occupies.
//
// A Neff proof is a fixed sequence of points and scalars, all fixed-width, so
// the length is a function of the group and the deck size and nothing else -
// not the permutation, not the randomness. It is measured rather than written
// down as a constant, because a constant is a claim about kyber's internals
// that a dependency bump could quietly falsify, and this one is load-bearing.
//
// Costs one shuffle the first time a given size is seen and nothing after.
var (
	proofLenMu sync.Mutex
	proofLens  = map[int]int{}
)

func proofLen(k int) (int, error) {
	if k <= 0 {
		return 0, fmt.Errorf("a deck of %d cards has no shuffle proof", k)
	}
	proofLenMu.Lock()
	defer proofLenMu.Unlock()
	if n, ok := proofLens[k]; ok {
		return n, nil
	}
	x := make([]kyber.Point, k)
	y := make([]kyber.Point, k)
	for i := range x {
		p := suite.Point().Mul(suite.Scalar().SetInt64(int64(i)+1), nil)
		x[i], y[i] = p, p
	}
	_, _, prover := shuffle.Shuffle(suite, nil, suite.Point().Base(), x, y, random.New())
	b, err := proof.HashProve(suite, shuffleBase+"/measure", prover)
	if err != nil {
		return 0, fmt.Errorf("measure a shuffle proof: %w", err)
	}
	proofLens[k] = len(b)
	return len(b), nil
}

// VerifyShuffle checks somebody else's shuffle before building on it.
//
// Every player runs this on every other player's shuffle, before the next one
// starts. Skipping it is not an optimisation - an unverified shuffle is an
// unconstrained one, and the whole guarantee is gone.
func VerifyShuffle(c Context, joint kyber.Point, in, out Deck, proofBytes []byte) error {
	if len(in) != len(out) {
		return fmt.Errorf("a shuffle of %d cards returned %d", len(in), len(out))
	}
	// Length first, because kyber will not check it.
	//
	// Its verifier reads the elements it expects and simply stops; anything
	// after that is never looked at, so a valid proof with bytes appended
	// verifies happily. That makes a proof malleable - the same proof has
	// unboundedly many valid encodings - which is harmless until something
	// hashes, signs, dedups or logs the bytes, all of which this is about to
	// do. A Neff proof is a fixed number of fixed-width elements, so the
	// honest length is a constant and anything else is not an honest proof.
	want, err := proofLen(len(in))
	if err != nil {
		return err
	}
	if len(proofBytes) != want {
		return fmt.Errorf("a shuffle proof of %d cards is %d bytes, and this is %d",
			len(in), want, len(proofBytes))
	}
	x, y := in.split()
	xbar, ybar := out.split()

	lbl, err := label(shuffleBase, c, joint, in, out)
	if err != nil {
		return err
	}
	verifier := shuffle.Verifier(suite, nil, joint, x, y, xbar, ybar)
	if err := proof.HashVerify(suite, lbl, verifier, proofBytes); err != nil {
		return fmt.Errorf("shuffle proof: %w", err)
	}
	return nil
}
