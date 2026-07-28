package gamelog

import (
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
)

func TestACheckpointIsSignedAndVerifies(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	cp, err := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := cp.Verify(); err != nil {
		t.Fatalf("an honest checkpoint did not verify: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(*Checkpoint)
	}{
		{"different stacks", func(x *Checkpoint) { x.Stacks = []int64{2000, 0} }},
		{"a different hand", func(x *Checkpoint) { x.Hand = 8 }},
		{"a different seat", func(x *Checkpoint) { x.Seat = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := *cp
			bad.Stacks = append([]int64(nil), cp.Stacks...)
			tc.bend(&bad)
			if err := bad.Verify(); err == nil {
				t.Fatal("a tampered checkpoint verified")
			}
		})
	}
}

func TestACheckpointIsRefusedOnBadTerms(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	elsewhere, err := forfeit.NewLogKey("another-table")
	if err != nil {
		t.Fatalf("log key: %v", err)
	}

	for _, tc := range []struct {
		name   string
		seat   uint32
		stacks []int64
		key    *forfeit.LogKey
	}{
		{"a seat not at the table", 9, []int64{1000, 1000}, privs[0]},
		{"another seat's key", 1, []int64{1000, 1000}, privs[0]},
		{"a key from another match", 0, []int64{1000, 1000}, elsewhere},
		{"the wrong number of stacks", 0, []int64{1000}, privs[0]},
		{"a negative stack", 0, []int64{1000, -1}, privs[0]},
		{"no key", 0, []int64{1000, 1000}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Checkpoint(tc.seat, 1, tc.stacks, tc.key); err == nil {
				t.Fatal("signed a checkpoint that should have been refused")
			}
		})
	}
}

// The third way to equivocate, closed like the other two. A seat that told one
// player the chips landed one way and another that they landed differently
// could settle against whichever of them looked away first.
func TestSigningTwoCheckpointsForOneHandHandsOverTheKey(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	a, err := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	b, err := c.Checkpoint(0, 7, []int64{1900, 100}, privs[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := ConflictingCheckpoints(a, b); err != nil {
		t.Fatalf("two different checkpoints at one hand were not a conflict: %v", err)
	}
	got, err := RecoverKeyFromCheckpoints(a, b)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.PubKey().IsEqual(privs[0].Public()) {
		t.Fatal("recovered a key that is not the one that signed both")
	}
}

// And signing the same checkpoint twice - which happens whenever a message is
// resent - must hand over nothing.
func TestResendingACheckpointLeaksNothing(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	a, _ := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])
	b, _ := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])

	if string(a.Sig) != string(b.Sig) {
		t.Fatal("signing the same checkpoint twice was not deterministic")
	}
	if err := ConflictingCheckpoints(a, b); err == nil {
		t.Fatal("resending a checkpoint was treated as equivocation")
	}
	if _, err := RecoverKeyFromCheckpoints(a, b); err == nil {
		t.Fatal("resending a checkpoint exposed the key")
	}
}

// A checkpoint at hand 5 and a log entry at sequence 5 are both routine and both
// honest. Without separate nonce domains they would share a nonce and a seat
// would publish its own key for playing correctly.
func TestACheckpointAndAnEntryAtTheSameNumberLeakNothing(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	e := c.Next(0, 5, StreetPreFlop, ActionCheck, 0)
	if err := e.Sign(privs[0]); err != nil {
		t.Fatalf("sign: %v", err)
	}
	cp, err := c.Checkpoint(0, 5, []int64{1000, 1000}, privs[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	att, err := c.AttestHead(0, privs[0])
	if err != nil {
		t.Fatalf("attest: %v", err)
	}

	if string(e.Sig[:32]) == string(cp.Sig[:32]) {
		t.Fatal("an entry and a checkpoint at the same number shared a nonce")
	}
	if string(att.Sig[:32]) == string(cp.Sig[:32]) {
		t.Fatal("an attestation and a checkpoint at the same number shared a nonce")
	}

	eh, err := e.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	d := cp.Digest()
	if _, err := forfeit.Recover(privs[0].Public(), eh[:], e.Sig, d[:], cp.Sig); err == nil {
		t.Fatal("signing an entry and a checkpoint at one number exposed the key")
	}
}
