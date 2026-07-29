package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/vctt94/pokerbisonrelay/pkg/client"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
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

	// logPriv is the key this seat signs entries and checkpoints with. Kept
	// from the credentials the table was joined under, because it is needed
	// long after joining and deriving it twice would risk deriving it
	// differently.
	//
	// The private half and not a forfeit.LogKey, because a log key carries
	// the match it may sign for and at join time this table does not have
	// one yet: the match is the roster hash, and there is no roster until
	// every seat has committed. Binding it early bound it to something else
	// - the group chat and the session - and everything downstream then
	// refused it, because the log chain and the driver both identify this
	// table by its roster. A hand could not be opened at all.
	logPriv *secp256k1.PrivateKey

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
	refresh  map[string]*refresh
	bondedAt map[uint32]string
	// releases is each seat's bond going back, once the table is over, and
	// bondValue is what the chain says each bond holds - needed to build the
	// release and known only from having looked.
	releases  map[uint32]*release
	bondValue map[uint32]int64
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

	// held is frames for a hand this peer had not started when they arrived.
	// Kept rather than dropped, because every seat starts at a different
	// moment and this channel never sends anything twice. See hold.
	held []*schema.Message

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

	// stakeWaiting and bondWaiting are announcements not accepted yet, and how
	// far off they are. Kept only so a person can be told the difference
	// between money on its way and money that was never sent; nothing here
	// decides anything. See waiting.
	stakeWaiting map[uint32]*waiting
	bondWaiting  map[uint32]*waiting

	// announcedAt is the height this table last said where our stake is, so
	// it is repeated once a block rather than once a poll.
	announcedAt int64

	// bondedAnnouncedAt is the same for the bond. Kept apart because the two
	// are paid minutes apart and each has to be repeated on its own account.
	bondedAnnouncedAt int64

	// payoutAnnouncedAt is the same for where this seat wants to be paid.
	// Kept apart from the other two for a different reason: they are repeated
	// because a payment needs confirmations, and this is repeated because the
	// first telling may have been sent before there was a seat to tell anybody
	// about. See announcePayoutAgain.
	payoutAnnouncedAt int64

	// stalledAt is how far the hand had got when this seat last said its
	// messages again, so a stall is answered once rather than every tick, and
	// stalledSaidAt is the height it said them at - because a repeat that was
	// itself lost has to be tried again, and nothing about our own state will
	// ever say that it was.
	stalledAt     string
	stalledSaidAt int64

	// finished says this player has got up from the table and it is kept
	// only because it still holds their money. See tables.drop.
	finished bool

	// archived says this table's log has been written down. Once, because
	// a table that is over stops producing entries and rewriting the same
	// bytes every half minute is work nobody asked for.
	archived bool

	// events is what this table did on the chain, kept so it can be reported
	// and not only logged.
	//
	// Every outcome here was already written to the log, which is the
	// operator's record: it is read by whoever runs the container, after the
	// fact, if they think to look. That is the wrong audience for the one
	// that matters. A player whose bond is being taken finds out from this
	// or does not find out at all, and "check the container's stdout" is not
	// an answer when the thing being lost is their money.
	//
	// Bounded, because a table that has been running all week is not a
	// reason to hold every line it ever wrote.
	events []chainEvent
}

// chainEvent is one thing this table did on the chain, and why.
type chainEvent struct {
	// At is the hand this happened during, or zero before there was one.
	At   uint64  `json:"at"`
	Kind string  `json:"kind"`
	Text string  `json:"text"`
	TxID string  `json:"txid,omitempty"`
	Seat *uint32 `json:"seat,omitempty"`
}

// The kinds of thing a table records. They are few on purpose: this is what
// happened to the money, not a trace.
const (
	// eventProposed and eventCosigned are a claim being built against
	// somebody, and eventRefused this peer declining to help.
	eventProposed = "proposed"
	eventCosigned = "cosigned"
	eventRefused  = "refused"
	// eventClaimed is a bond taken, eventAnswered one saved.
	eventClaimed  = "claimed"
	eventAnswered = "answered"
	// eventUnanswerable is the one that matters: claimed against, and
	// nothing held that would answer it. It is the moment a bond is going
	// and, until this existed, the moment nothing told the player.
	eventUnanswerable = "unanswerable"
	eventSettled      = "settled"
	eventBlocked      = "blocked"
)

// maxEvents is how much of a table's history is kept.
const maxEvents = 64

