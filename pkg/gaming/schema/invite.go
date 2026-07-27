package schema

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// A game invite is an ordinary chat message, not a wire frame.
//
// Gameplay rides the hidden --gaming[...]-- envelope because it is between the
// running games and must never read as conversation. An invitation is the
// opposite in both respects. It is addressed to a person, so it has to stay
// legible to whoever receives it - including someone whose client knows nothing
// about games, who should see an invitation they can act on rather than
// silence. And it is what *starts* a game, so at the moment it arrives there is
// no game running to deliver it to.
//
//	gaming://poker/table?buyin=10000000&seats=6&sid=<hex>
//
// The host is the game id, the same routing key the wire envelope carries, so
// one renderer in a host serves every game. Only the terms a host can present
// honestly are standardised - what it costs and how many seats - and anything
// else is the game's own business.
const InviteScheme = "gaming"

// InviteKindTable offers a seat at a table.
const InviteKindTable = "table"

var (
	inviteGameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	inviteKindRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

// Invite is an offer to join a game.
type Invite struct {
	Game string
	Kind string
	// BuyInAtoms is the stake a seat costs. Zero states no terms.
	BuyInAtoms uint64
	// Seats is how many the table holds. Zero states none.
	Seats uint32
	// SID identifies the session the invite refers to.
	SID string
}

// String renders the invite as the link that goes in a chat message.
func (i Invite) String() (string, error) {
	game := strings.ToLower(strings.TrimSpace(i.Game))
	kind := strings.ToLower(strings.TrimSpace(i.Kind))
	if !inviteGameRE.MatchString(game) {
		return "", fmt.Errorf("invite game %q is not a routing key", i.Game)
	}
	if !inviteKindRE.MatchString(kind) {
		return "", fmt.Errorf("invite kind %q is not valid", i.Kind)
	}

	q := url.Values{}
	if i.BuyInAtoms > 0 {
		q.Set("buyin", strconv.FormatUint(i.BuyInAtoms, 10))
	}
	if i.Seats > 0 {
		q.Set("seats", strconv.FormatUint(uint64(i.Seats), 10))
	}
	if i.SID != "" {
		q.Set("sid", i.SID)
	}

	out := fmt.Sprintf("%s://%s/%s", InviteScheme, game, kind)
	if encoded := q.Encode(); encoded != "" {
		out += "?" + encoded
	}
	return out, nil
}

// ParseInvite reads an invite link back.
//
// It exists so the format has one definition with a round trip that can be
// tested, rather than a writer here and a reader in a host that drift until
// somebody's invitation stops rendering.
func ParseInvite(link string) (Invite, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return Invite{}, fmt.Errorf("parse invite: %w", err)
	}
	if u.Scheme != InviteScheme {
		return Invite{}, fmt.Errorf("not a %s:// link", InviteScheme)
	}

	game := strings.ToLower(u.Host)
	kind := strings.ToLower(strings.Trim(u.Path, "/"))
	if !inviteGameRE.MatchString(game) {
		return Invite{}, fmt.Errorf("invite names no game")
	}
	if !inviteKindRE.MatchString(kind) {
		return Invite{}, fmt.Errorf("invite names no kind")
	}

	inv := Invite{Game: game, Kind: kind, SID: u.Query().Get("sid")}
	if v := u.Query().Get("buyin"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return Invite{}, fmt.Errorf("invite buy-in %q is not a number of atoms", v)
		}
		inv.BuyInAtoms = n
	}
	if v := u.Query().Get("seats"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Invite{}, fmt.Errorf("invite seats %q is not a number", v)
		}
		inv.Seats = uint32(n)
	}
	return inv, nil
}
