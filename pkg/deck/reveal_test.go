package deck

import (
	"testing"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/util/random"
)

// A table: n players, their keys, the joint key, and a deck they have all
// shuffled in turn with every shuffle verified by everybody else.
type table struct {
	t      *testing.T
	ctx    Context
	keys   []*KeyPair
	pubs   []kyber.Point
	joint  kyber.Point
	deck   Deck
	proofs [][]byte
}

func seat(t *testing.T, n int) *table {
	t.Helper()
	tb := &table{t: t, ctx: testContext()}
	for i := 0; i < n; i++ {
		kp := NewKeyPair()
		tb.keys = append(tb.keys, kp)
		tb.pubs = append(tb.pubs, kp.Public)
	}
	joint, err := JointKey(tb.pubs)
	if err != nil {
		t.Fatalf("joint key: %v", err)
	}
	tb.joint = joint
	tb.deck = Fresh(joint)

	// Each player shuffles once, and every other player verifies before the
	// next one goes. Skipping the verification is the whole attack.
	for i, kp := range tb.keys {
		c := tb.ctx
		c.Round = uint32(i)
		c.Prover = kp.Public

		out, prf, _, err := Shuffle(c, joint, tb.deck)
		if err != nil {
			t.Fatalf("player %d shuffle: %v", i, err)
		}
		if err := VerifyShuffle(c, joint, tb.deck, out, prf); err != nil {
			t.Fatalf("player %d's shuffle did not verify: %v", i, err)
		}
		tb.deck = out
		tb.proofs = append(tb.proofs, prf)
	}
	return tb
}

// open collects every player's share for one card, the way a table would.
func (tb *table) open(slot int) (Card, error) {
	tb.t.Helper()
	o, err := NewOpening(tb.ctx, tb.joint, tb.deck[slot], tb.pubs)
	if err != nil {
		return 0, err
	}
	for _, kp := range tb.keys {
		s, err := Reveal(tb.ctx, tb.joint, kp, tb.deck[slot])
		if err != nil {
			return 0, err
		}
		if err := o.Add(kp.Public, s); err != nil {
			return 0, err
		}
	}
	return o.Card()
}

// The test the whole package exists to pass.
//
// Three players shuffle a deck nobody can read, and it opens to exactly the
// fifty-two cards - each one once. This is what neither audited implementation
// can do: luca-patrignani has no shuffle proof at all, so a deck of fifty-two
// aces passes its tests, and Geometry's proofs stop verifying once they cross a
// network. A permutation is not something to assert, it is something to check.
func TestADeckShuffledByEverybodyOpensToFiftyTwoCards(t *testing.T) {
	tb := seat(t, 3)

	seen := make(map[Card]int, Size)
	for slot := 0; slot < Size; slot++ {
		card, err := tb.open(slot)
		if err != nil {
			t.Fatalf("slot %d: %v", slot, err)
		}
		seen[card]++
	}
	if len(seen) != Size {
		t.Fatalf("the deck opened to %d distinct cards, not %d", len(seen), Size)
	}
	for c, n := range seen {
		if n != 1 {
			t.Fatalf("card %d appeared %d times", c, n)
		}
	}

	// And it is genuinely shuffled - a deck that opened in order would pass
	// everything above while being worthless.
	inOrder := 0
	for slot := 0; slot < Size; slot++ {
		if card, _ := tb.open(slot); int(card) == slot {
			inOrder++
		}
	}
	if inOrder == Size {
		t.Fatal("the deck opened in its original order")
	}
}

