package deck

import (
	"errors"
	"testing"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/kyber/v4/util/random"
)

// The audit is the defence that does not depend on the proofs being right, so
// the tests for it are written to defeat the proofs rather than to satisfy
// them. Several of these construct a hand that would pass every verification in
// the package and assert the audit catches it anyway - that is the whole claim,
// and it would be worth nothing if only tested against cheats the proofs
// already reject.

// play runs an honest hand and returns its transcript and everybody's secrets.
func play(t *testing.T, n int) (*Hand, []*Secrets) {
	t.Helper()
	tb := seat(t, n)

	h := &Hand{Match: tb.ctx.Match, Hand: tb.ctx.Hand, Pubs: tb.pubs}
	secrets := make([]*Secrets, n)

	deck := Fresh(tb.joint)
	for i, kp := range tb.keys {
		c := tb.ctx
		c.Round = uint32(i)
		c.Prover = kp.Public

		out, prf, sec, err := Shuffle(c, tb.joint, deck)
		if err != nil {
			t.Fatalf("player %d shuffle: %v", i, err)
		}
		if err := VerifyShuffle(c, tb.joint, deck, out, prf); err != nil {
			t.Fatalf("player %d's shuffle did not verify: %v", i, err)
		}
		h.Steps = append(h.Steps, Step{By: kp.Public, Deck: out, Proof: prf})
		secrets[i] = &Secrets{Key: kp.Secret, Shuffle: sec}
		deck = out
	}
	tb.deck = deck
	return h, secrets
}

func mustCheat(t *testing.T, err error, want kyber.Point) *Cheat {
	t.Helper()
	if err == nil {
		t.Fatal("the audit passed a hand that was not honest")
	}
	var c *Cheat
	if !errors.As(err, &c) {
		t.Fatalf("expected a Cheat naming a player, got a plain error: %v", err)
	}
	if want != nil && !c.By.Equal(want) {
		t.Fatalf("the audit blamed the wrong player: %v", c)
	}
	return c
}

// An honest hand audits clean. Everything below is meaningless without this.
func TestAnHonestHandAudits(t *testing.T) {
	h, secrets := play(t, 3)
	if err := Audit(h, secrets); err != nil {
		t.Fatalf("an honest hand was called a cheat: %v", err)
	}
}

// The one that justifies the whole file.
//
// A shuffler duplicates a card - the attack the shuffle proof exists to stop -
// and the proof is simply assumed to have passed, as it would if kyber's
// soundness were broken the way its dleq is. Nothing in the transcript looks
// wrong. The audit catches it from the shuffler's own published secrets, and
// names them.
func TestTheAuditCatchesADuplicatedCardWithNoWorkingProof(t *testing.T) {
	h, secrets := play(t, 3)
	cheat := 1

	// Rebuild player 1's shuffle so that input slot 0 is dealt twice and
	// slot 1 never appears. This is not a permutation, and a sound proof
	// could not exist for it - which is exactly the assumption being
	// removed.
	sec := secrets[cheat].Shuffle
	sec.Pi[0] = sec.Pi[1]

	joint, err := JointKey(h.Pubs)
	if err != nil {
		t.Fatalf("joint: %v", err)
	}
	in := h.Steps[cheat-1].Deck
	h.Steps[cheat].Deck = remask(in, joint, sec.Pi, sec.Beta)

	// Everything after has to follow from the forged deck, or the audit
	// would catch the *next* player instead.
	deck := h.Steps[cheat].Deck
	for i := cheat + 1; i < len(h.Steps); i++ {
		h.Steps[i].Deck = remask(deck, joint, secrets[i].Shuffle.Pi, secrets[i].Shuffle.Beta)
		deck = h.Steps[i].Deck
	}

	c := mustCheat(t, Audit(h, secrets), h.Pubs[cheat])
	t.Logf("caught: %v", c)
}

