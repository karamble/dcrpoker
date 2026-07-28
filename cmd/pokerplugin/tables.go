package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/vctt94/pokerbisonrelay/pkg/client"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// table is one table this process is at.
type table struct {
	terms membership.Terms
	// gcID is the group chat the invitation arrived in. It decides only
	// where frames go and where they are accepted from; it never decides
	// who is believed about the table, which is the membership's job.
	gcID string
	form *membership.Formation

	// watch is this player's own copy of the table's history, built once
	// the membership settles - because until then there is no roster to
	// check signatures against.
	watch *client.ChainWatch

	// log is the key this seat signs entries and checkpoints with. Kept
	// from the credentials the table was joined under, because it is needed
	// long after joining and deriving it twice would risk deriving it
	// differently.
	log *forfeit.LogKey

	// payouts is where each seat wants coin sent that it did not pay for
	// itself, and claims is the proposals in flight against seats that have
	// stopped.
	payouts map[uint32]string
	claims  map[driver.Duty]*claim
	// refresh is each seat's pre-agreed answer to a claim, and settle the
	// table's payout. Both are gathered while everybody is cooperating,
	// because neither can be gathered when it is needed.
	// refresh is every pre-agreed answer, filed by the output it spends, and
	// bondedAt is where a seat's bond sits now if it has answered a claim
	// and moved it.
	refresh   map[string]*refresh
	bondedAt  map[uint32]string
	refreshed bool
	settle    *settlement
	settled   bool
	// session is the key this seat signs table business with, and netParams
	// the network its addresses belong to. Both are needed long after the
	// table was joined, and deriving them twice risks deriving them
	// differently.
	session   *secp256k1.PrivateKey
	netParams stdaddr.AddressParams
	// chain is what a finished claim is broadcast through. Held here rather
	// than reached for, so the one thing that talks to the network from a
	// table is visible in the table.
	chain broadcaster

	// play is the hand in progress and the ones after it, once the table is
	// seated and every stake is on the chain. Nil until then: dealing before
	// the money is down would be dealing for nothing.
	play *driver.Table

	bound bool

	// askedAt is the height this table last asked to be caught up at, so a
	// table short of a commit asks once a block rather than once a poll.
	askedAt int64

	// funded is the output each seat's stake sits in, once that seat said
	// so and the chain agreed. A seat missing from here has not been paid
	// for as far as this peer can tell, whoever says otherwise.
	funded map[uint32]string

	// bonded is the same for each seat's forfeitable bond. Kept apart from
	// funded because they answer different questions: a stake is what a
	// player can lose at cards, a bond is what they lose for not playing.
	bonded map[uint32]string

	// announcedAt is the height this table last said where our stake is, so
	// it is repeated once a block rather than once a poll.
	announcedAt int64
}

// outgoing is something to publish once the registry lock is released.
// Sending under the lock would let one slow request stall every other table.
type outgoing struct {
	gcID  string
	sid   string
	match string
	kind  schema.Kind
	body  any
	class wire.Class
}

// tables is every table this process is at, by session.
type tables struct {
	mu    sync.Mutex
	m     map[string]*table
	store *store

	// checkBond decides whether a join's deposit is real. It goes to the
	// chain, so it is never called while the lock is held: one slow lookup
	// must not stall every other table.
	checkBond func(ctx context.Context, j *membership.Join) error

	// chain and params are what a stake is checked against: the deposit
	// script a seat must pay is derived locally, and whether it was paid is
	// the chain's answer. Both are set once, at startup.
	chain  *transport.Bridge
	params stdaddr.AddressParams

	// signFunding signs this peer's own announcement. Injected the way
	// checkBond is, because this registry holds tables and not keys - and
	// an announcement has to be repeatable after a restart, when the one
	// signed at the time is long gone.
	signFunding func(terms membership.Terms, seat uint32, outpoint string) (*membership.Funding, error)
}

func newTables(st *store) *tables {
	return &tables{m: make(map[string]*table), store: st}
}

// termsFor reports a table's terms, if this is a session and a conversation
// frames are admitted from.
func (t *tables) termsFor(sid, gcID string) (membership.Terms, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil || tbl.gcID != gcID {
		return membership.Terms{}, false
	}
	return tbl.terms, true
}

