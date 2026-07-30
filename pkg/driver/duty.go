package driver

import (
	"fmt"
	"sort"
)

// What a seat owes, and why a claim has to name it.
//
// A claim used to say "this player is gone". That cannot be checked by anybody,
// and it turns out to be worse than useless: a player who stalls - answering at
// the last possible moment on every decision, never triggering anything - can
// wait for their opponent to give up, then claim the bond of the person who
// stopped playing because of them. Heads-up the claim needs only the one
// opponent's signature, so the staller is the sole co-signer of the claim
// against their own victim.
//
// The fix is not a clock. Blocks average five minutes and are Poisson, so there
// is no timer here fast enough for poker and none that would be a fact rather
// than one machine's opinion. What *is* a fact is whose turn it is: every peer
// derives it identically from a log they all converge on.
//
// So a claim names an obligation - "the log says seat 2 owes the entry at
// sequence 9" - and a peer asked to co-sign checks that against its own copy.
// A seat that owes nothing cannot be claimed against at all, which is precisely
// the case the staller needs and no longer has: while they are the one holding
// the table up, it is their turn, so the player waiting on them owes nothing.
//
// None of this is enforced by script, and it should not be. The transaction
// layer knows about signatures and timelocks; it cannot read a log and must not
// pretend to. This is a rule about what a peer agrees to sign.

// DutyKind is the sort of thing a seat can owe.
type DutyKind string

const (
	// DutyCardKey is a deck key for the hand being set up.
	DutyCardKey DutyKind = "cardkey"
	// DutyShuffle is this seat's turn to permute the deck.
	DutyShuffle DutyKind = "shuffle"
	// DutyShare is a decryption share the rest of the table is waiting on.
	DutyShare DutyKind = "share"
	// DutyAction is this seat's turn to act.
	DutyAction DutyKind = "action"
	// DutyCheckpoint is a signature over the stacks at a hand boundary.
	DutyCheckpoint DutyKind = "checkpoint"
	// DutyReveal is a challenged hand's secrets. Owed by every seat the
	// moment any seat challenges, discharged by revealing.
	DutyReveal DutyKind = "reveal"
	// DutyShuffleAnswer is a disputed shuffle's secret. Owed by the shuffler
	// of a complained-about round, discharged by publishing the secret so
	// every peer can recompute the shuffle and name whoever was wrong.
	DutyShuffleAnswer DutyKind = "shuffleanswer"
)

// Duty is one thing the log says a seat still has to do.
//
// At means whatever the Kind says it does - the shuffling round, the deck slot,
// the sequence number, and nothing at all for a card key or a checkpoint, which
// are identified by their hand.
type Duty struct {
	Seat int      `json:"seat"`
	Kind DutyKind `json:"kind"`
	Hand uint64   `json:"hand"`
	At   uint64   `json:"at"`
}

func (d Duty) String() string {
	switch d.Kind {
	case DutyCardKey:
		return fmt.Sprintf("seat %d owes a card key for hand %d", d.Seat, d.Hand)
	case DutyShuffle:
		return fmt.Sprintf("seat %d owes shuffle %d of hand %d", d.Seat, d.At, d.Hand)
	case DutyShare:
		return fmt.Sprintf("seat %d owes a share for slot %d of hand %d", d.Seat, d.At, d.Hand)
	case DutyAction:
		return fmt.Sprintf("seat %d owes the entry at sequence %d", d.Seat, d.At)
	case DutyCheckpoint:
		return fmt.Sprintf("seat %d owes a checkpoint for hand %d", d.Seat, d.Hand)
	case DutyReveal:
		return fmt.Sprintf("seat %d owes its deck secrets for challenged hand %d", d.Seat, d.Hand)
	case DutyShuffleAnswer:
		return fmt.Sprintf("seat %d owes its shuffle secret for disputed hand %d", d.Seat, d.Hand)
	}
	return fmt.Sprintf("seat %d owes %q", d.Seat, d.Kind)
}

