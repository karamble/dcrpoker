package server

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/poker"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
	"github.com/vctt94/pokerbisonrelay/pkg/server/internal/db"
)

// loggedTable seeds a two-seat table whose roster has closed, so it keeps a
// signed action log, and returns the seats' signing keys.
func loggedTable(t *testing.T, srv *Server, tableID string) (*poker.Table, []*forfeit.LogKey) {
	t.Helper()
	const amount = uint64(1_000_000)
	table := seedRosterTable(t, srv, tableID, 2, amount)

	privs := make([]*forfeit.LogKey, 2)
	uids := []string{
		"0000000000000000000000000000000000000000000000000000000000000001",
		"0000000000000000000000000000000000000000000000000000000000000002",
	}
	for i, uid := range uids {
		priv, err := forfeit.NewLogKey("logkeys")
		require.NoError(t, err)
		privs[i] = priv
		openRosterEscrow(t, srv, table, uid, "tok"+uid[63:], uint32(i), amount,
			priv.Public().SerializeCompressed())
	}
	return table, privs
}

// playerID returns the id the table knows a seat by.
func playerID(t *testing.T, table *poker.Table, seat uint32) string {
	t.Helper()
	for _, u := range table.GetUsers() {
		if uint32(u.TableSeat) == seat {
			return u.ID
		}
	}
	t.Fatalf("no player at seat %d", seat)
	return ""
}

func signAt(t *testing.T, srv *Server, table *poker.Table, priv *forfeit.LogKey, seat uint32, action gamelog.Action, amount int64) *pokerrpc.SignedAction {
	t.Helper()
	chain, err := srv.chainFor(table.GetConfig().ID)
	require.NoError(t, err)
	require.NotNil(t, chain, "table should keep a log")

	e := chain.Next(seat, 1, gamelog.StreetPreFlop, action, amount)
	require.NoError(t, e.Sign(priv))
	return actionToWire(e)
}

func actionToWire(e *gamelog.Entry) *pokerrpc.SignedAction {
	return &pokerrpc.SignedAction{
		Version:  uint32(e.Version),
		PrevHash: e.PrevHash[:],
		Seq:      e.Seq,
		Hand:     e.Hand,
		Street:   uint32(e.Street),
		Seat:     e.Seat,
		Signer:   e.Signer,
		Action:   string(e.Action),
		Amount:   e.Amount,
		Sig:      e.Sig,
	}
}

// A table with an escrow roster keeps a log, and an action has to be signed by
// the seat taking it before the engine is allowed to see it.
func TestRecordActionAcceptsSignedPlay(t *testing.T) {
	srv := newTestServerWithState(t)
	table, privs := loggedTable(t, srv, "logged-table")

	for i, step := range []struct {
		seat   uint32
		action gamelog.Action
		amount int64
	}{{0, gamelog.ActionBet, 500}, {1, gamelog.ActionCall, 0}, {0, gamelog.ActionCheck, 0}} {
		signed := signAt(t, srv, table, privs[step.seat], step.seat, step.action, step.amount)
		err := srv.recordAction(table, playerID(t, table, step.seat), signed, step.action, step.amount)
		require.NoError(t, err, "step %d", i)
	}

	chain, err := srv.chainFor("logged-table")
	require.NoError(t, err)
	require.Equal(t, 3, chain.Len())

	// The log is exportable and verifies on its own, which is the whole
	// point of keeping it.
	blob, err := srv.ExportActionLog("logged-table")
	require.NoError(t, err)
	back, err := gamelog.Unmarshal(blob)
	require.NoError(t, err)
	require.Equal(t, 3, back.Len())
}

