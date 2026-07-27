package schema

import (
	"encoding/hex"
	"fmt"

	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// The wire shapes above are hex strings; the formation logic works in bytes and
// knows nothing about this package. These convert between them, in one place,
// so neither side has to know how the other spells a key.
//
// Conversion validates nothing beyond decoding. Whether a join is genuine is
// membership's answer, and asking it twice in two places is how the two answers
// eventually differ.

// TermsFrom renders terms for the wire.
func TermsFrom(t membership.Terms) Terms {
	return Terms{
		Game:       t.Game,
		GameVer:    t.GameVer,
		SID:        t.SID,
		BuyInAtoms: t.BuyInAtoms,
		Seats:      t.Seats,
		CSVBlocks:  t.CSVBlocks,
	}
}

// Into reads terms back.
func (t Terms) Into() membership.Terms {
	return membership.Terms{
		Game:       t.Game,
		GameVer:    t.GameVer,
		SID:        t.SID,
		BuyInAtoms: t.BuyInAtoms,
		Seats:      t.Seats,
		CSVBlocks:  t.CSVBlocks,
	}
}

// JoinFrom renders a join for the wire.
func JoinFrom(j *membership.Join) Join {
	if j == nil {
		return Join{}
	}
	return Join{
		Key: hex.EncodeToString(j.Key),
		Sig: hex.EncodeToString(j.Sig),
	}
}

// Into reads a join back. The signature it carries is checked by membership
// against the terms the reader holds, not here.
func (j Join) Into() (*membership.Join, error) {
	key, err := hex.DecodeString(j.Key)
	if err != nil {
		return nil, fmt.Errorf("join key: %w", err)
	}
	sig, err := hex.DecodeString(j.Sig)
	if err != nil {
		return nil, fmt.Errorf("join signature: %w", err)
	}
	return &membership.Join{Key: key, Sig: sig}, nil
}

// CommitFrom renders a commit for the wire.
func CommitFrom(c *membership.Commit) Commit {
	if c == nil {
		return Commit{}
	}
	return Commit{
		Roster: hex.EncodeToString(c.Roster[:]),
		Signer: hex.EncodeToString(c.Signer),
		Sig:    hex.EncodeToString(c.Sig),
	}
}

// Into reads a commit back.
func (c Commit) Into() (*membership.Commit, error) {
	roster, err := hex.DecodeString(c.Roster)
	if err != nil {
		return nil, fmt.Errorf("commit roster: %w", err)
	}
	if len(roster) != 32 {
		return nil, fmt.Errorf("commit roster is %d bytes, want 32", len(roster))
	}
	signer, err := hex.DecodeString(c.Signer)
	if err != nil {
		return nil, fmt.Errorf("commit signer: %w", err)
	}
	sig, err := hex.DecodeString(c.Sig)
	if err != nil {
		return nil, fmt.Errorf("commit signature: %w", err)
	}
	out := &membership.Commit{Signer: signer, Sig: sig}
	copy(out.Roster[:], roster)
	return out, nil
}

// RosterFrom renders the membership a peer holds, with the joins it was
// computed from, so a peer that missed one can check its way to the same
// answer instead of believing this one.
func RosterFrom(terms membership.Terms, seats map[uint32][]byte, joins []*membership.Join) Roster {
	out := Roster{
		Seats: make(map[uint32]string, len(seats)),
		Joins: make([]Join, 0, len(joins)),
	}
	for seat, key := range seats {
		out.Seats[seat] = hex.EncodeToString(key)
	}
	for _, j := range joins {
		out.Joins = append(out.Joins, JoinFrom(j))
	}
	t := TermsFrom(terms)
	out.Terms = &t
	return out
}

// SeatKeys reads a roster's seats back as bytes, which is the shape the action
// log and the escrow scripts both want.
func (r Roster) SeatKeys() (map[uint32][]byte, error) {
	out := make(map[uint32][]byte, len(r.Seats))
	for seat, h := range r.Seats {
		key, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("seat %d key: %w", seat, err)
		}
		out[seat] = key
	}
	return out, nil
}