// note records an outcome, beside the log line rather than instead of it.
//
// Requires the registry lock, which every caller already holds: these are all
// reached from deliver or tick.
func (tbl *table) note(kind, text, txid string, seat *uint32) {
	var at uint64
	if tbl.play != nil {
		if h := tbl.play.Hand(); h != nil {
			at = h.State().Hand
		}
	}
	// Some of these are reached once per message or once per block, so the
	// same refusal can be reported for as long as whatever caused it lasts.
	// Saying it once is the report; saying it forty times is a log, and the
	// ring is only long enough to hold what happened if it is not filled with
	// one thing that kept happening.
	if n := len(tbl.events); n > 0 {
		if last := tbl.events[n-1]; last.Kind == kind && last.Text == text && last.TxID == txid {
			return
		}
	}
	tbl.events = append(tbl.events, chainEvent{At: at, Kind: kind, Text: text, TxID: txid, Seat: seat})
	if n := len(tbl.events); n > maxEvents {
		tbl.events = append(tbl.events[:0], tbl.events[n-maxEvents:]...)
	}
}

// seatp is a seat number as a pointer, for the events that name one.
func seatp(seat int) *uint32 {
	if seat < 0 {
		return nil
	}
	s := uint32(seat)
	return &s
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

	// signBonded is the same for this seat's forfeitable bond, and exists
	// for the same reason: a bond said once is a bond the other seats may
	// never have accepted.
	signBonded func(terms membership.Terms, seat uint32, outpoint string) (*membership.Bonded, error)

	// payoutAddr is where this player wants to be paid, asked for rather than
	// stored, because the host may rebind the gaming account between one
	// announcement and the next. Injected like the signers above: this
	// registry holds tables, not the identity that owns them.
	payoutAddr func() string

	// height is where the host last said the chain was. Kept because every
	// deadline here is a block number, and a block number reported on its
	// own is not information - "funded by 1101193" says nothing to somebody
	// who cannot see how far away that is.
	height int64
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
	rec := &record{Terms: schema.TermsFrom(tbl.terms), Bound: tbl.bound,
		Finished: tbl.finished, GCID: tbl.gcID}
	if len(tbl.bonded) > 0 {
		rec.Bonded = make(map[uint32]string, len(tbl.bonded))
		for seat, outpoint := range tbl.bonded {
			rec.Bonded[seat] = outpoint
		}
	}
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
	tbl := &table{terms: terms, gcID: gcID, form: form,
		funded: map[uint32]string{}, bonded: map[uint32]string{},
		stakeWaiting: map[uint32]*waiting{}, bondWaiting: map[uint32]*waiting{},
		releases: map[uint32]*release{}, bondValue: map[uint32]int64{},
		payouts: map[uint32]string{}, claims: map[driver.Duty]*claim{},
		refresh: map[string]*refresh{}, bondedAt: map[uint32]string{},
		session: creds.Session, netParams: t.params, chain: t.chain, logPriv: creds.Log}

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
	tbl.finished = rec.Finished
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
	for seat, outpoint := range rec.Bonded {
		tbl.bonded[seat] = outpoint
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
		t.drop(sid, tbl)
		return nil, true
	}
	out, err := tbl.play.Leave()
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", sid, err)
		t.drop(sid, tbl)
		return nil, true
	}
	return tbl.publish(out), true
}

// drop forgets a table, unless forgetting it would lose the only record of
// where this player's money is.
//
// A stake sits behind a timelock that outlives the table by hours, and the
// outpoint holding it is not written anywhere a person can see except in the
// table that put it there. Dropping the table at the moment the cards stop
// leaves coin on the chain with nothing pointing at it, recoverable only by
// somebody who thought to write the outpoint down first.
//
// So a table that still holds something is kept and marked finished instead. It
// deals nothing, admits nothing and is not played; it is a receipt.
//
// Requires the registry lock.
func (t *tables) drop(sid string, tbl *table) {
	if tbl.holdsOurs() {
		tbl.finished = true
		t.persist(tbl)
		t.archive(tbl)
		return
	}
	delete(t.m, sid)
}

