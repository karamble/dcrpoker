// Package driver runs a hand.
//
// Every piece of a trustless hand existed before this and none of them played
// one: the deck could shuffle and open, the reducer could fold a log into
// chips, the bond could punish walking out, and nothing put a card in anybody's
// hand. This is the part that does - once, for one peer, with no server and
// nobody in charge.
//
// It is a reducer over messages, for the same reason the betting is a reducer
// over entries. Feed it what arrived and it returns what to send: no goroutines,
// no channels, no timers and no clock. Two peers fed the same messages reach the
// same state, which is the only arrangement that works when neither is trusted
// and either might be replaying the hand a week later to check it.
//
// # What each peer publishes, and when
//
// The whole privacy of the game is a schedule of who reveals what:
//
//   - A seat's hole cards: everyone *else* publishes a share as soon as the
//     deck is shuffled. The owner adds their own and reads the card; nobody
//     else can, because the owner's share is the one missing.
//   - The board: everyone publishes, but only when the street turns. A share
//     published early is not caught later - it is the leak.
//   - At showdown: a seat still contesting publishes its own hole shares, and
//     that is what makes its hand readable at all.
//
// Getting that schedule wrong is not a bug that shows up as a failed check. It
// shows up as somebody seeing a card they should not have, and nothing in the
// protocol notices.
package driver