// A share invented rather than computed.
func TestAForgedShareIsRejected(t *testing.T) {
	tb := seat(t, 2)
	m := tb.deck[0]

	honest, err := Reveal(tb.ctx, tb.joint, tb.keys[0], m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	forged := &Share{
		D:     suite.Point().Mul(suite.Scalar().Pick(random.New()), m.C1),
		Proof: honest.Proof,
	}
	c := tb.ctx
	c.Prover = tb.keys[0].Public
	if err := VerifyShare(c, tb.joint, tb.keys[0].Public, m, forged); err == nil {
		t.Fatal("a share that was not computed from the sharer's key was accepted")
	}
}

// The attack that decides showdowns: a player claiming a card they were not
// dealt, by sending a share that opens their hole card to something better.
func TestAShareThatOpensACardToALieIsRejected(t *testing.T) {
	tb := seat(t, 2)
	m := tb.deck[0]

	// Work out what the card really is, then aim for a different one.
	real, err := tb.open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := Card((int(real) + 7) % Size)

	// The share that would make the card open as `want`, given the other
	// player's honest share. Computing it needs no secret at all - it is
	// simply the difference - which is exactly why it must be proved.
	other := suite.Point().Mul(tb.keys[1].Secret, m.C1)
	lie := suite.Point().Sub(suite.Point().Sub(m.C2, want.point()), other)

	honest, err := Reveal(tb.ctx, tb.joint, tb.keys[0], m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	o, err := NewOpening(tb.ctx, tb.joint, m, tb.pubs)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if err := o.Add(tb.keys[0].Public, &Share{D: lie, Proof: honest.Proof}); err == nil {
		t.Fatal("a share crafted to open the card as a different card was accepted")
	}
}

// A share proved under one key, presented as another player's.
func TestAShareFromTheWrongKeyIsRejected(t *testing.T) {
	tb := seat(t, 2)
	m := tb.deck[0]

	s, err := Reveal(tb.ctx, tb.joint, tb.keys[0], m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	c := tb.ctx
	c.Prover = tb.keys[1].Public
	if err := VerifyShare(c, tb.joint, tb.keys[1].Public, m, s); err == nil {
		t.Fatal("one player's share verified as another player's")
	}
}

// A share for one card offered as the share for another.
func TestAShareForAnotherCardIsRejected(t *testing.T) {
	tb := seat(t, 2)

	s, err := Reveal(tb.ctx, tb.joint, tb.keys[0], tb.deck[0])
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	c := tb.ctx
	c.Prover = tb.keys[0].Public
	if err := VerifyShare(c, tb.joint, tb.keys[0].Public, tb.deck[1], s); err == nil {
		t.Fatal("a share for one card verified against another card")
	}
}

// A share replayed into a later hand, or at another table.
func TestAShareFromAnotherHandIsRejected(t *testing.T) {
	tb := seat(t, 2)
	m := tb.deck[0]

	s, err := Reveal(tb.ctx, tb.joint, tb.keys[0], m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	base := tb.ctx
	base.Prover = tb.keys[0].Public

	later := base
	later.Hand++
	if err := VerifyShare(later, tb.joint, tb.keys[0].Public, m, s); err == nil {
		t.Fatal("a share from one hand verified in another")
	}

	elsewhere := base
	elsewhere.Match = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := VerifyShare(elsewhere, tb.joint, tb.keys[0].Public, m, s); err == nil {
		t.Fatal("a share from one table verified at another")
	}
}

// A missing share must be an error, never a group element that looks like a
// card. This is the bug in Geometry's unmask, which sums whatever it is handed.
func TestAMissingShareIsAnErrorAndNotACard(t *testing.T) {
	tb := seat(t, 3)
	m := tb.deck[0]

	o, err := NewOpening(tb.ctx, tb.joint, m, tb.pubs)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	for _, kp := range tb.keys[:2] {
		s, err := Reveal(tb.ctx, tb.joint, kp, m)
		if err != nil {
			t.Fatalf("reveal: %v", err)
		}
		if err := o.Add(kp.Public, s); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if o.Missing() != 1 {
		t.Fatalf("expected 1 share outstanding, got %d", o.Missing())
	}
	if _, err := o.Card(); err == nil {
		t.Fatal("a card opened while a share was still missing")
	}
}

// One player sending twice must not stand in for a player who never sent.
func TestADuplicateShareCannotFillInForAMissingOne(t *testing.T) {
	tb := seat(t, 3)
	m := tb.deck[0]

	o, err := NewOpening(tb.ctx, tb.joint, m, tb.pubs)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	s, err := Reveal(tb.ctx, tb.joint, tb.keys[0], m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if err := o.Add(tb.keys[0].Public, s); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := o.Add(tb.keys[0].Public, s); err == nil {
		t.Fatal("the same player's share was counted twice")
	}
	if o.Missing() != 2 {
		t.Fatalf("expected 2 shares outstanding, got %d", o.Missing())
	}
}

// A share from somebody who is not at the table.
func TestAShareFromAStrangerIsRejected(t *testing.T) {
	tb := seat(t, 2)
	m := tb.deck[0]

	stranger := NewKeyPair()
	s, err := Reveal(tb.ctx, tb.joint, stranger, m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	o, err := NewOpening(tb.ctx, tb.joint, m, tb.pubs)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	// Note the share is internally valid - it is a correct share for a real
	// key. It is rejected because that key is not seated, which is a roster
	// question and not a cryptographic one.
	if err := o.Add(stranger.Public, s); err == nil {
		t.Fatal("a share from a key not at the table was accepted")
	}
}

// Two seats holding the same key would make "everyone must help" off by one.
func TestATableWithADuplicateKeyIsRefused(t *testing.T) {
	kp := NewKeyPair()
	other := NewKeyPair()
	if _, err := JointKey([]kyber.Point{kp.Public, other.Public, kp.Public}); err == nil {
		t.Fatal("a joint key was built over a duplicated player key")
	}
	if _, err := NewOpening(testContext(), other.Public, Fresh(other.Public)[0],
		[]kyber.Point{kp.Public, kp.Public}); err == nil {
		t.Fatal("an opening was started over a duplicated player key")
	}
}

// The identity adds nothing to a sum, so a seat publishing it is absent from the
// masking every other seat believes it is behind.
//
// The joint key collapses to the sum of the remaining seats, who then hold its
// secret between them: heads-up that is one opponent, alone, able to open the
// contributor's own hole cards.
func TestATableWithAnIdentityKeyIsRefused(t *testing.T) {
	kp := NewKeyPair()
	id := suite.Point().Null()
	if _, err := JointKey([]kyber.Point{kp.Public, id}); err == nil {
		t.Fatal("a joint key was built over an identity player key")
	}
	if _, err := NewOpening(testContext(), kp.Public, Fresh(kp.Public)[0],
		[]kyber.Point{kp.Public, id}); err == nil {
		t.Fatal("an opening was started over an identity player key")
	}
}

// Why that refusal has to happen when a key is admitted and cannot wait until it
// shares: a zero secret is a legitimate witness.
//
// pub = 0*G and D = 0*C1 both hold, so the Rep conjunction proves exactly what
// it claims and the share verifies. Nothing downstream can tell it from an
// honest one, and the card still opens, because the sum it joins is unchanged.
// A seat contributing the identity plays a whole hand without masking anything.
func TestAZeroSecretProducesAShareThatVerifies(t *testing.T) {
	honest := NewKeyPair()
	zero := &KeyPair{Secret: suite.Scalar().Zero(), Public: suite.Point().Null()}

	// What the joint key collapses to when the other seat contributes nothing.
	joint := honest.Public
	m := Fresh(joint)[0]

	c := testContext()
	c.Prover = zero.Public
	s, err := Reveal(c, joint, zero, m)
	if err != nil {
		t.Fatalf("a zero secret could not produce a share: %v", err)
	}
	if !s.D.Equal(suite.Point().Null()) {
		t.Fatal("a zero secret produced a share that is not the identity")
	}
	if err := VerifyShare(c, joint, zero.Public, m, s); err != nil {
		t.Fatalf("the share was caught at share time, so the guard need not be at admission: %v", err)
	}
}

// Corrupting a share proof, and misshaping it.
func TestACorruptedShareProofIsRejected(t *testing.T) {
	tb := seat(t, 2)
	m := tb.deck[0]

	s, err := Reveal(tb.ctx, tb.joint, tb.keys[0], m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	c := tb.ctx
	c.Prover = tb.keys[0].Public
	t.Logf("share proof is %d bytes", len(s.Proof))

	for i := range s.Proof {
		bad := make([]byte, len(s.Proof))
		copy(bad, s.Proof)
		bad[i] ^= 0x01
		if err := VerifyShare(c, tb.joint, tb.keys[0].Public, m, &Share{D: s.D, Proof: bad}); err == nil {
			t.Fatalf("a share proof with byte %d flipped was accepted", i)
		}
	}

	for _, bad := range [][]byte{
		nil,
		{},
		s.Proof[:len(s.Proof)-1],
		s.Proof[1:],
		append(append([]byte{}, s.Proof...), 0x00),
		append(append([]byte{}, s.Proof...), s.Proof...),
	} {
		if err := VerifyShare(c, tb.joint, tb.keys[0].Public, m, &Share{D: s.D, Proof: bad}); err == nil {
			t.Fatalf("a share proof of length %d was accepted", len(bad))
		}
	}
}

// Shares cross a network too.
func TestAShareSurvivesASerializationRoundTrip(t *testing.T) {
	tb := seat(t, 2)
	m := tb.deck[0]

	s, err := Reveal(tb.ctx, tb.joint, tb.keys[0], m)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	c := reread(t, Context{
		Match:  tb.ctx.Match,
		Hand:   tb.ctx.Hand,
		Round:  tb.ctx.Round,
		Prover: tb.keys[0].Public,
	})
	wire := &Share{D: reread2(t, s.D), Proof: append([]byte{}, s.Proof...)}
	if err := VerifyShare(c, reread2(t, tb.joint), reread2(t, tb.keys[0].Public),
		rereadDeck(t, Deck{m})[0], wire); err != nil {
		t.Fatalf("a share stopped verifying after a round trip: %v", err)
	}
}
