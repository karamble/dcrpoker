// Package replay folds a signed action log into the state of a hand.
//
// The server holds the game in a set of communicating state machines - reply
// channels, event channels that drop on backpressure, three families of
// wall-clock timers - and the log is written alongside as a record of what the
// engine decided. That arrangement has two sources of truth, and every problem
// worth solving here comes from the gap between them: the chain can hold an
// action the engine refused, the engine can accept one the chain never got, and
// a peer replaying the chain reaches a different answer than the table did.
//
// So there is no engine here. The log *is* the state:
//
//	apply(state, entry) -> state'
//
// No goroutines, no channels, no timers, no clock. Every peer folds the same
// verified log and lands on the same state, which is the only arrangement that
// works when nobody is in charge. Legality stops being something checked before
// or after recording an action and becomes part of applying it: an entry the
// rules refuse is refused identically by everyone, and never reaches any state.
//
// # What is deliberately not here
//
// Cards. A hand's cards come from pkg/deck and its winner from the hand
// evaluator; this package decides who may act, what it costs them, and where
// the chips end up. Keeping them apart means the betting can be replayed and
// checked without opening a single card, which is exactly what a peer needs to
// do while a hand is still being played.
//
// Time. Nothing here can time a player out, because a timeout is one machine's
// opinion about another and cannot be replayed. What the docs forbid is money
// moving on one machine's clock, and a reducer with no clock cannot do it.
package replay

