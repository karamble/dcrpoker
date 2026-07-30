package main

import (
	"encoding/hex"
	"fmt"
	"log"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

// Dealing, once the money is down.
//
// Everything before this is about agreeing who is playing and getting their
// stakes onto the chain. This is where the table starts dealing, and the
// condition for starting is deliberately strict: seated, and every seat's stake
// seen on the chain by this peer. Dealing a hand for a stake that is not there
// is dealing for nothing.

// blindsFor derives a blind schedule from the buy-in.
//
// Derived rather than stated, because the terms are hashed into every join and
// commit at this table - adding a field would change that hash and make every
// invitation already in circulation unreadable. Deriving costs nothing and
// every peer reaches the same numbers from the same invitation, which is the
// only property that matters.
//
// A hundredth of the buy-in as the big blind gives a hundred big blinds to
// start with, which is the shape of a cash game rather than a tournament. The
// blinds do not escalate: a table that ends when somebody busts does not need
// them to, and a schedule nobody agreed would be a schedule to disagree about.
func blindsFor(buyIn uint64) (replay.Schedule, error) {
	big := int64(buyIn) / 100
	if big < 2 {
		return replay.Schedule{}, fmt.Errorf(
			"a buy-in of %d is too small to split into blinds", buyIn)
	}
	return replay.Schedule{Levels: []replay.Blinds{{Small: big / 2, Big: big}}}, nil
}

// startPlaying opens the table for dealing, if it is ready and has not already.
//
// Called wherever the table's state might have changed, so it has to be cheap
// and idempotent: it answers "is there anything to start" and almost always
// says no.
func (tbl *table) startPlaying() []outgoing {
	if tbl.play != nil || tbl.watch == nil {
		return nil
	}
	if tbl.finished {
		// This player got up. The table is kept only as a record of coin
		// still on the chain, and a late arrival must not seat them at
		// something they left.
		return nil
	}
	if tbl.dealt {
		// A hand was opened here before this process started, and hand
		// numbers are signing positions: opening hand one again with a
		// different deck would sign a used position over different bytes
		// and publish this seat's log key. What was under way was never
		// agreed by anybody, so there is nothing to resume - the table
		// settles at the last boundary every seat signed, which is what a
		// table that stops does anyway.
		return nil
	}
	if len(tbl.funded) < int(tbl.terms.Seats) {
		// Not every stake is on the chain yet, as far as this peer can
		// tell. Somebody else saying otherwise is not the same thing.
		return nil
	}
	if len(tbl.bonded) < int(tbl.terms.Seats) {
		// And not every seat has posted what it loses for walking out.
		// Dealing before then is dealing with no answer to somebody who
		// stops, which is the whole thing the bond is for.
		return nil
	}
	if !tbl.ourMoneyConfirmed() {
		// Our own two payments are in the maps above from the moment they
		// were broadcast, so the counts can be complete while the chain has
		// yet to agree. Every other seat is held to two confirmations and
		// this seat is not exempt from its own rule.
		return nil
	}
	seats, ok := tbl.form.LogSeats()
	if !ok {
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
	// The log key is bound to its match here and not at join, because until
	// the roster settled there was no match to bind it to. It has to be the
	// same string the log chain identifies this table by, or every entry
	// this seat signs is refused by its own chain before it ever reaches
	// anybody: a table that looked like it had started and could not open a
	// hand.
	logKey, err := forfeit.LogKeyFrom(tbl.logPriv, match)
	if err != nil {
		log.Printf("pokerplugin: table %s cannot start dealing: %v", tbl.terms.SID, err)
		return nil
	}
	// What this key has already signed, from disk, so the refusal to sign one
	// position twice outlives a crash.
	logKey.Remember(&signBook{tbl: tbl})

	// Recorded before a hand can be opened, never after. A crash between the
	// two costs this table; the other order costs the bond.
	tbl.dealt = true
	_ = tbl.save()
	schedule, err := blindsFor(tbl.terms.BuyInAtoms)
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}
	stakes := make([]int64, len(seats))
	for i := range stakes {
		stakes[i] = int64(tbl.terms.BuyInAtoms)
	}

	// The button starts where the seating put seat zero. Seating is drawn
	// from a block hash nobody chose, so this is as arbitrary as it needs to
	// be and every peer derives the same one.
	p, err := driver.NewTable(driver.TableConfig{
		Match:    match,
		Seat:     int(seat),
		Log:      logKey,
		Roster:   seats,
		Stakes:   stakes,
		Schedule: schedule,
		Button:   0,
	})
	if err != nil {
		log.Printf("pokerplugin: table %s cannot start dealing: %v", tbl.terms.SID, err)
		return nil
	}
	tbl.play = p
	if tbl.atHeight > 0 {
		// Before Start, because Start is what makes this table's first
		// entries and a driver nobody has told stamps them with zero.
		p.AtHeight(uint32(tbl.atHeight))
	}

	out, err := p.Start()
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}
	log.Printf("pokerplugin: table %s is dealing, %d seats, blinds %d/%d",
		tbl.terms.SID, len(seats), schedule.Levels[0].Small, schedule.Levels[0].Big)
	return append(tbl.publish(out), tbl.drainHeld()...)
}

