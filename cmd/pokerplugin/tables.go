package main

import (
	"context"
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
	mu sync.Mutex
	m  map[string]*table
}

func newTables() *tables { return &tables{m: make(map[string]*table)} }

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

	form, err := membership.NewFormation(terms, priv)
	if err != nil {
		return nil, err
	}
	tbl := &table{terms: terms, gcID: gcID, priv: priv, form: form}
	t.m[terms.SID] = tbl

	// Announce ourselves. Everything else follows from other people's
	// joins arriving.
	return tbl.publishJoin(), nil
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
	for _, j := range joins {
		if err := tbl.form.AddJoin(j); err != nil {
			return err
		}
	}
	return nil
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
		if !tbl.bound {
			// Binding as soon as the table is full is a policy, and
			// a provisional one. It is safe while nothing funds,
			// and the honest fix is a closed admission window every
			// peer derives the same way - a block height, not a
			// clock - so that "no more joins are coming" is a fact
			// rather than a hope. Until then, binding early risks
			// binding to a membership a late join would have
			// aborted, and the table simply fails to settle.
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
	if s, ok := tbl.form.Seats(); ok {
		seats = s
	}
	body := schema.RosterFrom(tbl.terms, seats, tbl.form.Joins())
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
