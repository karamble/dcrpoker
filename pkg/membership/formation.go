package membership

import (
	"encoding/hex"
	"fmt"

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
	creds Credentials
	self  []byte // our own compressed session key

	joins   map[string]*Join    // key hex -> join
	commits map[string]*Commit  // signer hex -> the one commit they made
	asserts map[string][32]byte // signer hex -> the membership they last claimed
	closed  bool                // the admission window has shut

	state     State
	reason    string
	canonical [][]byte
	roster    [32]byte
	conflict  *ConflictingCommits
}

// NewFormation starts forming the table these terms describe, joining it with
// the given session key.
func NewFormation(t Terms, c Credentials) (*Formation, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	ours, err := SignJoin(t, c)
	if err != nil {
		return nil, err
	}
	f := &Formation{
		terms:   t,
		creds:   c,
		self:    ours.Key,
		joins:   make(map[string]*Join),
		commits: make(map[string]*Commit),
		asserts: make(map[string][32]byte),
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

// CloseWindow shuts admission, which the caller does when it sees the chain
// past the deadline the terms name.
//
// This is what makes "no more joins are coming" a fact rather than a hope. A
// table still short of its seats at that point cannot form, and one that has
// exactly its seats can bind knowing nothing will displace it.
//
// The height is not read here. Formation stays a pure function of what it has
// been told, so the one thing that has to consult a clock or a chain is the
// caller - and it can be tested by saying so directly.
func (f *Formation) CloseWindow() {
	if f.closed || f.state == Aborted || f.state == Settled {
		return
	}
	f.closed = true
	if f.state == Joining {
		// Short of a full table when admission shut. Nothing else can
		// arrive, so there is nothing left to wait for.
		f.abort(fmt.Sprintf("only %d of %d seats were taken before the deadline",
			len(f.joins), f.terms.Seats))
	}
}

// WindowClosed reports whether admission has shut.
func (f *Formation) WindowClosed() bool { return f.closed }

// Agreed reports whether every member of this peer's membership has said it
// holds the same one.
//
// It is what lets a full table form without waiting out its deadline. Everybody
// having asserted the same membership is not proof that no straggler exists -
// only the deadline is that - but it does mean every member has seen exactly
// this set, which is enough to bind on when the alternative is a lobby nobody
// is watching for ten minutes.
func (f *Formation) Agreed() bool {
	if f.canonical == nil {
		return false
	}
	for _, k := range f.canonical {
		if got, ok := f.asserts[keyID(k)]; !ok || got != f.roster {
			return false
		}
	}
	return true
}

// AddAssertion records what another peer says it holds, and adopts the joins it
// showed to back the claim.
//
// The joins are what make the claim checkable, and they are checked before any
// is kept: rejection has to be wholesale, or a member could get a key nobody
// joined with admitted by burying it among real ones.
func (f *Formation) AddAssertion(a *Assertion, joins []*Join) error {
	if f.state == Aborted || f.state == Settled {
		return nil
	}
	if a == nil {
		return fmt.Errorf("no assertion")
	}
	if err := a.Verify(f.terms); err != nil {
		return fmt.Errorf("assertion: %w", err)
	}
	for i, j := range joins {
		if j == nil {
			return fmt.Errorf("assertion join %d: missing", i)
		}
		if err := j.Verify(f.terms); err != nil {
			return fmt.Errorf("assertion join %d: %w", i, err)
		}
	}

	f.asserts[keyID(a.Signer)] = a.Roster
	for _, j := range joins {
		if err := f.AddJoin(j); err != nil {
			return err
		}
	}
	f.recompute()
	return nil
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
	if f.closed {
		// Admission shut. Accepting now would be accepting a join the
		// peers who saw the deadline pass have already refused.
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
func (f *Formation) Bind() (*Commit, error) {
	switch f.state {
	case Committed, Settled:
		return f.commits[keyID(f.self)], nil
	case Formed:
	default:
		return nil, fmt.Errorf("cannot bind while %s", f.state)
	}

	c, err := SignCommit(f.terms, f.roster, f.creds.Session)
	if err != nil {
		return nil, err
	}
	f.commits[keyID(c.Signer)] = c
	f.state = Committed
	f.recompute()
	return c, nil
}

// Assertion is this peer's own claim about what it holds, to publish.
//
// Making one records it, because this peer's own view counts toward agreement
// exactly as anyone else's does - and a caller that had to remember to file its
// own would eventually not.
func (f *Formation) Assertion() (*Assertion, error) {
	if f.canonical == nil {
		return nil, fmt.Errorf("no membership to assert yet")
	}
	a, err := SignAssertion(f.terms, f.roster, f.creds.Session)
	if err != nil {
		return nil, err
	}
	f.asserts[keyID(a.Signer)] = a.Roster
	return a, nil
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