// Owes reports what a seat is holding the table up on, if anything.
//
// One duty at a time, in the order the protocol needs them, so two peers at the
// same point name the same thing. A peer that is behind reports what it can see
// - which will be an earlier duty or none - and that is the safe direction: a
// co-signer that cannot see the obligation refuses to sign, and the claimant
// retries when it can.
//
// Reports nothing once the table is over - except a reveal, which is the one
// duty that outlives the game, because a hand is challenged after it settles
// and most often after the table ends. It also outranks everything in play:
// dutySince holds one stamp per seat and resets whenever the duty changes, so
// a reveal that ranked below the hand in progress would have its count reset
// every time the refuser's turn came round, and playing on would be a way to
// never answer.
func (t *Table) Owes(seat int) (Duty, bool) {
	if t == nil || seat < 0 || seat >= t.seats {
		return Duty{}, false
	}
	// A disputed shuffle's answer, first of everything: it is what unwedges
	// a hand that cannot otherwise move, and like a reveal it must outrank
	// the game or playing on - or the table being over - would reset its
	// count forever.
	for _, hand := range t.openComplaints() {
		if t.complaints[hand] == seat {
			return Duty{Seat: seat, Kind: DutyShuffleAnswer, Hand: hand}, true
		}
	}
	for _, hand := range t.openChallenges() {
		if !t.challenges[hand][seat] {
			return Duty{Seat: seat, Kind: DutyReveal, Hand: hand}, true
		}
	}
	if t.over {
		return Duty{}, false
	}

	// Between hands, waiting for keys. The hand is being set up, so nothing
	// later than this can be owed yet.
	if t.hands == nil {
		if t.hand > 0 && t.keys != nil && t.keys[seat] == nil {
			return Duty{Seat: seat, Kind: DutyCardKey, Hand: t.hand}, true
		}
		return Duty{}, false
	}
	d := t.hands
	hand := d.state.Hand

	// The boundary. A finished hand is waiting on signatures, and until they
	// are all in nothing else can start.
	if d.state.Done {
		if t.agreed[hand][seat] == nil {
			return Duty{Seat: seat, Kind: DutyCheckpoint, Hand: hand}, true
		}
		return Duty{}, false
	}

	// Shuffling, in turn. Only the seat whose round it is owes anything.
	if d.phase == PhaseShuffling {
		if d.round == seat {
			return Duty{Seat: seat, Kind: DutyShuffle, Hand: hand, At: uint64(d.round)}, true
		}
		return Duty{}, false
	}

	// A share the table is waiting on. Checked before the action, because a
	// seat that has not dealt cannot be expected to act - and because a hand
	// stuck on a missing share looks exactly like one stuck on a missing
	// move if you only ask about turns.
	if slot, ok := d.owesShare(seat); ok {
		return Duty{Seat: seat, Kind: DutyShare, Hand: hand, At: uint64(slot)}, true
	}

	// Whose turn it is.
	if d.phase == PhaseBetting && d.state.ToAct == seat {
		return Duty{Seat: seat, Kind: DutyAction, Hand: hand, At: d.state.Seq + 1}, true
	}
	return Duty{}, false
}

// owesShare reports the lowest slot this seat has not published a share for and
// the rest of the table is waiting on.
//
// Lowest rather than any, so that two peers naming a duty name the same one.
func (d *Driver) owesShare(seat int) (int, bool) {
	for _, slot := range d.slotsOwedBy(seat) {
		if !d.sharedBy(slot, seat) {
			return slot, true
		}
	}
	return 0, false
}

