package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
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
