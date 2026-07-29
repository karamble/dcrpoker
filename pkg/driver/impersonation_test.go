package driver

import (
	"bytes"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
)

// Sitting in somebody else's chair.
//
// Each test here plays the cheat. The frames are real - a genuine Neff proof, a
// genuine card key, a well-formed leaving - and the only thing wrong with them is
// who made them.
//
// That case is one a harness of honest peers cannot produce, which is why it needs
// its own file. Every other test in this package builds an In value and hands it
// straight to a peer, so nothing in them ever asks who sent one; a suite like that
// can be entirely green while nothing checks.

// A shuffle proof says a deck was permuted honestly. It does not say by whom: its
// witness is the permutation and the masking, with no secret key anywhere in it,
// so anybody can make a valid one under anybody's card key. An unsigned frame
// would let the seat it names be pre-empted - and because the round moves on, that
// seat's real shuffle would then arrive late and be dropped as a repeat.
func TestAShuffleCannotBeSentInAnotherSeatsName(t *testing.T) {
	n := seat(t, 3, 1000)

	out, err := n.peers[0].Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sh := out[0].(OutShuffle)

	// Seat 0 has shuffled, so it is seat 1's turn. Seat 1 makes its real
	// shuffle now; it is held back rather than delivered, which is what a
	// lossy channel does anyway and what gives the forgery its opening.
	victim := n.peers[1]
	produced, err := victim.Handle(inbound(sh))
	if err != nil {
		t.Fatalf("the victim could not take the first shuffle: %v", err)
	}
	var real1 *OutShuffle
	for _, m := range produced {
		if s, ok := m.(OutShuffle); ok {
			real1 = &s
		}
	}
	if real1 == nil {
		t.Fatal("the victim produced no shuffle")
	}

	// Seat 2 forges seat 1's: a real permutation of the real deck, proved
	// correctly under seat 1's card key - which the deck layer allows and
	// must, since a shuffle proof is about the deck and not the shuffler -
	// and signed with seat 2's own key, because seat 2 has no other.
	forgedDeck, forgedProof := forgeShuffle(t, n, 1, sh.Deck)

	bystander := n.peers[2]
	if _, err := bystander.Handle(inbound(sh)); err != nil {
		t.Fatalf("the bystander could not take the first shuffle: %v", err)
	}
	forged := shuffleAs(t, 1, 1, n.logs[2], forgedDeck, forgedProof)
	if _, err := bystander.Handle(forged); err == nil {
		t.Fatal("a peer accepted a shuffle for seat 1 that seat 2 signed")
	}

	// And the round was not consumed by the attempt, which is the half that
	// matters: a forgery that got as far as moving the round on would make
	// seat 1's genuine shuffle look like a repeat and be dropped in silence.
	if _, err := bystander.Handle(inbound(*real1)); err != nil {
		t.Fatalf("seat 1's own shuffle was refused after the forgery: %v", err)
	}
}

