// Package schema is what a table says to itself, independent of how it is
// carried.
//
// The messages here are the game's own vocabulary. They travel inside
// --gaming[…]-- frames over Bison Relay, and today the same information also
// moves over gRPC; neither transport is part of their definition. That is the
// point. The gRPC surface is transitional, so anything defined in terms of it
// would have to be defined again later, and a signature over a payload would
// have to be recomputed rather than carried.
//
// Two rules run through all of it.
//
// Identity is never a field. The sender's Bison Relay uid is the authenticated
// identity, and a payload claiming one could only ever be a way to lie about
// it. Where a message needs to say which seat acted, it carries a signature
// that proves it rather than a name that asserts it.
//
// Seat, action, amount, signature and chain head are payload, never envelope.
// If a router or a content filter ever needed to understand a fold in order to
// move it, the separation would already have failed.
package schema

import (
	"encoding/json"
	"fmt"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// Version is the game protocol version, carried as gv in the envelope and
// separate from the framing version.
const Version = 1

// Game is the routing key for poker traffic.
const Game = "poker"

// Kind identifies a message. It is a string so an unknown one reads as itself
// in a log rather than as a number nobody can place.
type Kind string

const (
	// KindRoster announces the seats a table is playing with and the
	// session keys their actions will be signed by. It is informational:
	// the authoritative roster is the one the escrow scripts commit to
	// on-chain, and a member checks this against that rather than the
	// other way round.
	KindRoster Kind = "roster"

	// KindAction is one signed entry of the action log.
	KindAction Kind = "action"

	// KindHead is a seat's signature over what it believes the log head to
	// be. Under group-chat fan-out no member sees another's stream, so
	// these are how a fork is found at all.
	KindHead Kind = "head"

	// KindResync asks for the entries a member is missing, and answers.
	KindResync      Kind = "resync"
	KindResyncReply Kind = "resync_reply"

	// KindDispute carries evidence that a seat contradicted itself. It is
	// self-contained: whoever receives it can check it without asking
	// anyone, which is what keeps it different from an accusation.
	KindDispute Kind = "dispute"
)

// Message is the envelope every payload travels in.
type Message struct {
	// V is the schema version. It is repeated inside the payload as well as
	// in the frame so a stored message stays interpretable on its own,
	// without the frame it happened to arrive in.
	V     int             `json:"v"`
	Kind  Kind            `json:"kind"`
	Match string          `json:"match"`
	Body  json.RawMessage `json:"body"`
}

// Roster is the table's seats and the keys their actions are signed with.
type Roster struct {
	Seats map[uint32]string `json:"seats"` // seat -> hex compressed session pubkey
}

// Action is one signed log entry, in the same hex-rendered shape a transcript
// uses so a message and a stored transcript agree field for field.
type Action struct {
	Entry gamelog.TranscriptEntry `json:"entry"`
}

// Head is a seat's claim about the log head.
type Head struct {
	Seq    uint64 `json:"seq"`
	Hash   string `json:"hash"`
	Seat   uint32 `json:"seat"`
	Signer string `json:"signer"`
	Sig    string `json:"sig"`
}

// Resync asks for everything after a sequence number.
type Resync struct {
	After uint64 `json:"after"`
}

// ResyncReply carries the missing entries, in order.
type ResyncReply struct {
	Entries []gamelog.TranscriptEntry `json:"entries"`
}

// Dispute is evidence that one seat signed two different things at one point in
// the log. Both entries travel whole, because the proof is the pair.
type Dispute struct {
	A gamelog.TranscriptEntry `json:"a"`
	B gamelog.TranscriptEntry `json:"b"`
	// Reason is for humans reading a log. Nothing acts on it: the proof is
	// the two entries, and a receiver that believed this field instead
	// would be trusting the accuser.
	Reason string `json:"reason,omitempty"`
}

// Encode wraps a body as a message ready to be framed.
func Encode(kind Kind, matchID string, body any) ([]byte, error) {
	if matchID == "" {
		return nil, fmt.Errorf("message must name its match")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s body: %w", kind, err)
	}
	return json.Marshal(Message{V: Version, Kind: kind, Match: matchID, Body: raw})
}

// Decode reads a message. The body is left raw so a caller can dispatch on
// Kind and unmarshal only what it understands - a message for a kind this build
// has never heard of has to be skippable, not fatal.
func Decode(blob []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	if m.V != Version {
		return nil, fmt.Errorf("message is schema version %d, this build speaks %d", m.V, Version)
	}
	if m.Match == "" {
		return nil, fmt.Errorf("message names no match")
	}
	if m.Kind == "" {
		return nil, fmt.Errorf("message has no kind")
	}
	return &m, nil
}

// Into unmarshals a message body into v.
func (m *Message) Into(v any) error {
	if err := json.Unmarshal(m.Body, v); err != nil {
		return fmt.Errorf("decode %s body: %w", m.Kind, err)
	}
	return nil
}
