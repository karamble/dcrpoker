package main

import (
	"fmt"
	"log"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
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
	if len(tbl.funded) < int(tbl.terms.Seats) {
		// Not every stake is on the chain yet, as far as this peer can
		// tell. Somebody else saying otherwise is not the same thing.
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
		Log:      tbl.log,
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

	out, err := p.Start()
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}
	log.Printf("pokerplugin: table %s is dealing, %d seats, blinds %d/%d",
		tbl.terms.SID, len(seats), schedule.Levels[0].Small, schedule.Levels[0].Big)
	return tbl.publish(out)
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
		return schema.KindLeaving, schema.Leaving{Seat: uint32(v.Seat), Hand: v.Hand}, nil
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
		// Not dealing yet. Frames for a hand this peer has not started are
		// kept by nobody: the sender repeats them, or the hand stalls and
		// is answered as an absence.
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
		return driver.InLeaving{Seat: int(body.Seat), Hand: body.Hand}, nil
	}
	return nil, nil
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