import (
	"fmt"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

// Phase is how far through a hand this peer has got.
type Phase int

const (
	// PhaseShuffling is every seat permuting the deck in turn.
	PhaseShuffling Phase = iota
	// PhaseDealing is the shares that make hole cards readable by their
	// owners and nobody else.
	PhaseDealing
	// PhaseBetting is the hand proper.
	PhaseBetting
	// PhaseShowdown is contesting seats opening what they held.
	PhaseShowdown
	// PhaseDone is settled.
	PhaseDone
)

func (p Phase) String() string {
	switch p {
	case PhaseShuffling:
		return "shuffling"
	case PhaseDealing:
		return "dealing"
	case PhaseBetting:
		return "betting"
	case PhaseShowdown:
		return "showdown"
	case PhaseDone:
		return "done"
	}
	return fmt.Sprintf("phase(%d)", int(p))
}

// Config is everything a peer needs to play one hand.
//
// All of it is agreed before the hand starts - the roster from the escrow, the
// card keys from the seats, the stacks from the last checkpoint - so nothing
// here is anybody's opinion at the time it matters.
type Config struct {
	Match  string
	Seat   int
	Hand   uint64
	Button int

	// Log signs this peer's entries. Its public half must be the one the
	// chain's roster holds for this seat.
	Log *forfeit.LogKey
	// Card is this peer's per-hand deck key, and CardKeys is every seat's
	// public half, seat-indexed.
	Card     *deck.KeyPair
	CardKeys []kyber.Point

	// Chain is the log this hand is appended to. It carries the roster the
	// entries are checked against.
	Chain *gamelog.Chain
	// Table and Stacks are what the betting is folded against.
	Table  *replay.Table
	Stacks []int64
}

// Driver is one peer playing one hand.
type Driver struct {
	cfg   Config
	seats int
	phase Phase

	joint  kyber.Point
	layout replay.Layout
	deck   deck.Deck
	// round is how many shuffles have been applied.
	round int

	state *replay.State

	// openings collects shares for the slots this peer may read. A slot with
	// no entry here is one this peer is not entitled to open, which is the
	// difference between a card it has not read yet and one it never will.
	openings map[int]*deck.Opening
	cards    map[int]deck.Card
	// shared records the slots this peer has already published a share for,
	// so a message arriving twice does not produce a second one.
	shared map[int]bool
	// held is shares that arrived for a slot this peer was not yet
	// collecting.
	//
	// They cannot be discarded. A share for the river can arrive before the
	// river turns, and a showdown share before this peer has finished folding
	// the action that ended the betting - neither is wrong, both are just
	// early, and dropping one strands a card that will never be sent again.
	// This channel loses messages and reorders them; a driver that assumed
	// otherwise would work in a test and hang at a table.
	held map[int][]heldShare
	// seen records which seats have published a share for which slot,
	// whether or not this peer is collecting that slot.
	//
	// Tracked for slots this peer will never open, which is the point: a seat
	// that has not published its share for somebody else's hole card owes one,
	// and knowing that is what lets a claim name an obligation rather than a
	// person. Without it a peer could only say "you are gone", which is the
	// accusation nobody can check.
	seen map[int]map[int]bool
}

type heldShare struct {
	seat  int
	share *deck.Share
}

// New starts a hand.
func New(cfg Config) (*Driver, error) {
	switch {
	case cfg.Match == "":
		return nil, fmt.Errorf("no match")
	case cfg.Log == nil:
		return nil, fmt.Errorf("no log key")
	case cfg.Card == nil:
		return nil, fmt.Errorf("no card key")
	case cfg.Chain == nil:
		return nil, fmt.Errorf("no chain")
	case cfg.Table == nil:
		return nil, fmt.Errorf("no table")
	case cfg.Hand == 0:
		return nil, fmt.Errorf("hands are numbered from 1")
	}
	if cfg.Log.Match() != cfg.Match {
		return nil, fmt.Errorf("the log key belongs to match %s, not %s", cfg.Log.Match(), cfg.Match)
	}
	n := len(cfg.CardKeys)
	if n < 2 {
		return nil, fmt.Errorf("a hand needs at least 2 card keys, not %d", n)
	}
	if cfg.Seat < 0 || cfg.Seat >= n {
		return nil, fmt.Errorf("seat %d is not at a %d seat table", cfg.Seat, n)
	}
	if !cfg.CardKeys[cfg.Seat].Equal(cfg.Card.Public) {
		return nil, fmt.Errorf("this peer's card key is not the one seat %d published", cfg.Seat)
	}
	joint, err := deck.JointKey(cfg.CardKeys)
	if err != nil {
		return nil, err
	}
	layout := replay.Layout{Seats: n}
	if _, err := layout.Board(); err != nil {
		return nil, err
	}

	state, err := replay.StartHand(cfg.Table, cfg.Hand, cfg.Button, cfg.Stacks)
	if err != nil {
		return nil, err
	}
	// The reducer numbers from the start of a hand and the chain numbers from
	// the start of the table. They have to agree, or the first entry of the
	// second hand is refused by one and accepted by the other.
	_, seq := cfg.Chain.Head()
	state.Seq = seq

	return &Driver{
		cfg:      cfg,
		seats:    n,
		phase:    PhaseShuffling,
		joint:    joint,
		layout:   layout,
		deck:     deck.Fresh(joint),
		state:    state,
		openings: map[int]*deck.Opening{},
		cards:    map[int]deck.Card{},
		shared:   map[int]bool{},
		held:     map[int][]heldShare{},
		seen:     map[int]map[int]bool{},
	}, nil
}

// Phase reports how far through the hand this peer is.
func (d *Driver) Phase() Phase { return d.phase }

// Shuffled reports how many seats have shuffled this hand.
//
// A count of checks, not of messages. A shuffle from anybody else is verified
// against its own proof before it is built on, and one that did not verify was
// never applied - so this is how much of the deck's provenance this process
// established itself, which is the only kind it can honestly report.
func (d *Driver) Shuffled() int { return d.round }

// State is the betting, as folded from the log so far.
func (d *Driver) State() *replay.State { return d.state }

// Out is something to send to the rest of the table.
type Out interface{ out() }

// OutShuffle is this peer's shuffle and the proof it did nothing else.
type OutShuffle struct {
	Seat  int
	Deck  deck.Deck
	Proof []byte
}

// OutShare is one decryption share for one slot.
type OutShare struct {
	Seat  int
	Slot  int
	Share *deck.Share
}

// OutAction is a signed log entry.
type OutAction struct{ Entry gamelog.Entry }

func (OutShuffle) out() {}
func (OutShare) out()   {}
func (OutAction) out()  {}

// In is something that arrived.
type In interface{ in() }

// InShuffle is another seat's shuffle.
type InShuffle struct {
	Seat  int
	Deck  deck.Deck
	Proof []byte
}

// InShare is another seat's share for a slot.
type InShare struct {
	Seat  int
	Slot  int
	Share *deck.Share
}

// InAction is another seat's signed entry.
type InAction struct{ Entry gamelog.Entry }

func (InShuffle) in() {}
func (InShare) in()   {}
func (InAction) in()  {}

// Start produces whatever this peer owes before anything has arrived.
//
// Which is a shuffle if it is first, and nothing otherwise. A peer that is not
// first says nothing at all rather than announcing itself, because a message
// that carries no information is still a message that can be lost, replayed or
// disagreed about.
func (d *Driver) Start() ([]Out, error) {
	if d.phase != PhaseShuffling {
		return nil, fmt.Errorf("the hand has already started")
	}
	if d.round != d.cfg.Seat {
		return nil, nil
	}
	return d.shuffle()
}

// shuffle permutes the deck in this peer's turn and proves it.
func (d *Driver) shuffle() ([]Out, error) {
	ctx, err := d.state.DeckContext(uint32(d.round), d.cfg.Card.Public)
	if err != nil {
		return nil, err
	}
	out, prf, _, err := deck.Shuffle(ctx, d.joint, d.deck)
	if err != nil {
		return nil, fmt.Errorf("shuffle: %w", err)
	}
	d.deck = out
	d.round++

	msgs := []Out{OutShuffle{Seat: d.cfg.Seat, Deck: out, Proof: prf}}
	done, err := d.maybeDeal()
	if err != nil {
		return nil, err
	}
	return append(msgs, done...), nil
}

// maybeDeal moves to dealing once every seat has shuffled.
func (d *Driver) maybeDeal() ([]Out, error) {
	if d.round < d.seats {
		return nil, nil
	}
	d.phase = PhaseDealing

	// A share for every other seat's hole cards, and none for its own. This
	// is what makes a hole card private: the owner is the only one who can
	// complete it, because the owner's share is the only one missing.
	var msgs []Out
	for seat := range d.seats {
		if seat == d.cfg.Seat {
			continue
		}
		slots, err := d.layout.Hole(seat)
		if err != nil {
			return nil, err
		}
		for _, slot := range slots {
			m, err := d.share(slot)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m...)
		}
	}

	// And this peer starts collecting its own, which it can only finish once
	// everybody else has sent theirs.
	own, err := d.layout.Hole(d.cfg.Seat)
	if err != nil {
		return nil, err
	}
	for _, slot := range own {
		if err := d.expect(slot); err != nil {
			return nil, err
		}
	}
	d.phase = PhaseBetting
	return msgs, nil
}