// verifyBonds checks every join a message carries, before any of it is admitted.
//
// This is what makes a seat cost something. Without it a join is free, and a
// stranger can fill a table's seats or answer it once too often so nobody can
// form it, as many times as they like - which would make every rule above it
// decorative.
//
// It runs outside the registry lock because it goes to the chain, and it
// refuses the whole message if any join fails: rejection has to be wholesale,
// or a bondless key could ride in beside real ones.
func (t *tables) verifyBonds(ctx context.Context, terms membership.Terms, msg *schema.Message) error {
	if msg == nil || t.checkBond == nil {
		return nil
	}

	var joins []schema.Join
	switch msg.Kind {
	case schema.KindJoin:
		var body schema.Join
		if err := msg.Into(&body); err != nil {
			return err
		}
		joins = []schema.Join{body}
	case schema.KindRoster:
		var body schema.Roster
		if err := msg.Into(&body); err != nil {
			return err
		}
		joins = body.Joins
	default:
		return nil
	}

	for i, wj := range joins {
		j, err := wj.Into()
		if err != nil {
			return fmt.Errorf("join %d: %w", i, err)
		}
		// The signature and the proof of possession first: they cost
		// nothing and settle most of it, and there is no reason to ask
		// the chain about a join that does not check out on its face.
		if err := j.Verify(terms); err != nil {
			return fmt.Errorf("join %d: %w", i, err)
		}
		if err := t.checkBond(ctx, j); err != nil {
			return fmt.Errorf("join %d: %w", i, err)
		}
	}
	return nil
}

// persist writes a table's irrevocable position to disk.
//
// Failure is logged and not returned, but it is not harmless: a position this
// process cannot write down is one a restart could contradict. There is nobody
// to report it to, so the log is where it has to be visible.
func (t *tables) persist(tbl *table) {
	if t.store == nil {
		return
	}
	if err := t.store.save(tbl.terms.SID, tbl.record()); err != nil {
		log.Printf("pokerplugin: table %s: cannot record its position: %v", tbl.terms.SID, err)
	}
}

// record renders what has to survive a restart.
func (tbl *table) record() *record {
	rec := &record{Terms: schema.TermsFrom(tbl.terms), Bound: tbl.bound}
	for _, j := range tbl.form.Joins() {
		rec.Joins = append(rec.Joins, schema.JoinFrom(j))
	}
	for _, c := range tbl.form.Commits() {
		rec.Commits = append(rec.Commits, schema.CommitFrom(c))
	}
	if b := tbl.form.Beacon(); len(b) > 0 {
		rec.Beacon = hex.EncodeToString(b)
	}
	if len(tbl.funded) > 0 {
		rec.Funded = make(map[uint32]string, len(tbl.funded))
		for seat, outpoint := range tbl.funded {
			rec.Funded[seat] = outpoint
		}
	}
	if h, ok := tbl.form.RosterHash(); ok && tbl.bound {
		rec.Roster = hex.EncodeToString(h[:])
	}
	if tbl.form.State() == membership.Aborted {
		rec.Aborted, rec.Reason = true, tbl.form.Reason()
	}
	return rec
}

// authorized reports whether frames for a session should be admitted at all.
//
// This is what stands in front of the reassembly buffers, and it is the reason
// a table has to be joined explicitly. Gaming frames are invisible to the user
// by design, so admitting anyone would be a silent way to fill this process's
// memory with fragments of messages for a table that does not exist.
//
// It does not check the sender. During formation there is no membership yet -
// that is what is being decided - so who may speak cannot be known in advance;
// what bounds it is that the session had to be accepted from an invitation
// first, and that the frames arrive in one group chat.
func (t *tables) authorized(sid, _ string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.m[sid]
	return ok
}

// join accepts an invitation and starts forming its table.
func (t *tables) join(inv schema.Invite, gcID string, id *identity) ([]outgoing, error) {
	if inv.Kind != schema.InviteKindTable {
		return nil, fmt.Errorf("this is an invitation to %q, not a table", inv.Kind)
	}
	if inv.Game != schema.Game {
		return nil, fmt.Errorf("this is an invitation to %q, not %s", inv.Game, schema.Game)
	}
	terms := membership.Terms{
		Game:       inv.Game,
		GameVer:    schema.Version,
		SID:        inv.SID,
		BuyInAtoms: inv.BuyInAtoms,
		Seats:      inv.Seats,
		CSVBlocks:  inv.CSVBlocks,
		Until:      inv.Until,
	}
	if err := terms.Validate(); err != nil {
		return nil, fmt.Errorf("this invitation states no table: %w", err)
	}

	creds, err := id.credentials(terms.SID)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if existing, ok := t.m[terms.SID]; ok {
		// Accepting twice is not an error. A host may retry, and a
		// second join under different terms is a different table
		// wearing this one's session id.
		if existing.terms != terms {
			return nil, fmt.Errorf("already at a different table under this session")
		}
		return nil, nil
	}

	rec, err := t.store.load(terms.SID)
	if err != nil {
		return nil, err
	}
	if rec != nil {
		if rec.Aborted {
			// Terminal, and it has to stay terminal across a
			// restart: a commit arriving after everyone else gave
			// up would otherwise put this process back into a
			// membership nobody is bound to.
			return nil, fmt.Errorf("this session already ended: %s", rec.Reason)
		}
		if rec.Terms.Into() != terms {
			return nil, fmt.Errorf("this session was joined under different terms")
		}
	}

	form, err := membership.NewFormation(terms, creds)
	if err != nil {
		return nil, err
	}
	logKey, err := forfeit.LogKeyFrom(creds.Log, membership.MatchID(gcID, terms.SID))
	if err != nil {
		return nil, err
	}
	tbl := &table{terms: terms, gcID: gcID, form: form,
		funded: map[uint32]string{}, bonded: map[uint32]string{},
		payouts: map[uint32]string{}, claims: map[driver.Duty]*claim{},
		refresh: map[string]*refresh{}, bondedAt: map[uint32]string{},
		session: creds.Session, netParams: t.params, chain: t.chain, log: logKey}

	if rec != nil {
		if err := tbl.resume(rec); err != nil {
			return nil, err
		}
	}
	t.m[terms.SID] = tbl
	t.persist(tbl)

	// Announce ourselves. Everything else follows from other people's
	// joins arriving.
	out := tbl.publishJoin()
	if rec != nil {
		// A table read back from disk is the clearest case of having
		// missed something: anything published while this process was
		// not running was published once and is gone. Our own join will
		// not shake it loose either, because a peer that has already
		// committed has nothing new to say about it.
		out = append(out, tbl.publishResync()...)
	}
	return out, nil
}

