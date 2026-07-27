package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

const testGC = "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"

// hub stands in for dcrpulse: it takes a frame on /gaming/send and fans it out
// to every other member of the group chat, the way Bison Relay does - as
// separate deliveries, with no shared transcript and no ordering.
type hub struct {
	mu    sync.Mutex
	peers map[string]*plugin // token -> peer
	srv   *httptest.Server
}

func newHub(t *testing.T) *hub {
	t.Helper()
	h := &hub{peers: make(map[string]*plugin)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gaming/send" {
			http.NotFound(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		var req struct {
			GCID  string `json:"gcid"`
			Frame string `json:"frame"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)

		h.mu.Lock()
		targets := make([]*plugin, 0, len(h.peers))
		for tok, p := range h.peers {
			if tok != token {
				targets = append(targets, p)
			}
		}
		h.mu.Unlock()

		// Deliver out of band. A member receiving a frame usually sends
		// one of its own, and doing that on this call's stack would
		// make the fan-out reentrant in a way the real host is not.
		for _, p := range targets {
			go p.router.HandleGCMessage(req.GCID, token, req.Frame, time.Now())
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hub) join(t *testing.T, name string) *plugin {
	t.Helper()
	dir := t.TempDir()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	p, err := newPlugin(context.Background(), h.srv.URL+"/gaming", name, id, newStore(dir))
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}
	h.mu.Lock()
	h.peers[name] = p
	h.mu.Unlock()
	return p
}

func testInvite(seats uint32) schema.Invite {
	return schema.Invite{
		Game:       schema.Game,
		Kind:       schema.InviteKindTable,
		BuyInAtoms: 10_000_000,
		Seats:      seats,
		SID:        "0123456789abcdef",
		CSVBlocks:  64,
	}
}

func acceptInvite(t *testing.T, p *plugin, inv schema.Invite) {
	t.Helper()
	link, err := inv.String()
	if err != nil {
		t.Fatalf("render invite: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"invite": link, "gcid": testGC})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/table/join", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+p.token)
	p.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("join returned %d: %s", rec.Code, rec.Body.String())
	}
}

// waitFor polls until every peer reports the state, which is how a test of
// something asynchronous stays honest about being asynchronous.
func waitFor(t *testing.T, want membership.State, peers ...*plugin) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		done := true
		for _, p := range peers {
			for _, s := range p.tables.snapshots() {
				if s.State != want.String() {
					done = false
				}
			}
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			for i, p := range peers {
				t.Logf("peer %d: %+v", i, p.tables.snapshots())
			}
			t.Fatalf("peers never reached %s", want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The whole point: two processes that were only ever handed the same invitation
// arrive at the same membership, with nobody deciding it for them.
func TestTwoPeersFormATableFromAnInviteAlone(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	a, b := h.join(t, "tok-a"), h.join(t, "tok-b")

	acceptInvite(t, a, inv)
	acceptInvite(t, b, inv)

	waitFor(t, membership.Settled, a, b)

	sa, sb := a.tables.snapshots()[0], b.tables.snapshots()[0]
	if sa.MatchID == "" {
		t.Fatal("settled with no match id")
	}
	if sa.MatchID != sb.MatchID {
		t.Fatalf("peers settled different memberships:\n a %s\n b %s", sa.MatchID, sb.MatchID)
	}
	if sa.Joined != 2 || sb.Joined != 2 {
		t.Fatalf("joined counts are %d and %d, want 2", sa.Joined, sb.Joined)
	}
}

func TestSixPeersFormATableFromAnInviteAlone(t *testing.T) {
	h := newHub(t)
	inv := testInvite(6)

	peers := make([]*plugin, 0, 6)
	for i := range 6 {
		peers = append(peers, h.join(t, fmt.Sprintf("tok-%d", i)))
	}
	for _, p := range peers {
		acceptInvite(t, p, inv)
	}

	waitFor(t, membership.Settled, peers...)

	want := peers[0].tables.snapshots()[0].MatchID
	for i, p := range peers {
		if got := p.tables.snapshots()[0].MatchID; got != want {
			t.Fatalf("peer %d settled a different membership", i)
		}
	}
}

// Three answering a two seat table is not settled by anybody choosing, and it
// is not a seat lottery either - a key cannot be ground to win one.
//
// What must hold is that the table never splits: no two peers end up at
// different tables under one invitation. Which of the two outcomes happens
// depends on what arrived when. Either two peers complete before the third's
// join reaches them and the third is left out, or somebody sees all three
// joins first and refuses outright. That indeterminacy is the honest
// consequence of binding as soon as a table looks full, and a closed admission
// window every peer derives the same way is what would remove it.
func TestAnOversubscribedTableNeverSplits(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)

	peers := make([]*plugin, 0, 3)
	for i := range 3 {
		peers = append(peers, h.join(t, fmt.Sprintf("tok-%d", i)))
	}
	for _, p := range peers {
		acceptInvite(t, p, inv)
	}

	// Let it come to rest: this is about where it lands, not how fast.
	time.Sleep(2 * time.Second)

	settled := map[string]int{}
	states := make([]string, 0, len(peers))
	for _, p := range peers {
		for _, s := range p.tables.snapshots() {
			states = append(states, s.State)
			if s.State == membership.Settled.String() {
				settled[s.MatchID]++
			}
			if s.State == membership.Aborted.String() && s.Reason == "" {
				t.Fatal("aborted without saying why")
			}
		}
	}

	if len(settled) > 1 {
		t.Fatalf("one invitation produced %d different tables: %v (states %v)", len(settled), settled, states)
	}
	for id, n := range settled {
		if n > int(inv.Seats) {
			t.Fatalf("%d peers settled a %d seat table (%s)", n, inv.Seats, id)
		}
	}
	if len(settled) == 1 {
		// Somebody has to have been left out; a third seat cannot have
		// appeared.
		for _, n := range settled {
			if n == len(peers) {
				t.Fatal("all three peers settled a two seat table")
			}
		}
	}
}

// Nothing is admitted for a session this process was not told to join. Gaming
// frames never reach the user, so an unsolicited one is a silent way to fill
// memory rather than something anybody would notice.
func TestOnlyJoinedSessionsAreAdmitted(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok-a")

	if p.tables.authorized("0123456789abcdef", "somebody") {
		t.Fatal("a session nobody joined was authorized")
	}
	acceptInvite(t, p, testInvite(2))
	if !p.tables.authorized("0123456789abcdef", "somebody") {
		t.Fatal("a joined session was not authorized")
	}
	if p.tables.authorized("beefbeefbeefbeef", "somebody") {
		t.Fatal("joining one session authorized another")
	}

	// Leaving revokes it, so a table stops costing anything.
	if !p.tables.leave("0123456789abcdef") {
		t.Fatal("leaving a joined table reported nothing to leave")
	}
	if p.tables.authorized("0123456789abcdef", "somebody") {
		t.Fatal("a session stayed authorized after leaving")
	}
}

// The session is ours; another group chat is not. Frames for one table arriving
// in a different conversation are not this table's, whoever sent them.
func TestFramesFromAnotherGroupChatAreIgnored(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok-a")
	acceptInvite(t, p, testInvite(2))

	before := p.tables.snapshots()[0].Joined
	other := strings.Repeat("ff", 32)

	priv, err := p.id.sessionKey("beef")
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	terms := membership.Terms{
		Game: schema.Game, GameVer: schema.Version, SID: "0123456789abcdef",
		BuyInAtoms: 10_000_000, Seats: 2, CSVBlocks: 64,
	}
	j, err := membership.SignJoin(terms, priv)
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	blob, err := schema.Encode(schema.KindJoin, terms.SID, schema.JoinFrom(j))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	out := p.tables.deliver(transport.Delivery{
		GCID: other, Sender: "somebody", SID: terms.SID, Msg: msg,
	})
	if out != nil {
		t.Fatal("a frame from another group chat produced a reply")
	}
	if got := p.tables.snapshots()[0].Joined; got != before {
		t.Fatalf("holding %d joins, want the original %d", got, before)
	}
}

// Every route but health needs the token the host issued, and refusal looks
// like the route is not there.
func TestDrivingThisProcessNeedsTheHostsToken(t *testing.T) {
	p := testPlugin(t)
	for _, path := range []string{"/tables", "/cmd", "/table/join", "/table/leave"} {
		rec := httptest.NewRecorder()
		p.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s without a token returned %d, want 404", path, rec.Code)
		}

		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer wrong")
		p.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with the wrong token returned %d, want 404", path, rec.Code)
		}
	}
}

func TestJoiningRefusesInvitationsToNothing(t *testing.T) {
	p := testPlugin(t)
	for _, tc := range []struct{ name, invite, gcid string }{
		{"not an invite", "hello", testGC},
		{"another game", "gaming://chess/table?seats=2&sid=ab&csv=64", testGC},
		{"no seats", "gaming://poker/table?sid=ab&csv=64", testGC},
		{"no timelock", "gaming://poker/table?seats=2&sid=ab", testGC},
		{"bad group chat", "gaming://poker/table?seats=2&sid=ab&csv=64", "nothex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"invite": tc.invite, "gcid": tc.gcid})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/table/join", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer "+p.token)
			p.routes().ServeHTTP(rec, req)

			if rec.Code == http.StatusOK {
				t.Fatalf("accepted: %s", rec.Body.String())
			}
		})
	}
}

// deliverJoin hands a table somebody else's join, as the router would.
func deliverJoin(t *testing.T, p *plugin, terms membership.Terms, priv *secp256k1.PrivateKey) {
	t.Helper()
	j, err := membership.SignJoin(terms, priv)
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	blob, err := schema.Encode(schema.KindJoin, terms.SID, schema.JoinFrom(j))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p.tables.deliver(transport.Delivery{GCID: testGC, Sender: "them", SID: terms.SID, Msg: msg})
}

func inviteTerms(inv schema.Invite) membership.Terms {
	return membership.Terms{
		Game: inv.Game, GameVer: schema.Version, SID: inv.SID,
		BuyInAtoms: inv.BuyInAtoms, Seats: inv.Seats, CSVBlocks: inv.CSVBlocks,
	}
}

// restart builds a second process over the same data directory, which is what
// makes it the same player: the seed is there, so the session keys it derives
// are the ones it held before.
func restart(t *testing.T, dir, token string) *plugin {
	t.Helper()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	p, err := newPlugin(context.Background(), "http://host/gaming", token, id, newStore(dir))
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}
	return p
}

// The one that matters. A session key is derived from the seed and the session,
// so a restart re-derives the same key - and if it then bound to some other
// membership it would have signed two contradictory commits with one key, which
// is exactly the proof of equivocation the protocol runs on. An honest player
// would have framed themselves by rebooting.
func TestARestartCommitsToTheSameMembership(t *testing.T) {
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)

	other, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	before := restart(t, dir, "tok")
	acceptInvite(t, before, inv)
	deliverJoin(t, before, terms, other)

	first := before.tables.snapshots()[0]
	if first.State != membership.Committed.String() && first.State != membership.Settled.String() {
		t.Fatalf("state is %s, want it bound", first.State)
	}
	if first.MatchID == "" {
		t.Fatal("bound with no membership")
	}

	// Same directory, so the same player comes back.
	after := restart(t, dir, "tok")
	acceptInvite(t, after, inv)

	second := after.tables.snapshots()[0]
	if second.MatchID != first.MatchID {
		t.Fatalf("a restart committed to %s, having already committed to %s",
			second.MatchID, first.MatchID)
	}
	if second.State != first.State {
		t.Fatalf("state came back as %s, was %s", second.State, first.State)
	}
	if second.Joined != first.Joined {
		t.Fatalf("came back holding %d joins, had %d", second.Joined, first.Joined)
	}
}

// Aborted is terminal, and has to stay terminal across a restart: a commit
// arriving after everyone else gave up would otherwise put this process back
// into a membership nobody is bound to.
func TestARestartWillNotRejoinASessionThatEnded(t *testing.T) {
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)

	p := restart(t, dir, "tok")
	acceptInvite(t, p, inv)

	// Two more answer a two seat table, arriving together in somebody's
	// assertion, so the table is over-full before anyone binds.
	joins := make([]*membership.Join, 0, 2)
	for range 2 {
		priv, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		j, err := membership.SignJoin(terms, priv)
		if err != nil {
			t.Fatalf("sign join: %v", err)
		}
		joins = append(joins, j)
	}
	blob, err := schema.Encode(schema.KindRoster, terms.SID,
		schema.RosterFrom(terms, map[uint32][]byte{}, joins))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p.tables.deliver(transport.Delivery{GCID: testGC, Sender: "them", SID: terms.SID, Msg: msg})
	if got := p.tables.snapshots()[0].State; got != membership.Aborted.String() {
		t.Fatalf("state is %s, want aborted", got)
	}

	link, _ := inv.String()
	body, _ := json.Marshal(map[string]string{"invite": link, "gcid": testGC})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/table/join", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer tok")
	restart(t, dir, "tok").routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a session that ended was rejoined after a restart: %s", rec.Body.String())
	}
}
