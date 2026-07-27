package membership

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
)

// State is where one peer has got to in forming a table.
type State uint8

const (
	// Joining is still collecting joins. No membership exists yet.
	Joining State = iota

	// Formed holds exactly the seats the table needs, so a membership can
	// be computed - but this peer is not bound to it yet, and a further
	// join would still abort.
	Formed

	// Committed means this peer has bound itself and will never sign
	// another roster for this session. Later joins are ignored from here,
	// which is what makes the binding worth anything.
	Committed

	// Settled means every member bound itself to the same membership.
	Settled

	// Aborted is terminal. It must be persisted: a commit arriving after
	// the others gave up would otherwise resurrect this peer into a roster
	// nobody else is bound to.
	Aborted
)

func (s State) String() string {
	switch s {
	case Joining:
		return "joining"
	case Formed:
		return "formed"
	case Committed:
		return "committed"
	case Settled:
		return "settled"
	case Aborted:
		return "aborted"
	}
	return "unknown"
}

// Formation is one peer's view of a table forming, with no referee.
//
// Every peer runs this and reaches the membership itself; nobody proposes one
// and nobody closes the table. The rule is exact fill: a membership exists only
// when the joins held are exactly the seats the table has.
//
// That is deliberately not "the lowest N keys of whatever has arrived", which
// is the obvious rule and does not work. Healing a lossy channel only ever
// grows the set of joins a peer holds, and the lowest N of a set is not
// monotone as the set grows - one low key arriving late ejects the previous
// highest member. A peer could therefore settle around a member another peer
// had already dropped. Under exact fill a peer holding a strict subset computes
// no membership at all and stays silent, so being under-informed is
// self-evident rather than indistinguishable from being well-informed.
type Formation struct {
	terms Terms
	self  []byte // our own compressed session key

	joins   map[string]*Join   // key hex -> join
	commits map[string]*Commit // signer hex -> the one commit they made

	state     State
	reason    string
	canonical [][]byte
	roster    [32]byte
	conflict  *ConflictingCommits
}

// NewFormation starts forming the table these terms describe, joining it with
// the given session key.
func NewFormation(t Terms, priv *secp256k1.PrivateKey) (*Formation, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	ours, err := SignJoin(t, priv)
	if err != nil {
		return nil, err
	}
	f := &Formation{
		terms:   t,
		self:    ours.Key,
		joins:   make(map[string]*Join),
		commits: make(map[string]*Commit),
	}
	if err := f.AddJoin(ours); err != nil {
		return nil, err
	}
	return f, nil
}

// Terms reports what this table was formed under.
func (f *Formation) Terms() Terms { return f.terms }

// State reports where forming has got to.
func (f *Formation) State() State { return f.state }

// Reason explains an abort, for a person rather than for code.
func (f *Formation) Reason() string { return f.reason }

// Ours is this peer's own join, the one to publish.
func (f *Formation) Ours() *Join { return f.joins[keyID(f.self)] }

// Joins reports every verified join held, in canonical key order. This is what
// a roster assertion carries, so a peer that missed one can learn it from
// anyone who did and check it rather than take their word.
func (f *Formation) Joins() []*Join {
	out := make([]*Join, 0, len(f.joins))
	for _, k := range sortKeys(f.keys()) {
		out = append(out, f.joins[keyID(k)])
	}
	return out
}

// AddJoin admits one join, whether it arrived from its author or was relayed
// inside somebody's roster assertion.
//
// A join that fails to verify is refused and changes nothing. That matters most
// for relayed ones: rejection has to be wholesale, or a member could inject a
// key nobody joined with by burying it among real joins.
func (f *Formation) AddJoin(j *Join) error {
	if f.state == Aborted || f.state == Settled {
		return nil
	}
	if j == nil {
		return fmt.Errorf("no join")
	}
	if err := j.Verify(f.terms); err != nil {
		return fmt.Errorf("join: %w", err)
	}
	id := keyID(j.Key)
	if _, have := f.joins[id]; have {
		// Re-delivery is ordinary: joins are re-broadcast because the
		// wire layer never retransmits. Accumulating a set makes that
		// idempotent for free.
		return nil
	}

	if f.state == Committed {
		// Bound already. A late arrival cannot change what this peer
		// signed, and must not: that is the whole point of committing.
		return nil
	}

	f.joins[id] = j
	f.recompute()
	return nil
}

// AddCommit admits one peer's binding.
//
// Commits are accepted before this peer has a membership of its own, because
// the joins they were computed from may still be in flight. What cannot be
// accepted is a second, different commit from a key that already made one -
// that is a contradiction, and it ends the table with the proof retained.
func (f *Formation) AddCommit(c *Commit) error {
	if f.state == Aborted {
		return nil
	}
	if c == nil {
		return fmt.Errorf("no commit")
	}
	if err := c.Verify(f.terms); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	id := keyID(c.Signer)
	if prev, have := f.commits[id]; have {
		if prev.Roster == c.Roster {
			return nil
		}
		// Keep the proof either way, but do not unwind a table that
		// already settled. Settled is terminal, and it is still sound:
		// this member bound itself to ours as well, so satisfying our
		// escrows still needs every signature it always did - and to
		// make the other table real they would have to fund it too.
		f.conflict = &ConflictingCommits{A: prev, B: c}
		if f.state != Settled {
			f.abort(fmt.Sprintf("member %s… bound itself to two different rosters", id[:8]))
		}
		return nil
	}
	f.commits[id] = c
	f.recompute()
	return nil
}

