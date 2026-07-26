package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/poker"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
	"github.com/vctt94/pokerbisonrelay/pkg/server/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// actionLogs holds one signed action chain per match.
//
// A chain exists only where there is an escrow roster to verify against. The
// roster supplies the seat keys, and it is the one membership list everyone
// whose money is at stake has agreed to on-chain; a table playing for nothing
// has no keys and therefore no log. That is not a gap - the log exists to make
// a contested pot auditable, and there is nothing to contest without one.
// A nil actionLogs means no logging at all, which every accessor tolerates.
// Servers are constructed directly in several tests, and a missing log store
// should degrade to "this table keeps no log" rather than crash the game.
type actionLogs struct {
	mu     sync.Mutex
	chains map[string]*gamelog.Chain
	// hands maps a match's hand number to its database row, so actions can
	// be filed against the hand they belong to. A hand row is created by
	// the first action of that hand rather than at deal time: the log is
	// what is being persisted, and a hand nobody acted in has no history.
	hands map[handKey]int64
	ord   map[handKey]int
}

// handKey identifies one hand of one match.
type handKey struct {
	matchID string
	hand    uint64
}

func newActionLogs() *actionLogs {
	return &actionLogs{
		chains: make(map[string]*gamelog.Chain),
		hands:  make(map[handKey]int64),
		ord:    make(map[handKey]int),
	}
}

// chainFor returns the match's chain, creating it from the escrow roster the
// first time an action arrives. It returns nil when the table keeps no log.
func (s *Server) chainFor(matchID string) (*gamelog.Chain, error) {
	if s.actionLogs == nil {
		return nil, nil
	}
	s.actionLogs.mu.Lock()
	defer s.actionLogs.mu.Unlock()

	if c, ok := s.actionLogs.chains[matchID]; ok {
		return c, nil
	}

	s.referee.mu.RLock()
	roster := s.referee.rosters[matchID]
	seats := gamelog.Roster{}
	closed := roster.closed()
	if roster != nil {
		for seat, key := range roster.Seats {
			seats[seat] = append([]byte(nil), key...)
		}
	}
	s.referee.mu.RUnlock()

	// An open roster is not yet the table's final membership, so a chain
	// built from it would verify against keys that could still change.
	if !closed || len(seats) < 2 {
		return nil, nil
	}

	c, err := gamelog.NewChain(matchID, seats)
	if err != nil {
		return nil, fmt.Errorf("start action log: %w", err)
	}
	s.actionLogs.chains[matchID] = c
	return c, nil
}

// logHead reports the chain position a client needs before it can sign its next
// action. The zero value means the table keeps no log.
func (s *Server) logHead(matchID string) (head []byte, seq uint64) {
	if s.actionLogs == nil {
		return nil, 0
	}
	s.actionLogs.mu.Lock()
	c := s.actionLogs.chains[matchID]
	s.actionLogs.mu.Unlock()
	if c == nil {
		return nil, 0
	}
	h, n := c.Head()
	return h[:], n
}

// dropActionLog forgets a match's chain once it has settled.
func (s *Server) dropActionLog(matchID string) {
	if s.actionLogs == nil {
		return
	}
	s.actionLogs.mu.Lock()
	delete(s.actionLogs.chains, matchID)
	for k := range s.actionLogs.hands {
		if k.matchID == matchID {
			delete(s.actionLogs.hands, k)
			delete(s.actionLogs.ord, k)
		}
	}
	s.actionLogs.mu.Unlock()
}

// recordAction verifies a player's signed action and appends it to the match's
// log, before the engine is allowed to act on it.
//
// The order matters. An action the log will not accept must not reach the game,
// or the engine's state and the record of how it got there would diverge - and
// the record is the part a third party can check.
func (s *Server) recordAction(table *poker.Table, playerID string, signed *pokerrpc.SignedAction, action gamelog.Action, amount int64) error {
	matchID := table.GetConfig().ID
	chain, err := s.chainFor(matchID)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if chain == nil {
		// No roster, no log. Refuse a signature we have no way to check
		// rather than accept it unverified and imply it was.
		if signed != nil {
			return status.Error(codes.FailedPrecondition,
				"this table keeps no action log; open an escrow first")
		}
		return nil
	}
	if signed == nil {
		return status.Error(codes.InvalidArgument,
			"this table's actions must be signed")
	}

	user := table.GetUser(playerID)
	if user == nil {
		return status.Error(codes.FailedPrecondition, "player not at table")
	}
	seat := uint32(user.TableSeat)
	if signed.GetSeat() != seat {
		return status.Errorf(codes.InvalidArgument,
			"action is signed for seat %d but you hold seat %d", signed.GetSeat(), seat)
	}
	// A signature over a different action than the one being asked for would
	// leave the log saying one thing and the game doing another.
	if gamelog.Action(signed.GetAction()) != action {
		return status.Errorf(codes.InvalidArgument,
			"action is signed as %q but was sent as %q", signed.GetAction(), action)
	}
	if signed.GetAmount() != amount {
		return status.Errorf(codes.InvalidArgument,
			"action is signed for %d but was sent for %d", signed.GetAmount(), amount)
	}

	entry, err := entryFromProto(signed)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "malformed signed action: %v", err)
	}

	s.actionLogs.mu.Lock()
	err = chain.Append(entry)
	s.actionLogs.mu.Unlock()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "action log: %v", err)
	}

	s.log.Debugf("Logged %s by seat %d at seq %d on table %s",
		action, seat, entry.Seq, matchID)

	// The signed log is the authority; the database is a queryable copy of
	// it. A write that fails must not undo an action the table has already
	// agreed to, so it is reported and not returned.
	if err := s.persistAction(entry, matchID); err != nil {
		s.log.Warnf("Could not persist action for table %s: %v", matchID, err)
	}
	return nil
}