// resume puts back the position this key already took for this session.
//
// Rebinding reproduces the same commit rather than a second, different one:
// the membership is the one that was recorded, and signing is deterministic,
// so the bytes match what was published before. That is the whole reason this
// exists - the alternative is a restart signing a contradiction with a key
// that is derived, not stored, and so is exactly the key it was before.
func (tbl *table) resume(rec *record) error {
	for i, wj := range rec.Joins {
		j, err := wj.Into()
		if err != nil {
			return fmt.Errorf("recorded join %d: %w", i, err)
		}
		if err := tbl.form.AddJoin(j); err != nil {
			return fmt.Errorf("recorded join %d: %w", i, err)
		}
	}
	if rec.Bound {
		c, err := tbl.form.Bind()
		if err != nil {
			return fmt.Errorf("cannot take back up the position this key already committed to: %w", err)
		}
		if got := hex.EncodeToString(c.Roster[:]); got != rec.Roster {
			// Refusing to publish anything is the only safe answer:
			// this key has already said something else about this
			// session.
			return fmt.Errorf("resuming would commit to %s, but this key already committed to %s", got, rec.Roster)
		}
		tbl.bound = true
	}

	// Every member's commit, not only ours. Ours is reproducible by binding
	// again; theirs arrived as messages nobody will send twice, so without
	// these a table that had already settled comes back a signature short
	// and waits for one that is never coming. Each is verified against the
	// terms on the way in, so a file that was edited is refused rather than
	// believed.
	for i, wc := range rec.Commits {
		c, err := wc.Into()
		if err != nil {
			return fmt.Errorf("recorded commit %d: %w", i, err)
		}
		if err := tbl.form.AddCommit(c); err != nil {
			return fmt.Errorf("recorded commit %d: %w", i, err)
		}
	}

	if rec.Beacon != "" {
		beacon, err := hex.DecodeString(rec.Beacon)
		if err != nil {
			return fmt.Errorf("recorded beacon: %w", err)
		}
		if err := tbl.form.SetBeacon(beacon); err != nil {
			return fmt.Errorf("recorded beacon: %w", err)
		}
	}

	for seat, outpoint := range rec.Funded {
		tbl.funded[seat] = outpoint
	}

	// A resumed table takes its own history back up rather than waiting for
	// something to advance it: joining is the only path here, and it ends by
	// publishing a join rather than by moving the state machine. Without this
	// a table that came back settled and seated would still follow nothing.
	tbl.startWatching()
	return nil
}

// leave forgets a table, which also stops admitting frames for it.
// leave gets this player up from a table.
//
// It used to delete the table from a map and say nothing, which meant the only
// way out of a table was the one that looks exactly like walking out on it - and
// walking out is what a bond claim is for. A player who simply wanted to stop
// had no move that was not also the thing they could be punished for.
//
// So a table that is dealing is told, and settles at its next boundary: the seat
// folds the hand it is in and the table ends there. One that never got as far as
// dealing is dropped as before, because there is nothing to settle and nobody
// waiting on it.
func (t *tables) leave(sid string) ([]outgoing, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl, ok := t.m[sid]
	if !ok {
		return nil, false
	}
	if tbl.play == nil || tbl.play.Over() {
		delete(t.m, sid)
		return nil, true
	}
	out, err := tbl.play.Leave()
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", sid, err)
		delete(t.m, sid)
		return nil, true
	}
	return tbl.publish(out), true
}