// Every one of these would let the log say something other than what happened.
func TestRecordActionRejectsBadSignatures(t *testing.T) {
	srv := newTestServerWithState(t)
	table, privs := loggedTable(t, srv, "logged-reject")
	seat0 := playerID(t, table, 0)

	cases := []struct {
		name   string
		build  func(t *testing.T) *pokerrpc.SignedAction
		action gamelog.Action
		amount int64
		want   string
	}{{
		name:   "not signed at all",
		build:  func(*testing.T) *pokerrpc.SignedAction { return nil },
		action: gamelog.ActionBet, amount: 500,
		want: "must be signed",
	}, {
		name: "signed by another seat's key",
		build: func(t *testing.T) *pokerrpc.SignedAction {
			return signAt(t, srv, table, privs[1], 0, gamelog.ActionBet, 500)
		},
		action: gamelog.ActionBet, amount: 500,
		want: "signed by another key",
	}, {
		name: "claiming a seat the caller does not hold",
		build: func(t *testing.T) *pokerrpc.SignedAction {
			return signAt(t, srv, table, privs[1], 1, gamelog.ActionBet, 500)
		},
		action: gamelog.ActionBet, amount: 500,
		want: "you hold seat",
	}, {
		name: "signed for one action, sent as another",
		build: func(t *testing.T) *pokerrpc.SignedAction {
			return signAt(t, srv, table, privs[0], 0, gamelog.ActionCheck, 0)
		},
		action: gamelog.ActionFold, amount: 0,
		want: "signed as",
	}, {
		name: "signed for one amount, sent for another",
		build: func(t *testing.T) *pokerrpc.SignedAction {
			return signAt(t, srv, table, privs[0], 0, gamelog.ActionBet, 500)
		},
		action: gamelog.ActionBet, amount: 900,
		want: "signed for 500",
	}, {
		name: "amount raised after signing",
		build: func(t *testing.T) *pokerrpc.SignedAction {
			s := signAt(t, srv, table, privs[0], 0, gamelog.ActionBet, 500)
			s.Amount = 900
			return s
		},
		action: gamelog.ActionBet, amount: 900,
		want: "does not verify",
	}, {
		name: "chained to a history that is not ours",
		build: func(t *testing.T) *pokerrpc.SignedAction {
			s := signAt(t, srv, table, privs[0], 0, gamelog.ActionBet, 500)
			s.PrevHash = make([]byte, 32)
			return s
		},
		action: gamelog.ActionBet, amount: 500,
		want: "does not chain",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := srv.recordAction(table, seat0, tc.build(t), tc.action, tc.amount)
			require.ErrorContains(t, err, tc.want)
		})
	}

	// None of it reached the log.
	chain, err := srv.chainFor("logged-reject")
	require.NoError(t, err)
	require.Zero(t, chain.Len(), "a rejected action must leave no trace in the log")
}

// A table with no escrow roster has no keys to check signatures against, so it
// keeps no log - and must not pretend to by accepting one unverified.
func TestTableWithoutRosterKeepsNoLog(t *testing.T) {
	srv := newTestServerWithState(t)
	table := seedRosterTable(t, srv, "unlogged", 2, 0)

	chain, err := srv.chainFor("unlogged")
	require.NoError(t, err)
	require.Nil(t, chain)

	user := poker.NewUser("player-1", table, &poker.AddUserOptions{Seat: 0})
	require.NoError(t, table.AddUser(user))

	// Unsigned play is fine here.
	require.NoError(t, srv.recordAction(table, "player-1", nil, gamelog.ActionCheck, 0))

	// A signature is refused rather than waved through, since accepting one
	// would imply it had been checked.
	priv, err := forfeit.NewLogKey("logkeys")
	require.NoError(t, err)
	e := &gamelog.Entry{Version: gamelog.Version, Seq: 1, Seat: 0, Action: gamelog.ActionCheck}
	require.NoError(t, e.Sign(priv))
	err = srv.recordAction(table, "player-1", actionToWire(e), gamelog.ActionCheck, 0)
	require.ErrorContains(t, err, "keeps no action log")
}

