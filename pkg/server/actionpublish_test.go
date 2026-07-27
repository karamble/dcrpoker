package server

import (
	"context"
	"testing"

	"github.com/companyzero/bisonrelay/zkidentity"
	"github.com/stretchr/testify/require"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
)

// capturePublisher records what the server broadcast.
type capturePublisher struct {
	msgs []capturedMsg
	fail error
}

type capturedMsg struct {
	GCID    string
	MatchID string
	Kind    schema.Kind
	Body    any
}

func (c *capturePublisher) Publish(_ context.Context, gcID, _, matchID string, kind schema.Kind, body any) error {
	if c.fail != nil {
		return c.fail
	}
	c.msgs = append(c.msgs, capturedMsg{GCID: gcID, MatchID: matchID, Kind: kind, Body: body})
	return nil
}

// Every verified action is relayed to the table exactly as its seat signed it,
// so a player can rebuild the chain instead of taking the server's word.
func TestVerifiedActionsArePublished(t *testing.T) {
	srv := newTestServerWithState(t)
	pub := &capturePublisher{}
	srv.SetFramePublisher(pub)
	srv.BindMatchGroupChat("published", "gc-1")

	table, privs := loggedTable(t, srv, "published")

	for _, step := range []struct {
		seat   uint32
		action gamelog.Action
		amount int64
	}{{0, gamelog.ActionBet, 500}, {1, gamelog.ActionCall, 0}} {
		signed := signAt(t, srv, table, privs[step.seat], step.seat, step.action, step.amount)
		require.NoError(t, srv.recordAction(table, playerID(t, table, step.seat), signed, step.action, step.amount))
	}

	require.Len(t, pub.msgs, 2)
	for i, m := range pub.msgs {
		require.Equal(t, "gc-1", m.GCID)
		require.Equal(t, "published", m.MatchID)
		require.Equal(t, schema.KindAction, m.Kind, "message %d", i)

		body, ok := m.Body.(schema.Action)
		require.True(t, ok, "message %d is not an action", i)
		require.EqualValues(t, i+1, body.Entry.Seq)
		require.NotEmpty(t, body.Entry.Sig, "the seat's signature must be relayed")
	}
}

// A table with no group chat publishes nothing, and a publisher that fails must
// not undo an action the hand already accepted.
func TestPublishingIsNeverLoadBearing(t *testing.T) {
	srv := newTestServerWithState(t)
	table, privs := loggedTable(t, srv, "unpublished")

	// No group chat bound: nothing to publish to.
	pub := &capturePublisher{}
	srv.SetFramePublisher(pub)
	signed := signAt(t, srv, table, privs[0], 0, gamelog.ActionBet, 500)
	require.NoError(t, srv.recordAction(table, playerID(t, table, 0), signed, gamelog.ActionBet, 500))
	require.Empty(t, pub.msgs)

	// Bound, but the send fails. The action still stands: making the hand
	// depend on a group chat is the dependency the pre-signed drafts and
	// the CSV refund exist to avoid.
	srv.BindMatchGroupChat("unpublished", "gc-1")
	pub.fail = context.DeadlineExceeded
	signed = signAt(t, srv, table, privs[1], 1, gamelog.ActionCall, 0)
	require.NoError(t, srv.recordAction(table, playerID(t, table, 1), signed, gamelog.ActionCall, 0))

	chain, err := srv.chainFor("unpublished")
	require.NoError(t, err)
	require.Equal(t, 2, chain.Len(), "a failed broadcast must not lose an action")
}

// Unbinding stops the relay.
func TestUnbindingAGroupChatStopsPublishing(t *testing.T) {
	srv := newTestServerWithState(t)
	pub := &capturePublisher{}
	srv.SetFramePublisher(pub)
	srv.BindMatchGroupChat("unbound", "gc-1")
	table, privs := loggedTable(t, srv, "unbound")

	signed := signAt(t, srv, table, privs[0], 0, gamelog.ActionBet, 500)
	require.NoError(t, srv.recordAction(table, playerID(t, table, 0), signed, gamelog.ActionBet, 500))
	require.Len(t, pub.msgs, 1)

	srv.BindMatchGroupChat("unbound", "")
	signed = signAt(t, srv, table, privs[1], 1, gamelog.ActionCall, 0)
	require.NoError(t, srv.recordAction(table, playerID(t, table, 1), signed, gamelog.ActionCall, 0))
	require.Len(t, pub.msgs, 1, "unbinding should stop the relay")
}

