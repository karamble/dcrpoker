package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

// Taking a turn.
//
// This is the only place a person's decision enters the protocol. Everything
// else a table does is a consequence of the rules or of somebody else's
// message; this is the one input that comes from outside.
//
// It is deliberately thin. The rules are checked inside the driver, before
// anything is signed, so a refusal here means nothing was put on the wire and
// the log was not touched - and the same check runs on arrival at every other
// seat, so a move that is accepted here is accepted everywhere.

// handleAct takes this player's decision at a table.
func (p *plugin) handleAct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SID    string `json:"sid"`
		Action string `json:"action"`
		// Amount is the seat's total commitment for this street after the
		// action, not the difference. Absolute, because a difference has to
		// be read against a state the reader may not share - and the whole
		// point is that every peer can check the number without agreeing
		// with this one first. Ignored for fold, check and call, whose size
		// is not the player's to choose.
		Amount int64 `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sid := strings.ToLower(strings.TrimSpace(req.SID))
	action := strings.ToLower(strings.TrimSpace(req.Action))

	out, err := p.tables.Act(sid, action, req.Amount)
	if err != nil {
		// Every refusal here is the rules saying no, which is a caller
		// problem rather than a server one: it is not this seat's turn, or
		// the move is not legal in this spot.
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p.publish(r.Context(), out)

	view, err := p.tables.HandView(sid)
	if err != nil {
		// The action went out; only the summary could not be built.
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	// Everybody watching hears about this now rather than within half a
	// second. It is the one place a person is waiting on their own input.
	p.notifyNow()
	writeJSON(w, view)
}

// handleHand reports what this player can see of the hand in progress.
//
// Its own cards and the board, and only what has actually opened - a card this
// peer cannot read yet is absent rather than blank, because the difference
// between "not dealt" and "not readable" is the difference between waiting and
// being stuck.
func (p *plugin) handleHand(w http.ResponseWriter, r *http.Request) {
	sid := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sid")))
	view, err := p.tables.HandView(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, view)
}

// handView is what one player can see.
type handView struct {
	SID    string `json:"sid"`
	Match  string `json:"match"`
	Seat   int    `json:"seat"`
	Hand   uint64 `json:"hand"`
	Phase  string `json:"phase"`
	Street string `json:"street"`

	// ToAct is the seat whose turn it is, or -1 when nobody is to act.
	// Ours says whether that is this player, which is the one thing a
	// caller always needs and should not have to work out.
	ToAct int  `json:"toAct"`
	Ours  bool `json:"ours"`

	// ToCall is what this seat owes to stay in, and Legal is what it may do
	// right now. Both are derived from the same state the driver checks
	// against, so an action listed here is one that will be accepted.
	ToCall   int64    `json:"toCall"`
	MinRaise int64    `json:"minRaise"`
	Legal    []string `json:"legal,omitempty"`

	Hole  []string `json:"hole,omitempty"`
	Board []string `json:"board,omitempty"`

	// Pot is what earlier streets collected, and Chairs carries what each
	// seat has put in on this one. They are separate because the reducer
	// keeps them separate, and it keeps them separate because an uncalled
	// bet has to be returnable - so chips on the current street are still
	// the seat's until the street ends.
	//
	// A caller that showed only Pot would show nothing moving for the whole
	// of a betting round, which is most of a hand.
	Pot    int64   `json:"pot"`
	Chairs []chair `json:"chairs,omitempty"`
	Stacks []int64 `json:"stacks"`

	// Shuffles is where the deck came from, seat by seat. It is the one
	// question a player has to be able to answer about a hand they cannot
	// see being dealt, and the answer is per hand: nothing carries over.
	Shuffles []shuffleView `json:"shuffles,omitempty"`

	Done    bool             `json:"done"`
	Awards  []replay.Award   `json:"awards,omitempty"`
	Settled *settledBoundary `json:"settled,omitempty"`
}

// settledBoundary is the last hand every seat signed off on, which is where a
// table that stopped would settle.
type settledBoundary struct {
	Hand   uint64  `json:"hand"`
	Stacks []int64 `json:"stacks"`
}

// chair is one seat's position in the hand being played.
//
// Distinct from the stacks a table carries, which are the ones every seat has
// signed for at the last boundary. These move within a hand and mean nothing
// until it ends; those are agreed and mean everything. Showing one where the
// other belongs is the difference between a promise and a fact, so they are
// two fields rather than one.
type chair struct {
	Seat int `json:"seat"`
	// Stack is what is still behind, and Committed what this seat has put
	// in on this street - the chips in front of them, not yet in the pot.
	Stack     int64 `json:"stack"`
	Committed int64 `json:"committed"`
	// Total is what this seat has put in across the whole hand.
	Total  int64 `json:"total"`
	Folded bool  `json:"folded,omitempty"`
	AllIn  bool  `json:"allIn,omitempty"`
}

// shuffleView is one seat's turn with the deck, and what this peer can say
// about it.
//
// Three states and not two. "We permuted it ourselves" is a stronger claim than
// "we checked their proof", and both are stronger than "the network verified
// it", which is not a thing this process can know and must never be rendered.
// Collapsing ours into verified would overstate by exactly the amount that
// matters.
type shuffleView struct {
	Seat  uint32 `json:"seat"`
	State string `json:"state"`
}

// The states a shuffle can be in, from this peer's own point of view.
const (
	shuffleOurs     = "ours"
	shuffleVerified = "verified"
	shuffleAwaited  = "awaited"
)

// shuffles reports the deck's provenance for the hand in progress.
//
// Seats shuffle in seat order and a shuffle is counted only once it has been
// verified and built on, so the count divides the table cleanly: below it is
// established, at and above it is still owed.
func shuffles(d *driver.Driver, seats int, ours uint32) []shuffleView {
	done := d.Shuffled()
	out := make([]shuffleView, 0, seats)
	for seat := range uint32(seats) {
		state := shuffleAwaited
		if int(seat) < done {
			state = shuffleVerified
			if seat == ours {
				state = shuffleOurs
			}
		}
		out = append(out, shuffleView{Seat: seat, State: state})
	}
	return out
}

// HandView reports what this player can see of a table's hand.
func (t *tables) HandView(sid string) (*handView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return nil, fmt.Errorf("not at a table under session %s", sid)
	}
	if tbl.play == nil {
		return nil, fmt.Errorf("this table is not dealing yet")
	}
	seat, _ := tbl.form.OurSeat()
	match, _ := tbl.form.MatchID()

	v := &handView{
		SID:    sid,
		Match:  match,
		Seat:   int(seat),
		Phase:  "between hands",
		ToAct:  -1,
		Stacks: tbl.play.Stacks(),
	}
	at, stacks := tbl.play.Settled()
	v.Settled = &settledBoundary{Hand: at, Stacks: stacks}

	h := tbl.play.Hand()
	if h == nil {
		v.Done = tbl.play.Over()
		return v, nil
	}
	st := h.State()
	v.Hand = st.Hand
	v.Phase = h.Phase().String()
	v.Street = st.Street.String()
	v.ToAct = st.ToAct
	v.Ours = st.ToAct == int(seat)
	v.Pot = st.Pot
	v.Done = st.Done

	if int(seat) < len(st.Seats) {
		me := st.Seats[seat]
		if owed := st.Bet - me.Committed; owed > 0 {
			v.ToCall = owed
		}
		v.MinRaise = st.MinRaise
		if v.Ours {
			v.Legal = legalActions(st, int(seat))
		}
	}
	v.Shuffles = shuffles(h, len(tbl.play.Stacks()), seat)
	for i := range st.Seats {
		s := st.Seats[i]
		v.Chairs = append(v.Chairs, chair{
			Seat: i, Stack: s.Stack, Committed: s.Committed,
			Total: s.Total, Folded: s.Folded, AllIn: s.AllIn,
		})
	}

	if hole, ok := h.Hole(); ok {
		for _, c := range hole {
			v.Hole = append(v.Hole, c.String())
		}
	}
	for _, c := range h.Board() {
		v.Board = append(v.Board, c.String())
	}
	if st.Done {
		if awards, err := tbl.play.Hand().Settle(); err == nil {
			v.Awards = awards
		}
	}
	return v, nil
}

// legalActions lists what this seat may do, derived from the same state the
// driver checks against.
//
// Advisory but not a guess: an action listed here is one Apply accepts in this
// spot. A caller that offered a fold when the seat could check would be showing
// somebody a button that throws their hand away for nothing.
func legalActions(st *replay.State, seat int) []string {
	if st.ToAct != seat || seat >= len(st.Seats) {
		return nil
	}
	me := st.Seats[seat]
	owed := st.Bet - me.Committed
	var out []string

	// Folding is always available, and always last in the list: it is the
	// one action that is never the answer to a question the player was not
	// asked.
	if owed > 0 {
		out = append(out, string(gamelog.ActionCall))
	} else {
		out = append(out, string(gamelog.ActionCheck))
	}
	// Raising needs enough behind to clear the bar; short of that the only
	// way to put more in is all-in.
	if me.Stack > owed {
		if st.Bet == 0 {
			if me.Stack >= st.MinRaise {
				out = append(out, string(gamelog.ActionBet))
			}
		} else if me.Committed+me.Stack >= st.Bet+st.MinRaise {
			out = append(out, string(gamelog.ActionRaise))
		}
		out = append(out, string(gamelog.ActionAllIn))
	}
	return append(out, string(gamelog.ActionFold))
}
