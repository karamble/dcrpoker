package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vctt94/pokerbisonrelay/pkg/client"
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
	priv *secp256k1.PrivateKey
	form *membership.Formation

	// watch is this player's own copy of the table's history, built once
	// the membership settles - because until then there is no roster to
	// check signatures against.
	watch *client.ChainWatch

	bound bool
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
}

func newTables(st *store) *tables {
	return &tables{m: make(map[string]*table), store: st}
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

	priv, err := id.sessionKey(terms.SID)
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

	form, err := membership.NewFormation(terms, priv)
	if err != nil {
		return nil, err
	}
	tbl := &table{terms: terms, gcID: gcID, priv: priv, form: form}

	if rec != nil {
		if err := tbl.resume(rec); err != nil {
			return nil, err
		}
	}
	t.m[terms.SID] = tbl
	t.persist(tbl)

	// Announce ourselves. Everything else follows from other people's
	// joins arriving.
	return tbl.publishJoin(), nil
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
	if !rec.Bound {
		return nil
	}
	c, err := tbl.form.Bind(tbl.priv)
	if err != nil {
		return fmt.Errorf("cannot take back up the position this key already committed to: %w", err)
	}
	if got := hex.EncodeToString(c.Roster[:]); got != rec.Roster {
		// Refusing to publish anything is the only safe answer: this
		// key has already said something else about this session.
		return fmt.Errorf("resuming would commit to %s, but this key already committed to %s", got, rec.Roster)
	}
	tbl.bound = true
	return nil
}

// leave forgets a table, which also stops admitting frames for it.
func (t *tables) leave(sid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.m[sid]; !ok {
		return false
	}
	delete(t.m, sid)
	return true
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
		before, beforeJoins := tbl.form.State(), len(tbl.form.Joins())
		if !tbl.deadlinePassed(height) {
			continue
		}
		out = append(out, tbl.advance(before, beforeJoins)...)
		t.persist(tbl)
	}
	return out
}

// deliver routes one decoded message to the table it is for.
func (t *tables) deliver(d transport.Delivery) []outgoing {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[d.SID]
	if tbl == nil {
		return nil
	}
	if d.GCID != tbl.gcID {
		// The session is ours; this group chat is not. Frames for one
		// table arriving in another conversation are not this table's,
		// whoever sent them.
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

	case schema.KindAction:
		if tbl.watch == nil {
			// No membership yet, so no keys to check a signature
			// against. Dropping is right: accepting it unverified
			// would imply it had been verified.
			return nil, nil
		}
		return nil, tbl.watch.Apply(msg)

	default:
		// A kind this build does not know must not take the table down.
		return nil, nil
	}

	return tbl.advance(beforeState, beforeJoins), nil
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
			c, err := tbl.form.Bind(tbl.priv)
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
	seats, ok := tbl.form.Seats()
	if !ok {
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
	seats := map[uint32][]byte{}
	var assertion *membership.Assertion
	if s, ok := tbl.form.Seats(); ok {
		seats = s
		// Only a peer that has a membership can claim one. Short of a
		// full table there is nothing to assert, and saying so anyway
		// would be claiming agreement with a set nobody holds.
		if a, err := tbl.form.Assertion(tbl.priv); err == nil {
			assertion = a
		}
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