// maxHeld is how many frames are kept for a table that has not started dealing.
//
// A hand's opening is a card key from each seat, so a full table needs at most
// one per seat and this is generous. It is a bound and not a buffer: the point
// is that an unbounded one is a way to fill this process's memory with frames
// for a table that will never deal.
const maxHeld = 32

// hold keeps a frame for a hand this peer has not started yet.
//
// Every seat starts dealing at a different moment, because each waits for the
// chain to confirm the last bond and each sees that in its own time. The seat
// that sees it first opens the hand and publishes its card key immediately -
// to a table where nobody else is dealing yet.
//
// Dropping those frames deadlocks the table, and does so at every table rather
// than rarely: the first seat to start collects everybody else's keys, and
// everybody else is missing that first seat's. Both then sit owing each other
// nothing they can see, until the obligation stands long enough for the bonds
// to start being claimed - over a hand that was never dealt.
//
// The driver already holds shares that arrive before their slot is open, for
// exactly this reason and in almost these words. This is the same rule one
// layer up: early is not wrong, it is just early.
func (tbl *table) hold(msg *schema.Message) {
	if tbl.watch == nil {
		// No membership, so no table these could belong to. Before the
		// roster settles there is nothing to feed them into and nothing
		// to check them against.
		return
	}
	if len(tbl.held) >= maxHeld {
		return
	}
	tbl.held = append(tbl.held, msg)
}

// drainHeld feeds in what arrived before this peer was dealing.
func (tbl *table) drainHeld() []outgoing {
	held := tbl.held
	tbl.held = nil

	var out []outgoing
	for _, msg := range held {
		out = append(out, tbl.deal(msg)...)
	}
	return out
}

// publish turns what the driver wants to send into frames.
//
// A message that cannot be rendered is dropped with a note rather than taking
// the table down. It is a bug if it happens, and the hand stalling into a bond
// claim is a better outcome than a peer falling out of a table it is still
// seated at.
func (tbl *table) publish(msgs []driver.Out) []outgoing {
	if len(msgs) == 0 {
		return nil
	}
	hand := tbl.playingHand()
	var out []outgoing
	for _, m := range msgs {
		kind, body, err := renderDriver(m, hand)
		if err != nil {
			log.Printf("pokerplugin: table %s: cannot send %T: %v", tbl.terms.SID, m, err)
			continue
		}
		out = append(out, tbl.frame(kind, body, wire.ClassState))
	}
	return out
}

// playingHand is the hand number the driver is on, for stamping messages that
// do not carry one of their own.
func (tbl *table) playingHand() uint64 {
	if tbl.play == nil {
		return 0
	}
	if h := tbl.play.Hand(); h != nil {
		return h.State().Hand
	}
	return 0
}

