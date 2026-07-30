package main

import (
	"encoding/hex"
	"fmt"

	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
)

// What this seat has already put its name to, kept on disk.
//
// A log key refuses to sign one position twice over different messages, because
// doing so publishes it. In memory that catches the mistake inside one process,
// and the mistake that matters spans two: a restart that opens a hand it has
// already played. So the book is written through to the session record, and a key
// read back at startup knows what it signed before the crash.

type signBook struct{ tbl *table }

func (b *signBook) key(p forfeit.Position) string {
	return fmt.Sprintf("%s/%d", p.Domain, p.Seq)
}

func (b *signBook) Used(p forfeit.Position) ([32]byte, bool) {
	var out [32]byte
	raw, ok := b.tbl.signed[b.key(p)]
	if !ok {
		return out, false
	}
	got, err := hex.DecodeString(raw)
	if err != nil || len(got) != 32 {
		// Unreadable is treated as used with an unknown digest, which
		// refuses rather than guesses.
		for i := range out {
			out[i] = 0xff
		}
		return out, true
	}
	copy(out[:], got)
	return out, true
}

func (b *signBook) Record(p forfeit.Position, digest [32]byte) error {
	if b.tbl.signed == nil {
		b.tbl.signed = map[string]string{}
	}
	b.tbl.signed[b.key(p)] = hex.EncodeToString(digest[:])
	// Written here rather than on the next tick, and a failure refuses the
	// signature: one that has left this process while the book has not
	// reached the disk is the state this exists to prevent. The entry stays
	// in memory so the same digest can try again and a different one is
	// still refused meanwhile.
	if err := b.tbl.save(); err != nil {
		return fmt.Errorf("the book did not reach the disk: %w", err)
	}
	return nil
}
