package driver

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
)

// A shuffle that arrives and does not verify leaves each seat owing the other.
//
// Not the same as a lost message. A lost shuffle is re-sent by the repair while
// its hand is still going, and the hand recovers; a shuffle whose proof does not
// verify is refused every time it arrives, so saying it again changes nothing.
// This is what the first live table hit.
//
// The two peers then hold different accounts of the same hand and each is
// consistent. The one that refused the shuffle is still shuffling and says the
// shuffler owes it; the shuffler believes the deck is finished and says the other
// owes the shares it is waiting for. Both are entitled to accuse, both answer
// with their own key, and neither can ever take the other's bond - the bond
// answers a seat that has stopped, and neither of these has stopped.
// wedgedOnAProof builds a two seat table whose hand cannot proceed, because the
// shuffle seat 0 received from seat 1 does not verify and no repeat of it will.
func wedgedOnAProof(t *testing.T) *tnet {
	t.Helper()
	n := seatTable(t, 2, 1000)

	// Routed by hand, because seat 1's shuffle must never reach seat 0: the
	// whole point is the one it gets instead.
	to := func(peer int, m Out) []Out {
		t.Helper()
		out, err := n.peers[peer].Handle(tableInbound(m))
		if err != nil {
			t.Fatalf("peer %d could not take %T: %v", peer, m, err)
		}
		return out
	}
	var keys [2][]Out
	for i, p := range n.peers {
		out, err := p.Start()
		if err != nil {
			t.Fatalf("peer %d start: %v", i, err)
		}
		keys[i] = out
	}
	// Card keys both ways. Seat 0 shuffles as soon as it holds both.
	var mine []Out
	for _, m := range keys[0] {
		to(1, m)
	}
	for _, m := range keys[1] {
		mine = append(mine, to(0, m)...)
	}
	var ours OutShuffle
	for _, m := range mine {
		if sh, ok := m.(OutShuffle); ok {
			ours = sh
		}
	}
	if ours.Deck == nil {
		t.Fatalf("seat 0 did not shuffle once it held both keys; got %v", mine)
	}
	// Seat 1 takes it and shuffles in its own turn.
	var theirs OutShuffle
	for _, m := range to(1, ours) {
		if sh, ok := m.(OutShuffle); ok {
			theirs = sh
		}
	}
	if theirs.Deck == nil {
		t.Fatal("seat 1 never shuffled, so there is nothing to disagree about")
	}
	bad := append([]byte(nil), theirs.Proof...)
	bad[len(bad)/2] ^= 0xff
	if _, err := n.peers[0].Handle(shuffleAs(t, 1, 1, n.logs[1], theirs.Deck, bad)); err == nil {
		t.Fatal("a shuffle whose proof does not verify was accepted")
	}

	return n
}

func TestARefusedShuffleLeavesEachSeatOwingTheOther(t *testing.T) {
	n := wedgedOnAProof(t)

	// Each seat owes the other something, and each is right by its own log.
	d0, ok := n.peers[0].Owes(1)
	if !ok || d0.Kind != DutyShuffle {
		t.Fatalf("the seat that refused the shuffle says seat 1 owes %v, want a shuffle", d0)
	}
	d1, ok := n.peers[1].Owes(0)
	if !ok || d1.Kind != DutyShare {
		t.Fatalf("the shuffler says seat 0 owes %v, want a share", d1)
	}

	// And both duties stand long enough to be claimed, so both peers propose
	// against each other. Nothing here breaks the tie.
	const start uint32 = 1_100_000
	for _, p := range n.peers {
		p.AtHeight(start)
		p.AtHeight(start + 8)
	}
	if _, ok := n.peers[0].Claimable(3); !ok {
		t.Fatal("the refused shuffle never became claimable")
	}
	if _, ok := n.peers[1].Claimable(3); !ok {
		t.Fatal("the awaited share never became claimable")
	}
}

// Leaving a wedged hand takes everybody, and then it settles at the last
// boundary they signed.
//
// The unanimity is what makes stopping safe: a seat losing the hand in progress
// must not be able to void the pot by getting up. It cannot fold its way out
// either - autoFold only acts in betting, and this hand never got there - so
// this is the case where the rule has to hold on its own.
func TestLeavingAWedgedHandTakesEverybody(t *testing.T) {
	n := wedgedOnAProof(t)

	if _, err := n.peers[0].Leave(); err != nil {
		t.Fatalf("seat 0 leaving: %v", err)
	}
	if n.peers[0].Over() {
		t.Fatal("one seat ended a table by getting up, which voids the hand in progress")
	}
	digest := leavingDigest(testMatch, 1, 1)
	sig, err := n.logs[1].Sign(forfeit.DomainLeaving, 1, digest[:])
	if err != nil {
		t.Fatalf("sign leaving: %v", err)
	}
	if _, err := n.peers[0].Handle(InLeaving{Seat: 1, Hand: 1, Sig: sig}); err != nil {
		t.Fatalf("seat 0 taking seat 1's leaving: %v", err)
	}
	if !n.peers[0].Over() {
		t.Fatal("every seat asked to leave and the table did not end")
	}
	if last, _ := n.peers[0].Settled(); last != 0 {
		t.Fatalf("it settled at hand %d, and no boundary was ever signed", last)
	}
}
