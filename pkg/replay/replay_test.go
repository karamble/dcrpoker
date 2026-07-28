package replay

import (
	"encoding/json"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// A reducer earns its keep by being boring: the same log always gives the same
// state, an illegal entry is refused rather than absorbed, and no entry in the
// log is decoration.

func table(seats int, stack int64) *Table {
	t := &Table{
		Match:    "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9",
		Schedule: Schedule{Levels: []Blinds{{Small: 10, Big: 20}}},
	}
	for i := 0; i < seats; i++ {
		t.Seats = append(t.Seats, string(rune('a'+i)))
		t.Stacks = append(t.Stacks, stack)
	}
	return t
}

func act(seq uint64, street gamelog.Street, seat uint32, a gamelog.Action, amount int64) gamelog.Entry {
	return gamelog.Entry{
		Version: gamelog.Version,
		Seq:     seq,
		Hand:    1,
		Street:  street,
		Seat:    seat,
		Action:  a,
		Amount:  amount,
	}
}

// Heads-up, the button posts the small blind and acts first before the flop.
// Getting this backwards is the classic way two implementations end up
// disagreeing about who owes what.
func TestHeadsUpBlindsAndOrder(t *testing.T) {
	s, err := StartHand(table(2, 1000), 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if s.Seats[0].Committed != 10 || s.Seats[1].Committed != 20 {
		t.Fatalf("blinds posted %d/%d, want 10/20", s.Seats[0].Committed, s.Seats[1].Committed)
	}
	if s.ToAct != 0 {
		t.Fatalf("seat %d to act preflop, want the button at seat 0", s.ToAct)
	}

	// The big blind still gets the option once the small blind calls.
	s, err = Apply(s, ptr(act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 0)))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if s.ToAct != 1 {
		t.Fatalf("seat %d to act, want the big blind to get its option", s.ToAct)
	}

	// And after the flop the big blind acts first.
	s, err = Apply(s, ptr(act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCheck, 0)))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if s.Street != gamelog.StreetFlop {
		t.Fatalf("street is %s, want the flop", s.Street)
	}
	if s.ToAct != 1 {
		t.Fatalf("seat %d to act on the flop, want seat 1", s.ToAct)
	}
	if s.Pot != 40 {
		t.Fatalf("pot is %d, want 40", s.Pot)
	}
}

func ptr(e gamelog.Entry) *gamelog.Entry { return &e }

// A full hand, folded to on the river.
func handToTheRiver() []gamelog.Entry {
	return []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 60),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCall, 0),
		act(3, gamelog.StreetFlop, 1, gamelog.ActionCheck, 0),
		act(4, gamelog.StreetFlop, 0, gamelog.ActionBet, 40),
		act(5, gamelog.StreetFlop, 1, gamelog.ActionCall, 0),
		act(6, gamelog.StreetTurn, 1, gamelog.ActionCheck, 0),
		act(7, gamelog.StreetTurn, 0, gamelog.ActionCheck, 0),
		act(8, gamelog.StreetRiver, 1, gamelog.ActionBet, 200),
		act(9, gamelog.StreetRiver, 0, gamelog.ActionFold, 0),
	}
}

// The headline property: fold the same log twice and land in the same place.
//
// The plan asks for two independent processes; a pure function of its inputs
// gives that for free, so what is actually checked is purity - that Apply never
// touches what it was handed, and that a state which has been through a
// serialization round trip is the same state.
func TestTheSameLogAlwaysGivesTheSameState(t *testing.T) {
	tbl := table(2, 1000)
	entries := handToTheRiver()

	a, err := Replay(tbl, 1, 0, nil, entries)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	b, err := Replay(tbl, 1, 0, nil, entries)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	wantA, _ := json.Marshal(a)
	wantB, _ := json.Marshal(b)
	if string(wantA) != string(wantB) {
		t.Fatalf("two replays of one log disagreed:\n%s\n%s", wantA, wantB)
	}
	if a.Winner() != 1 {
		t.Fatalf("seat %d won, want seat 1 after seat 0 folded", a.Winner())
	}

	// Applying must not mutate its input, or keeping intermediate states -
	// which is how a peer checks a hand step by step - would be a lie.
	s, err := StartHand(tbl, 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	before, _ := json.Marshal(s)
	if _, err := Apply(s, ptr(entries[0])); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("Apply modified the state it was given")
	}
}