// share publishes this peer's share for a slot, once.
func (d *Driver) share(slot int) ([]Out, error) {
	if d.shared[slot] {
		return nil, nil
	}
	if slot < 0 || slot >= len(d.deck) {
		return nil, fmt.Errorf("slot %d is not in the deck", slot)
	}
	ctx, err := d.state.DeckContext(0, d.cfg.Card.Public)
	if err != nil {
		return nil, err
	}
	s, err := deck.Reveal(ctx, d.joint, d.cfg.Card, d.deck[slot])
	if err != nil {
		return nil, fmt.Errorf("reveal slot %d: %w", slot, err)
	}
	d.shared[slot] = true
	return []Out{OutShare{Seat: d.cfg.Seat, Slot: slot, Share: s}}, nil
}

// expect starts collecting shares for a slot this peer may read, contributing
// its own straight away.
func (d *Driver) expect(slot int) error {
	if _, ok := d.openings[slot]; ok {
		return nil
	}
	ctx, err := d.state.DeckContext(0, d.cfg.Card.Public)
	if err != nil {
		return err
	}
	o, err := deck.NewOpening(ctx, d.joint, d.deck[slot], d.cfg.CardKeys)
	if err != nil {
		return err
	}
	own, err := deck.Reveal(ctx, d.joint, d.cfg.Card, d.deck[slot])
	if err != nil {
		return err
	}
	if err := o.Add(d.cfg.Card.Public, own); err != nil {
		return err
	}
	d.openings[slot] = o

	// Anything that arrived early for this slot goes in now.
	for _, h := range d.held[slot] {
		if err := o.Add(d.cfg.CardKeys[h.seat], h.share); err != nil {
			return fmt.Errorf("seat %d's share for slot %d: %w", h.seat, slot, err)
		}
	}
	delete(d.held, slot)
	return d.tryOpen(slot)
}

// tryOpen reads a slot if every share has arrived.
func (d *Driver) tryOpen(slot int) error {
	o, ok := d.openings[slot]
	if !ok || o.Missing() != 0 {
		return nil
	}
	if _, done := d.cards[slot]; done {
		return nil
	}
	c, err := o.Card()
	if err != nil {
		return fmt.Errorf("slot %d: %w", slot, err)
	}
	d.cards[slot] = c
	return nil
}