// The roster is not final until every seat has opened, so a chain built early
// would verify against keys that could still change.
func TestNoLogUntilRosterCloses(t *testing.T) {
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	table := seedRosterTable(t, srv, "half-open", 2, amount)

	priv, err := forfeit.NewLogKey("logkeys")
	require.NoError(t, err)
	openRosterEscrow(t, srv, table,
		"0000000000000000000000000000000000000000000000000000000000000001", "tokA", 0, amount,
		priv.Public().SerializeCompressed())

	chain, err := srv.chainFor("half-open")
	require.NoError(t, err)
	require.Nil(t, chain, "one seat open is not a roster")
}

// The head a client is handed is the head it must chain to.
func TestLogHeadTracksTheChain(t *testing.T) {
	srv := newTestServerWithState(t)
	table, privs := loggedTable(t, srv, "logged-head")

	head, seq := srv.logHead("logged-head")
	require.Nil(t, head, "no log exists until the first action creates it")
	require.Zero(t, seq)

	signed := signAt(t, srv, table, privs[0], 0, gamelog.ActionBet, 500)
	require.NoError(t, srv.recordAction(table, playerID(t, table, 0), signed, gamelog.ActionBet, 500))

	head, seq = srv.logHead("logged-head")
	require.Len(t, head, 32)
	require.EqualValues(t, 1, seq)
	require.NotEqual(t, hex.EncodeToString(make([]byte, 32)), hex.EncodeToString(head))

	srv.dropActionLog("logged-head")
	head, _ = srv.logHead("logged-head")
	require.Nil(t, head)
}

// countingDB records what the hand-history writer actually wrote, so the
// persistence path is checked rather than assumed.
type countingDB struct {
	Database
	hands   int
	actions []db.Action
}

func (c *countingDB) BeginHand(_ context.Context, _ *db.Hand) (int64, error) {
	c.hands++
	return int64(c.hands), nil
}

func (c *countingDB) AppendAction(_ context.Context, a *db.Action) (int64, error) {
	c.actions = append(c.actions, *a)
	return int64(len(c.actions)), nil
}

// The hand-history tables have existed and gone unwritten since they were
// added. The signed log is what fills them, so what lands there is what a seat
// actually put its name to.
func TestActionsArePersistedToHandHistory(t *testing.T) {
	srv := newTestServerWithState(t)
	counting := &countingDB{Database: srv.db}
	srv.db = counting

	table, privs := loggedTable(t, srv, "persisted")

	for _, step := range []struct {
		seat   uint32
		action gamelog.Action
		amount int64
	}{{0, gamelog.ActionBet, 500}, {1, gamelog.ActionCall, 0}, {0, gamelog.ActionCheck, 0}} {
		signed := signAt(t, srv, table, privs[step.seat], step.seat, step.action, step.amount)
		require.NoError(t, srv.recordAction(table, playerID(t, table, step.seat), signed, step.action, step.amount))
	}

	// One hand row, created by the first action rather than at deal time.
	require.Equal(t, 1, counting.hands)
	require.Len(t, counting.actions, 3)

	// Ordering within the hand is what makes the history replayable.
	for i, want := range []struct {
		ord    int
		seat   int
		action string
		amount int64
	}{{1, 0, "bet", 500}, {2, 1, "call", 0}, {3, 0, "check", 0}} {
		got := counting.actions[i]
		require.Equal(t, want.ord, got.Ord)
		require.Equal(t, want.seat, got.ActorSeat)
		require.Equal(t, want.action, got.Action)
		require.Equal(t, want.amount, got.Amount)
		require.Equal(t, "preflop", got.Street)
	}
}

// A rejected action must not reach the history either, or the stored record
// would contain plays the table never accepted.
func TestRejectedActionsAreNotPersisted(t *testing.T) {
	srv := newTestServerWithState(t)
	counting := &countingDB{Database: srv.db}
	srv.db = counting

	table, privs := loggedTable(t, srv, "persist-reject")

	// Signed by the wrong seat's key.
	signed := signAt(t, srv, table, privs[1], 0, gamelog.ActionBet, 500)
	require.Error(t, srv.recordAction(table, playerID(t, table, 0), signed, gamelog.ActionBet, 500))

	require.Zero(t, counting.hands)
	require.Empty(t, counting.actions)
}