func renderDriver(m driver.Out, hand uint64) (schema.Kind, any, error) {
	switch v := m.(type) {
	case driver.OutCardKey:
		body, err := schema.CardKeyFrom(v)
		return schema.KindCardKey, body, err
	case driver.OutShuffle:
		body, err := schema.ShuffleFrom(v, hand)
		return schema.KindShuffle, body, err
	case driver.OutShare:
		body, err := schema.ShareFrom(v, hand)
		return schema.KindShare, body, err
	case driver.OutAction:
		return schema.KindAction, schema.Action{Entry: v.Entry.Transcript()}, nil
	case driver.OutCheckpoint:
		return schema.KindCheckpoint, schema.CheckpointFrom(v.Checkpoint), nil
	case driver.OutLeaving:
		return schema.KindLeaving, schema.Leaving{
			Seat: uint32(v.Seat), Hand: v.Hand, Sig: hex.EncodeToString(v.Sig),
		}, nil
	}
	return "", nil, fmt.Errorf("nothing renders a %T", m)
}

// deal folds a dealing message into the hand in progress.
//
// Errors are logged and swallowed, deliberately. Every message here is signed or
// proved, so one that does not check out is a peer misbehaving or a frame that
// arrived mangled - and neither is a reason for this process to stop playing at
// a table its money is already in. The bond is what answers misbehaviour.
func (tbl *table) deal(msg *schema.Message) []outgoing {
	if tbl.play == nil {
		tbl.hold(msg)
		return nil
	}
	in, err := decodeDriver(msg)
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}
	if in == nil {
		return nil
	}
	out, err := tbl.play.Handle(in)
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}
	return tbl.publish(out)
}

func decodeDriver(msg *schema.Message) (driver.In, error) {
	switch msg.Kind {
	case schema.KindCardKey:
		var body schema.CardKey
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		in, err := body.Into()
		return in, err

	case schema.KindShuffle:
		var body schema.Shuffle
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		in, err := body.Into()
		return in, err

	case schema.KindShare:
		var body schema.Share
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		in, err := body.Into()
		return in, err

	case schema.KindCheckpoint:
		var body schema.Checkpoint
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		cp, err := body.Into()
		if err != nil {
			return nil, err
		}
		return driver.InCheckpoint{Checkpoint: cp}, nil

	case schema.KindLeaving:
		var body schema.Leaving
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		sig, err := hex.DecodeString(body.Sig)
		if err != nil {
			return nil, fmt.Errorf("leaving signature: %w", err)
		}
		return driver.InLeaving{Seat: int(body.Seat), Hand: body.Hand, Sig: sig}, nil

	case schema.KindAction:
		// The bet, and the one that was missing. Three kinds carry the
		// dealing - a card key, a shuffle, a share - and this carries
		// everything anybody does with the cards afterwards. It was left
		// out when the dealing was wired up and nothing said so: the
		// registry routes an action here while a hand is running, the
		// fall-through below returned no error and no input, and deal
		// discards a nil input in silence.
		//
		// So every action any peer ever sent was dropped by every peer
		// that received it. Two seats, both dealt in, each waiting for
		// the other to act, neither logging a thing.
		var body schema.Action
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		entry, err := body.Entry.Entry()
		if err != nil {
			return nil, fmt.Errorf("an action that cannot be read back: %w", err)
		}
		return driver.InAction{Entry: *entry}, nil
	}
	// Loud, because the silence is what cost a table. Every kind the
	// registry sends here is handled above, so reaching this means a new
	// one was added to the routing and not to the decoding.
	return nil, fmt.Errorf("nothing here knows how to read a %q into the hand", msg.Kind)
}

// Act takes this player's decision at a table.
//
// The rules are checked before anything is signed, inside the driver, so a
// refusal here means the move was never put on the wire.
func (t *tables) Act(sid string, action string, amount int64) ([]outgoing, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return nil, fmt.Errorf("not at a table under session %s", sid)
	}
	if tbl.play == nil {
		return nil, fmt.Errorf("this table is not dealing yet")
	}
	act := gamelog.Action(action)
	if !act.Valid() {
		return nil, fmt.Errorf("%q is not an action", action)
	}
	out, err := tbl.play.Act(act, amount)
	if err != nil {
		return nil, err
	}
	return tbl.publish(out), nil
}