// Bind commits this peer to the membership it holds, irrevocably.
//
// It is a separate step from reaching Formed, and the caller's to take, because
// when to take it is a policy question this type cannot answer: binding early
// risks binding to a membership a late join would have aborted, and binding
// late risks the table never forming at all. What this type guarantees is that
// once bound, nothing moves it.
func (f *Formation) Bind(priv *secp256k1.PrivateKey) (*Commit, error) {
	switch f.state {
	case Committed, Settled:
		return f.commits[keyID(f.self)], nil
	case Formed:
	default:
		return nil, fmt.Errorf("cannot bind while %s", f.state)
	}

	c, err := SignCommit(f.terms, f.roster, priv)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(c.Signer, f.self) {
		return nil, fmt.Errorf("binding key is not the one that joined")
	}
	f.commits[keyID(c.Signer)] = c
	f.state = Committed
	f.recompute()
	return c, nil
}

// Members reports the membership in canonical key order, which is the order
// every escrow script names them in and the order signatures are supplied in.
// It is available from Formed onwards.
func (f *Formation) Members() ([][]byte, bool) {
	if f.canonical == nil {
		return nil, false
	}
	out := make([][]byte, len(f.canonical))
	copy(out, f.canonical)
	return out, true
}

// Seats reports which key holds which seat.
//
// Seat order and canonical key order are deliberately separate concepts, as
// escrow.CanonicalMembers says of its own ordering: it fixes the script layout
// "regardless of seat assignment". They coincide here only because there is
// nothing better yet to assign seats with. That is a placeholder and a known
// weakness - the first hand's button is seat 0, and session keys are free to
// generate, so ordering seats by key hands the button to whoever grinds the
// lowest one. Permuting seats by a chain beacon drawn after everyone has
// committed is what fixes it, and this is the one function that has to change.
func (f *Formation) Seats() (map[uint32][]byte, bool) {
	if f.canonical == nil {
		return nil, false
	}
	seats := make(map[uint32][]byte, len(f.canonical))
	for i, k := range f.canonical {
		seats[uint32(i)] = k
	}
	return seats, true
}

// RosterHash is what a commit binds to. Available from Formed onwards.
func (f *Formation) RosterHash() ([32]byte, bool) {
	if f.canonical == nil {
		return [32]byte{}, false
	}
	return f.roster, true
}

// MatchID identifies the match everything else hangs off: the signed action
// log, and the frames that carry it.
//
// It is the roster hash rather than the session id because the session id is
// only a routing key - it says which conversation frames belong to, and two
// groups that somehow formed different tables under one invitation would
// collide on it. The roster hash cannot collide, because it is what they
// disagreed about.
func (f *Formation) MatchID() (string, bool) {
	if f.canonical == nil {
		return "", false
	}
	return hex.EncodeToString(f.roster[:]), true
}

// Roster renders the settled membership in the shape the escrow layer wants.
func (f *Formation) Roster() (*Roster, bool) {
	seats, ok := f.Seats()
	if !ok || f.state != Settled {
		return nil, false
	}
	members, _ := f.Members()
	return &Roster{
		Size:    int(f.terms.Seats),
		Seats:   seats,
		Escrows: make(map[uint32]string),
		Members: members,
	}, true
}

// Conflict returns the proof that ended the table, if that is what ended it.
func (f *Formation) Conflict() *ConflictingCommits { return f.conflict }

// recompute advances the state machine after anything was admitted.
func (f *Formation) recompute() {
	switch f.state {
	case Aborted, Settled:
		return
	}

	// Membership first, since everything after it needs one.
	switch {
	case f.state == Committed:
		// Bound: the membership is fixed and joins no longer move it.
	case len(f.joins) > int(f.terms.Seats):
		f.abort(fmt.Sprintf("%d players answered a %d seat table", len(f.joins), f.terms.Seats))
		return
	case len(f.joins) == int(f.terms.Seats):
		canonical, err := escrow.CanonicalMembers(f.keys())
		if err != nil {
			f.abort(fmt.Sprintf("membership is not usable: %v", err))
			return
		}
		roster, err := RosterHash(f.terms, canonical)
		if err != nil {
			f.abort(fmt.Sprintf("membership is not usable: %v", err))
			return
		}
		f.canonical, f.roster, f.state = canonical, roster, Formed
	default:
		// Still short of a full table. Nothing to compute, and nothing
		// to say: a peer holding a subset must stay silent rather than
		// assert a membership it has no reason to believe is whole.
		f.canonical, f.state = nil, Joining
		return
	}

	if f.state != Committed {
		return
	}

	// Settled needs every member of our own membership bound to our own
	// roster. Anyone else's commit is somebody else's table.
	for _, k := range f.canonical {
		c, ok := f.commits[keyID(k)]
		if !ok || c.Roster != f.roster {
			return
		}
	}
	f.state = Settled
}

func (f *Formation) abort(reason string) {
	f.state = Aborted
	f.reason = reason
	f.canonical = nil
}

func (f *Formation) keys() [][]byte {
	out := make([][]byte, 0, len(f.joins))
	for _, j := range f.joins {
		out = append(out, j.Key)
	}
	return out
}