// slotsOwedBy lists the slots a seat is expected to have shared by now, in
// order.
//
// This is the publication schedule read backwards. A seat owes a share for
// every other seat's hole cards as soon as the deck is dealt, for each board
// card once its street has turned, and for its own hole cards once it is at a
// showdown it is still contesting - and for nothing else, ever. A duty derived
// any more eagerly than the schedule would have a peer claiming against
// somebody for not revealing a card early.
func (d *Driver) slotsOwedBy(seat int) []int {
	if d.phase == PhaseShuffling || seat < 0 || seat >= d.seats {
		return nil
	}
	var out []int

	// Everybody else's hole cards, from the moment dealing began.
	for other := range d.seats {
		if other == seat {
			continue
		}
		if slots, err := d.layout.Hole(other); err == nil {
			out = append(out, slots[:]...)
		}
	}
	// The board, as far as it has turned. At a showdown the whole board is
	// owed, because betting that ended early still runs out.
	if d.phase == PhaseShowdown || d.phase == PhaseDone {
		if board, err := d.layout.Board(); err == nil {
			out = append(out, board[:]...)
		}
		// And its own hand, if it is still contesting one.
		if seat < len(d.state.Seats) && !d.state.Seats[seat].Folded {
			if slots, err := d.layout.Hole(seat); err == nil {
				out = append(out, slots[:]...)
			}
		}
	} else if slots, err := d.layout.BoardAt(d.state.Street); err == nil {
		out = append(out, slots...)
	}
	return out
}

// Claimable reports a duty that has stood long enough to be worth opening a
// claim over, if there is one.
//
// The waiting is local and it has to be. Nothing records when an obligation
// arose - the log records what happened, not what did not - so each peer times
// it from when it first saw the duty itself. That is fine because it decides
// only when this peer *proposes*: every co-signer checks the duty is still
// outstanding against its own log before signing, and the accused answers on
// chain, so a peer that started counting early has proposed early and convinced
// nobody.
//
// After is in blocks. It wants to be long enough that a reconnect finishes
// inside it, because the cost of being wrong is somebody's bond and the cost of
// waiting is a few more minutes.
//
// Never reports this peer's own obligation. Knowing what it owes is what Owes is
// for; this answers the different question of whether there is a claim to
// propose, and there never is against oneself.
func (t *Table) Claimable(after uint32) (Duty, bool) {
	if t == nil {
		return Duty{}, false
	}
	if t.over && len(t.challenges) == 0 && len(t.complaints) == 0 {
		// Nothing is owed at a table that ended cleanly. An open challenge
		// or an open shuffle dispute is the exception: what either obliges
		// is owed precisely when the game no longer holds anybody here.
		return Duty{}, false
	}
	for seat := range t.seats {
		if seat == t.cfg.Seat {
			// Never against itself. A peer that owes something has an
			// answer available to it - do the thing - and proposing to
			// take its own bond instead is not one of the options.
			continue
		}
		d, ok := t.Owes(seat)
		if !ok {
			continue
		}
		since, seen := t.dutySince[seat]
		if !seen || since.duty != d {
			continue
		}
		if t.height >= since.height+after {
			return d, true
		}
	}
	return Duty{}, false
}

// A challenge, as the duty machinery sees it.
//
// The wire, the signatures and the secrets live with the plugin; what belongs
// here is only who still owes a reveal, because that is what Owes and Claimable
// answer from. Deliberately alive when t.over: the bond is what a challenge
// leans on, and the bond outlives the game.

// OpenChallenge records that a seat has challenged a settled hand.
//
// One open challenge per challenger at a time - the grief bound. A repeat of
// the same challenge is not an error; this channel repeats everything.
func (t *Table) OpenChallenge(hand uint64, by int) error {
	if by < 0 || by >= t.seats {
		return fmt.Errorf("seat %d is not at this table", by)
	}
	if hand == 0 || hand > t.last {
		return fmt.Errorf("hand %d is not settled, and only a settled hand can be challenged", hand)
	}
	if open, ok := t.challengedBy[by]; ok {
		if open == hand {
			return nil
		}
		return fmt.Errorf("seat %d already has hand %d challenged; one at a time", by, open)
	}
	if t.challenges[hand] != nil {
		// Somebody else already has this hand open. Joining costs nothing
		// and takes no second slot.
		return nil
	}
	if t.challenges == nil {
		t.challenges = map[uint64]map[int]bool{}
	}
	if t.challengedBy == nil {
		t.challengedBy = map[int]uint64{}
	}
	t.challenges[hand] = map[int]bool{}
	t.challengedBy[by] = hand
	return nil
}