// Handle folds one arriving message in and returns whatever it obliges this
// peer to send.
func (d *Driver) Handle(in In) ([]Out, error) {
	switch m := in.(type) {
	case InShuffle:
		return d.onShuffle(m)
	case InShare:
		return d.onShare(m)
	case InAction:
		return d.onAction(m)
	}
	return nil, fmt.Errorf("%T is not a message this hand understands", in)
}

func (d *Driver) onShuffle(m InShuffle) ([]Out, error) {
	if d.phase != PhaseShuffling {
		return nil, fmt.Errorf("a shuffle arrived while %s", d.phase)
	}
	if m.Seat < d.round {
		// Already folded in. This channel loses messages, so anything
		// that must arrive gets said again, and the second telling is a
		// repeat rather than a peer shuffling twice. The early case is
		// held a few lines down for the same reason; this is its mirror.
		return nil, nil
	}
	if m.Seat != d.round {
		return nil, fmt.Errorf("seat %d shuffled out of turn; it is seat %d's turn", m.Seat, d.round)
	}
	if m.Seat == d.cfg.Seat {
		return nil, fmt.Errorf("this peer's own shuffle came back to it")
	}
	if m.Seat < 0 || m.Seat >= d.seats {
		return nil, fmt.Errorf("seat %d is not at this table", m.Seat)
	}
	ctx, err := d.state.DeckContext(uint32(d.round), d.cfg.CardKeys[m.Seat])
	if err != nil {
		return nil, err
	}
	// Verified before it is built on, every time. An unverified shuffle is an
	// unconstrained one and the whole guarantee is gone.
	if err := deck.VerifyShuffle(ctx, d.joint, d.deck, m.Deck, m.Proof); err != nil {
		return nil, fmt.Errorf("seat %d's shuffle: %w", m.Seat, err)
	}
	d.deck = m.Deck
	d.round++

	if d.round == d.cfg.Seat {
		return d.shuffle()
	}
	return d.maybeDeal()
}

func (d *Driver) onShare(m InShare) ([]Out, error) {
	if m.Seat < 0 || m.Seat >= d.seats {
		return nil, fmt.Errorf("seat %d is not at this table", m.Seat)
	}
	if m.Seat == d.cfg.Seat {
		return nil, fmt.Errorf("this peer's own share came back to it")
	}
	if d.sharedBy(m.Slot, m.Seat) {
		// Counted already. Adding it twice is what an opening refuses,
		// and a retransmission must not look like a fault.
		return nil, nil
	}

	o, ok := d.openings[m.Slot]
	if !ok {
		// Not opening this slot yet. That is usually somebody else's hole
		// card and will never be opened here, and is sometimes a card whose
		// street has not turned - and the two are indistinguishable now, so
		// it is kept rather than judged. Keeping a share nobody needs costs
		// a few hundred bytes; dropping one that was merely early strands
		// the card for good.
		for _, h := range d.held[m.Slot] {
			if h.seat == m.Seat {
				return nil, nil // already held; a duplicate is not an error
			}
		}
		d.held[m.Slot] = append(d.held[m.Slot], heldShare{seat: m.Seat, share: m.Share})
		d.note(m.Slot, m.Seat)
		return nil, nil
	}
	if err := o.Add(d.cfg.CardKeys[m.Seat], m.Share); err != nil {
		return nil, fmt.Errorf("seat %d's share for slot %d: %w", m.Seat, m.Slot, err)
	}
	// Noted only now that it is known to be good, which is the whole point of
	// where this line sits.
	//
	// It used to be noted on arrival, before o.Add had judged it, and that turned
	// one bad share into a permanently stranded card: the seat was recorded as
	// having published, so Owes stopped naming it and every retransmission was
	// dropped by the duplicate check above - including the good copy that would
	// have fixed it. A hand stuck that way cannot recover by any means, because
	// the peer has nothing left to say and this peer has nothing left to ask.
	//
	// It was reached in practice by the previous hand's shares being republished
	// against this hand's deck, which fail exactly this way. Both halves are
	// fixed; either alone would have been enough to strand a table.
	d.note(m.Slot, m.Seat)
	return nil, d.tryOpen(m.Slot)
}