// The joint key is the sum of every seat's card key, so a key accepted for the
// wrong seat means every card at the table is masked to a secret that seat does
// not hold.
func TestACardKeyCannotBeSentInAnotherSeatsName(t *testing.T) {
	n := seatTable(t, 3, 1000)
	// Started, so the table is setting up hand 1: a key for a hand it is not
	// on is ignored as stale long before it is checked, and the test would
	// prove nothing.
	if _, err := n.peers[0].Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	stranger := deck.NewKeyPair()
	digest, err := cardKeyDigest(testMatch, 1, 1, stranger.Public)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	// Seat 2 signs it. Everything else about the announcement is correct.
	sig, err := n.logs[2].SignCommitted(forfeit.DomainCardKey, 1, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	forged := InCardKey{Seat: 1, Hand: 1, Key: stranger.Public, Sig: sig}
	if _, err := n.peers[0].Handle(forged); err == nil {
		t.Fatal("a table took a card key for seat 1 that seat 2 signed")
	}

	// Unsigned is refused too.
	if _, err := n.peers[0].Handle(InCardKey{Seat: 1, Hand: 1, Key: stranger.Public}); err == nil {
		t.Fatal("a table took an unsigned card key")
	}
}

// Leaving is a seat's own decision. Once every seat has said it the table
// settles, so an unsigned one lets anybody who can reach the table end somebody
// else's game at a moment of their choosing.
func TestALeavingCannotBeSentInAnotherSeatsName(t *testing.T) {
	n := seatTable(t, 3, 1000)

	digest := leavingDigest(testMatch, 1, 1)
	sig, err := n.logs[2].Sign(forfeit.DomainLeaving, 1, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := n.peers[0].Handle(InLeaving{Seat: 1, Hand: 1, Sig: sig}); err == nil {
		t.Fatal("a table stood seat 1 up on seat 2's signature")
	}
	if _, err := n.peers[0].Handle(InLeaving{Seat: 1, Hand: 1}); err == nil {
		t.Fatal("a table stood seat 1 up on nothing at all")
	}
}

// Saying the same frame again is not equivocation and must not look like it.
//
// The repair discipline repeats anything that has to arrive, so if a repeat
// produced a second, different signature at one position, honest play would leak
// the seat's own key. The nonce comes from the position rather than the message,
// so it does not.
func TestRepeatingAFrameProducesTheSameSignature(t *testing.T) {
	n := seat(t, 2, 1000)

	key := n.logs[0]
	d := deck.Fresh(n.peers[0].joint)
	digest, err := shuffleDigest(testMatch, 1, 0, d, []byte("proof"))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	first, err := key.SignCommitted(forfeit.DomainShuffle, 1, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	again, err := key.SignCommitted(forfeit.DomainShuffle, 1, digest[:])
	if err != nil {
		t.Fatalf("sign again: %v", err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("saying the same frame twice produced two different signatures")
	}
}

// Signing two decks for one hand does NOT publish the key.
//
// A shuffle's content is fresh blinding, so its position cannot fix it: a table
// whose hand counter starts over would sign hand one again with a different deck.
// Under a position nonce that publishes the log key - the key the bond's punishment
// branch pays on - so an ordinary restart would forfeit the restarting player's own
// bond. Deck frames therefore commit to the message, and equivocating on one stays
// provable without being self-punishing.
func TestSigningTwoDecksForOneHandDoesNotPublishTheKey(t *testing.T) {
	n := seat(t, 2, 1000)
	key := n.logs[0]

	one := deck.Fresh(n.peers[0].joint)
	two := deck.Fresh(n.peers[0].joint)
	two[0], two[1] = one[1], one[0]

	digestA, err := shuffleDigest(testMatch, 1, 0, one, []byte("proof"))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	digestB, err := shuffleDigest(testMatch, 1, 0, two, []byte("proof"))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if digestA == digestB {
		t.Fatal("two different decks hashed the same way")
	}

	sigA, err := key.SignCommitted(forfeit.DomainShuffle, 1, digestA[:])
	if err != nil {
		t.Fatalf("sign a: %v", err)
	}
	sigB, err := key.SignCommitted(forfeit.DomainShuffle, 1, digestB[:])
	if err != nil {
		t.Fatalf("sign b: %v", err)
	}

	if _, err := forfeit.Recover(key.Public(), digestA[:], sigA, digestB[:], sigB); err == nil {
		t.Fatal("two shuffles for one hand yielded the signing key")
	}
}

// forgeShuffle produces a genuine, verifiable shuffle in a seat's turn without
// that seat's involvement - which the deck layer permits, and must, because a
// shuffle proof is about the deck and not about the shuffler.
func forgeShuffle(t *testing.T, n *net, seatNum int, from deck.Deck) (deck.Deck, []byte) {
	t.Helper()
	victim := n.peers[seatNum]
	ctx, err := victim.state.DeckContext(uint32(seatNum), victim.cfg.CardKeys[seatNum])
	if err != nil {
		t.Fatalf("deck context: %v", err)
	}
	out, prf, _, err := deck.Shuffle(ctx, victim.joint, from)
	if err != nil {
		t.Fatalf("forge shuffle: %v", err)
	}
	return out, prf
}
