package schema

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
)

func handSecrets(t *testing.T, n int) (*deck.Hand, []*deck.Secrets) {
	t.Helper()
	keys := make([]*deck.KeyPair, n)
	h := &deck.Hand{Match: "m", Hand: 1}
	for i := range keys {
		keys[i] = deck.NewKeyPair()
		h.Pubs = append(h.Pubs, keys[i].Public)
	}
	joint, err := deck.JointKey(h.Pubs)
	if err != nil {
		t.Fatalf("joint: %v", err)
	}
	secrets := make([]*deck.Secrets, n)
	d := deck.Fresh(joint)
	for i := range keys {
		c := deck.Context{Match: "m", Hand: 1, Round: uint32(i), Prover: keys[i].Public}
		out, prf, sec, err := deck.Shuffle(c, joint, d)
		if err != nil {
			t.Fatalf("shuffle %d: %v", i, err)
		}
		h.Steps = append(h.Steps, deck.Step{By: keys[i].Public, Deck: out, Proof: prf})
		secrets[i] = &deck.Secrets{Key: keys[i].Secret, Shuffle: sec}
		d = out
	}
	return h, secrets
}

// The wire bodies round-trip, and the shapes that cannot be secrets are named.
func TestChallengeAndSecretsRoundTrip(t *testing.T) {
	c := ChallengeFrom(1, 7, []byte{0xaa, 0xbb})
	seat, hand, sig, err := c.Into()
	if err != nil || seat != 1 || hand != 7 || len(sig) != 2 {
		t.Fatalf("challenge round trip: %d %d %x %v", seat, hand, sig, err)
	}

	_, secrets := handSecrets(t, 2)
	s, err := SecretsFrom(1, 7, secrets[1], []byte{0xcc})
	if err != nil {
		t.Fatalf("render secrets: %v", err)
	}
	seat, hand, sec, sig, err := s.Into()
	if err != nil || seat != 1 || hand != 7 || len(sig) != 1 {
		t.Fatalf("secrets round trip: %d %d %x %v", seat, hand, sig, err)
	}
	if !sec.Key.Equal(secrets[1].Key) {
		t.Fatal("the card key did not survive the trip")
	}
	for i := range sec.Shuffle.Pi {
		if sec.Shuffle.Pi[i] != secrets[1].Shuffle.Pi[i] {
			t.Fatal("the permutation did not survive the trip")
		}
		if !sec.Shuffle.Beta[i].Equal(secrets[1].Shuffle.Beta[i]) {
			t.Fatal("a blinding factor did not survive the trip")
		}
	}
}

func TestSecretsRefusesTheWrongShapes(t *testing.T) {
	_, secrets := handSecrets(t, 2)
	good, err := SecretsFrom(0, 1, secrets[0], nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	short := good
	short.Pi = good.Pi[:deck.Size-1]
	if _, _, _, _, err := short.Into(); err == nil {
		t.Fatal("a 51 entry permutation decoded")
	}
	bent := good
	bent.Beta = good.Beta[:len(good.Beta)-8]
	if _, _, _, _, err := bent.Into(); err == nil {
		t.Fatal("a truncated blinding blob decoded")
	}
	junk := good
	junk.Key = "not base64!"
	if _, _, _, _, err := junk.Into(); err == nil {
		t.Fatal("a junk card key decoded")
	}
}

// A stored bundle decodes back to a transcript that still audits.
func TestAHandRecordRoundTripsAndStillAudits(t *testing.T) {
	h, secrets := handSecrets(t, 3)
	v, err := HandRecordFrom(&driver.HandRecord{Hand: h, Secrets: secrets[0]}, 0, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	back, own, err := v.Into()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if own == nil || !own.Key.Equal(secrets[0].Key) {
		t.Fatal("own secrets did not survive storage")
	}
	all := append([]*deck.Secrets{own}, secrets[1:]...)
	if err := deck.Audit(back, all); err != nil {
		t.Fatalf("a stored transcript no longer audits: %v", err)
	}
}