// archive keeps a table's signed log beside its record.
//
// The record says where the money is; the log says how it got there, and it is
// what a dispute is argued from. It lives in memory, so a table kept as a
// receipt across a restart would otherwise come back with nothing behind it.
//
// Requires the registry lock.
func (t *tables) archive(tbl *table) {
	if t.store == nil || tbl.archived {
		return
	}
	blob, err := tbl.log()
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot render its log: %v", tbl.terms.SID, err)
		return
	}
	if blob == nil {
		return
	}
	if err := t.store.saveTranscript(tbl.terms.SID, blob); err != nil {
		log.Printf("pokerplugin: table %s: cannot keep its log: %v", tbl.terms.SID, err)
		return
	}
	tbl.archived = true
}

// holdsOurs reports whether this player still has coin at this table that
// nothing has taken back out.
func (tbl *table) holdsOurs() bool {
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return false
	}
	return tbl.funded[seat] != "" || tbl.bonded[seat] != "" || tbl.bondedAt[seat] != ""
}

// resumeHeld brings back the tables that still hold this player's money.
//
// Nothing else loads a session at startup: a table comes back when its
// invitation is accepted again, which is right for one being played and useless
// for one that is over. A finished table is a receipt, and a receipt that only
// exists while the process happens not to have restarted is not one - the coin
// is behind a timelock measured in days and the process is not.
//
// These are constructed as receipts and nothing more. They publish no join, ask
// for no resync and cannot start dealing, so bringing one back says nothing to
// anybody: it only puts the outpoints back where a person can see them.
func (t *tables) resumeHeld(id *identity) {
	if t.store == nil {
		return
	}
	sids, err := t.store.sessions()
	if err != nil {
		log.Printf("pokerplugin: cannot list past tables: %v", err)
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for _, sid := range sids {
		if _, live := t.m[sid]; live {
			continue
		}
		rec, err := t.store.load(sid)
		if err != nil || rec == nil {
			continue
		}
		if !heldByUs(rec) {
			// Nothing anybody paid into it, so there is nothing to
			// keep. An aborted table nobody funded is just history.
			continue
		}
		tbl, err := t.receipt(rec, id)
		if err != nil {
			log.Printf("pokerplugin: cannot read back table %s: %v", sid, err)
			continue
		}
		if !tbl.holdsOurs() {
			// Somebody paid into it and it was not this player. The
			// record says where their coin is, which is their
			// business and not a receipt to keep here.
			continue
		}
		t.m[sid] = tbl
		if tbl.finished {
			log.Printf("pokerplugin: table %s is finished and still holds coin", sid)
		} else {
			log.Printf("pokerplugin: table %s is still forming and holds coin; carrying on", sid)
		}
	}
}

// heldByUs reports whether a stored table still names an output of ours.
//
// Read from the record rather than from a rebuilt table, because it decides
// whether to rebuild one at all.
func heldByUs(rec *record) bool {
	return len(rec.Funded) > 0 || len(rec.Bonded) > 0
}

// everDealt reports whether a table had everything it needs to start dealing.
//
// There is no play state in a record - no deck, no round, no bets - so a table
// that had begun a hand cannot be rejoined from disk and must come back as a
// receipt. This is the marker for that, and it is exact rather than
// approximate: startPlaying deals the moment every seat is both funded and
// bonded, so a record showing both complete is a record of a table that had
// started, or was about to on the same tick.
func everDealt(rec *record) bool {
	seats := int(rec.Terms.Seats)
	return seats > 0 && len(rec.Funded) >= seats && len(rec.Bonded) >= seats
}

// receipt rebuilds a table from its record.
//
// Not always a receipt. Forming and funding a table takes as many blocks as the
// terms say - hours, at a day's timelock - and a restart in that window used to
// come back finished, which is a table nobody can play and nobody left. Every
// funded table on this box died that way once, to a container restart.
//
// So the record is believed about what it records: this player got up, or the
// table had begun a hand it cannot be put back into. Anything else comes back
// as what it was.
func (t *tables) receipt(rec *record, id *identity) (*table, error) {
	terms := rec.Terms.Into()
	creds, err := id.credentials(terms.SID)
	if err != nil {
		return nil, err
	}
	form, err := membership.NewFormation(terms, creds)
	if err != nil {
		return nil, err
	}
	done := rec.Finished || rec.Aborted || everDealt(rec)
	tbl := &table{terms: terms, gcID: rec.GCID, form: form,
		funded: map[uint32]string{}, bonded: map[uint32]string{},
		stakeWaiting: map[uint32]*waiting{}, bondWaiting: map[uint32]*waiting{},
		releases: map[uint32]*release{}, bondValue: map[uint32]int64{},
		payouts: map[uint32]string{}, claims: map[driver.Duty]*claim{},
		refresh: map[string]*refresh{}, bondedAt: map[uint32]string{},
		session: creds.Session, netParams: t.params, chain: t.chain,
		logPriv: creds.Log, finished: done}

	if err := tbl.resume(rec); err != nil {
		return nil, err
	}
	tbl.finished = done
	return tbl, nil
}

// tick tells every table where the chain is, which is how a deadline passes.
//
// The height comes from the host: the sandbox has no node of its own, and a
// deadline read from a local clock would be one each machine read differently.
func (t *tables) tick(height int64) []outgoing {
	t.mu.Lock()
	defer t.mu.Unlock()

	if height > 0 {
		t.height = height
	}

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
		if tbl.play != nil && tbl.play.Over() {
			// The table has stopped producing entries, so this is
			// the last chance to write them down that does not
			// depend on somebody pressing leave.
			t.archive(tbl)
		}
		out = append(out, tbl.askAgain(height)...)
		out = append(out, tbl.proposeClaim(t.params)...)
		out = append(out, tbl.presignRefreshes()...)
		out = append(out, tbl.proposeSettlement()...)
		out = append(out, tbl.proposeReleases()...)
		out = append(out, t.announceAgain(tbl, height)...)
		out = append(out, t.announceBondAgain(tbl, height)...)
		out = append(out, t.announcePayoutAgain(tbl, height)...)
		out = append(out, t.exchangeHeads(tbl)...)
		out = append(out, t.republishStalled(tbl, height)...)
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

// announceBondAgain says again where this seat's bond is.
//
// Exactly the fault announceAgain was written for, one field over, and it cost
// a live table before anybody noticed the asymmetry. A bond needs two
// confirmations; it is announced the moment it is broadcast, so the first
// telling is refused by everyone, every time, on purpose. The stake recovered
// from that because it was repeated. The bond was said once and never again, so
// two peers sat with every stake down, each holding its own bond and neither
// holding the other's, waiting on a message nothing would ever send.
//
// Not bounded by the funding deadline, unlike the stake. Past that deadline a
// stake stops mattering because the table lapses - but a table short only of a
// bond does not lapse, so a deadline here would leave exactly the state this is
// meant to fix, permanently. It stops when the table stops needing it: when it
// deals, or when this player gets up.
func (t *tables) announceBondAgain(tbl *table, height int64) []outgoing {
	if height <= 0 || height <= tbl.bondedAnnouncedAt || t.signBonded == nil {
		return nil
	}
	// Deliberately not "stop once this table is dealing". Dealing means our
	// own view is complete, and the peer that still needs to hear this is by
	// definition the one whose view differs - the same reasoning written out
	// at length in announceAgain, and ignored here once already: this peer
	// heard the other's bond, started dealing, stopped repeating its own, and
	// left the other seat short of the one thing it was waiting for.
	//
	// So it stops only when this player is up from the table. A peer that has
	// the bond already drops the repeat on arrival, so the cost of saying it
	// is one small frame a block and the cost of not saying it is a seat that
	// waits forever.
	if tbl.finished {
		return nil
	}
	if tbl.form.State() != membership.Settled {
		return nil
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	outpoint := tbl.bonded[seat]
	if outpoint == "" {
		return nil
	}
	bn, err := t.signBonded(tbl.terms, seat, outpoint)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot say again where our bond is: %v", tbl.terms.SID, err)
		return nil
	}
	tbl.bondedAnnouncedAt = height
	return []outgoing{tbl.frame(schema.KindBonded, schema.BondedFrom(bn), wire.ClassState)}
}

// announcePayoutAgain says again where this seat wants to be paid.
//
// The third repeat, and the odd one out. The two above exist because a payment
// needs confirmations, so the first telling is refused on purpose. This one
// exists because the first telling may have been sent when there was nothing to
// tell: the address is announced when the host mints a panel session, and a
// payout can only be signed against a seat, which does not exist until the
// beacon block is drawn. Somebody accepting an invitation opens the panel during
// registration - that is when one accepts an invitation - so the announcement
// went out, found no seat, and returned nothing. Nothing said it again.
//
// Nothing heals it either. publishResync carries joins and commits, not payouts,
// and dealing is gated on stakes and bonds rather than on this. So the table
// deals, plays to the end, and only there discovers it cannot build a payout at
// all, because settlement and every claim read tbl.payouts. Seen live at table
// cb2b558c, where one box happened to have its panel reopened after seating and
// the other did not.
//
// Stopped by this player getting up rather than by a funding deadline, like the
// bond and for the same reason: the address matters right until the money moves,
// and our own record of it says nothing about whether anybody else holds it.
func (t *tables) announcePayoutAgain(tbl *table, height int64) []outgoing {
	if height <= 0 || height <= tbl.payoutAnnouncedAt || t.payoutAddr == nil {
		return nil
	}
	if tbl.finished || tbl.form.State() != membership.Settled {
		return nil
	}
	out := tbl.payoutFrame(t.payoutAddr())
	if out == nil {
		// No seat yet, or the host has no address to give. Both are
		// states to come back from, so the height is left alone.
		return nil
	}
	tbl.payoutAnnouncedAt = height
	return out
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
//
// It returns the payout address this seat now has somewhere to be signed against.
// Drawing the seating is the moment that becomes possible, and saying it here
// rather than waiting for the next tick saves half a minute in the ordinary case;
// announcePayoutAgain is what covers every other case.
func (t *tables) seat(sid string, beacon []byte) []outgoing {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return nil
	}
	if err := tbl.form.SetBeacon(beacon); err != nil {
		log.Printf("pokerplugin: table %s: %v", sid, err)
		return nil
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
	if t.payoutAddr == nil {
		return nil
	}
	return tbl.payoutFrame(t.payoutAddr())
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

	case schema.KindHead:
		var body schema.Head
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		return tbl.acceptHead(body), nil

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

	case schema.KindRelease:
		var body schema.Release
		if err := msg.Into(&body); err != nil {
			return nil, err
		}
		return tbl.adoptRelease(context.Background(), body), nil

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

	// Until is the height admission closed at. Reported because it is what
	// these are ordered by, and an order a caller cannot see is one it cannot
	// preserve when it holds tables of its own.
	Until uint32 `json:"until,omitempty"`

	// SeatsAt is the block the seating is drawn from. Derived here rather
	// than left to the caller, because it is Until plus a protocol constant
	// and an interface that added the constant itself would be a second copy
	// of it - wrong the moment the depth changes, and wrong in the one place
	// that tells somebody how long they are waiting.
	SeatsAt uint32 `json:"seatsAt,omitempty"`
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

	// Bonded counts the seats whose table bond this peer has found on the
	// chain, the same way Funded counts stakes. Dealing needs every one of
	// them, so a table can be fully funded and still not deal - which,
	// without this, reads as nothing happening for no reason.
	Bonded int `json:"bonded"`

	// Dealing says whether there is a hand to look at, and Over whether
	// there will be another. /table/hand answers 400 until dealing starts,
	// which is an ordinary state and not a failure; a caller with no way to
	// tell the two apart shows somebody an error for waiting.
	Dealing bool   `json:"dealing"`
	Over    bool   `json:"over,omitempty"`
	Hand    uint64 `json:"hand,omitempty"`

	// Settled is the last boundary every seat put their name to and Live is
	// what the table thinks right now. The pair is the whole of "where the
	// money is": Settled is what a table that stopped would pay out, Live is
	// a promise, and the difference is what the hand in progress voids back
	// to if it never finishes.
	Settled *settledBoundary `json:"settled,omitempty"`
	Live    []int64          `json:"live,omitempty"`

	// Height is where this peer last read the chain, so a deadline above has
	// something to be measured against.
	Height int64 `json:"height,omitempty"`

	// PayoutSet reports whether this player has said where to pay it. Until
	// every seat has, no claim can be built at this table at all.
	PayoutSet bool `json:"payoutSet"`

	// Finished says this player has left and the table is kept only as a
	// record of coin still on the chain. It is not played and not joined.
	Finished bool `json:"finished,omitempty"`

	// Roster is what this peer knows about each seat, and how it knows it.
	// Named for the escrow roster rather than for the group, because that is
	// what it is: the membership the money is bound to.
	Roster []seatView `json:"roster,omitempty"`
}

// seatView is one seat, and the difference between what the chain says and what
// somebody said.
//
// Every field here is this peer's own answer. Stake and Bond are outpoints it
// found itself; an empty one means it has not seen the money, which is not the
// same as the money not being there and must not be rendered as though it were.
type seatView struct {
	Seat uint32 `json:"seat"`
	Ours bool   `json:"ours,omitempty"`
	Key  string `json:"key,omitempty"`

	Stake string `json:"stake,omitempty"`
	Bond  string `json:"bond,omitempty"`

	// StakeWait and BondWait are what this seat announced that has not been
	// accepted, and how far off it is. An empty Stake with a StakeWait is
	// money on its way; an empty Stake with neither is money nobody here has
	// heard of, and telling a person those apart is the whole point.
	StakeWait *waiting `json:"stakeWait,omitempty"`
	BondWait  *waiting `json:"bondWait,omitempty"`
	// BondAt is where this seat's bond sits now, if it has answered a claim
	// and moved it. Later claims are against this and not Bond.
	BondAt string `json:"bondAt,omitempty"`
	// Payout is where this seat wants coin it did not pay for itself. While
	// any seat's is empty no claim at this table can be built.
	Payout  string `json:"payout,omitempty"`
	Leaving bool   `json:"leaving,omitempty"`

	// Owes is what the log says this seat is holding the table up on, and
	// Says is the same thing in the words the protocol already uses for it.
	// Both, because a caller must not have to invent the wording and a claim
	// is checked against the structure.
	Owes *driver.Duty `json:"owes,omitempty"`
	Says string       `json:"says,omitempty"`
}

// snapshots describes every table this player is at, in a fixed order.
//
// The order is part of the answer, not a detail of it. These are built by
// ranging a map, and Go randomises that on every call - so a caller asking twice
// a second, which is exactly what a panel does, got the same tables in a
// different order each time and rendered a row of buttons that swapped places
// continuously. Sorting here rather than in the client fixes both transports at
// once, since the stream and the poll answer with the same bodies.
//
// Newest first, because the table somebody just joined is the one they are
// looking for, and tables that are over sink below the live ones. The final
// comparison is the session id, which is arbitrary but total: two tables that
// tie on everything else must still not be free to swap.
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
			Until:      tbl.terms.Until,
			SeatsAt:    membership.BeaconHeight(tbl.terms),
		}
		if id, ok := tbl.form.MatchID(); ok {
			s.MatchID = id
		}
		if tbl.watch != nil {
			s.Waiting = tbl.watch.Waiting()
		}

		s.Funded = len(tbl.funded)
		s.Bonded = len(tbl.bonded)
		s.Height = t.height
		s.Finished = tbl.finished
		s.Roster = tbl.seatViews()
		if seat, ok := tbl.form.OurSeat(); ok {
			s.Seat = &seat
			s.Stake = tbl.funded[seat]
			s.PayoutSet = tbl.payouts[seat] != ""
			s.FundingDeadline = membership.FundingDeadline(tbl.terms)
			// Reporting the address is what lets a host show somebody
			// where their money is going before it goes.
			if dep, err := tbl.deposit(seat, t.params); err == nil {
				s.DepositAddr = dep.DepositAddr
			}
		}
		if tbl.play != nil {
			s.Dealing = true
			s.Over = tbl.play.Over()
			s.Live = tbl.play.Stacks()
			at, stacks := tbl.play.Settled()
			s.Settled = &settledBoundary{Hand: at, Stacks: stacks}
			if h := tbl.play.Hand(); h != nil {
				s.Hand = h.State().Hand
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Finished != b.Finished {
			return !a.Finished
		}
		if a.Until != b.Until {
			return a.Until > b.Until
		}
		return a.SID < b.SID
	})
	return out
}

// seatViews is what this peer knows about every seat.
//
// Empty until the seating is drawn, which is deliberate: before that there are
// joins but no seats, and numbering them would invent an order the table has
// not agreed to.
//
// Requires the registry lock.
func (tbl *table) seatViews() []seatView {
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	mine, ours := tbl.form.OurSeat()

	out := make([]seatView, 0, len(seats))
	for seat := range uint32(len(seats)) {
		v := seatView{
			Seat:   seat,
			Ours:   ours && seat == mine,
			Key:    hex.EncodeToString(seats[seat]),
			Stake:  tbl.funded[seat],
			Bond:   tbl.bonded[seat],
			BondAt: tbl.bondedAt[seat],
			Payout: tbl.payouts[seat],
		}
		if v.Stake == "" {
			v.StakeWait = tbl.stakeWaiting[seat]
		}
		if v.Bond == "" {
			v.BondWait = tbl.bondWaiting[seat]
		}
		if tbl.play != nil {
			v.Leaving = tbl.play.Leaving(int(seat))
			if d, owes := tbl.play.Owes(int(seat)); owes {
				v.Owes = &d
				v.Says = d.String()
			}
		}
		out = append(out, v)
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