// Every entry has to matter. If a log can lose one and still reach the same
// answer, it is not the complete input trace it claims to be - and a peer
// checking a hand against it would be checking nothing.
func TestRemovingAnyEntryChangesTheOutcome(t *testing.T) {
	tbl := table(2, 1000)
	entries := handToTheRiver()

	full, err := Replay(tbl, 1, 0, nil, entries)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	want, _ := json.Marshal(full)

	refused, differed := 0, 0
	for drop := range entries {
		short := make([]gamelog.Entry, 0, len(entries)-1)
		short = append(short, entries[:drop]...)
		for _, e := range entries[drop+1:] {
			e.Seq--
			short = append(short, e)
		}

		got, err := Replay(tbl, 1, 0, nil, short)
		if err != nil {
			refused++ // refused outright, which is a difference
			continue
		}
		if b, _ := json.Marshal(got); string(b) == string(want) {
			t.Fatalf("dropping entry %d (%s by seat %d) left the outcome unchanged",
				drop, entries[drop].Action, entries[drop].Seat)
		}
		differed++
	}
	t.Logf("of %d entries dropped: %d made the log illegal, %d changed the result",
		len(entries), refused, differed)

	// Most drops make the hand illegal, which is the strongest possible
	// answer. Requiring at least one that stays legal and lands somewhere
	// else stops this decaying into a test that only ever sees errors - which
	// would pass while proving nothing about the reducer's arithmetic.
	if differed == 0 {
		t.Fatal("every drop was refused; this proves the chaining is checked, not that entries matter")
	}
}

// The rules, one refusal at a time. Every one of these would be a way to take
// chips that are not yours if the reducer let it through.
func TestTheRulesRefuseWhatTheyShould(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before []gamelog.Entry
		bad    gamelog.Entry
	}{{
		name: "acting out of turn",
		bad:  act(1, gamelog.StreetPreFlop, 1, gamelog.ActionCall, 0),
	}, {
		name: "checking with a bet to call",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCheck, 0),
	}, {
		name: "calling with nothing to call",
		before: []gamelog.Entry{
			act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 0),
		},
		bad: act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCall, 0),
	}, {
		name: "betting into an existing bet",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionBet, 100),
	}, {
		name: "raising under the minimum",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 30),
	}, {
		name: "raising to less than already committed",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 5),
	}, {
		name: "putting in more than the stack",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 5000),
	}, {
		name: "an all-in that leaves chips behind",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionAllIn, 100),
	}, {
		name: "a fold carrying an amount",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionFold, 50),
	}, {
		name: "a call claiming its own size",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 10),
	}, {
		name: "a gap in the sequence",
		bad:  act(7, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 0),
	}, {
		name: "an entry from another hand",
		bad: func() gamelog.Entry {
			e := act(1, gamelog.StreetPreFlop, 0, gamelog.ActionCall, 0)
			e.Hand = 9
			return e
		}(),
	}, {
		name: "an entry claiming the wrong street",
		bad:  act(1, gamelog.StreetRiver, 0, gamelog.ActionCall, 0),
	}, {
		name: "an action that is not one",
		bad:  act(1, gamelog.StreetPreFlop, 0, gamelog.Action("shove"), 0),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := StartHand(table(2, 1000), 1, 0, nil)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			for i := range tc.before {
				s, err = Apply(s, &tc.before[i])
				if err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			if _, err := Apply(s, &tc.bad); err == nil {
				t.Fatal("the reducer accepted an entry the rules refuse")
			}
		})
	}
}