// persistAction files a verified entry against its hand in the database, which
// is what finally populates the hand-history tables.
func (s *Server) persistAction(e *gamelog.Entry, matchID string) error {
	if s.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	key := handKey{matchID: matchID, hand: e.Hand}

	s.actionLogs.mu.Lock()
	handID, known := s.actionLogs.hands[key]
	s.actionLogs.mu.Unlock()

	if !known {
		id, err := s.db.BeginHand(ctx, &db.Hand{
			TableID:   matchID,
			HandNo:    int64(e.Hand),
			StartedAt: time.Now(),
		})
		if err != nil {
			return fmt.Errorf("begin hand %d: %w", e.Hand, err)
		}
		handID = id
		s.actionLogs.mu.Lock()
		s.actionLogs.hands[key] = handID
		s.actionLogs.mu.Unlock()
	}

	s.actionLogs.mu.Lock()
	s.actionLogs.ord[key]++
	ord := s.actionLogs.ord[key]
	s.actionLogs.mu.Unlock()

	_, err := s.db.AppendAction(ctx, &db.Action{
		HandID:    handID,
		Ord:       ord,
		Street:    e.Street.String(),
		ActorSeat: int(e.Seat),
		Action:    string(e.Action),
		Amount:    e.Amount,
		IsAllIn:   e.Action == gamelog.ActionAllIn,
		CreatedAt: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("append action: %w", err)
	}
	return nil
}

// entryFromProto converts a wire action into a log entry. The signature is not
// checked here - gamelog rebuilds the bytes it covers from these fields and
// checks it there, so nothing on the wire is ever trusted as the signed form.
func entryFromProto(a *pokerrpc.SignedAction) (*gamelog.Entry, error) {
	if a == nil {
		return nil, fmt.Errorf("no action")
	}
	if len(a.GetPrevHash()) != 32 {
		return nil, fmt.Errorf("prev_hash is %d bytes, want 32", len(a.GetPrevHash()))
	}
	if a.GetStreet() > 0xff {
		return nil, fmt.Errorf("street %d out of range", a.GetStreet())
	}
	e := &gamelog.Entry{
		Version: uint16(a.GetVersion()),
		Seq:     a.GetSeq(),
		Hand:    a.GetHand(),
		Street:  gamelog.Street(a.GetStreet()),
		Seat:    a.GetSeat(),
		Signer:  a.GetSigner(),
		Action:  gamelog.Action(a.GetAction()),
		Amount:  a.GetAmount(),
		Sig:     a.GetSig(),
	}
	copy(e.PrevHash[:], a.GetPrevHash())
	return e, nil
}

// ExportActionLog renders a match's signed log as a transcript. It is the
// artifact a player hands to somebody who was not at the table: every entry
// carries its own signature and its link to the one before, so it can be
// checked without trusting whoever produced it.
func (s *Server) ExportActionLog(matchID string) ([]byte, error) {
	if s.actionLogs == nil {
		return nil, fmt.Errorf("no action log for match %s", matchID)
	}
	s.actionLogs.mu.Lock()
	c := s.actionLogs.chains[matchID]
	s.actionLogs.mu.Unlock()
	if c == nil {
		return nil, fmt.Errorf("no action log for match %s", matchID)
	}
	return c.Marshal()
}

// actionLogSummary is a short description of a chain, for logs and debugging.
func (s *Server) actionLogSummary(matchID string) string {
	if s.actionLogs == nil {
		return "none"
	}
	s.actionLogs.mu.Lock()
	c := s.actionLogs.chains[matchID]
	s.actionLogs.mu.Unlock()
	if c == nil {
		return "none"
	}
	h, seq := c.Head()
	return fmt.Sprintf("seq=%d head=%s entries=%d", seq, hex.EncodeToString(h[:8]), c.Len())
}