// tick tells every table where the chain is, which is how a deadline passes.
//
// The height comes from the host: the sandbox has no node of its own, and a
// deadline read from a local clock would be one each machine read differently.
func (t *tables) tick(height int64) []outgoing {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []outgoing
	for _, tbl := range t.m {
		if tbl.play != nil && height > 0 {
			// The entries this peer signs are stamped with where the chain
			// is, and the host is the only thing here that knows.
			tbl.play.AtHeight(uint32(height))
		}
		before, beforeJoins := tbl.form.State(), len(tbl.form.Joins())
		if tbl.deadlinePassed(height) {
			out = append(out, tbl.advance(before, beforeJoins)...)
			t.persist(tbl)
		}
		out = append(out, tbl.askAgain(height)...)
		out = append(out, tbl.proposeClaim(t.params)...)
		out = append(out, tbl.presignRefreshes()...)
		out = append(out, tbl.proposeSettlement()...)
		out = append(out, t.announceAgain(tbl, height)...)
		if tbl.fundingLapsed(height) {
			log.Printf("pokerplugin: table %s: %s", tbl.terms.SID, tbl.form.Reason())
			t.persist(tbl)
		}
	}
	return out
}

// announceAgain repeats where our stake is while the table is short of funded.
//
// A stake is announced the moment it is paid, and every other peer refuses it
// until the chain has confirmed it - which it has not, seconds after the
// payment was broadcast. Said once, that is a message guaranteed to be refused
// and never repeated: the table would sit unfunded with the money already
// spent. It is the same fault that left two tables stuck at committed tonight,
// one layer up, and it wants the same answer.
//
// Once a block, and only while somebody is still missing. A peer that already
// has our stake recorded ignores a repeat, so the cost of saying it again is
// one small message and the cost of not saying it is a table that never funds.
func (t *tables) announceAgain(tbl *table, height int64) []outgoing {
	if height <= 0 || height <= tbl.announcedAt || t.signFunding == nil {
		return nil
	}
	if tbl.form.State() != membership.Settled {
		return nil
	}
	// Deliberately not "stop once we think every seat is funded". The peer
	// that still needs to hear this is, by definition, the one whose view
	// differs from ours, and we cannot see their view - so our own count is
	// the one thing that must not decide when to stop. It cost a live table:
	// a stake was announced a second before its block reached the other
	// node, refused for having no confirmations, and never repeated, because
	// by then the payer could see both stakes and thought everybody could.
	//
	// The deadline decides instead. Past it the stake stops mattering, and
	// until then this is one small frame per block.
	if height >= int64(membership.FundingDeadline(tbl.terms)) {
		return nil
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	outpoint := tbl.funded[seat]
	if outpoint == "" {
		// We have not paid, so there is nothing of ours to repeat.
		return nil
	}

	fn, err := t.signFunding(tbl.terms, seat, outpoint)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot say again where our stake is: %v", tbl.terms.SID, err)
		return nil
	}
	tbl.announcedAt = height
	return []outgoing{tbl.frame(schema.KindFunded, schema.FundedFrom(fn), wire.ClassState)}
}

// fundingLapsed gives up on a settled table that was never fully paid for.
//
// A seat that is never funded costs everyone who did fund: the stake sits in a
// script needing every member's signature to move, so until the abort
// transaction exists here the only way out is each of them waiting out their
// own refund timelock. Ending it at a known height at least starts that clock
// rather than leaving a table that will never deal still looking alive.
//
// Which seats are funded is this peer's own answer - what it checked against
// the chain - not what anybody announced.
func (tbl *table) fundingLapsed(height int64) bool {
	if height <= 0 || tbl.form.State() != membership.Settled {
		return false
	}
	if height < int64(membership.FundingDeadline(tbl.terms)) {
		return false
	}
	if len(tbl.funded) >= int(tbl.terms.Seats) {
		return false
	}
	tbl.form.Abandon(fmt.Sprintf("only %d of %d seats were funded before the deadline",
		len(tbl.funded), tbl.terms.Seats))
	return true
}

// askAgain re-asks for a commit this table is still short of.
//
// A resync is published on a gap or on resume, and if nobody happened to be
// listening at that moment nothing sends it again - which is the same fault it
// was written to cure, one layer up. It cost two peers an hour apiece: each
// asked while the other was still starting, and neither asked twice.
//
// A table that has closed its window and bound itself is waiting on somebody
// else's commit and nothing else, so that is the state worth repeating in. Once
// a block rather than once a poll, because the poll is much faster than the
// chain and a peer already in step answers with silence anyway.
func (tbl *table) askAgain(height int64) []outgoing {
	if height <= 0 || height <= tbl.askedAt {
		return nil
	}
	switch {
	case tbl.form.State() == membership.Joining && !tbl.form.WindowClosed():
		// A join is dropped by any peer that has not accepted the
		// invitation yet: it names a table they are not at, and there is
		// nothing to hold it against. Two peers joining seconds apart
		// therefore each drop the other's, and if a join is only ever
		// published once they both sit holding one join until the
		// deadline ends a table that both of them wanted. Say it again,
		// and ask for theirs while we are at it.
		tbl.askedAt = height
		return append(tbl.publishJoin(), tbl.publishResync()...)

	case tbl.form.State() == membership.Committed && tbl.form.WindowClosed():
		tbl.askedAt = height
		return tbl.publishResync()
	}
	return nil
}

