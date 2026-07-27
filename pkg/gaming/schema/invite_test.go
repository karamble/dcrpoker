package schema

import "testing"

func TestInviteRoundTrips(t *testing.T) {
	want := Invite{
		Game: Game, Kind: InviteKindTable,
		BuyInAtoms: 10_000_000, Seats: 6, SID: "0123456789abcdef",
	}
	link, err := want.String()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := ParseInvite(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	if got != want {
		t.Fatalf("round trip lost something: %+v != %+v", got, want)
	}
}

// An invite with no terms is still an invitation, and must stay renderable.
func TestInviteWithoutTerms(t *testing.T) {
	link, err := Invite{Game: Game, Kind: InviteKindTable}.String()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if link != "gaming://poker/table" {
		t.Fatalf("link is %q", link)
	}
	got, err := ParseInvite(link)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.BuyInAtoms != 0 || got.Seats != 0 || got.SID != "" {
		t.Fatalf("terms invented from nothing: %+v", got)
	}
}

func TestInviteRejectsMalformed(t *testing.T) {
	for name, inv := range map[string]Invite{
		"no game":        {Kind: InviteKindTable},
		"no kind":        {Game: Game},
		"game not a key": {Game: "Poker!", Kind: InviteKindTable},
	} {
		if _, err := inv.String(); err == nil {
			t.Errorf("%s should not render", name)
		}
	}

	for name, link := range map[string]string{
		"another scheme":     "https://poker/table",
		"no game":            "gaming:///table",
		"no kind":            "gaming://poker",
		"buyin not a number": "gaming://poker/table?buyin=lots",
		"seats not a number": "gaming://poker/table?seats=many",
		"not a link":         "hello",
	} {
		if _, err := ParseInvite(link); err == nil {
			t.Errorf("%s should not parse: %q", name, link)
		}
	}
}

// The host's renderer matches on this shape, so a change here that the host
// does not follow stops invitations rendering. This pins the literal.
func TestInviteLinkShapeIsStable(t *testing.T) {
	link, err := Invite{
		Game: Game, Kind: InviteKindTable,
		BuyInAtoms: 10_000_000, Seats: 6, SID: "abc123",
	}.String()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	const want = "gaming://poker/table?buyin=10000000&seats=6&sid=abc123"
	if link != want {
		t.Fatalf("invite link is %q, want %q - the host's parser matches on this", link, want)
	}
}
