package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bridgePublisher relays the signed action log through a host that holds the
// Bison Relay credentials.
//
// The server does not hold them itself, for the same reason a game does not: it
// runs beside a wallet and a Bison Relay identity, and what it needs is a way
// to speak, not an identity to speak as. The host carries frames and reads only
// the routing key.
type bridgePublisher struct {
	pub *transport.Publisher
}

// Publish sends one message as turn traffic.
//
// Actions are ClassTurn deliberately: they must go stale quickly, so a raise
// held by store-and-forward cannot arrive after the hand has settled. Evidence
// would be sent as ClassDispute, which outlives a player rebooting.
func (b *bridgePublisher) Publish(ctx context.Context, gcID, sid, matchID string, kind schema.Kind, body any) error {
	return b.pub.Send(ctx, gcID, sid, matchID, kind, body, wire.ClassTurn)
}

// initGamingBridge installs the frame publisher when a bridge is configured.
//
// With no bridge the signed log still exists and is still verified before the
// engine acts; it simply stays local. That is the honest default: relaying is
// what lets a player check the server, and a deployment without Bison Relay
// should not pretend to offer it.
func (s *Server) initGamingBridge(cfg ServerConfig) error {
	if cfg.GamingBridgeURL == "" || cfg.GamingBridgeToken == "" {
		return nil
	}
	bridge, err := transport.NewBridge(cfg.GamingBridgeURL, cfg.GamingBridgeToken, nil)
	if err != nil {
		return fmt.Errorf("gaming bridge: %w", err)
	}
	pub, err := transport.NewPublisher(schema.Game, schema.Version, bridge)
	if err != nil {
		return fmt.Errorf("gaming bridge: %w", err)
	}
	s.SetFramePublisher(&bridgePublisher{pub: pub})
	s.log.Infof("Relaying signed action logs through the gaming bridge at %s", cfg.GamingBridgeURL)
	return nil
}

// BindTableGroupChat records which Bison Relay group chat a table's players
// share, so the signed log can be relayed to them.
//
// A player at the table sets it. Nothing about the group chat is trusted: its
// membership is administered by one member, is never attested and can differ
// between them, so it decides only where frames are sent, never who is believed
// about the table's history. That stays with the escrow roster.
//
// The relay is an addition, not a dependency. A table with no group chat plays
// exactly as before; players simply have no independent copy of the log to
// check the server against.
func (s *Server) BindTableGroupChat(ctx context.Context, req *pokerrpc.BindTableGroupChatRequest) (*pokerrpc.BindTableGroupChatResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	uid, _, ok := s.payoutForToken(s.tokenFrom(ctx, req.GetToken()))
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired session")
	}

	tableID := strings.TrimSpace(req.GetTableId())
	if tableID == "" {
		return nil, status.Error(codes.InvalidArgument, "table_id required")
	}
	table, found := s.getTable(tableID)
	if !found || table == nil {
		return nil, status.Error(codes.NotFound, "table not found")
	}
	if table.GetUser(uid.String()) == nil {
		return nil, status.Error(codes.PermissionDenied, "you are not seated at this table")
	}

	gcID := strings.ToLower(strings.TrimSpace(req.GetGcId()))
	if gcID != "" && !gcIDRe.MatchString(gcID) {
		return nil, status.Error(codes.InvalidArgument, "gc_id must be 64 hex characters")
	}

	matchID := membership.MatchID(tableID, req.GetSessionId())
	s.BindMatchGroupChat(matchID, gcID)

	publisher, bound := s.publishTarget(matchID)
	relaying := publisher != nil && bound != ""
	if gcID == "" {
		s.log.Infof("Table %s no longer relays its action log", matchID)
	} else {
		s.log.Infof("Table %s relays its action log to group chat %s (relaying=%v)", matchID, gcID, relaying)
	}

	return &pokerrpc.BindTableGroupChatResponse{
		MatchId: matchID,
		GcId:    gcID,
		// relaying reports whether frames will actually go out. A table
		// can be bound while the server has no bridge configured, and
		// saying so beats leaving a player to wonder why nothing arrives.
		Relaying: relaying,
	}, nil
}

var gcIDRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
