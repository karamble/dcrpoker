package client

import (
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// ChainWatch is a player's own copy of the table's history.
//
// Until now a player could only take the server's word for what happened at the
// table: it ran the game, decided the winner and kept the sole account of it.
// The entries arriving here are signed by the seats that took them, so a player
// can rebuild the chain themselves and check every one - and the server has no
// seat and no key, so there is nothing it could contribute that would be worth
// anything.
//
// What that buys is not detection of a lie in itself; it is that a
// disagreement becomes evidence. If this chain diverges from the head the
// server reports, the player holds signed messages that say so, verifiable by
// anyone, rather than a complaint.
type ChainWatch struct {
	mu    sync.Mutex
	chain *gamelog.Chain
	// pending holds entries that arrived before the one they chain to.
	// Group chat delivery has no ordering at all, so out-of-order arrival is
	// the ordinary case and not a fault.
	pending map[uint64]gamelog.Entry
}

// NewChainWatch starts a verifier for a match.
//
// The roster must be the one the escrow scripts commit to - from
// OpenEscrowResponse.member_pubkeys - keyed by seat. It is the only membership
// list everyone whose money is at stake has agreed to; a group chat's own
// membership is administered by one member and can differ between them.
func NewChainWatch(matchID string, roster map[uint32][]byte) (*ChainWatch, error) {
	chain, err := gamelog.NewChain(matchID, gamelog.Roster(roster))
	if err != nil {
		return nil, err
	}
	return &ChainWatch{chain: chain, pending: make(map[uint64]gamelog.Entry)}, nil
}

// Apply feeds one decoded message in.
//
// Messages of other kinds are ignored rather than refused: a build that has not
// been taught about a message kind must stay in the hand, not fall out of it.
func (w *ChainWatch) Apply(msg *schema.Message) error {
	if msg == nil || msg.Kind != schema.KindAction {
		return nil
	}
	var body schema.Action
	if err := msg.Into(&body); err != nil {
		return err
	}
	entry, err := entryFromTranscript(body.Entry)
	if err != nil {
		return fmt.Errorf("entry at seq %d: %w", body.Entry.Seq, err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	_, seq := w.chain.Head()
	if entry.Seq <= seq {
		// Already have it. Re-delivery is normal - the relay replays what
		// was not acked - and is not a fault.
		return nil
	}
	w.pending[entry.Seq] = *entry
	return w.drainLocked()
}

// drainLocked appends every pending entry that now follows the head.
func (w *ChainWatch) drainLocked() error {
	for {
		_, seq := w.chain.Head()
		next, ok := w.pending[seq+1]
		if !ok {
			return nil
		}
		delete(w.pending, next.Seq)
		// Append verifies the signature, the signer against the roster,
		// the sequence and the link. A relayed entry that fails any of
		// them is exactly what this exists to catch.
		if err := w.chain.Append(&next); err != nil {
			return fmt.Errorf("entry at seq %d: %w", next.Seq, err)
		}
	}
}

// Head reports the verified head and how far the chain has been built.
func (w *ChainWatch) Head() (hash [32]byte, seq uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.chain.Head()
}

// Waiting reports how many entries arrived early and cannot be applied yet.
// A number that stays above zero means entries are missing, not merely late.
func (w *ChainWatch) Waiting() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending)
}

// Transcript renders the verified chain, for handing to somebody else.
func (w *ChainWatch) Transcript() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.chain.Marshal()
}

// AgreesWith reports whether the independently verified chain matches the head
// the server reported, which is the whole point of keeping one.
//
// A false answer is not proof of dishonesty on its own - entries travel over a
// group chat with no delivery guarantee, so a player who is simply behind will
// disagree too. It says the two accounts differ and that this one is built from
// signatures. Waiting reports whether the gap is explained by entries not yet
// arrived.
func (w *ChainWatch) AgreesWith(head []byte, seq uint64) bool {
	ourHash, ourSeq := w.Head()
	if ourSeq != seq {
		return false
	}
	return len(head) == 32 && hex.EncodeToString(ourHash[:]) == hex.EncodeToString(head)
}

// entryFromTranscript converts the hex-rendered wire shape back to an entry.
// The bytes a signature covers are rebuilt by gamelog from these fields, so
// nothing that arrived is trusted as the signed form.
func entryFromTranscript(te gamelog.TranscriptEntry) (*gamelog.Entry, error) {
	prev, err := hex.DecodeString(te.PrevHash)
	if err != nil || len(prev) != 32 {
		return nil, fmt.Errorf("bad prev_hash")
	}
	signer, err := hex.DecodeString(te.Signer)
	if err != nil {
		return nil, fmt.Errorf("bad signer: %w", err)
	}
	sig, err := hex.DecodeString(te.Sig)
	if err != nil {
		return nil, fmt.Errorf("bad sig: %w", err)
	}
	street, err := streetFromName(te.Street)
	if err != nil {
		return nil, err
	}

	e := &gamelog.Entry{
		Version: te.Version,
		Seq:     te.Seq,
		Hand:    te.Hand,
		Street:  street,
		Seat:    te.Seat,
		Signer:  signer,
		Action:  gamelog.Action(te.Action),
		Amount:  te.Amount,
		Sig:     sig,
	}
	copy(e.PrevHash[:], prev)
	return e, nil
}

func streetFromName(s string) (gamelog.Street, error) {
	switch s {
	case "preflop":
		return gamelog.StreetPreFlop, nil
	case "flop":
		return gamelog.StreetFlop, nil
	case "turn":
		return gamelog.StreetTurn, nil
	case "river":
		return gamelog.StreetRiver, nil
	}
	return 0, fmt.Errorf("unknown street %q", s)
}
