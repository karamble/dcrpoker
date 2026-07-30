package driver

import (
	"sort"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
)

// What a finished hand leaves behind.
//
// Nothing else retains what an audit needs. The deck is overwritten by every
// shuffle, shares are folded into their opening's sum the moment they verify,
// and the card key is rotated at the boundary - so a hand that has been played
// is a hand nobody can recompute unless it was written down as it went. This is
// the writing down: the public transcript exactly as this peer verified it, and
// this peer's own secrets for it.

// HandRecord is one hand's transcript and this peer's own secrets for it.
//
// The transcript is what everyone saw; the secrets are what only this peer
// knew. Together with every other seat's secrets they recompute the hand
// without consulting a single proof, which is what a challenge demands.
type HandRecord struct {
	Hand    *deck.Hand
	Secrets *deck.Secrets
}

// noteStep records one shuffle exactly as it was verified and built on.
func (d *Driver) noteStep(by kyber.Point, out deck.Deck, proof []byte) {
	d.rec.Hand.Steps = append(d.rec.Hand.Steps, deck.Step{By: by, Deck: out, Proof: proof})
}

// noteShareValue records a share that has verified, by slot and seat.
//
// Only verified shares: a held share for a slot this peer never opens is never
// checked, and a transcript is what this peer established, not what it heard.
func (d *Driver) noteShareValue(slot, seat int, share kyber.Point) {
	if d.recShares == nil {
		d.recShares = map[int][]kyber.Point{}
	}
	if d.recShares[slot] == nil {
		d.recShares[slot] = make([]kyber.Point, d.seats)
	}
	d.recShares[slot][seat] = share
}

// takeRecord finishes the record: every card this peer read, in slot order,
// with the shares that opened it.
func (d *Driver) takeRecord() *HandRecord {
	slots := make([]int, 0, len(d.cards))
	for slot := range d.cards {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	for _, slot := range slots {
		d.rec.Hand.Shown = append(d.rec.Hand.Shown, deck.Shown{
			Slot:   slot,
			Card:   d.cards[slot],
			Shares: d.recShares[slot],
		})
	}
	return d.rec
}

// TakeFinishedHands drains the records of every hand that has settled here.
//
// The caller owns what comes back: a record not written down before the next
// crash is a hand this peer can no longer answer a challenge on.
func (t *Table) TakeFinishedHands() []*HandRecord {
	out := t.finished
	t.finished = nil
	return out
}
