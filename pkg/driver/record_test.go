package driver

import (
	"errors"
	"testing"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// checkDown plays the hand in progress to a showdown, everybody checking or
// calling whatever is in front of them.
func (n *tnet) checkDown() {
	n.t.Helper()
	h := n.peers[0].Hand()
	if h == nil {
		n.t.Fatal("no hand in progress")
	}
	hand := h.State().Hand
	for range 64 {
		cur := n.peers[0].Hand()
		if cur == nil || cur.State().Hand != hand || cur.State().ToAct < 0 {
			return
		}
		turn := cur.State().ToAct
		out, err := n.peers[turn].Act(gamelog.ActionCheck, 0)
		if err != nil {
			out, err = n.peers[turn].Act(gamelog.ActionCall, 0)
		}
		if err != nil {
			n.t.Fatalf("seat %d could neither check nor call: %v", turn, err)
		}
		n.send(turn, out)
		n.deliver()
	}
	n.t.Fatalf("hand %d never reached a showdown", hand)
}

// records drains every peer's finished hands and requires exactly one each.
func records(t *testing.T, n *tnet) []*HandRecord {
	t.Helper()
	out := make([]*HandRecord, len(n.peers))
	for i, p := range n.peers {
		recs := p.TakeFinishedHands()
		if len(recs) != 1 {
			t.Fatalf("peer %d handed over %d records, want 1", i, len(recs))
		}
		out[i] = recs[0]
	}
	return out
}

// Every peer's own transcript, joined with every peer's secrets, recomputes the
// hand - which is the whole property a challenge stands on.
func TestAFinishedHandHandsOverATranscriptThatAudits(t *testing.T) {
	n := seatTable(t, 3, 1000)
	n.start()
	n.checkDown()

	recs := records(t, n)
	secrets := make([]*deck.Secrets, len(recs))
	for seat, r := range recs {
		if r.Hand.Hand != 1 {
			t.Fatalf("peer %d recorded hand %d, want 1", seat, r.Hand.Hand)
		}
		if r.Secrets == nil || r.Secrets.Key == nil || r.Secrets.Shuffle == nil {
			t.Fatalf("peer %d's own secrets are incomplete", seat)
		}
		secrets[seat] = r.Secrets
	}
	for seat, r := range recs {
		if err := deck.Audit(r.Hand, secrets); err != nil {
			t.Fatalf("peer %d's transcript did not audit: %v", seat, err)
		}
		if len(r.Hand.Shown) == 0 {
			t.Fatalf("peer %d recorded a showdown with nothing shown", seat)
		}
		for other := range recs {
			if err := deck.VerifySecrets(r.Hand, other, secrets[other]); err != nil {
				t.Fatalf("peer %d's transcript refuses seat %d's honest secrets: %v",
					seat, other, err)
			}
		}
	}
}

// A tampered secret or transcript does not recompute, and names the seat.
func TestATamperedTranscriptOrSecretDoesNotAudit(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()
	n.checkDown()

	recs := records(t, n)
	secrets := []*deck.Secrets{recs[0].Secrets, recs[1].Secrets}

	bent := *recs[1].Secrets
	beta := append([]kyber.Scalar(nil), bent.Shuffle.Beta...)
	beta[7] = beta[7].Clone().Add(beta[7], beta[7])
	bent.Shuffle = &deck.ShuffleSecret{Pi: bent.Shuffle.Pi, Beta: beta}

	var cheat *deck.Cheat
	err := deck.Audit(recs[0].Hand, []*deck.Secrets{secrets[0], &bent})
	if !errors.As(err, &cheat) {
		t.Fatalf("a tampered blinding factor audited as %v, want a cheat", err)
	}
	if !cheat.By.Equal(recs[0].Hand.Pubs[1]) {
		t.Fatal("the cheat names the wrong seat")
	}

	wrong := *recs[0]
	shown := append([]deck.Shown(nil), wrong.Hand.Shown...)
	shown[0].Card = (shown[0].Card + 1) % deck.Size
	wrong.Hand = &deck.Hand{Match: wrong.Hand.Match, Hand: wrong.Hand.Hand,
		Pubs: wrong.Hand.Pubs, Steps: wrong.Hand.Steps, Shown: shown}
	if err := deck.Audit(wrong.Hand, secrets); err == nil {
		t.Fatal("a transcript claiming a different card audited clean")
	}
}

// The handoff happens at the boundary, once, and drains.
func TestFinishedHandsAreHandedOverOnceAndAtTheBoundary(t *testing.T) {
	n := seatTable(t, 2, 1000)
	n.start()
	if got := n.peers[0].TakeFinishedHands(); len(got) != 0 {
		t.Fatalf("%d records before any hand finished", len(got))
	}
	n.checkDown()
	if got := n.peers[0].TakeFinishedHands(); len(got) != 1 {
		t.Fatalf("%d records after one hand, want 1", len(got))
	}
	if got := n.peers[0].TakeFinishedHands(); len(got) != 0 {
		t.Fatalf("a second drain returned %d records, want 0", len(got))
	}
	n.checkDown()
	if got := n.peers[0].TakeFinishedHands(); len(got) != 1 {
		t.Fatalf("%d records after the second hand, want 1", len(got))
	}
}