// A shuffler who publishes a deck that is not what their secrets produce.
//
// The other way to cheat: shuffle honestly on the wire, then lie at audit time
// about what you did. It fails for the same reason - the two have to agree.
func TestTheAuditCatchesADeckThatDoesNotMatchItsSecrets(t *testing.T) {
	h, secrets := play(t, 3)
	cheat := 2

	// Swap two cards in the published deck and leave the secrets honest.
	d := h.Steps[cheat].Deck
	d[4], d[9] = d[9], d[4]

	mustCheat(t, Audit(h, secrets), h.Pubs[cheat])
}

// Lying about the key at audit time.
//
// A cheat's escape route: publish a *different* card key at the end, chosen so
// the forged hand recomputes cleanly. It fails because the key was committed to
// before the hand and the audit checks the published one against that
// commitment rather than against the hand.
func TestTheAuditCatchesAKeyThatIsNotTheCommittedOne(t *testing.T) {
	h, secrets := play(t, 2)
	secrets[0].Key = suite.Scalar().Pick(random.New())

	mustCheat(t, Audit(h, secrets), h.Pubs[0])
}

// A player who reported a card as something other than what it was.
func TestTheAuditCatchesACardPlayedAsSomethingElse(t *testing.T) {
	h, secrets := play(t, 2)
	if err := Audit(h, secrets); err != nil {
		t.Fatalf("honest hand: %v", err)
	}

	// Work out what slot 0 truly was, then claim it was something else.
	total := suite.Scalar().Zero()
	for _, s := range secrets {
		total = total.Add(total, s.Key)
	}
	final := h.Steps[len(h.Steps)-1].Deck
	real, err := CardOf(suite.Point().Sub(final[0].C2, suite.Point().Mul(total, final[0].C1)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	h.Shown = []Shown{{Slot: 0, Card: real}}
	if err := Audit(h, secrets); err != nil {
		t.Fatalf("the audit rejected a truthfully reported card: %v", err)
	}

	h.Shown = []Shown{{Slot: 0, Card: Card((int(real) + 1) % Size)}}
	if err := Audit(h, secrets); err == nil {
		t.Fatal("the audit accepted a card played as something it was not")
	}
}

// And a wrong card gets traced to whoever's share caused it.
//
// Detection without attribution cannot take anybody's bond, so the share each
// player published is checked against the key they have now revealed.
func TestTheAuditBlamesWhoeverPublishedTheWrongShare(t *testing.T) {
	h, secrets := play(t, 3)
	final := h.Steps[len(h.Steps)-1].Deck

	total := suite.Scalar().Zero()
	for _, s := range secrets {
		total = total.Add(total, s.Key)
	}
	real, err := CardOf(suite.Point().Sub(final[7].C2, suite.Point().Mul(total, final[7].C1)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Honest shares first, to be sure the check is not simply always firing.
	shares := make([]kyber.Point, len(h.Pubs))
	for i, s := range secrets {
		shares[i] = suite.Point().Mul(s.Key, final[7].C1)
	}
	h.Shown = []Shown{{Slot: 7, Card: real, Shares: shares}}
	if err := Audit(h, secrets); err != nil {
		t.Fatalf("the audit rejected honest shares: %v", err)
	}

	// Now player 2 sends a share that is not what their key produces.
	shares[2] = suite.Point().Mul(suite.Scalar().Pick(random.New()), final[7].C1)
	mustCheat(t, Audit(h, secrets), h.Pubs[2])
}

// A player who withholds their secrets is not a cheat the audit can name - it
// is an absence, and the transcript cannot tell a liar from a dropped
// connection. It has to be reported as what it is, because the answer is the
// bond and not an accusation.
func TestWithheldSecretsAreReportedAsMissingAndNotAsCheating(t *testing.T) {
	h, secrets := play(t, 3)

	held := *secrets[1]
	secrets[1] = &Secrets{Key: held.Key} // key published, shuffle secret not
	err := Audit(h, secrets)
	if err == nil {
		t.Fatal("the audit passed a hand with secrets missing")
	}
	var c *Cheat
	if errors.As(err, &c) {
		t.Fatalf("a missing secret was reported as a cheat: %v", err)
	}

	secrets[1] = nil
	if err := Audit(h, secrets); err == nil {
		t.Fatal("the audit passed a hand with a player's secrets absent entirely")
	}
}

// Malformed transcripts are errors, never accusations.
func TestAMalformedTranscriptIsNotAnAccusation(t *testing.T) {
	h, secrets := play(t, 3)

	for _, tc := range []struct {
		name string
		bend func(*Hand, []*Secrets) (*Hand, []*Secrets)
	}{
		{"no players", func(h *Hand, s []*Secrets) (*Hand, []*Secrets) {
			return &Hand{}, nil
		}},
		{"fewer shuffles than players", func(h *Hand, s []*Secrets) (*Hand, []*Secrets) {
			c := *h
			c.Steps = c.Steps[:1]
			return &c, s
		}},
		{"fewer secrets than players", func(h *Hand, s []*Secrets) (*Hand, []*Secrets) {
			return h, s[:1]
		}},
		{"a shuffle attributed to the wrong player", func(h *Hand, s []*Secrets) (*Hand, []*Secrets) {
			c := *h
			c.Steps = append([]Step(nil), c.Steps...)
			c.Steps[0].By = NewKeyPair().Public
			return &c, s
		}},
		{"a card acted on outside the deck", func(h *Hand, s []*Secrets) (*Hand, []*Secrets) {
			c := *h
			c.Shown = []Shown{{Slot: 999, Card: 0}}
			return &c, s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bh, bs := tc.bend(h, secrets)
			err := Audit(bh, bs)
			if err == nil {
				t.Fatal("a malformed transcript audited clean")
			}
			var c *Cheat
			if errors.As(err, &c) {
				t.Fatalf("a malformed transcript produced an accusation: %v", err)
			}
		})
	}
}

// Permutation checking, directly - the property a cheating shuffler breaks.
func TestOnlyAPermutationIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name string
		pi   []int
	}{
		{"a repeat", []int{0, 1, 1, 3}},
		{"out of range", []int{0, 1, 2, 9}},
		{"negative", []int{0, 1, 2, -1}},
		{"too short", []int{0, 1, 2}},
	} {
		if err := checkPerm(tc.pi, 4); err == nil {
			t.Fatalf("%s was accepted as a permutation", tc.name)
		}
	}
	if err := checkPerm([]int{3, 0, 2, 1}, 4); err != nil {
		t.Fatalf("a real permutation was rejected: %v", err)
	}
}

// The audit reproduces the shuffle exactly. If remask and kyber's own formula
// ever drift apart, honest hands start looking like cheats - so this pins them
// together rather than trusting that they were copied correctly.
func TestTheAuditReproducesTheShuffleItIsChecking(t *testing.T) {
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	out, _, sec, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	if !sameDeck(remask(in, joint, sec.Pi, sec.Beta), out) {
		t.Fatal("replaying a shuffle from its own secrets did not reproduce it")
	}
}

// One seat's reveal checks against its own step alone, so a bad one is refused
// on receipt instead of poisoning the audit later.
func TestOneSeatsSecretsVerifyAgainstItsOwnStep(t *testing.T) {
	h, secrets := play(t, 3)
	for seat := range secrets {
		if err := VerifySecrets(h, seat, secrets[seat]); err != nil {
			t.Fatalf("seat %d's honest secrets refused: %v", seat, err)
		}
	}
}

func TestVerifySecretsCatchesAForeignKeyAndAForeignShuffle(t *testing.T) {
	h, secrets := play(t, 3)

	wrongKey := *secrets[1]
	wrongKey.Key = suite.Scalar().SetInt64(7)
	var cheat *Cheat
	if err := VerifySecrets(h, 1, &wrongKey); !errors.As(err, &cheat) {
		t.Fatalf("a foreign card key came back as %v, want a cheat naming seat 1", err)
	} else if !cheat.By.Equal(h.Pubs[1]) {
		t.Fatal("the cheat names the wrong player")
	}

	wrongShuffle := *secrets[2]
	beta := append([]kyber.Scalar(nil), secrets[2].Shuffle.Beta...)
	beta[3] = suite.Scalar().SetInt64(9)
	wrongShuffle.Shuffle = &ShuffleSecret{Pi: secrets[2].Shuffle.Pi, Beta: beta}
	cheat = nil
	if err := VerifySecrets(h, 2, &wrongShuffle); !errors.As(err, &cheat) {
		t.Fatalf("a tampered blinding factor came back as %v, want a cheat naming seat 2", err)
	} else if !cheat.By.Equal(h.Pubs[2]) {
		t.Fatal("the cheat names the wrong player")
	}
}

// The audited deck is the audit with the cards kept: every slot opened, no
// duplicates, and only ever with a nil error.
func TestAuditedDeckOpensEveryCard(t *testing.T) {
	h, secrets := play(t, 2)
	cards, err := AuditedDeck(h, secrets)
	if err != nil {
		t.Fatalf("an honest hand did not audit: %v", err)
	}
	if len(cards) != Size {
		t.Fatalf("opened %d cards of %d", len(cards), Size)
	}
	seen := map[Card]bool{}
	for _, c := range cards {
		if seen[c] {
			t.Fatalf("card %d twice", c)
		}
		seen[c] = true
	}
}

// A bad share for a card the hand acted on names the seat that published it.
//
// A wrong opened card is caused by a bad share or a bad shuffle, and the
// shuffles are all attributed already - so the share is where the rest of the
// blame lives. Checked before the card comparison, because after it the only
// path that reaches the share check is the one where nothing is wrong.
func TestABadShareForAnActedOnCardNamesItsSeat(t *testing.T) {
	h, secrets := play(t, 3)

	// Slot 0 opened. An honest share for a card is that seat's key applied to
	// the card's own C1, so every other seat's is built that way and seat 1's
	// is not.
	final := h.Steps[len(h.Steps)-1].Deck
	shares := make([]kyber.Point, 3)
	for i := range shares {
		shares[i] = suite.Point().Mul(secrets[i].Key, final[0].C1)
	}
	shares[1] = suite.Point().Mul(suite.Scalar().SetInt64(3), final[0].C1)
	// The card the table claims to have read is also wrong, which is what a
	// bad share produces - and is exactly the case that used to come back
	// unattributed.
	h.Shown = []Shown{{Slot: 0, Card: Card(7), Shares: shares}}

	var cheat *Cheat
	err := Audit(h, secrets)
	if !errors.As(err, &cheat) {
		t.Fatalf("a bad share for an acted-on card came back as %v, want a cheat naming seat 1", err)
	}
	if !cheat.By.Equal(h.Pubs[1]) {
		t.Fatal("the cheat names the wrong seat")
	}
}

// A card this peer recorded reading that the replay disagrees with is a fault in
// this machine, not a verdict about the hand.
//
// A slot only reaches Shown once its opening had every share, and auditShares has
// just checked every one of those shares against its publisher's revealed key -
// so the sum this peer subtracted to read the card is the sum the replay
// subtracts, over the same inputs. They cannot differ unless the local record is
// corrupt, and nobody else can check a claim about that. It must not be a *Wrong:
// a *Wrong stops the table being paid out, and one client's corruption may not
// veto everybody's money.
func TestAMisreadCardIsThisMachinesFaultAndNotTheHands(t *testing.T) {
	h, secrets := play(t, 2)

	cards, err := AuditedDeck(h, secrets)
	if err != nil {
		t.Fatalf("an honest hand did not audit: %v", err)
	}
	// Slot 4 read wrongly, with every share honest - which is the only way
	// this is reachable at all.
	final := h.Steps[len(h.Steps)-1].Deck
	shares := make([]kyber.Point, 2)
	for i := range shares {
		shares[i] = suite.Point().Mul(secrets[i].Key, final[4].C1)
	}
	h.Shown = []Shown{{Slot: 4, Card: (cards[4] + 1) % Size, Shares: shares}}

	err = Audit(h, secrets)
	if err == nil {
		t.Fatal("a card recorded wrongly audited clean")
	}
	var wrong *Wrong
	if errors.As(err, &wrong) {
		t.Fatal("one machine's own record vetoed the table: it came back as a *Wrong")
	}
	var cheat *Cheat
	if errors.As(err, &cheat) {
		t.Fatal("a card this peer misread named somebody else")
	}
}
