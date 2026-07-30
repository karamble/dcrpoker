package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log"
	"time"

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
	// Over means the log has stopped growing on every seat, so there is no
	// gap left for a head to reveal. A table nobody pressed leave on would
	// otherwise say where its log ended every poll until the process died.
	if tbl.play == nil || tbl.finished || tbl.play.Over() {
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

// How long a stuck hand waits before saying its messages again.
//
// Two intervals, because a quiet table means two different things depending on
// what it is quiet during.
//
// While somebody is betting, the common reason nothing is moving is that a
// person is thinking, and a person may think for minutes. So that one is slow,
// counted in blocks, and repeating every four is enough to unstick a lost frame
// without shouting at anybody.
//
// While the deck is being shuffled or dealt there is no person in the loop at
// all - it is machines exchanging frames that should cross in seconds. Quiet
// there does not mean deliberation, it means something was lost. Waiting four
// blocks to find that out is about twenty minutes of a table looking broken,
// which is what a live table spent this afternoon doing.
//
// The dealing one is wall-clock rather than block height on purpose. Every
// deadline in this protocol is a block because two peers have to agree on it;
// this is not a deadline and nobody else has to agree - it is one peer deciding
// when to repeat itself, exactly like the message TTLs in pkg/gaming/wire.
const (
	stallEvery     = 4
	dealStallEvery = 45 * time.Second
)

// republishStalled says again what this seat has already sent.
//
// Only when this seat owes nothing. A seat that still owes something is a seat
// the others are rightly waiting on, and repeating ourselves would not help
// anybody. A seat that owes nothing and is still not being asked for anything is
// in the one situation this exists for.
//
// Said again when the hand moves, and again every stallEvery blocks while it
// does not.
//
// The second half is the one that matters, and leaving it out cost a stuck hand
// roughly one run in ten. Bounding this by progress alone looks right - a table
// where somebody is merely thinking should not be shouted at - but it stops on
// exactly the wrong condition. If the repeat is itself lost, nothing moves, so
// the fingerprint does not change, so it is never said again: the one message
// that would have unstuck the table is the one message the rule now forbids.
// Our own view has not changed *because* the peer never got it.
//
// It is the fault this codebase has been bitten by four times over, in a new
// dress. Anything that must arrive is repeated on a clock and stopped by a
// deadline, never by our own state looking settled - so this now does both:
// immediately when the hand moves, because that is new information, and
// otherwise on a slow timer for as long as the hand is stuck.
func (t *tables) republishStalled(tbl *table, height int64) []outgoing {
	if tbl.play == nil || tbl.finished || tbl.play.Over() {
		return nil
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	at := tbl.progress()
	moved := at != tbl.stalledAt
	if !moved && !tbl.stallDue(height, t.clock()) {
		// Said recently, and nothing has moved since.
		return nil
	}
	if _, owes := tbl.play.Owes(int(seat)); owes && moved {
		// Ours to move, and the hand is moving. The others are rightly
		// waiting on us and our next message is the one they want.
		//
		// Only *and moved*, which is the whole correction. Owing something
		// now says nothing about what anybody received earlier, and the two
		// are not even about the same phase: a seat that has verified every
		// shuffle and gone on to dealing owes a share, while the seat behind
		// it is still waiting on the shuffle that seat sent long ago. Bailing
		// out here on "we owe something" made that peer silent about the one
		// message that would have unstuck the table, permanently, and left
		// two seats each convinced the other was the holdup.
		//
		// It killed a real table on 2026-07-29 and reproduces in this suite
		// about one run in ten. So while we owe, we stay quiet only for as
		// long as the hand is actually progressing.
		tbl.stalledAt = at
		return nil
	}
	tbl.stalledAt = at
	tbl.stalledSaidAt = height
	tbl.stalledSaidWhen = t.clock()

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

// clock is the time these repairs are paced by, which a test may replace.
func (t *tables) clock() time.Time {
	if t.now == nil {
		return time.Now()
	}
	return t.now()
}

// stallDue reports whether enough has passed to say our messages again.
//
// Which clock it reads depends on what the table is waiting for. Dealing is
// machines and answers to seconds; betting is a person and answers to blocks.
// See the constants above.
func (tbl *table) stallDue(height int64, now time.Time) bool {
	if tbl.play != nil {
		if h := tbl.play.Hand(); h != nil {
			switch h.Phase() {
			case driver.PhaseShuffling, driver.PhaseDealing:
				return now.Sub(tbl.stalledSaidWhen) >= dealStallEvery
			}
		}
	}
	if height <= 0 {
		return true
	}
	return height >= tbl.stalledSaidAt+stallEvery
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