// forgetStake drops a seat's stake, once it has been spent somewhere else.
//
// It is our own stake and our own spend, so this is bookkeeping rather than a
// decision - but it has to happen, because a table still citing an output it
// has already spent would go on announcing a stake that is not there, and every
// peer would go on refusing it.
func (t *tables) forgetStake(sid string, seat uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return
	}
	delete(tbl.funded, seat)
	t.persist(tbl)
}

// resync asks every table this process is at for whatever it missed.
//
// This is what a gap in the stream costs: the host drops frames rather than
// blocking, so one game that stops draining cannot stall the others, and the
// loss clusters at reconnection. A table that ended is skipped - it has nothing
// to catch up on, and asking would only invite answers it must refuse.
func (t *tables) resync() []outgoing {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []outgoing
	for _, tbl := range t.m {
		if tbl.form.State() == membership.Aborted {
			continue
		}
		out = append(out, tbl.publishResync()...)
	}
	return out
}

// needSeating reports the tables that have agreed a membership and are waiting
// on the block their seating is drawn from.
func (t *tables) needSeating(height int64) map[string]uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := map[string]uint32{}
	for sid, tbl := range t.m {
		at := tbl.form.BeaconHeight()
		if tbl.form.State() == membership.Settled && !tbl.form.Seated() && height >= int64(at) {
			out[sid] = at
		}
	}
	return out
}

// seat draws a table's seating from the block the whole table derives.
func (t *tables) seat(sid string, beacon []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return
	}
	if err := tbl.form.SetBeacon(beacon); err != nil {
		log.Printf("pokerplugin: table %s: %v", sid, err)
		return
	}
	// The draw is the one step of forming a table that no player has any
	// say in, so it is said out loud with the block it came from. Two peers
	// at the same table must draw from the same block; if they ever do not,
	// they are seating different tables under one match id, and this line
	// is where that becomes visible instead of showing up later as a
	// disagreement about whose turn it is.
	log.Printf("pokerplugin: table %s drew its seats from block %x", sid, beacon)
	tbl.startWatching()
	// Write the draw down. It happens once, and a table that came back
	// unseated would draw again - from whatever block stands at that height
	// by then, which after a reorganisation is not the block it was dealt
	// from.
	t.persist(tbl)
}