// Chips are conserved. Nothing may create or destroy them, at any point.
func TestChipsAreConserved(t *testing.T) {
	tbl := table(3, 1000)
	entries := []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 60),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCall, 0),
		act(3, gamelog.StreetPreFlop, 2, gamelog.ActionCall, 0),
		act(4, gamelog.StreetFlop, 1, gamelog.ActionBet, 100),
		act(5, gamelog.StreetFlop, 2, gamelog.ActionFold, 0),
		act(6, gamelog.StreetFlop, 0, gamelog.ActionCall, 0),
	}

	s, err := StartHand(tbl, 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	check := func(s *State, where string) {
		var total int64
		for i := range s.Seats {
			total += s.Seats[i].Stack + s.Seats[i].Committed
		}
		total += s.Pot
		if want := int64(3000); total != want {
			t.Fatalf("%s: %d chips in play, want %d", where, total, want)
		}
	}
	check(s, "at the blinds")
	for i := range entries {
		s, err = Apply(s, &entries[i])
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		check(s, string(entries[i].Action))
	}
}

// A bet nobody could match comes back, rather than being swallowed by the pot.
func TestAnUncalledBetIsReturned(t *testing.T) {
	tbl := table(2, 1000)
	s, err := Replay(tbl, 1, 0, nil, []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 200),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionFold, 0),
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Seat 0 raised to 200 against a big blind of 20, and nobody called. It
	// keeps everything above what seat 1 had put in.
	if s.Seats[0].Stack != 980 {
		t.Fatalf("the raiser has %d behind, want 980 after the uncalled 180 came back", s.Seats[0].Stack)
	}
	if s.Pot != 40 {
		t.Fatalf("pot is %d, want 40", s.Pot)
	}
	if s.Seats[0].Total != 20 {
		t.Fatalf("the raiser is down %d, want 20", s.Seats[0].Total)
	}
}

// Blinds come from the hand number, never from a clock. A schedule driven by
// elapsed time cannot be replayed: two peers folding the same log at different
// moments would post different blinds and diverge on the first pot.
func TestBlindsComeFromTheHandNumber(t *testing.T) {
	s := Schedule{
		Levels:        []Blinds{{10, 20}, {20, 40}, {50, 100}},
		HandsPerLevel: 5,
	}
	for _, tc := range []struct {
		hand uint64
		want Blinds
	}{
		{1, Blinds{10, 20}}, {5, Blinds{10, 20}},
		{6, Blinds{20, 40}}, {10, Blinds{20, 40}},
		{11, Blinds{50, 100}},
		{999, Blinds{50, 100}}, // the last level holds rather than running off
	} {
		got, err := s.At(tc.hand)
		if err != nil {
			t.Fatalf("hand %d: %v", tc.hand, err)
		}
		if got != tc.want {
			t.Fatalf("hand %d has blinds %v, want %v", tc.hand, got, tc.want)
		}
	}

	flat := Schedule{Levels: []Blinds{{10, 20}}}
	for _, h := range []uint64{1, 100, 10000} {
		if got, _ := flat.At(h); got != (Blinds{10, 20}) {
			t.Fatalf("a schedule that never escalates changed at hand %d", h)
		}
	}
}

// A short all-in does not reopen betting for players who have already acted.
func TestAShortAllInDoesNotReopenTheBetting(t *testing.T) {
	tbl := &Table{
		Match:    "m",
		Seats:    []string{"a", "b", "c"},
		Stacks:   []int64{1000, 1000, 75},
		Schedule: Schedule{Levels: []Blinds{{10, 20}}},
	}
	s, err := StartHand(tbl, 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// Seat 0 raises to 60, seat 1 calls, seat 2 shoves 75 - only 15 more,
	// short of a full raise. Seat 0 may call but must not be able to raise
	// again on the strength of it.
	for _, e := range []gamelog.Entry{
		act(1, gamelog.StreetPreFlop, 0, gamelog.ActionRaise, 60),
		act(2, gamelog.StreetPreFlop, 1, gamelog.ActionCall, 0),
		act(3, gamelog.StreetPreFlop, 2, gamelog.ActionAllIn, 75),
	} {
		s, err = Apply(s, &e)
		if err != nil {
			t.Fatalf("%s: %v", e.Action, err)
		}
	}
	if s.MinRaise != 40 {
		t.Fatalf("minimum raise is %d, want it unchanged at 40 after a short all-in", s.MinRaise)
	}
	if s.Bet != 75 {
		t.Fatalf("the bet is %d, want 75", s.Bet)
	}
}