import (
	"fmt"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// Blinds is one level of a blind schedule.
type Blinds struct {
	Small, Big int64
}

// Schedule says what the blinds are at a given hand.
//
// Indexed by hand number rather than by elapsed time. The server escalates
// levels on a wall clock, which cannot be replayed: two peers folding the same
// log days apart would post different blinds and diverge on the first pot. Hand
// number is in the log already and costs nothing.
type Schedule struct {
	Levels        []Blinds
	HandsPerLevel uint64 // 0 never escalates
}

// At reports the blinds in force for a hand.
func (s Schedule) At(hand uint64) (Blinds, error) {
	if len(s.Levels) == 0 {
		return Blinds{}, fmt.Errorf("a table needs at least one blind level")
	}
	if s.HandsPerLevel == 0 || hand == 0 {
		return s.Levels[0], nil
	}
	i := (hand - 1) / s.HandsPerLevel
	if i >= uint64(len(s.Levels)) {
		i = uint64(len(s.Levels)) - 1
	}
	return s.Levels[i], nil
}

// Table is what every peer agrees before the first hand: who is seated, in what
// order, with what stacks, playing what blinds.
//
// It comes from the escrow roster, so it is agreed on chain by everyone whose
// money is at stake rather than asserted by whoever is speaking.
type Table struct {
	Match    string
	Seats    []string // seat index to player id, in seat order
	Stacks   []int64  // starting stacks, parallel to Seats
	Schedule Schedule
}

func (t *Table) check() error {
	if t == nil {
		return fmt.Errorf("no table")
	}
	if len(t.Seats) < 2 {
		return fmt.Errorf("a table has at least 2 seats, not %d", len(t.Seats))
	}
	if len(t.Stacks) != len(t.Seats) {
		return fmt.Errorf("%d seats but %d stacks", len(t.Seats), len(t.Stacks))
	}
	for i, s := range t.Stacks {
		if s <= 0 {
			return fmt.Errorf("seat %d starts with %d chips", i, s)
		}
	}
	if _, err := t.Schedule.At(1); err != nil {
		return err
	}
	return nil
}

// Seat is one player's position in a hand.
type Seat struct {
	// Stack is what is still behind.
	Stack int64
	// Committed is what this seat has put in on this street.
	Committed int64
	// Total is what this seat has put in across the whole hand.
	Total int64
	// Folded and AllIn are terminal for the hand.
	Folded bool
	AllIn  bool
	// Acted records whether this seat has acted since the last raise, which
	// is what decides when a betting round is over.
	Acted bool
}

// State is a hand, and nothing but what the log put there.
type State struct {
	Table  *Table
	Hand   uint64
	Button int
	Street gamelog.Street
	Seats  []Seat

	// Pot is what earlier streets collected. Chips committed on the current
	// street are still on the seats until the street ends, because an
	// uncalled bet has to be returnable.
	Pot int64
	// Bet is the amount to match on this street, and MinRaise the smallest
	// legal increment above it.
	Bet      int64
	MinRaise int64
	// ToAct is the seat whose turn it is, or -1 when the hand is over.
	ToAct int
	// Seq is the sequence number of the last entry folded in.
	Seq uint64
	// Done is set when no further action is possible: everyone has folded
	// but one, or the river's betting is complete.
	Done bool
}

// Clone returns a deep copy, so that Apply never mutates its input.
func (s *State) Clone() *State {
	out := *s
	out.Seats = make([]Seat, len(s.Seats))
	copy(out.Seats, s.Seats)
	return &out
}

// live reports the seats that can still be given a decision.
func (s *State) live() []int {
	var out []int
	for i := range s.Seats {
		if !s.Seats[i].Folded && !s.Seats[i].AllIn {
			out = append(out, i)
		}
	}
	return out
}

// contesting reports the seats still in the hand, all-in included.
func (s *State) contesting() []int {
	var out []int
	for i := range s.Seats {
		if !s.Seats[i].Folded {
			out = append(out, i)
		}
	}
	return out
}

// next walks to the following seat that can act, or -1 if none can.
func (s *State) next(from int) int {
	n := len(s.Seats)
	for i := 1; i <= n; i++ {
		j := (from + i) % n
		if !s.Seats[j].Folded && !s.Seats[j].AllIn {
			return j
		}
	}
	return -1
}

// heads reports whether this is a two-handed hand, which reverses the blinds.
func (s *State) heads() bool { return len(s.Seats) == 2 }

// StartHand builds the state at the moment the blinds are posted.
//
// Everything here is derived from the table, the hand number and the button, so
// two peers reach the same starting position without exchanging anything.
func StartHand(t *Table, hand uint64, button int, stacks []int64) (*State, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	if hand == 0 {
		return nil, fmt.Errorf("hands are numbered from 1")
	}
	if button < 0 || button >= len(t.Seats) {
		return nil, fmt.Errorf("seat %d cannot hold the button at a %d seat table", button, len(t.Seats))
	}
	if stacks == nil {
		stacks = t.Stacks
	}
	if len(stacks) != len(t.Seats) {
		return nil, fmt.Errorf("%d seats but %d stacks", len(t.Seats), len(stacks))
	}
	blinds, err := t.Schedule.At(hand)
	if err != nil {
		return nil, err
	}
	if blinds.Small <= 0 || blinds.Big < blinds.Small {
		return nil, fmt.Errorf("blinds of %d/%d are not a level", blinds.Small, blinds.Big)
	}

	s := &State{
		Table:  t,
		Hand:   hand,
		Button: button,
		Street: gamelog.StreetPreFlop,
		Seats:  make([]Seat, len(t.Seats)),
	}
	for i := range s.Seats {
		s.Seats[i].Stack = stacks[i]
	}

	// Heads-up the button is the small blind and acts first before the flop;
	// at a fuller table the blinds sit to its left. Getting this backwards
	// is the classic way to make two implementations disagree about who owes
	// what, so it is written once here and derived everywhere else.
	var sb, bb int
	if s.heads() {
		sb, bb = button, (button+1)%2
	} else {
		sb, bb = (button+1)%len(s.Seats), (button+2)%len(s.Seats)
	}
	post(&s.Seats[sb], blinds.Small)
	post(&s.Seats[bb], blinds.Big)

	s.Bet = blinds.Big
	s.MinRaise = blinds.Big
	s.Pot = 0

	// A blind is not an action: posting it does not satisfy the seat's turn,
	// so the big blind still gets the option to raise.
	first := s.next(bb)
	s.ToAct = first
	if first < 0 || len(s.live()) < 2 {
		// Everyone is all-in on their blinds; there is nothing to decide.
		s.ToAct = -1
		s.Done = true
	}
	return s, nil
}

// post moves chips from a stack into the current street, capping at the stack.
func post(seat *Seat, amount int64) {
	if amount > seat.Stack {
		amount = seat.Stack
	}
	seat.Stack -= amount
	seat.Committed += amount
	seat.Total += amount
	if seat.Stack == 0 {
		seat.AllIn = true
	}
}

// Apply folds one entry into the state and returns the result.
//
// The input is never modified: a caller can keep every intermediate state, which
// is what makes "remove one entry and see whether the outcome changes" a
// question that can actually be asked.
//
// Legality is decided here and nowhere else. There is no separate engine to
// agree with, so an entry that the rules refuse is refused by every peer
// identically and enters no state at all.
func Apply(s *State, e *gamelog.Entry) (*State, error) {
	if s == nil {
		return nil, fmt.Errorf("no state")
	}
	if e == nil {
		return nil, fmt.Errorf("no entry")
	}
	if s.Done {
		return nil, fmt.Errorf("the hand is over; seat %d cannot %s", e.Seat, e.Action)
	}
	if e.Seq != s.Seq+1 {
		return nil, fmt.Errorf("entry is seq %d, expected %d", e.Seq, s.Seq+1)
	}
	if e.Hand != s.Hand {
		return nil, fmt.Errorf("entry belongs to hand %d, not %d", e.Hand, s.Hand)
	}
	if e.Street != s.Street {
		return nil, fmt.Errorf("entry is on the %s, but the hand is on the %s", e.Street, s.Street)
	}
	if int(e.Seat) != s.ToAct {
		return nil, fmt.Errorf("it is seat %d's turn, not seat %d's", s.ToAct, e.Seat)
	}
	if int(e.Seat) >= len(s.Seats) {
		return nil, fmt.Errorf("seat %d is not at this table", e.Seat)
	}

	out := s.Clone()
	seat := &out.Seats[e.Seat]
	toCall := out.Bet - seat.Committed

	switch e.Action {
	case gamelog.ActionFold:
		if e.Amount != 0 {
			return nil, fmt.Errorf("a fold carries no amount, but this one carries %d", e.Amount)
		}
		seat.Folded = true

	case gamelog.ActionCheck:
		if e.Amount != 0 {
			return nil, fmt.Errorf("a check carries no amount, but this one carries %d", e.Amount)
		}
		if toCall != 0 {
			return nil, fmt.Errorf("seat %d cannot check with %d to call", e.Seat, toCall)
		}

	case gamelog.ActionCall:
		// A call's size is forced, so it is derived rather than read from
		// the entry. Nothing about a call is the caller's to choose, and a
		// number that cannot be chosen cannot be logged wrongly.
		if e.Amount != 0 {
			return nil, fmt.Errorf("a call's size is forced; this one claims %d", e.Amount)
		}
		if toCall <= 0 {
			return nil, fmt.Errorf("seat %d has nothing to call", e.Seat)
		}
		post(seat, toCall)

	case gamelog.ActionBet, gamelog.ActionRaise, gamelog.ActionAllIn:
		if err := aggress(out, seat, e); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("%q is not an action", e.Action)
	}

	seat.Acted = true
	out.Seq = e.Seq
	out.advance()
	return out, nil
}

// aggress applies a bet, a raise or an all-in.
//
// One function because they differ only in what is legal, not in what happens:
// all three set the amount this seat has committed on this street, and the
// entry's Amount is that total rather than the difference. Absolute, because a
// difference has to be read against a state the reader may not share, and the
// whole point is that two peers can check the number without agreeing first.
func aggress(s *State, seat *Seat, e *gamelog.Entry) error {
	if e.Amount <= seat.Committed {
		return fmt.Errorf("seat %d's %s to %d does not raise its own %d",
			e.Seat, e.Action, e.Amount, seat.Committed)
	}
	need := e.Amount - seat.Committed
	if need > seat.Stack {
		return fmt.Errorf("seat %d cannot put in %d with %d behind", e.Seat, need, seat.Stack)
	}
	allIn := need == seat.Stack

	switch e.Action {
	case gamelog.ActionBet:
		if s.Bet != 0 {
			return fmt.Errorf("seat %d cannot bet into a bet of %d; that is a raise", e.Seat, s.Bet)
		}
		if e.Amount < s.MinRaise && !allIn {
			return fmt.Errorf("a bet of %d is under the %d minimum", e.Amount, s.MinRaise)
		}
	case gamelog.ActionRaise:
		if s.Bet == 0 {
			return fmt.Errorf("seat %d cannot raise with no bet to raise; that is a bet", e.Seat)
		}
		if e.Amount < s.Bet+s.MinRaise && !allIn {
			return fmt.Errorf("a raise to %d is under the %d minimum", e.Amount, s.Bet+s.MinRaise)
		}
	case gamelog.ActionAllIn:
		if !allIn {
			return fmt.Errorf("seat %d called it an all-in with %d still behind",
				e.Seat, seat.Stack-need)
		}
	}

	// A raise that clears the bar reopens the betting; a short all-in does
	// not, so seats that have already acted do not act again.
	full := e.Amount >= s.Bet+s.MinRaise
	if full {
		s.MinRaise = e.Amount - s.Bet
	}
	post(seat, need)
	if e.Amount > s.Bet {
		s.Bet = e.Amount
	}
	if full {
		for i := range s.Seats {
			if i != int(e.Seat) {
				s.Seats[i].Acted = false
			}
		}
	}
	return nil
}

// advance moves the turn on, ending the street or the hand when it should.
func (s *State) advance() {
	// One player left with a claim: the hand is over whatever the streets say.
	if len(s.contesting()) <= 1 {
		s.collect()
		s.ToAct = -1
		s.Done = true
		return
	}

	if !s.streetComplete() {
		s.ToAct = s.next(s.ToAct)
		if s.ToAct >= 0 {
			return
		}
	}

	// The street is over. Chips come off the seats and into the pot, and the
	// next street starts from the seat left of the button.
	s.collect()
	if s.Street == gamelog.StreetRiver {
		s.ToAct = -1
		s.Done = true
		return
	}
	s.Street++
	s.Bet = 0
	blinds, err := s.Table.Schedule.At(s.Hand)
	if err == nil {
		s.MinRaise = blinds.Big
	}
	for i := range s.Seats {
		s.Seats[i].Acted = false
	}

	// With fewer than two seats able to act, the rest of the hand is a
	// runout and no further entries are expected.
	if len(s.live()) < 2 {
		s.ToAct = -1
		s.Done = true
		return
	}
	s.ToAct = s.next(s.Button)
}

// streetComplete reports whether every seat that can act has acted and matched.
func (s *State) streetComplete() bool {
	for _, i := range s.live() {
		if !s.Seats[i].Acted || s.Seats[i].Committed != s.Bet {
			return false
		}
	}
	return true
}

// collect sweeps the street's commitments into the pot, returning an uncalled
// bet first.
//
// The refund is what stops a player who bet more than anyone could match from
// losing the excess. It is computed from the sorted seats rather than from a
// map, so that it is deterministic by construction rather than by the accident
// of two loops happening to reach the same answer in any order.
func (s *State) collect() {
	hi, second := int64(0), int64(0)
	hiSeat := -1
	for i := range s.Seats {
		c := s.Seats[i].Committed
		switch {
		case c > hi:
			second = hi
			hi = c
			hiSeat = i
		case c > second:
			second = c
		}
	}
	if hiSeat >= 0 && hi > second {
		back := hi - second
		s.Seats[hiSeat].Committed -= back
		s.Seats[hiSeat].Total -= back
		s.Seats[hiSeat].Stack += back
		// Getting chips back means the seat is no longer all-in.
		if s.Seats[hiSeat].Stack > 0 {
			s.Seats[hiSeat].AllIn = false
		}
	}
	for i := range s.Seats {
		s.Pot += s.Seats[i].Committed
		s.Seats[i].Committed = 0
	}
}

// Replay folds a whole log into a final state.
//
// This is the operation a peer runs to check what a table told it. It takes the
// entries as given and asks only whether they are a legal hand - verifying the
// signatures and the chaining is pkg/gamelog's job, and doing it twice here
// would just be a second place to get it wrong.
func Replay(t *Table, hand uint64, button int, stacks []int64, entries []gamelog.Entry) (*State, error) {
	s, err := StartHand(t, hand, button, stacks)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		s, err = Apply(s, &entries[i])
		if err != nil {
			return nil, fmt.Errorf("entry %d (seq %d): %w", i, entries[i].Seq, err)
		}
	}
	return s, nil
}

// Committed reports what each seat has put in across the hand, which is what a
// settlement is computed from.
func (s *State) Committed() []int64 {
	out := make([]int64, len(s.Seats))
	for i := range s.Seats {
		out[i] = s.Seats[i].Total
	}
	return out
}

// Winner reports the only seat left when everyone else has folded.
//
// Returns -1 when the hand reached a showdown, which this package cannot
// decide: that needs the cards, and the cards are pkg/deck's business.
func (s *State) Winner() int {
	c := s.contesting()
	if len(c) == 1 {
		return c[0]
	}
	return -1
}
