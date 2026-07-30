package driver

import (
	"errors"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
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
	_, err := n.peers[0].Handle(shuffleAs(t, 1, 1, n.logs[1], theirs.Deck, bad))
	if err == nil {
		t.Fatal("a shuffle whose proof does not verify was accepted")
	}
	// Typed, so the layer that can publish knows there is evidence to
	// publish - every other error stays an error.
	var refused *ErrShuffleRefused
	if !errors.As(err, &refused) {
		t.Fatalf("a refused shuffle came back as a plain error: %v", err)
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

// A refusal keeps what only that moment holds: the input this peer verified
// against and the signed frame it refused. Nothing advances - the refusal is
// evidence, not progress.
func TestARefusedShuffleLeavesTheEvidence(t *testing.T) {
	n := wedgedOnAProof(t)
	d := n.peers[0].Hand()

	ref, ok := d.RefusedShuffle()
	if !ok {
		t.Fatal("a refused shuffle left no evidence")
	}
	if ref.Seat != 1 || ref.Round != 1 {
		t.Fatalf("the evidence names seat %d round %d, want seat 1 round 1", ref.Seat, ref.Round)
	}
	if len(ref.Deck) == 0 || len(ref.Proof) == 0 || len(ref.Sig) == 0 {
		t.Fatal("the evidence is missing the refused frame")
	}
	prior, err := d.PriorDeck(ref.Round)
	if err != nil {
		t.Fatalf("prior deck: %v", err)
	}
	if !deck.SameDeck(ref.Input, prior) {
		t.Fatal("the recorded input is not the deck this peer verified against")
	}
	// And the refusal consumed nothing: the peer still counts one accepted
	// shuffle, so a corrected retelling could still land.
	if d.Shuffled() != 1 {
		t.Fatalf("the round moved to %d on a refusal", d.Shuffled())
	}
}

// A complained-about shuffler owes the answer above everything else, at a table
// that is over as much as at one that is not - the dispute is exactly for the
// state where the game cannot press anybody.
func TestAComplainedShufflerOwesTheAnswer(t *testing.T) {
	n := wedgedOnAProof(t)
	tb := n.peers[0]

	if err := tb.OpenComplaint(1, 1); err != nil {
		t.Fatalf("open complaint: %v", err)
	}
	d, ok := tb.Owes(1)
	if !ok || d.Kind != DutyShuffleAnswer {
		t.Fatalf("the disputed shuffler owes %v, want the answer", d)
	}

	const start uint32 = 1_100_000
	tb.AtHeight(start)
	tb.AtHeight(start + 8)
	if got, ok := tb.Claimable(3); !ok || got.Kind != DutyShuffleAnswer {
		t.Fatalf("the unanswered dispute never became claimable: %v", got)
	}

	// The table ending does not discharge it - that is the whole point of a
	// duty the bond backs.
	tb.VoidWedgedHand()
	tb.AtHeight(start + 16)
	if got, ok := tb.Claimable(3); !ok || got.Kind != DutyShuffleAnswer {
		t.Fatalf("a table being over silenced the dispute: %v", got)
	}

	tb.CloseComplaint(1)
	if _, ok := tb.Owes(1); ok {
		t.Fatal("a closed complaint is still owed")
	}
}

// A proven wedge ends the table with no unanimity at all: no seat left, and it
// still settles at the boundary everybody signed, with the cause recorded so
// settlement at hand zero knows it is allowed.
func TestAProvenWedgeEndsTheTableWithoutUnanimity(t *testing.T) {
	n := wedgedOnAProof(t)
	tb := n.peers[0]

	if tb.Over() {
		t.Fatal("the table ended before anything was proven")
	}
	tb.VoidWedgedHand()
	if !tb.Over() {
		t.Fatal("a proven wedge did not end the table")
	}
	if tb.VoidCause() != VoidWedge {
		t.Fatalf("the void cause is %v, want the wedge", tb.VoidCause())
	}
	last, stacks := tb.Settled()
	if last != 0 {
		t.Fatalf("it settled at hand %d, and no boundary was ever signed", last)
	}
	if len(stacks) != 2 || stacks[0] != 1000 || stacks[1] != 1000 {
		t.Fatalf("hand zero's stacks are %v, want the stakes back", stacks)
	}
}