// deliver routes one decoded message to the table it is for.
func (t *tables) deliver(ctx context.Context, d transport.Delivery) []outgoing {
	// The session has to be one we joined, and the conversation the one the
	// invitation arrived in: frames for our table turning up somewhere else
	// are not our table's, whoever sent them.
	terms, ok := t.termsFor(d.SID, d.GCID)
	if !ok {
		return nil
	}
	// Bonds before the lock, because checking one goes to the chain.
	if err := t.verifyBonds(ctx, terms, d.Msg); err != nil {
		log.Printf("pokerplugin: table %s: %v", d.SID, err)
		return nil
	}

	// A stake is checked against the chain too, and for the same reason it
	// is handled apart from the state machine rather than inside it: the
	// lookup must not happen with the registry held.
	if d.Msg.Kind == schema.KindFunded {
		return t.acceptFunding(ctx, d)
	}
	if d.Msg.Kind == schema.KindBonded {
		return t.acceptBond(ctx, d)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[d.SID]
	if tbl == nil || tbl.gcID != d.GCID {
		// It went away while the chain was being asked.
		return nil
	}

	out, err := tbl.apply(d.Msg)
	t.persist(tbl)
	if err != nil {
		// A message that does not check is exactly what the signatures
		// are for. It changes nothing and is not worth failing over.
		log.Printf("pokerplugin: table %s: %v", d.SID, err)
		return nil
	}
	return out
}

// apply folds one message into the table and reports what to publish.
func (tbl *table) apply(msg *schema.Message) ([]outgoing, error) {
	if msg == nil {
		return nil, nil
	}
	beforeState := tbl.form.State()
	beforeJoins := len(tbl.form.Joins())

	switch msg.Kind {
	case schema.KindJoin:
		var body schema.Join
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		j, err := body.Into()
		if err != nil {
			return nil, err
		}
		if err := tbl.form.AddJoin(j); err != nil {
			return nil, err
		}

	case schema.KindRoster:
		var body schema.Roster
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		if err := tbl.adoptRoster(body); err != nil {
			return nil, err
		}

	case schema.KindCommit:
		var body schema.Commit
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		c, err := body.Into()
		if err != nil {
			return nil, err
		}
		if err := tbl.form.AddCommit(c); err != nil {
			return nil, err
		}

	case schema.KindResync:
		var body schema.Resync
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		// Answering tells us nothing we did not already know, so this
		// returns rather than falling through to advance.
		return tbl.answerResync(body), nil

	case schema.KindResyncReply:
		var body schema.ResyncReply
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		if err := tbl.adoptResync(body); err != nil {
			return nil, err
		}

	case schema.KindAction:
		if tbl.watch == nil {
			// No membership yet, so no keys to check a signature
			// against. Dropping is right: accepting it unverified
			// would imply it had been verified.
			return nil, nil
		}
		if tbl.play != nil {
			// Dealing, so the hand owns the log: it appends and folds
			// the entry itself, and letting the watcher append it too
			// would refuse the second of the two as a repeat.
			return tbl.deal(msg), nil
		}
		return nil, tbl.watch.Apply(msg)

	case schema.KindCardKey, schema.KindShuffle, schema.KindShare, schema.KindCheckpoint,
		schema.KindLeaving:
		return tbl.deal(msg), nil

	case schema.KindRefresh:
		var body schema.Refresh
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		return nil, tbl.adoptRefresh(body)

	case schema.KindSettle:
		var body schema.Settle
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		return tbl.adoptSettlement(context.Background(), body), nil

	case schema.KindPayout:
		var body schema.Payout
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		return nil, tbl.adoptPayout(body)

	case schema.KindClaim:
		var body schema.Claim
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		if seat, ok := tbl.form.OurSeat(); ok && int(seat) == body.Duty.Seat {
			// Claimed against. The answer was agreed in advance and needs
			// nobody's permission now.
			tbl.answerClaim(context.Background())
			return nil, nil
		}
		if body.Sig != "" {
			// A co-signature on something already proposed.
			tbl.collectClaimSig(context.Background(), tbl.chain, body)
			return nil, nil
		}
		return tbl.acceptClaim(body, tbl.netParams), nil

	default:
		// A kind this build does not know must not take the table down.
		return nil, nil
	}

	return tbl.advance(beforeState, beforeJoins), nil
}

// answerResync sends back what the asker said it did not have.
//
// Only the difference. A table healing itself should cost one message, not the
// whole formation again from every member at once - and a peer that is already
// in step is answered with silence rather than with its own table read back to
// it.
func (tbl *table) answerResync(ask schema.Resync) []outgoing {
	held := func(keys []string) map[string]struct{} {
		out := make(map[string]struct{}, len(keys))
		for _, k := range keys {
			out[strings.ToLower(strings.TrimSpace(k))] = struct{}{}
		}
		return out
	}
	hasJoin, hasCommit := held(ask.Joins), held(ask.Commits)

	var reply schema.ResyncReply
	for _, j := range tbl.form.Joins() {
		if _, ok := hasJoin[hex.EncodeToString(j.Key)]; !ok {
			reply.Joins = append(reply.Joins, schema.JoinFrom(j))
		}
	}
	for _, c := range tbl.form.Commits() {
		if _, ok := hasCommit[hex.EncodeToString(c.Signer)]; !ok {
			reply.Commits = append(reply.Commits, schema.CommitFrom(c))
		}
	}
	if len(reply.Joins) == 0 && len(reply.Commits) == 0 {
		return nil
	}
	return []outgoing{tbl.frame(schema.KindResyncReply, reply, wire.ClassState)}
}

// adoptResync folds in what somebody sent to catch this peer up.
//
// Everything here is signed by the member it concerns and checked on the way
// in, so this trusts the sender for nothing: a peer answering with keys nobody
// joined with, or a commit it forged, has them refused exactly as it would if
// it had published them directly.
//
// Log entries in the reply are not applied here. They belong to the action log,
// which keeps its own chain and checks entries against it.
func (tbl *table) adoptResync(body schema.ResyncReply) error {
	for i, wj := range body.Joins {
		j, err := wj.Into()
		if err != nil {
			return fmt.Errorf("resync join %d: %w", i, err)
		}
		if err := tbl.form.AddJoin(j); err != nil {
			return fmt.Errorf("resync join %d: %w", i, err)
		}
	}
	for i, wc := range body.Commits {
		c, err := wc.Into()
		if err != nil {
			return fmt.Errorf("resync commit %d: %w", i, err)
		}
		if err := tbl.form.AddCommit(c); err != nil {
			return fmt.Errorf("resync commit %d: %w", i, err)
		}
	}
	return nil
}

// publishResync asks the table for what this peer is missing.
func (tbl *table) publishResync() []outgoing {
	ask := schema.Resync{}
	for _, j := range tbl.form.Joins() {
		ask.Joins = append(ask.Joins, hex.EncodeToString(j.Key))
	}
	for _, c := range tbl.form.Commits() {
		ask.Commits = append(ask.Commits, hex.EncodeToString(c.Signer))
	}
	return []outgoing{tbl.frame(schema.KindResync, ask, wire.ClassState)}
}

// adoptRoster takes the joins out of somebody's assertion.
//
// Every join is checked before any is kept. Rejection has to be wholesale, or a
// member could get a key nobody joined with admitted by burying it among real
// ones - and the assertion itself is never believed, only used as a carrier.
func (tbl *table) adoptRoster(body schema.Roster) error {
	joins := make([]*membership.Join, 0, len(body.Joins))
	for i, wj := range body.Joins {
		j, err := wj.Into()
		if err != nil {
			return fmt.Errorf("roster join %d: %w", i, err)
		}
		if err := j.Verify(tbl.terms); err != nil {
			if body.Terms != nil && body.Terms.Into() != tbl.terms {
				return fmt.Errorf("roster was computed under different terms; we read different invitations")
			}
			return fmt.Errorf("roster join %d: %w", i, err)
		}
		joins = append(joins, j)
	}

	assertion, err := body.Assertion()
	if err != nil {
		return err
	}
	if assertion != nil {
		// A signed claim about what its sender holds. Everyone saying
		// the same thing is what lets the table form before its
		// deadline, so this is the part that has to be checked rather
		// than believed - AddAssertion does that and keeps nothing if
		// any of it fails.
		return tbl.form.AddAssertion(assertion, joins)
	}
	for _, j := range joins {
		if err := tbl.form.AddJoin(j); err != nil {
			return err
		}
	}
	return nil
}

// deadlinePassed shuts admission once the chain is past the deadline the terms
// name. It reports whether that changed anything.
func (tbl *table) deadlinePassed(height int64) bool {
	if tbl.form.WindowClosed() || height <= 0 || uint32(height) <= tbl.terms.Until {
		return false
	}
	tbl.form.CloseWindow()
	return true
}

// advance reacts to whatever the last message changed.
//
// An assertion goes out only when this peer learned something. Publishing on
// every message instead looks harmless and is not: an assertion arriving with
// nothing new in it would produce one in reply, which produces one in reply,
// and six peers echo at each other until they happen to fill up. Silence when
// there is nothing to say is what makes the healing bounded.
func (tbl *table) advance(beforeState membership.State, beforeJoins int) []outgoing {
	var out []outgoing
	learned := len(tbl.form.Joins()) > beforeJoins

	switch tbl.form.State() {
	case membership.Joining:
		if learned {
			// Tell everyone what we hold. This is what heals a
			// channel that loses messages: a peer that missed
			// somebody's join learns it here, with the signature
			// that lets them check it.
			out = append(out, tbl.publishRoster()...)
		}

	case membership.Formed:
		if learned || beforeState != membership.Formed {
			out = append(out, tbl.publishRoster()...)
		}
		// Bind when everyone says they hold this same membership, or
		// when admission shuts, whichever comes first.
		//
		// The deadline is what makes "no more joins are coming" a fact,
		// and on its own it would be enough - but it would also mean
		// every table takes as long as its window to form, which for a
		// card game is a lobby nobody watches. Unanimity is the fast
		// path: it does not prove no straggler exists, only the
		// deadline does, but it does mean every member has seen exactly
		// this set. What is left is a race that resolves to no game,
		// never to two tables.
		if !tbl.bound && (tbl.form.Agreed() || tbl.form.WindowClosed()) {
			c, err := tbl.form.Bind()
			if err != nil {
				log.Printf("pokerplugin: table %s: bind: %v", tbl.terms.SID, err)
				break
			}
			tbl.bound = true
			out = append(out, tbl.frame(schema.KindCommit, schema.CommitFrom(c), wire.ClassState))
			// Binding may have completed the table on its own, if
			// everyone else's commit arrived first.
			out = append(out, tbl.advance(membership.Formed, len(tbl.form.Joins()))...)
		}

	case membership.Committed:
		if learned || beforeState != membership.Committed {
			out = append(out, tbl.publishRoster()...)
		}

	case membership.Settled:
		tbl.startWatching()
		// Seating may be the last thing this table was waiting for, if the
		// stakes were already down.
		out = append(out, tbl.startPlaying()...)

	case membership.Aborted:
		if beforeState != membership.Aborted {
			log.Printf("pokerplugin: table %s did not form: %s", tbl.terms.SID, tbl.form.Reason())
		}
	}
	return out
}

// startWatching builds this player's own copy of the table's history.
//
// Until now a player could only take somebody else's word for what a table did.
// The entries are signed by the seats that took them, so with the membership in
// hand they can be rebuilt and checked here - and a disagreement becomes
// evidence rather than a complaint.
func (tbl *table) startWatching() {
	if tbl.watch != nil {
		return
	}
	// The log roster, not the session keys. Entries are signed with per-match
	// log keys - the ones that publish themselves if their owner equivocates -
	// and checking them against the keys that hold the stakes would reject
	// every entry a peer ever sent.
	seats, ok := tbl.form.LogSeats()
	if !ok {
		// No seating yet, so no table to follow. It arrives with the
		// block the seats are drawn from.
		return
	}
	matchID, ok := tbl.form.MatchID()
	if !ok {
		return
	}
	w, err := client.NewChainWatch(matchID, seats)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot follow the history: %v", tbl.terms.SID, err)
		return
	}
	tbl.watch = w
	log.Printf("pokerplugin: table %s settled with %d seats, match %s", tbl.terms.SID, len(seats), matchID)
}