// note records that a seat has published its share for a slot.
func (d *Driver) note(slot, seat int) {
	if d.seen[slot] == nil {
		d.seen[slot] = map[int]bool{}
	}
	d.seen[slot][seat] = true
}

// shared reports whether a seat has published its share for a slot. This peer's
// own shares count as published the moment it sends them.
func (d *Driver) sharedBy(slot, seat int) bool {
	if seat == d.cfg.Seat {
		return d.shared[slot]
	}
	return d.seen[slot][seat]
}

func (d *Driver) onAction(m InAction) ([]Out, error) {
	if d.phase != PhaseBetting {
		return nil, fmt.Errorf("an action arrived while %s", d.phase)
	}
	if int(m.Entry.Seat) == d.cfg.Seat {
		return nil, fmt.Errorf("this peer's own action came back to it")
	}
	if _, seq := d.cfg.Chain.Head(); m.Entry.Seq <= seq {
		// Already in the chain. Backfilling a peer that turned out to be
		// only one entry behind sends everything from its head, so the
		// overlap is normal and is not a fork: a genuine fork is two
		// different entries at one sequence, which Append still refuses.
		return nil, nil
	}
	return d.apply(&m.Entry)
}

// Act takes this peer's own decision, signs it and folds it in.
//
// The rules are checked before anything is signed, so a peer cannot put its
// name to a move the table would refuse - and because the same check runs on
// arrival everywhere, an entry that survives here survives at every seat.
func (d *Driver) Act(action gamelog.Action, amount int64, height uint32) ([]Out, error) {
	if d.phase != PhaseBetting {
		return nil, fmt.Errorf("cannot act while %s", d.phase)
	}
	if d.state.ToAct != d.cfg.Seat {
		return nil, fmt.Errorf("it is seat %d's turn, not this peer's", d.state.ToAct)
	}
	e := d.cfg.Chain.Next(uint32(d.cfg.Seat), d.cfg.Hand, d.state.Street, action, amount)
	e.Height = height
	if _, err := replay.Apply(d.state, e); err != nil {
		return nil, err
	}
	if err := e.Sign(d.cfg.Log); err != nil {
		return nil, err
	}
	msgs, err := d.apply(e)
	if err != nil {
		return nil, err
	}
	return append([]Out{OutAction{Entry: *e}}, msgs...), nil
}

// apply appends an entry to the log and folds it into the betting.
//
// Both, and in that order. The chain checks that the entry is signed by the
// seat it names and links to the history everyone holds; the reducer checks the
// rules of poker. Neither is the other's job and skipping either lets through a
// different kind of lie.
func (d *Driver) apply(e *gamelog.Entry) ([]Out, error) {
	if err := d.cfg.Chain.Append(e); err != nil {
		return nil, err
	}
	street := d.state.Street
	next, err := replay.Apply(d.state, e)
	if err != nil {
		return nil, err
	}
	d.state = next

	var msgs []Out
	// A street turned, so the cards it turns over may now be opened - and
	// not before, which is the only thing keeping the turn off the flop.
	if next.Street != street && !next.Done {
		m, err := d.revealBoard()
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m...)
	}
	if next.Done {
		m, err := d.finish()
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m...)
	}
	return msgs, nil
}

// revealBoard publishes shares for the community cards now face up.
func (d *Driver) revealBoard() ([]Out, error) {
	slots, err := d.layout.BoardAt(d.state.Street)
	if err != nil {
		return nil, err
	}
	var msgs []Out
	for _, slot := range slots {
		m, err := d.share(slot)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m...)
		if err := d.expect(slot); err != nil {
			return nil, err
		}
	}
	return msgs, nil
}

