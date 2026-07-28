package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
)

// Getting a hand moving again after a message went missing.
//
// This channel loses messages. Every layer above learned that the hard way and
// answers it the same way - a join, a commit, a stake and a bond are all said
// again on a timer, and stopped by a deadline. The hand itself never was, and it
// is the layer where it matters most: a lost frame there does not delay a table,
// it ends one. The seat that sent it believes it has played, the seat that never
// received it believes it is waiting, and both wait forever with nothing in
// either log to say why. That is exactly how a live table died on 2026-07-28.
//
// Two repairs, because a hand carries two kinds of message.
//
// The betting is a hash chain with sequence numbers, so who is behind is not a
// guess. Seats say where their head is; whoever is ahead sends the entries the
// other lacks. That is exact, it costs one small message a tick, and it needs no
// agreement about who asks.
//
// The dealing is not in the chain. A shuffle is the largest message in the
// protocol and there is nothing anywhere that holds a second copy, so the peer
// that produced it keeps it and says it again. What triggers that is the seat's
// own view of its obligations: a seat that owes nothing and is still waiting has
// done everything the hand asks of it, so if nothing is moving then what is
// missing is something of ours.

// exchangeHeads says where this seat's log has got to.
//
// Published rather than requested. A peer that is behind cannot know it is
// behind - that is what being behind means - so waiting to be asked would leave
// the one seat that needs the repair as the one seat that cannot start it.
func (t *tables) exchangeHeads(tbl *table) []outgoing {
	if tbl.play == nil || tbl.finished {
		return nil
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil
	}
	key, err := forfeit.LogKeyFrom(tbl.logPriv, match)
	if err != nil {
		return nil
	}
	att, err := tbl.play.Chain().AttestHead(seat, key)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot say where our log is: %v", tbl.terms.SID, err)
		return nil
	}
	return []outgoing{tbl.frame(schema.KindHead, schema.HeadFrom(att), wire.ClassState)}
}

// acceptHead answers another seat's claim about the log.
//
// Three outcomes, and only one of them sends anything. Behind us, and we hold
// what it is missing. Ahead of us, and it holds what we are missing - which
// needs nothing from here, because our own head goes out on the next tick and
// the repair comes back. Level with us but on a different hash, and one of us
// has a history the other does not, which no amount of sending will fix.
func (tbl *table) acceptHead(body schema.Head) []outgoing {
	if tbl.play == nil {
		return nil
	}
	att, err := body.Into()
	if err != nil {
		log.Printf("pokerplugin: table %s: head: %v", tbl.terms.SID, err)
		return nil
	}
	ours, ok := tbl.form.OurSeat()
	if !ok || att.Seat == ours {
		// Our own claim coming back. Nothing to do with it.
		return nil
	}
	// The key has to be the one this seat plays under, checked against the
	// membership rather than taken from the message, the same way a stake or
	// a bond announcement is checked.
	seats, ok := tbl.form.LogSeats()
	if !ok {
		return nil
	}
	want, ok := seats[att.Seat]
	if !ok || !bytes.Equal(want, att.Signer) {
		log.Printf("pokerplugin: table %s: a key that does not hold seat %d says where its log is",
			tbl.terms.SID, att.Seat)
		return nil
	}
	if err := att.Verify(); err != nil {
		log.Printf("pokerplugin: table %s: seat %d's head: %v", tbl.terms.SID, att.Seat, err)
		return nil
	}

	chain := tbl.play.Chain()
	head, seq := chain.Head()
	switch {
	case att.Seq == seq:
		if att.Hash == head {
			return nil
		}
		return tbl.noteFork(att, head, seq)
	case att.Seq > seq:
		// Behind. Our own head goes out on the next tick and whatever we
		// are missing comes back with it.
		return nil
	}

	// Ahead, so send what it lacks. Everything from its head forward, since
	// an entry it turns out to have already is ignored rather than refused.
	var out []outgoing
	for _, e := range chain.Entries() {
		if e.Seq <= att.Seq {
			continue
		}
		kind, body, err := renderDriver(driver.OutAction{Entry: e}, e.Hand)
		if err != nil {
			log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
			return out
		}
		out = append(out, tbl.frame(kind, body, wire.ClassState))
	}
	if len(out) > 0 {
		log.Printf("pokerplugin: table %s: seat %d is %d entries behind; sending them",
			tbl.terms.SID, att.Seat, len(out))
	}
	return out
}

// noteFork records two histories that cannot both be true.
//
// Recorded and said loudly, and deliberately nothing more. The signatures make
// this provable - gamelog.ConflictingHeads turns the pair into evidence - and a
// dispute takes somebody's bond. At this stage a disagreement is far likelier to
// be a fault in this code than a peer forging a history, and a table that files
// disputes over its own bugs is worse than one that does not file them at all.
// The evidence keeps; a person can act on it.
func (tbl *table) noteFork(theirs *gamelog.HeadAttestation, ourHead [32]byte, seq uint64) []outgoing {
	seat := theirs.Seat
	text := fmt.Sprintf("seat %d says the log at %d is %s, and ours is %s",
		seat, seq, hex.EncodeToString(theirs.Hash[:8]), hex.EncodeToString(ourHead[:8]))
	tbl.note(eventBlocked, text, "", &seat)
	log.Printf("pokerplugin: table %s: %s", tbl.terms.SID, text)
	return nil
}

// republishStalled says again what this seat has already sent.
//
// Only when this seat owes nothing. A seat that still owes something is a seat
// the others are rightly waiting on, and repeating ourselves would not help
// anybody. A seat that owes nothing and is still not being asked for anything is
// in the one situation this exists for.
//
// Bounded by progress rather than by a count: it fires once per stall, not once
// per tick, so a table where somebody is simply thinking sends nothing at all.
func (t *tables) republishStalled(tbl *table) []outgoing {
	if tbl.play == nil || tbl.finished || tbl.play.Over() {
		return nil
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	if _, owes := tbl.play.Owes(int(seat)); owes {
		// Ours to move. Nobody is waiting on a message.
		tbl.stalledAt = ""
		return nil
	}
	at := tbl.progress()
	if at == tbl.stalledAt {
		// Said already, and nothing has moved since. Saying it every
		// thirty seconds would be a peer shouting at a table that is
		// genuinely just slow.
		return nil
	}
	tbl.stalledAt = at

	held := tbl.play.Republish()
	if len(held) == 0 {
		return nil
	}
	var out []outgoing
	for _, m := range held {
		kind, body, err := renderDriver(m.Out, m.Hand)
		if err != nil {
			log.Printf("pokerplugin: table %s: cannot say %T again: %v", tbl.terms.SID, m.Out, err)
			continue
		}
		out = append(out, tbl.frame(kind, body, wire.ClassState))
	}
	log.Printf("pokerplugin: table %s: nothing owed here and nothing moving; saying our %d messages again",
		tbl.terms.SID, len(out))
	return out
}

// progress is a fingerprint of how far the hand has got.
//
// Enough of the state that anything moving changes it, and nothing else in it,
// so that "has this table moved since last time" is one comparison.
func (tbl *table) progress() string {
	if tbl.play == nil {
		return ""
	}
	_, seq := tbl.play.Chain().Head()
	hand := tbl.play.Hand()
	if hand == nil {
		return fmt.Sprintf("%d/%d/-", tbl.playingHand(), seq)
	}
	return fmt.Sprintf("%d/%d/%s/%d", tbl.playingHand(), seq, hand.Phase(), hand.Shuffled())
}