// A player at the table says which group chat its log is relayed to.
func TestBindTableGroupChat(t *testing.T) {
	srv := newTestServerWithState(t)
	pub := &capturePublisher{}
	srv.SetFramePublisher(pub)

	const gcID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	table, privs := loggedTable(t, srv, "bindable")

	resp, err := srv.BindTableGroupChat(context.Background(), &pokerrpc.BindTableGroupChatRequest{
		TableId: "bindable", GcId: gcID, Token: "tok1",
	})
	require.NoError(t, err)
	require.Equal(t, "bindable", resp.GetMatchId())
	require.Equal(t, gcID, resp.GetGcId())
	require.True(t, resp.GetRelaying(), "a bound table with a publisher should relay")

	signed := signAt(t, srv, table, privs[0], 0, gamelog.ActionBet, 500)
	require.NoError(t, srv.recordAction(table, playerID(t, table, 0), signed, gamelog.ActionBet, 500))
	require.Len(t, pub.msgs, 1)
	require.Equal(t, gcID, pub.msgs[0].GCID)

	// Empty unbinds.
	resp, err = srv.BindTableGroupChat(context.Background(), &pokerrpc.BindTableGroupChatRequest{
		TableId: "bindable", GcId: "", Token: "tok1",
	})
	require.NoError(t, err)
	require.False(t, resp.GetRelaying())
}

// The group chat decides only where frames go, so binding one is a table
// member's call and nobody else's.
func TestBindTableGroupChatChecksTheCaller(t *testing.T) {
	srv := newTestServerWithState(t)
	loggedTable(t, srv, "guarded")

	const gcID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	var outsider zkidentity.ShortID
	require.NoError(t, outsider.FromString("00000000000000000000000000000000000000000000000000000000000000ff"))
	srv.TestSeedSession("tok-out", outsider, "TsRnk22spGQJTpKFcRBc281rmfNFpywh337", outsider.String())

	_, err := srv.BindTableGroupChat(context.Background(), &pokerrpc.BindTableGroupChatRequest{
		TableId: "guarded", GcId: gcID, Token: "tok-out",
	})
	require.ErrorContains(t, err, "not seated")

	_, err = srv.BindTableGroupChat(context.Background(), &pokerrpc.BindTableGroupChatRequest{
		TableId: "guarded", GcId: "not-hex", Token: "tok1",
	})
	require.ErrorContains(t, err, "64 hex")

	_, err = srv.BindTableGroupChat(context.Background(), &pokerrpc.BindTableGroupChatRequest{
		TableId: "no-such-table", GcId: gcID, Token: "tok1",
	})
	require.ErrorContains(t, err, "table not found")
}

// A table can be bound while the server has no bridge, and should say so rather
// than leave a player wondering why nothing arrives.
func TestBindTableGroupChatReportsWhenNothingRelays(t *testing.T) {
	srv := newTestServerWithState(t)
	loggedTable(t, srv, "no-bridge")

	const gcID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	resp, err := srv.BindTableGroupChat(context.Background(), &pokerrpc.BindTableGroupChatRequest{
		TableId: "no-bridge", GcId: gcID, Token: "tok1",
	})
	require.NoError(t, err)
	require.Equal(t, gcID, resp.GetGcId())
	require.False(t, resp.GetRelaying(), "no bridge configured means nothing is relayed")
}

// With no bridge configured the log stays local, which is how the server runs
// without Bison Relay at all.
func TestNoBridgeConfiguredInstallsNoPublisher(t *testing.T) {
	srv := newTestServerWithState(t)
	require.NoError(t, srv.initGamingBridge(ServerConfig{}))
	publisher, _ := srv.publishTarget("anything")
	require.Nil(t, publisher)

	// A URL without a token is incomplete, not a bridge.
	require.NoError(t, srv.initGamingBridge(ServerConfig{GamingBridgeURL: "https://host/gaming"}))
	publisher, _ = srv.publishTarget("anything")
	require.Nil(t, publisher)

	require.NoError(t, srv.initGamingBridge(ServerConfig{
		GamingBridgeURL: "https://host/gaming", GamingBridgeToken: "tok",
	}))
	publisher, _ = srv.publishTarget("anything")
	require.NotNil(t, publisher, "a complete bridge config should install a publisher")
}