// finish moves to showdown, or straight to done when nobody has to show.
func (d *Driver) finish() ([]Out, error) {
	// One player left with a claim: nothing is shown, ever. The same reason
	// an abandoned hand can be settled without opening a card.
	if d.state.Winner() >= 0 {
		d.phase = PhaseDone
		return nil, nil
	}
	d.phase = PhaseShowdown

	// Everything still on the board comes out, whatever street the betting
	// stopped on - an all-in on the flop still runs out the turn and river.
	msgs, err := d.runout()
	if err != nil {
		return nil, err
	}

	// And this peer shows what it held, if it is still contesting.
	if !d.state.Seats[d.cfg.Seat].Folded {
		own, err := d.layout.Hole(d.cfg.Seat)
		if err != nil {
			return nil, err
		}
		for _, slot := range own {
			m, err := d.share(slot)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, m...)
		}
	}
	// The hands this peer must be able to read to settle: everyone still in.
	for seat := range d.seats {
		if d.state.Seats[seat].Folded {
			continue
		}
		slots, err := d.layout.Hole(seat)
		if err != nil {
			return nil, err
		}
		for _, slot := range slots {
			if err := d.expect(slot); err != nil {
				return nil, err
			}
		}
	}
	return msgs, nil
}

// runout opens the rest of the board when betting ended early.
func (d *Driver) runout() ([]Out, error) {
	board, err := d.layout.Board()
	if err != nil {
		return nil, err
	}
	var msgs []Out
	for _, slot := range board {
		m, err := d.share(slot)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m...)
		if err := d.expect(slot); err != nil {
			return nil, err
		}
	}
	return msgs, nil
}

// Hole reports this peer's own cards, once it can read them.
func (d *Driver) Hole() ([2]deck.Card, bool) { return d.Shown(d.cfg.Seat) }

// Shown reports a seat's cards, if they have opened here.
//
// Which for anybody but this peer means a showdown they contested: a seat still
// in at the end publishes the shares for its own hole cards, and that is the
// only thing that ever opens them. A seat that folded published nothing, so its
// slots never open, so this says no - not by a rule written here, but because
// there is genuinely nothing to read.
//
// That distinction is load-bearing rather than tidy. A folded hand staying shut
// is what lets an abandoned hand settle at all: nothing was revealed, so there
// is nothing to argue about. So this reports what opened and never reconstructs
// what did not, and a caller may show exactly what it is given.
//
// Both cards or neither. One card of a hand is not a hand, and a showdown
// rendered half-open would invite reading something into the gap.
func (d *Driver) Shown(seat int) ([2]deck.Card, bool) {
	if seat < 0 || seat >= d.seats {
		return [2]deck.Card{}, false
	}
	slots, err := d.layout.Hole(seat)
	if err != nil {
		return [2]deck.Card{}, false
	}
	var out [2]deck.Card
	for i, slot := range slots {
		c, ok := d.cards[slot]
		if !ok {
			return [2]deck.Card{}, false
		}
		out[i] = c
	}
	return out, true
}

// Board reports the community cards this peer has been able to read.
func (d *Driver) Board() []deck.Card {
	board, err := d.layout.Board()
	if err != nil {
		return nil
	}
	var out []deck.Card
	for _, slot := range board {
		c, ok := d.cards[slot]
		if !ok {
			break
		}
		out = append(out, c)
	}
	return out
}

// Settle works out the money, once everything needed is readable.
//
// It returns an error rather than a guess while anything is still missing: a
// hand settled on cards that have not all arrived is one settled on cards
// somebody could still choose.
func (d *Driver) Settle() ([]replay.Award, error) {
	if d.phase != PhaseShowdown && d.phase != PhaseDone {
		return nil, fmt.Errorf("the hand is not over; it is %s", d.phase)
	}
	if d.state.Winner() >= 0 {
		d.phase = PhaseDone
		return replay.Settle(d.state, nil)
	}

	board, err := d.layout.Board()
	if err != nil {
		return nil, err
	}
	var community [5]deck.Card
	for i, slot := range board {
		c, ok := d.cards[slot]
		if !ok {
			return nil, fmt.Errorf("the board is not complete; slot %d has not opened", slot)
		}
		community[i] = c
	}
	holes := make(map[int][2]deck.Card, d.seats)
	for seat := range d.seats {
		if d.state.Seats[seat].Folded {
			continue
		}
		slots, err := d.layout.Hole(seat)
		if err != nil {
			return nil, err
		}
		var pair [2]deck.Card
		for i, slot := range slots {
			c, ok := d.cards[slot]
			if !ok {
				return nil, fmt.Errorf("seat %d has not shown", seat)
			}
			pair[i] = c
		}
		holes[seat] = pair
	}
	awards, err := replay.Settle(d.state, &replay.Showdown{Holes: holes, Board: community})
	if err != nil {
		return nil, err
	}
	d.phase = PhaseDone
	return awards, nil
}
