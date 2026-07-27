package server

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

// FramePublisher sends one game message to a table's group chat.
//
// It is an interface so the server can be run without any of it: a table with
// no group chat publishes nothing, and the tests do not need Bison Relay to
// exercise the rest.
type FramePublisher interface {
	Publish(ctx context.Context, gcID, sid, matchID string, kind schema.Kind, body any) error
}

// actionPublish holds where a match's signed log is published, if anywhere.
type actionPublish struct {
	mu        sync.RWMutex
	publisher FramePublisher
	gcByMatch map[string]string
}

func newActionPublish() *actionPublish {
	return &actionPublish{gcByMatch: make(map[string]string)}
}

// SetFramePublisher installs the publisher used to broadcast signed actions.
func (s *Server) SetFramePublisher(p FramePublisher) {
	if s.publish == nil {
		return
	}
	s.publish.mu.Lock()
	s.publish.publisher = p
	s.publish.mu.Unlock()
}

// BindMatchGroupChat associates a match with the group chat its players share.
// Publishing is a no-op for a match with no group chat, which is every match
// until one is bound.
func (s *Server) BindMatchGroupChat(matchID, gcID string) {
	if s.publish == nil {
		return
	}
	s.publish.mu.Lock()
	if gcID == "" {
		delete(s.publish.gcByMatch, matchID)
	} else {
		s.publish.gcByMatch[matchID] = gcID
	}
	s.publish.mu.Unlock()
}

func (s *Server) publishTarget(matchID string) (FramePublisher, string) {
	if s.publish == nil {
		return nil, ""
	}
	s.publish.mu.RLock()
	defer s.publish.mu.RUnlock()
	return s.publish.publisher, s.publish.gcByMatch[matchID]
}

// publishAction broadcasts a verified log entry to the table.
//
// This is what the signing was for. Until now the log has been the server's own
// record, and a player could only take its word for what the table did; a
// player who receives the entries can build the same chain and check it. The
// entries are relayed exactly as their seats signed them, and the server adds
// nothing of its own - it has no seat and no key, so there is nothing it could
// add that would be worth anything.
//
// A player who then finds their chain disagrees with the head the server
// reports on GameUpdate is holding evidence rather than an opinion.
//
// Failure is logged, not returned. The hand has already accepted this action;
// refusing it now because a message did not go out would make the table's
// progress depend on a group chat, which is exactly the dependency the CSV
// refund and the pre-signed drafts exist to avoid.
func (s *Server) publishAction(entry *gamelog.Entry, matchID string) {
	publisher, gcID := s.publishTarget(matchID)
	if publisher == nil || gcID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body := schema.Action{Entry: transcriptEntry(entry)}
	if err := publisher.Publish(ctx, gcID, matchID, matchID, schema.KindAction, body); err != nil {
		s.log.Warnf("Could not publish action for table %s: %v", matchID, err)
	}
}

// transcriptEntry renders an entry in the hex shape the schema and a stored
// transcript both use, so a message and a transcript agree field for field.
func transcriptEntry(e *gamelog.Entry) gamelog.TranscriptEntry {
	return gamelog.TranscriptEntry{
		Version:  e.Version,
		PrevHash: hex.EncodeToString(e.PrevHash[:]),
		Seq:      e.Seq,
		Hand:     e.Hand,
		Street:   e.Street.String(),
		Seat:     e.Seat,
		Signer:   hex.EncodeToString(e.Signer),
		Action:   string(e.Action),
		Amount:   e.Amount,
		Sig:      hex.EncodeToString(e.Sig),
	}
}
