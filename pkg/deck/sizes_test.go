package deck

import (
	"testing"
	"time"
)

// What a hand costs on the wire, measured rather than estimated.
//
// This decides whether the gaming envelope needs anything it does not have.
// Frames cross Bison Relay, where a message is a resource-managed RM rather
// than a TCP write, so a hand that runs to hundreds of kilobytes is a design
// input and not an implementation detail.
//
// Run with `go test -run TestWhatAHandCosts -v ./pkg/deck/` to see the numbers.
func TestWhatAHandCosts(t *testing.T) {
	const (
		pointBytes = 32 // ed25519 compressed
		deckBytes  = Size * 2 * pointBytes
	)
	shuffleProof, err := proofLen(Size)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	shareProof, err := shareLen()
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	shareBytes := pointBytes + shareProof

	t.Logf("a masked deck is %d bytes", deckBytes)
	t.Logf("a shuffle proof is %d bytes", shuffleProof)
	t.Logf("a decryption share is %d bytes (%d point + %d proof)",
		shareBytes, pointBytes, shareProof)

	// Timings, from one honest shuffle and one verification.
	joint := testJoint(t)
	c := withProver(t, testContext())
	in := Fresh(joint)

	start := time.Now()
	out, prf, err := Shuffle(c, joint, in)
	if err != nil {
		t.Fatalf("shuffle: %v", err)
	}
	proving := time.Since(start)

	start = time.Now()
	if err := VerifyShuffle(c, joint, in, out, prf); err != nil {
		t.Fatalf("verify: %v", err)
	}
	verifying := time.Since(start)

	kp := NewKeyPair()
	start = time.Now()
	s, err := Reveal(c, joint, kp, out[0])
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	revealing := time.Since(start)

	rc := c
	rc.Prover = kp.Public
	start = time.Now()
	if err := VerifyShare(rc, joint, kp.Public, out[0], s); err != nil {
		t.Fatalf("verify share: %v", err)
	}
	checking := time.Since(start)

	t.Logf("shuffle+prove %v, verify %v, reveal %v, verify share %v",
		proving, verifying, revealing, checking)

	// A hold'em hand: n shuffles, then two hole cards each and five board
	// cards. A board card needs every player's share, broadcast. A hole card
	// needs every *other* player's share, sent only to its owner - the owner
	// supplies their own and never publishes it, which is what keeps the card
	// private. At showdown the owner publishes theirs for whoever is called.
	for _, n := range []int{2, 6} {
		shuffling := n * (deckBytes + shuffleProof)
		board := 5 * n * shareBytes
		holes := 2 * n * (n - 1) * shareBytes
		showdown := 2 * n * shareBytes

		// What one player has to send, as opposed to what crosses the
		// table in total - the number that bounds a slow connection.
		mine := deckBytes + shuffleProof + // my shuffle
			5*shareBytes + // my share of each board card
			2*(n-1)*shareBytes + // my share of everyone else's holes
			2*shareBytes // my own holes, at showdown

		t.Logf("%d seats: %d bytes per hand total (shuffling %d, board %d, holes %d, showdown %d); %d bytes sent by each player",
			n, shuffling+board+holes+showdown, shuffling, board, holes, showdown, mine)
		t.Logf("%d seats: shuffling takes about %v of proving and %v of verification per player",
			n, proving, time.Duration(n-1)*verifying)
	}
}