func (tbl *table) publishJoin() []outgoing {
	return []outgoing{tbl.frame(schema.KindJoin, schema.JoinFrom(tbl.form.Ours()), wire.ClassState)}
}

func (tbl *table) publishRoster() []outgoing {
	// Only a peer that has a membership can claim one: short of a full table
	// there is nothing to assert, and saying so anyway would be claiming
	// agreement with a set nobody holds.
	//
	// Seating is a later and separate thing - it waits for the block it is
	// drawn from - so an assertion must not wait on it, or no table would
	// ever agree and every one of them would have to sit out its deadline.
	var assertion *membership.Assertion
	if a, err := tbl.form.Assertion(); err == nil {
		assertion = a
	}
	seats := map[uint32][]byte{}
	if s, ok := tbl.form.Seats(); ok {
		seats = s
	}
	body := schema.RosterFrom(tbl.terms, seats, tbl.form.Joins(), assertion)
	return []outgoing{tbl.frame(schema.KindRoster, body, wire.ClassState)}
}

// frame addresses a message. Formation traffic is matched by the session,
// because a session that has not decided who is in it has no membership to be
// matched by; once it settles, the match id becomes the membership itself.
func (tbl *table) frame(kind schema.Kind, body any, class wire.Class) outgoing {
	match := tbl.terms.SID
	if id, ok := tbl.form.MatchID(); ok {
		match = id
	}
	return outgoing{
		gcID:  tbl.gcID,
		sid:   tbl.terms.SID,
		match: match,
		kind:  kind,
		body:  body,
		class: class,
	}
}

