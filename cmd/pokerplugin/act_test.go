package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/replay"
)

// The route a person actually uses.
//
// Everything below this is tested between objects in one process. These go
// through the HTTP surface, because that is what the host calls and a decision
// that cannot be expressed there cannot be taken at all.

func post(t *testing.T, p *plugin, path string, body any) (int, string) {
	t.Helper()
	blob, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(blob)))
	req.Header.Set("Authorization", "Bearer "+p.token)
	p.routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func get(t *testing.T, p *plugin, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	p.routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// A table nobody is dealing at answers plainly rather than pretending.
//
// The difference matters: "not dealing yet" is something a caller waits out,
// and a blank hand is something it would show somebody.
func TestActAndHandRefuseATableThatIsNotDealing(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	a := h.join(t, "tok-a")
	acceptInvite(t, a, inv)

	code, body := get(t, a, "/table/hand?sid="+inv.SID)
	if code != http.StatusBadRequest {
		t.Fatalf("hand returned %d: %s", code, body)
	}
	if !strings.Contains(body, "not dealing") {
		t.Fatalf("hand said %q, want it to say the table is not dealing", body)
	}

	code, body = post(t, a, "/table/act", map[string]any{"sid": inv.SID, "action": "check"})
	if code != http.StatusBadRequest {
		t.Fatalf("act returned %d: %s", code, body)
	}
	if !strings.Contains(body, "not dealing") {
		t.Fatalf("act said %q, want it to say the table is not dealing", body)
	}

	// And a session this player is not at is refused rather than answered.
	code, _ = get(t, a, "/table/hand?sid=nosuchsession")
	if code != http.StatusBadRequest {
		t.Fatalf("hand at an unknown session returned %d", code)
	}
}

// An action that is not one must be refused at the door, before anything is
// signed.
func TestActRefusesWhatIsNotAnAction(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	a := h.join(t, "tok-a")
	acceptInvite(t, a, inv)

	for _, action := range []string{"", "shove", "muck", "raise-all"} {
		code, body := post(t, a, "/table/act",
			map[string]any{"sid": inv.SID, "action": action})
		if code == http.StatusOK {
			t.Fatalf("act accepted %q: %s", action, body)
		}
	}
	if code, _ := post(t, a, "/table/act", "not an object"); code == http.StatusOK {
		t.Fatal("act accepted a body that is not a request")
	}
}

// GET is not a way to act, and POST is not a way to look.
func TestActAndHandUseTheRightMethods(t *testing.T) {
	h := newHub(t)
	a := h.join(t, "tok-a")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/table/act", nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	a.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /table/act returned %d, want 405", rec.Code)
	}
}

// The legal moves offered to a player have to be moves the rules accept, or a
// caller shows somebody a button that throws their hand away.
func TestTheLegalMovesOfferedAreOnesTheRulesAccept(t *testing.T) {
	tbl := &replay.Table{
		Match:    "m",
		Seats:    []string{"a", "b"},
		Stacks:   []int64{1000, 1000},
		Schedule: replay.Schedule{Levels: []replay.Blinds{{Small: 10, Big: 20}}},
	}
	st, err := replay.StartHand(tbl, 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Walk a few spots and check every offer against the reducer itself.
	for step := 0; step < 6 && !st.Done; step++ {
		seat := st.ToAct
		if seat < 0 {
			break
		}
		legal := legalActions(st, seat)
		if len(legal) == 0 {
			t.Fatalf("step %d: seat %d was offered nothing", step, seat)
		}
		for _, name := range legal {
			action := gamelog.Action(name)
			amount := int64(0)
			switch action {
			case gamelog.ActionBet:
				amount = st.MinRaise
			case gamelog.ActionRaise:
				amount = st.Bet + st.MinRaise
			case gamelog.ActionAllIn:
				amount = st.Seats[seat].Stack + st.Seats[seat].Committed
			}
			e := &gamelog.Entry{
				Version: gamelog.Version, Seq: st.Seq + 1, Hand: st.Hand,
				Street: st.Street, Seat: uint32(seat), Action: action, Amount: amount,
			}
			if _, err := replay.Apply(st, e); err != nil {
				t.Fatalf("step %d: %q was offered to seat %d and the rules refused it: %v",
					step, name, seat, err)
			}
		}
		// Take the first offer and move on.
		e := &gamelog.Entry{
			Version: gamelog.Version, Seq: st.Seq + 1, Hand: st.Hand,
			Street: st.Street, Seat: uint32(seat), Action: gamelog.Action(legal[0]),
		}
		next, err := replay.Apply(st, e)
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		st = next
	}
}

// Folding is offered last, because it is the one move that is never the answer
// to a question the player was not asked.
func TestFoldingIsOfferedLast(t *testing.T) {
	tbl := &replay.Table{
		Match:    "m",
		Seats:    []string{"a", "b"},
		Stacks:   []int64{1000, 1000},
		Schedule: replay.Schedule{Levels: []replay.Blinds{{Small: 10, Big: 20}}},
	}
	st, err := replay.StartHand(tbl, 1, 0, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	legal := legalActions(st, st.ToAct)
	if len(legal) == 0 {
		t.Fatal("no moves offered")
	}
	if legal[len(legal)-1] != string(gamelog.ActionFold) {
		t.Fatalf("moves offered as %v, want fold last", legal)
	}
	// And a seat that is not to act is offered nothing at all.
	if got := legalActions(st, 1-st.ToAct); got != nil {
		t.Fatalf("a seat out of turn was offered %v", got)
	}
}

// A card has to read as the card it is. The encoding is suit-major and lives in
// one place; this checks the place it is read from agrees with it.
func TestCardsReadAsThemselves(t *testing.T) {
	for _, tc := range []struct {
		card int
		want string
	}{
		{0, "2s"}, {12, "As"},
		{13, "2h"}, {25, "Ah"},
		{26, "2d"}, {38, "Ad"},
		{39, "2c"}, {51, "Ac"},
	} {
		if got := deck.Card(tc.card).String(); got != tc.want {
			t.Fatalf("card %d reads as %q, want %q", tc.card, got, tc.want)
		}
	}
	// Every card reads as something, and no two read alike - a collision
	// would show two seats the same card and nothing downstream would care.
	seen := map[string]int{}
	for i := range deck.Size {
		name := deck.Card(i).String()
		if prev, ok := seen[name]; ok {
			t.Fatalf("cards %d and %d both read as %q", prev, i, name)
		}
		seen[name] = i
	}
	if len(seen) != deck.Size {
		t.Fatalf("the deck reads as %d names, want %d", len(seen), deck.Size)
	}
	// And a card that is not one says so rather than indexing off the end.
	if got := deck.Card(deck.Size).String(); !strings.Contains(got, "52") {
		t.Fatalf("a card past the end reads as %q", got)
	}
}
