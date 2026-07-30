package driver

import (
	"fmt"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

// A table is a sequence of hands, and the seams between them are where the
// money actually settles.
//
// Three things happen at a boundary and each exists for its own reason. Fresh
// card keys, because a key is published the moment its hand ends and reusing
// one would open the next hand to anybody who watched the last. A rotated
// button, because a fixed one is a permanent positional edge. And a signed
// checkpoint, because a hand in progress was never agreed by anybody - so if
// the table stops, the last boundary everyone put their name to is the only
// place it can settle from.

// dutyStamp is one seat's outstanding obligation and the height it arose at.
type dutyStamp struct {
	duty   Duty
	height uint32
}

// TableConfig is a peer's whole seat at a table, across every hand it plays.
type TableConfig struct {
	Match string
	Seat  int
	// Log signs this peer's entries and checkpoints. One key for the whole
	// table, unlike the card keys.
	Log *forfeit.LogKey
	// Roster is the log key of every seat, which entries are checked
	// against.
	Roster gamelog.Roster
	// Stakes is the starting stack of every seat.
	Stakes []int64
	// Schedule is the blinds, by hand number.
	Schedule replay.Schedule
	// Button is which seat holds it for the first hand.
	Button int
}

// Table plays hand after hand for one peer.
type Table struct {
	cfg   TableConfig
	seats int

	chain  *gamelog.Chain
	table  *replay.Table
	hand   uint64
	button int
	stacks []int64

	// card is this peer's key for the hand being set up or played, and keys
	// is every seat's public half. A missing entry is a seat that has not
	// announced yet.
	card *deck.KeyPair
	keys []kyber.Point

	// hands is the driver for the hand in progress, nil between hands.
	hands *Driver
	// agreed collects the checkpoints for the hand just finished.
	agreed map[uint64]map[int]*gamelog.Checkpoint
	// last is the newest hand every seat signed off on, and the stacks they
	// signed. This is what a stopped table settles on.
	last      uint64
	lastStack []int64

	// produced is everything this peer has sent for the hand in progress.
	//
	// Kept so it can be said again. This channel loses messages, and a lost
	// one deadlocks a hand outright: the seat that sent it believes it has
	// done its part and the seat that never got it believes it is still
	// waiting, so both wait forever and neither has anything to log. The
	// entries are recoverable from the chain, but a shuffle and a share are
	// not in the chain and there is nothing else that holds them.
	//
	// Kept for the hand in progress and the one before it, and no further.
	//
	// One hand is not enough. The checkpoint that ends a hand is produced at
	// the end of it, and the seat that needs it is by definition the seat
	// that has not finished that hand - so by the time it asks, this peer has
	// opened the next one. Clearing on every open threw away the one message
	// most likely to be wanted.
	produced []HandOut
	prior    []HandOut

	// leaving is the seats that have said they are getting up. A seat on its
	// way out folds when its turn comes and the table dissolves at the next
	// boundary.
	leaving map[int]bool
	// height is the last block height this peer was told about, stamped onto
	// the entries it signs. Told rather than read: the sandbox has no node,
	// and a height taken from a local clock would be one each machine read
	// differently.
	height uint32
	// dutySince is what each seat owed when this peer last looked, and the
	// height it started owing it.
	dutySince map[int]dutyStamp

	// finished is the record of every settled hand, waiting to be taken and
	// written down. See record.go.
	finished []*HandRecord

	// challenges is which seats have discharged the reveal each challenged
	// hand obliges, and challengedBy is which hand each challenger has open.
	// See OpenChallenge.
	challenges   map[uint64]map[int]bool
	challengedBy map[int]uint64

	over bool
}

// NewTable seats this peer.
func NewTable(cfg TableConfig) (*Table, error) {
	switch {
	case cfg.Match == "":
		return nil, fmt.Errorf("no match")
	case cfg.Log == nil:
		return nil, fmt.Errorf("no log key")
	}
	n := len(cfg.Roster)
	if n < 2 {
		return nil, fmt.Errorf("a table has at least 2 seats, not %d", n)
	}
	if len(cfg.Stakes) != n {
		return nil, fmt.Errorf("%d seats but %d stakes", n, len(cfg.Stakes))
	}
	if cfg.Seat < 0 || cfg.Seat >= n {
		return nil, fmt.Errorf("seat %d is not at a %d seat table", cfg.Seat, n)
	}
	if cfg.Button < 0 || cfg.Button >= n {
		return nil, fmt.Errorf("seat %d cannot hold the button", cfg.Button)
	}
	chain, err := gamelog.NewChain(cfg.Match, cfg.Roster)
	if err != nil {
		return nil, err
	}
	rt := &replay.Table{Match: cfg.Match, Schedule: cfg.Schedule}
	for i := range n {
		rt.Seats = append(rt.Seats, fmt.Sprintf("seat%d", i))
		rt.Stacks = append(rt.Stacks, cfg.Stakes[i])
	}

	t := &Table{
		cfg:    cfg,
		seats:  n,
		chain:  chain,
		table:  rt,
		hand:   0,
		button: cfg.Button,
		stacks: append([]int64(nil), cfg.Stakes...),
		agreed: map[uint64]map[int]*gamelog.Checkpoint{},
		// Hand zero is the opening position: everybody starts with what
		// they staked, and nobody has to have signed anything for that to
		// be true. It is the checkpoint an abort settles on.
		last:      0,
		lastStack: append([]int64(nil), cfg.Stakes...),
		leaving:   map[int]bool{},
		dutySince: map[int]dutyStamp{},
	}
	return t, nil
}

// Hand is the hand in progress, or nil between hands.
func (t *Table) Hand() *Driver { return t.hands }

// Stacks is what every seat holds right now.
func (t *Table) Stacks() []int64 { return append([]int64(nil), t.stacks...) }

// Settled reports the newest hand every seat signed off on, and the stacks they
// agreed. A table that stops settles here and voids whatever was in progress.
func (t *Table) Settled() (uint64, []int64) {
	return t.last, append([]int64(nil), t.lastStack...)
}

// Over reports whether one seat holds everything.
func (t *Table) Over() bool { return t.over }

// Chain is the log of every hand played so far.
func (t *Table) Chain() *gamelog.Chain { return t.chain }

// OutCardKey announces this peer's deck key for one hand.
//
// Unsigned, deliberately. A seat that announced different keys to different
// players would give them different joint keys, so the first shuffle would fail
// to verify everywhere and the hand would never start - which is
// indistinguishable from walking out, and is answered the same way, by the
// claim on its bond. Adding a signature would let the same behaviour be named
// rather than merely punished, and naming it buys nothing that the bond does
// not already collect.
type OutCardKey struct {
	Seat int
	Hand uint64
	Key  kyber.Point
	// Sig is this seat saying the key is its own - see sign.go. Without it
	// a card key is a claim anybody could make, and the joint key is the
	// sum of all of them.
	Sig []byte
}

// InCardKey is another seat's deck key for a hand.
type InCardKey struct {
	Seat int
	Hand uint64
	Key  kyber.Point
	Sig  []byte
}

// OutCheckpoint is this peer's signature over the stacks at a hand boundary.
type OutCheckpoint struct{ Checkpoint *gamelog.Checkpoint }

// InCheckpoint is another seat's.
type InCheckpoint struct{ Checkpoint *gamelog.Checkpoint }

// OutLeaving says this seat is getting up.
//
// Leaving used to be indistinguishable from walking out, which is what made a
// staller's opponent unable to escape: stopping was the only exit and stopping
// is what a bond claim is for. Saying so turns it into an ordinary thing a
// player may do.
type OutLeaving struct {
	Seat int
	Hand uint64
	Sig  []byte
}

// InLeaving is another seat saying the same.
type InLeaving struct {
	Seat int
	Hand uint64
	Sig  []byte
}

func (OutLeaving) out() {}
func (InLeaving) in()   {}

func (OutCardKey) out()    {}
func (OutCheckpoint) out() {}
func (InCardKey) in()      {}
func (InCheckpoint) in()   {}

// Start opens the first hand.
// HandOut is something this peer sent, and the hand it belongs to.
//
// The hand has to travel with it. A share and a shuffle carry no hand of their
// own and are stamped with one on the way out, and a message kept from the last
// hand and stamped with this one is a message about a deck that no longer
// exists.
type HandOut struct {
	Hand uint64
	Out  Out
}

// keep records what this peer is about to send, so it can be sent again.
func (t *Table) keep(out []Out, err error) ([]Out, error) {
	if err == nil {
		for _, m := range out {
			t.produced = append(t.produced, HandOut{Hand: t.hand, Out: m})
		}
	}
	return out, err
}

// Republish returns everything this peer has sent that is still worth hearing.
//
// For a seat that owes nothing and is still waiting: from its own side it has
// done everything the hand asks of it, so if the hand is not moving then
// somebody else is missing something of ours. Owes reports the first half of
// that and this is the answer to it.
//
// The previous hand is included, but only the part of it that outlives its hand.
// A checkpoint does: a peer that missed one is stuck at a boundary and needs it
// to move at all. A card key, a shuffle and a share do not - they are answers
// about one deck, and against the next deck they are not merely useless but
// actively harmful, because a share that fails to verify used to mark its slot
// as delivered and strand the card for good. That is fixed in onShare, and this
// is the other end of it: do not send what cannot be right.
func (t *Table) Republish() []HandOut {
	out := make([]HandOut, 0, len(t.prior)+len(t.produced))
	for _, m := range t.prior {
		if outlivesItsHand(m.Out) {
			out = append(out, m)
		}
	}
	return append(out, t.produced...)
}

// outlivesItsHand reports whether a message is still worth saying once the hand
// it belonged to is over.
func outlivesItsHand(o Out) bool {
	switch o.(type) {
	case OutCheckpoint, OutAction, OutLeaving:
		// The log, and the fact somebody is getting up. Both are about the
		// table rather than about a deck, and a peer behind on either cannot
		// catch up without them.
		return true
	default:
		// OutCardKey, OutShuffle, OutShare: this hand's deck and nothing else.
		return false
	}
}

func (t *Table) Start() ([]Out, error) {
	if t.hand != 0 {
		return nil, fmt.Errorf("this table has already started")
	}
	return t.keep(t.openHand())
}

// openHand draws a fresh deck key and announces it.
//
// Fresh every hand, and that is not tidiness. A card key is published at the
// end of its hand under the reveal-and-recompute rule, and the deck it masked
// is worthless by then - but a key carried into the next hand would make that
// hand readable by anybody who kept the last one.
func (t *Table) openHand() ([]Out, error) {
	t.hand++
	t.prior, t.produced = t.produced, nil
	t.hands = nil
	t.card = deck.NewKeyPair()
	t.keys = make([]kyber.Point, t.seats)
	t.keys[t.cfg.Seat] = t.card.Public

	digest, err := cardKeyDigest(t.cfg.Match, t.hand, t.cfg.Seat, t.card.Public)
	if err != nil {
		return nil, err
	}
	sig, err := t.cfg.Log.SignCommitted(forfeit.DomainCardKey, t.hand, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign card key: %w", err)
	}
	return []Out{OutCardKey{Seat: t.cfg.Seat, Hand: t.hand, Key: t.card.Public, Sig: sig}}, nil
}

// Handle folds one message in.
//
// Table-level messages are taken here and everything else goes to the hand in
// progress, which is why a peer only ever has one thing to feed messages into.
func (t *Table) Handle(in In) ([]Out, error) { return t.keep(t.handle(in)) }

func (t *Table) handle(in In) ([]Out, error) {
	switch m := in.(type) {
	case InCardKey:
		return t.onCardKey(m)
	case InCheckpoint:
		return t.onCheckpoint(m)
	case InLeaving:
		return t.onLeaving(m)
	}
	if t.hands == nil {
		// Between hands, and this belongs to one. Almost always the tail of
		// the hand just finished arriving after this peer moved on.
		return nil, nil
	}
	out, err := t.hands.Handle(in)
	if err != nil {
		return nil, err
	}
	done, err := t.maybeFinish()
	if err != nil {
		return nil, err
	}
	out = append(out, done...)
	folded, err := t.autoFold()
	if err != nil {
		return nil, err
	}
	return append(out, folded...), nil
}

func (t *Table) onCardKey(m InCardKey) ([]Out, error) {
	if t.over {
		return nil, nil
	}
	if m.Seat < 0 || m.Seat >= t.seats {
		return nil, fmt.Errorf("seat %d is not at this table", m.Seat)
	}
	if m.Hand != t.hand {
		// A key for a hand this peer is not setting up. Either it is stale
		// or this peer is behind; neither is something to act on.
		return nil, nil
	}
	if m.Key == nil {
		return nil, fmt.Errorf("seat %d announced no key", m.Seat)
	}
	// Whose key this is, before it is recorded. The joint key is the sum of
	// every seat's, so a key accepted for the wrong seat puts a secret
	// nobody at this table holds into the one thing every card is masked to.
	if err := t.checkDealt(m.Seat, m.Sig, func() ([32]byte, error) {
		return cardKeyDigest(t.cfg.Match, m.Hand, m.Seat, m.Key)
	}); err != nil {
		return nil, err
	}
	if m.Seat == t.cfg.Seat {
		return nil, fmt.Errorf("this peer's own card key came back to it")
	}
	if t.keys[m.Seat] != nil {
		if !t.keys[m.Seat].Equal(m.Key) {
			return nil, fmt.Errorf("seat %d announced two different card keys for hand %d",
				m.Seat, m.Hand)
		}
		return nil, nil
	}
	t.keys[m.Seat] = m.Key

	for _, k := range t.keys {
		if k == nil {
			return nil, nil
		}
	}
	return t.deal()
}

// deal starts the hand once every seat's key is in.
func (t *Table) deal() ([]Out, error) {
	d, err := New(Config{
		Match:    t.cfg.Match,
		Seat:     t.cfg.Seat,
		Hand:     t.hand,
		Button:   t.button,
		Log:      t.cfg.Log,
		Card:     t.card,
		CardKeys: t.keys,
		Chain:    t.chain,
		Table:    t.table,
		Stacks:   t.stacks,
	})
	if err != nil {
		return nil, err
	}
	t.hands = d
	return d.Start()
}

// Leave gets this player up from the table.
//
// Two rules, and they are the ones a live game already uses. Between hands it is
// free: the boundary is signed, the table settles there and every bond releases.
// Mid-hand it is a fold - the commitment already in the pot stays there, which
// is what stops leaving being a way to un-bet a hand that is going badly.
//
// The fold happens now if it is this seat's turn and when its turn comes if not.
// That is the only automatic fold anywhere in this design, and it is one the
// player asked for: nothing here folds anybody on a timer, because a timer is
// one machine's opinion and this one would be moving money.
func (t *Table) Leave() ([]Out, error) { return t.keep(t.leave()) }

func (t *Table) leave() ([]Out, error) {
	if t.over {
		return nil, nil
	}
	if t.leaving[t.cfg.Seat] {
		return nil, nil
	}
	t.leaving[t.cfg.Seat] = true

	leaveDigest := leavingDigest(t.cfg.Match, t.hand, t.cfg.Seat)
	leaveSig, err := t.cfg.Log.Sign(forfeit.DomainLeaving, t.hand, leaveDigest[:])
	if err != nil {
		return nil, fmt.Errorf("sign leaving: %w", err)
	}
	out := []Out{OutLeaving{Seat: t.cfg.Seat, Hand: t.hand, Sig: leaveSig}}
	if t.everybodyLeaving() {
		// The last seat to ask. There is nobody left to fold to and
		// nothing left to play for, so the table stops here instead of
		// waiting for a boundary it may never reach.
		t.stopIfEverybodyLeft()
		return out, nil
	}
	folded, err := t.autoFold()
	if err != nil {
		return nil, err
	}
	return append(out, folded...), nil
}

// Leaving reports whether a seat has said it is getting up.
func (t *Table) Leaving(seat int) bool { return t.leaving[seat] }

func (t *Table) onLeaving(m InLeaving) ([]Out, error) {
	if m.Seat < 0 || m.Seat >= t.seats {
		return nil, fmt.Errorf("seat %d is not at this table", m.Seat)
	}
	if m.Seat == t.cfg.Seat {
		return nil, fmt.Errorf("this peer's own leaving came back to it")
	}
	// Getting up is a seat's own decision to announce. Unsigned, anybody who
	// could reach the table could stand somebody else's seat up, and once
	// every seat is marked leaving the table settles - so a forged one ends
	// a game at a moment its author picked.
	if err := t.checkDealt(m.Seat, m.Sig, func() ([32]byte, error) {
		return leavingDigest(t.cfg.Match, m.Hand, m.Seat), nil
	}); err != nil {
		return nil, err
	}
	t.leaving[m.Seat] = true
	// Nothing else happens now. The seat folds its own hand when its turn
	// comes, and the table dissolves at the boundary - dissolving mid-hand
	// would hand whoever was losing a way out of the pot.
	//
	// Unless every seat has asked to go, which is the one case where nobody
	// can be robbed by stopping. See everybodyLeaving.
	t.stopIfEverybodyLeft()
	return nil, nil
}

// everybodyLeaving reports that no seat still wants to play.
func (t *Table) everybodyLeaving() bool { return len(t.leaving) >= t.seats }

// stopIfEverybodyLeft ends a table that every seat has asked to leave, at the
// last result they all signed.
//
// The ordinary rule is that a table ends at a hand boundary, because voiding a
// hand in progress hands whoever is losing it a way out of the pot. That rule
// has a hole in it, and a live table fell straight through: a hand that cannot
// complete never reaches a boundary, so the table never ends, so it can never be
// paid out - and every seat asking to leave did not help, because leaving only
// set a flag and waited for the boundary that was never coming. The coin was
// then reachable only by each player waiting out their own refund timelock,
// which is the outcome this whole design exists to avoid.
//
// Unanimity is what makes stopping safe rather than exploitable. A player who is
// losing the hand in progress cannot void it by leaving, because leaving alone
// changes nothing; it takes everybody, and a seat that would rather have the pot
// than the boundary simply does not agree. The seat facing a peer that has
// genuinely stopped is not forced to agree either - it can claim the bond
// instead, which is what the bond is for. This only adds the option of both
// sides calling it a day.
//
// What it settles at is what Settled() already reported all along: the newest
// result every seat put their name to. Nothing in progress is invented or
// guessed at - it is voided, exactly as the interface has been saying it would
// be.
func (t *Table) stopIfEverybodyLeft() {
	if t.over || !t.everybodyLeaving() {
		return
	}
	t.over = true
	t.hands = nil
}

// autoFold folds this seat's hand if it is leaving and it is its turn.
func (t *Table) autoFold() ([]Out, error) {
	if !t.leaving[t.cfg.Seat] || t.hands == nil {
		return nil, nil
	}
	if t.hands.Phase() != PhaseBetting || t.hands.State().ToAct != t.cfg.Seat {
		return nil, nil
	}
	return t.Act(gamelog.ActionFold, 0)
}

// AtHeight tells this table where the chain is.
//
// Stamped onto the entries this peer signs from here on. It never goes
// backwards, because a height that regressed would produce entries the chain
// refuses - and being told an older height is a host problem, not a reason to
// sign something nobody will accept.
func (t *Table) AtHeight(height uint32) {
	if height > t.height {
		t.height = height
	}
	t.noteDuties()
}

// Act takes this peer's decision in the hand in progress.
func (t *Table) Act(action gamelog.Action, amount int64) ([]Out, error) {
	return t.keep(t.act(action, amount))
}

func (t *Table) act(action gamelog.Action, amount int64) ([]Out, error) {
	if t.hands == nil {
		return nil, fmt.Errorf("no hand is in progress")
	}
	out, err := t.hands.Act(action, amount, t.height)
	if err != nil {
		return nil, err
	}
	done, err := t.maybeFinish()
	if err != nil {
		return nil, err
	}
	return append(out, done...), nil
}

// maybeFinish settles a finished hand and signs the boundary.
func (t *Table) maybeFinish() ([]Out, error) {
	if t.hands == nil {
		return nil, nil
	}
	p := t.hands.Phase()
	if p != PhaseShowdown && p != PhaseDone {
		return nil, nil
	}
	// Once only. This runs after every message, and a finished hand keeps
	// receiving them - the tail of its own showdown, a share that took the
	// long way round. Signing the boundary again each time would put a fresh
	// checkpoint on the wire for every straggler, and each of those is itself
	// a message that arrives somewhere and triggers another.
	if t.agreed[t.hand][t.cfg.Seat] != nil {
		return nil, nil
	}
	awards, err := t.hands.Settle()
	if err != nil {
		// A showdown whose cards have not all arrived is not an error yet,
		// only unfinished. It settles when they do.
		return nil, nil
	}

	// What is left behind plus what was won. The reducer already took the
	// commitments off the stacks, so this adds back only the winnings.
	next := make([]int64, t.seats)
	st := t.hands.State()
	for i := range next {
		next[i] = st.Seats[i].Stack
	}
	for _, a := range awards {
		if a.Seat < 0 || a.Seat >= t.seats {
			return nil, fmt.Errorf("a pot was awarded to seat %d", a.Seat)
		}
		next[a.Seat] += a.Atoms
	}
	// Nothing may be created or destroyed between hands.
	var before, after int64
	for i := range t.stacks {
		before += t.stacks[i]
		after += next[i]
	}
	if before != after {
		return nil, fmt.Errorf("hand %d turned %d chips into %d", t.hand, before, after)
	}
	t.stacks = next

	cp, err := t.chain.Checkpoint(uint32(t.cfg.Seat), t.hand, next, t.cfg.Log)
	if err != nil {
		return nil, err
	}
	t.record(cp)
	// Harvested before the checkpoint leaves, never after: the signature is
	// what makes this hand challengeable, and the record is what answers.
	t.finished = append(t.finished, t.hands.takeRecord())

	// Ours may be the one that completes the set, and then this is the
	// boundary. Nothing else will notice: the only other place that advances
	// is a checkpoint arriving from somebody else, so a peer whose own
	// signature was the last one needed used to sign it, file it, and sit
	// there - hand finished, owing nothing, waiting for a message that had
	// already arrived.
	//
	// Which peer that is comes down to whether the other seat's checkpoint
	// landed before or after this one finished the hand. Both orders are
	// ordinary, so the table stopped about half the time.
	out := []Out{OutCheckpoint{Checkpoint: cp}}
	adv, err := t.maybeAdvance(t.hand)
	if err != nil {
		return nil, err
	}
	return append(out, adv...), nil
}

func (t *Table) onCheckpoint(m InCheckpoint) ([]Out, error) {
	cp := m.Checkpoint
	if cp == nil {
		return nil, fmt.Errorf("no checkpoint")
	}
	if int(cp.Seat) < 0 || int(cp.Seat) >= t.seats {
		return nil, fmt.Errorf("seat %d is not at this table", cp.Seat)
	}
	want, ok := t.cfg.Roster[cp.Seat]
	if !ok || string(want) != string(cp.Signer) {
		return nil, fmt.Errorf("checkpoint for seat %d is signed by another key", cp.Seat)
	}
	if err := cp.Verify(); err != nil {
		return nil, err
	}
	// Two different sets of stacks for one hand is equivocation, and it
	// publishes the key that signed both - see gamelog.RecoverKeyFromCheckpoints.
	if held := t.agreed[cp.Hand][int(cp.Seat)]; held != nil {
		if held.Digest() != cp.Digest() {
			return nil, fmt.Errorf("seat %d signed two different checkpoints for hand %d",
				cp.Seat, cp.Hand)
		}
		return nil, nil
	}
	t.record(cp)
	return t.maybeAdvance(cp.Hand)
}

func (t *Table) record(cp *gamelog.Checkpoint) {
	if t.agreed[cp.Hand] == nil {
		t.agreed[cp.Hand] = map[int]*gamelog.Checkpoint{}
	}
	t.agreed[cp.Hand][int(cp.Seat)] = cp
}

// maybeAdvance moves to the next hand once every seat has signed the boundary.
func (t *Table) maybeAdvance(hand uint64) ([]Out, error) {
	if hand != t.hand || t.over {
		return nil, nil
	}
	set := t.agreed[hand]
	if len(set) != t.seats {
		return nil, nil
	}
	// Everyone has to have signed the same stacks, or this is not a
	// boundary at all.
	mine := set[t.cfg.Seat]
	if mine == nil {
		return nil, nil
	}
	for seat, cp := range set {
		if len(cp.Stacks) != t.seats {
			return nil, fmt.Errorf("seat %d signed %d stacks", seat, len(cp.Stacks))
		}
		for i := range cp.Stacks {
			if cp.Stacks[i] != mine.Stacks[i] {
				return nil, fmt.Errorf("seat %d and this peer disagree about hand %d's result",
					seat, hand)
			}
		}
	}
	t.last = hand
	t.lastStack = append([]int64(nil), mine.Stacks...)

	// One seat holding everything ends the table.
	live := 0
	for _, s := range t.stacks {
		if s > 0 {
			live++
		}
	}
	if live <= 1 || len(t.leaving) > 0 {
		// Somebody is getting up, so the table ends here rather than
		// dealing on short-handed. Continuing without them would mean
		// re-forming the escrow, the bond and the roster, all of which
		// name the full membership - a new table with carried-over
		// stacks, not this one with a gap in it.
		t.over = true
		t.hands = nil
		return nil, nil
	}

	t.button = t.nextButton()
	return t.openHand()
}

// nextButton moves the button to the next seat that still has chips.
//
// Skipping busted seats rather than passing the button to an empty chair, which
// would give the seats beside it two big blinds in a row.
func (t *Table) nextButton() int {
	for i := 1; i <= t.seats; i++ {
		seat := (t.button + i) % t.seats
		if t.stacks[seat] > 0 {
			return seat
		}
	}
	return t.button
}