// snapshot is what the host is told about a table.
type snapshot struct {
	SID        string `json:"sid"`
	GCID       string `json:"gcid"`
	State      string `json:"state"`
	Seats      uint32 `json:"seats"`
	BuyInAtoms uint64 `json:"buyinAtoms"`
	CSVBlocks  uint32 `json:"csvBlocks"`
	Joined     int    `json:"joined"`
	MatchID    string `json:"matchId,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Waiting counts log entries that arrived before the one they chain
	// to. A number that stays above zero means entries are missing rather
	// than merely late.
	Waiting int `json:"waiting"`

	// Funded counts the seats this peer has checked against the chain
	// itself. It is deliberately not "how many players said they paid":
	// a host reading this is reading what the ledger supports.
	Funded int `json:"funded"`
	// Seat is which seat this player holds, once the seating is drawn. A
	// pointer because seat zero is a real seat and an absent one is not.
	Seat *uint32 `json:"seat,omitempty"`
	// DepositAddr is where this player's own stake has to be paid, and
	// Stake is the output holding it once it has been. Both are empty
	// until the table settles, because until then the address does not
	// exist: it is derived from the whole membership.
	DepositAddr string `json:"depositAddr,omitempty"`
	Stake       string `json:"stake,omitempty"`
	// FundingDeadline is the height after which an unfunded table is given
	// up on. Zero before there is anything to fund.
	FundingDeadline uint32 `json:"fundingDeadline,omitempty"`
}

func (t *tables) snapshots() []snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]snapshot, 0, len(t.m))
	for sid, tbl := range t.m {
		s := snapshot{
			SID:        sid,
			GCID:       tbl.gcID,
			State:      tbl.form.State().String(),
			Seats:      tbl.terms.Seats,
			BuyInAtoms: tbl.terms.BuyInAtoms,
			CSVBlocks:  tbl.terms.CSVBlocks,
			Joined:     len(tbl.form.Joins()),
			Reason:     tbl.form.Reason(),
		}
		if id, ok := tbl.form.MatchID(); ok {
			s.MatchID = id
		}
		if tbl.watch != nil {
			s.Waiting = tbl.watch.Waiting()
		}

		s.Funded = len(tbl.funded)
		if seat, ok := tbl.form.OurSeat(); ok {
			s.Seat = &seat
			s.Stake = tbl.funded[seat]
			s.FundingDeadline = membership.FundingDeadline(tbl.terms)
			// Reporting the address is what lets a host show somebody
			// where their money is going before it goes.
			if dep, err := tbl.deposit(seat, t.params); err == nil {
				s.DepositAddr = dep.DepositAddr
			}
		}
		out = append(out, s)
	}
	return out
}

// publish sends what a delivery produced. Failures are logged rather than
// returned: a frame that did not go out is a frame the others will not have,
// which the protocol already has to survive, and there is nobody to report it
// to anyway.
func (p *plugin) publish(ctx context.Context, out []outgoing) {
	for _, o := range out {
		sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := p.router.Send(sendCtx, o.gcID, o.sid, o.match, o.kind, o.body, o.class)
		cancel()
		if err != nil {
			log.Printf("pokerplugin: table %s: sending %s: %v", o.sid, o.kind, err)
		}
	}
}