// NoteRevealed marks one seat's reveal for a challenged hand as discharged.
// The last seat closes the challenge.
func (t *Table) NoteRevealed(hand uint64, seat int) {
	seats, ok := t.challenges[hand]
	if !ok || seat < 0 || seat >= t.seats {
		return
	}
	seats[seat] = true
	for s := range t.seats {
		if !seats[s] {
			return
		}
	}
	t.CloseChallenge(hand)
}

// CloseChallenge ends a challenge whatever its state - every seat revealed, or
// the refusal answered by the bond and there being nothing left to wait for.
func (t *Table) CloseChallenge(hand uint64) {
	delete(t.challenges, hand)
	for by, open := range t.challengedBy {
		if open == hand {
			delete(t.challengedBy, by)
		}
	}
}

// OpenChallenges is every challenged hand still waiting on reveals, lowest
// first, so two peers naming a duty name the same one.
func (t *Table) openChallenges() []uint64 {
	if len(t.challenges) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(t.challenges))
	for hand := range t.challenges {
		out = append(out, hand)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// OpenChallenges is the exported view of the same list.
func (t *Table) OpenChallenges() []uint64 { return t.openChallenges() }

// A shuffle dispute, as the duty machinery sees it.
//
// The evidence, the signatures and the verdict live with the plugin; what
// belongs here is only which seat owes the answer, because that is what Owes
// and Claimable answer from. Alive when t.over for the same reason a challenge
// is: the bond is what a dispute leans on, and the bond outlives the game.

// OpenComplaint records that a seat's shuffle for a hand is disputed and the
// shuffler owes its secret. A repeat is not an error; this channel repeats
// everything.
func (t *Table) OpenComplaint(hand uint64, shuffler int) error {
	if shuffler < 0 || shuffler >= t.seats {
		return fmt.Errorf("seat %d is not at this table", shuffler)
	}
	if open, ok := t.complaints[hand]; ok {
		if open == shuffler {
			return nil
		}
		return fmt.Errorf("hand %d's dispute names seat %d, not seat %d", hand, open, shuffler)
	}
	if t.complaints == nil {
		t.complaints = map[uint64]int{}
	}
	t.complaints[hand] = shuffler
	return nil
}

// CloseComplaint ends a dispute whatever resolved it - the answer arriving and
// being judged, or the refusal answered by the bond.
func (t *Table) CloseComplaint(hand uint64) { delete(t.complaints, hand) }

// openComplaints is every disputed hand still waiting on an answer, lowest
// first, so two peers naming a duty name the same one.
func (t *Table) openComplaints() []uint64 {
	if len(t.complaints) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(t.complaints))
	for hand := range t.complaints {
		out = append(out, hand)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// OpenComplaints is the exported view of the same list.
func (t *Table) OpenComplaints() []uint64 { return t.openComplaints() }

// noteDuties records what each seat owes and since when.
//
// Called as the chain moves rather than as messages arrive, so the clock this
// runs on is block height and not anything local. A duty that changes resets its
// stamp: a seat that owed a shuffle and now owes a move has done the first
// thing, and starting the count again is what stops one long hand accumulating
// into a claim against somebody who has been playing all along.
func (t *Table) noteDuties() {
	for seat := range t.seats {
		d, ok := t.Owes(seat)
		if !ok {
			delete(t.dutySince, seat)
			continue
		}
		if since, seen := t.dutySince[seat]; seen && since.duty == d {
			continue
		}
		t.dutySince[seat] = dutyStamp{duty: d, height: t.height}
	}
}

// Agrees reports whether this peer's own copy of the log says the same thing a
// claim does.
//
// What a peer runs before putting its signature to somebody else's claim. It is
// deliberately strict: anything other than an exact match is a refusal,
// including the case where this peer simply has not caught up yet. Refusing
// under uncertainty costs the claimant a retry; signing under it costs somebody
// their bond.
func (t *Table) Agrees(claimed Duty) error {
	mine, ok := t.Owes(claimed.Seat)
	if !ok {
		return fmt.Errorf("this peer's log says seat %d owes nothing", claimed.Seat)
	}
	if mine != claimed {
		return fmt.Errorf("this peer's log says %s, and the claim says %s", mine, claimed)
	}
	return nil
}
