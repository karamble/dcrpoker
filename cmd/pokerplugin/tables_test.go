package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	dcrwire "github.com/decred/dcrd/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

const testGC = "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"

// hub stands in for dcrpulse: it takes a frame on /gaming/send and fans it out
// to every other member of the group chat, the way Bison Relay does - as
// separate deliveries, with no shared transcript and no ordering.
type hub struct {
	mu    sync.Mutex
	peers map[string]*plugin // token -> peer
	// bonds is the chain, as far as these tests need one: which outpoint
	// pays which script. A join whose bond is not in here is a join with no
	// deposit behind it, which is exactly what must be refused.
	bonds map[string]string
	// muted is peers that have stopped, in both directions. A player who
	// walks away does not announce it, and the protocol never learns it -
	// it only ever infers it from an obligation that stands while the chain
	// moves. So a test cannot ask a peer to stop; it takes it off the wire.
	muted map[string]bool
	// sent is every transaction this chain was asked to relay, newest last,
	// and spent is the outpoints they consumed.
	//
	// A stand-in chain that answers lookups and cannot take a transaction can
	// watch a table play and not watch it pay, which is the half that holds
	// the money.
	sent  []*dcrwire.MsgTx
	spent map[string]bool

	// confs is how deep this chain says every output is. Settable because
	// maturity is the one thing a script engine cannot check, so the only way
	// to test a timelocked branch on both sides of its lock is to move the
	// chain rather than the transaction. Zero means the default.
	confs int64

	// swallow is how many more frames of a kind to lose on the way through,
	// and lost records what actually went missing.
	//
	// A hub that delivers everything cannot tell a protocol that repeats
	// itself from one that does not, which is why every fault of that shape
	// here has been found at a live table rather than in a test. This channel
	// loses messages; so does this one, on request.
	swallow map[schema.Kind]int
	lost    map[schema.Kind]int
	// inflight counts deliveries that have not finished. A delivery usually
	// produces a send, which produces more deliveries, so the count only
	// reaches zero when the table has actually come to rest.
	inflight sync.WaitGroup
	srv      *httptest.Server
}

// silence takes a peer off the wire, the way a machine that was switched off
// goes off it: no warning, and no way for anybody else to tell that from a
// network that is merely slow.
func (h *hub) silence(t *testing.T, token string) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.peers[token]; !ok {
		t.Fatalf("no peer %q to silence", token)
	}
	h.muted[token] = true
}

// bond gives a key a deposit and tells the chain about it.
func (h *hub) bond(t *testing.T, id *identity, nonce string) {
	t.Helper()
	script, err := id.bondScript()
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	_, pkScript, err := escrow.BondAddress(script, testParams)
	if err != nil {
		t.Fatalf("bond address: %v", err)
	}
	outpoint := fmt.Sprintf("%s:0", strings.Repeat(nonce, 64/len(nonce)))
	if err := id.setBondDeposit(outpoint); err != nil {
		t.Fatalf("record bond: %v", err)
	}
	h.mu.Lock()
	h.bonds[outpoint] = hex.EncodeToString(pkScript)
	h.mu.Unlock()
}

// lend registers a bond for a key that is not one of the hub's peers, so a test
// can hand a table a join from a stranger who has genuinely posted one.
func (h *hub) lend(t *testing.T, nonce string) membership.Credentials {
	t.Helper()
	session, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return h.lendTo(t, session, nonce)
}

func (h *hub) lendTo(t *testing.T, session *secp256k1.PrivateKey, nonce string) membership.Credentials {
	t.Helper()
	bond, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate bond key: %v", err)
	}
	script, err := escrow.BondScript(bond.PubKey().SerializeCompressed(), escrow.MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	_, pkScript, err := escrow.BondAddress(script, testParams)
	if err != nil {
		t.Fatalf("bond address: %v", err)
	}
	outpoint := fmt.Sprintf("%s:0", strings.Repeat(nonce, 64/len(nonce)))
	h.mu.Lock()
	h.bonds[outpoint] = hex.EncodeToString(pkScript)
	h.mu.Unlock()
	logKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate log key: %v", err)
	}
	return membership.Credentials{
		Session: session, Log: logKey, Bond: bond, BondOutpoint: outpoint, BondScript: script,
	}
}

// relayed is every transaction this chain was asked to send.
func (h *hub) relayed() []*dcrwire.MsgTx {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*dcrwire.MsgTx(nil), h.sent...)
}

// isSpent reports whether a payout has already taken an outpoint.
func (h *hub) isSpent(outpoint string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spent[outpoint]
}

// drop loses the next n frames of a kind, the way this channel does on its own
// given long enough.
func (h *hub) drop(kind schema.Kind, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.swallow[kind] += n
}

// dropped reports how many frames of a kind were actually lost, so a test can
// say it tested what it meant to rather than assume the drop ever fired.
func (h *hub) dropped(kind schema.Kind) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lost[kind]
}

// frameKind names what a frame carries, so the hub can lose one kind and not
// another. Single-part messages only, which every kind here is: the chunk limit
// is 200KB and the largest message in the protocol is a shuffle at about 23KB.
func frameKind(text string) (schema.Kind, bool) {
	part, ok := wire.Parse(text)
	if !ok || part.Total != 1 {
		return "", false
	}
	msg, err := schema.Decode(part.Chunk)
	if err != nil {
		return "", false
	}
	return msg.Kind, true
}

var testParams = chaincfg.SimNetParams()

// testOutpointAtoms is what this stand-in chain says every output holds.
//
// Enough for anything these tests ask about, and named rather than borrowed:
// it used to be escrow.MinBondAtoms, which is not a claim about buy-ins at all
// and only worked while the bond happened to be the larger number. Lowering
// the bond then made every stake look underfunded.
const testOutpointAtoms = int64(1_000_000_000)

func newHub(t *testing.T) *hub {
	t.Helper()
	h := &hub{
		peers:   make(map[string]*plugin),
		bonds:   make(map[string]string),
		muted:   make(map[string]bool),
		swallow: make(map[schema.Kind]int),
		lost:    make(map[schema.Kind]int),
		spent:   make(map[string]bool),
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gaming/chain/outpoint" {
			q := r.URL.Query()
			key := q.Get("txid") + ":" + q.Get("vout")
			h.mu.Lock()
			pkScript, gone := h.bonds[key], h.spent[key]
			confs := h.confs
			h.mu.Unlock()
			if confs == 0 {
				confs = int64(escrow.BondConfirmations)
			}
			_ = json.NewEncoder(w).Encode(transport.Outpoint{
				// Spent is indistinguishable from never-existed here, and
				// that is what the real lookup says too: it answers about
				// coin anybody can still take, not about history.
				Found:         pkScript != "" && !gone,
				ValueAtoms:    testOutpointAtoms,
				PkScriptHex:   pkScript,
				Confirmations: confs,
			})
			return
		}
		if r.URL.Path == "/gaming/chain/broadcast" {
			var req struct {
				RawTxHex string `json:"rawTxHex"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			raw, err := hex.DecodeString(req.RawTxHex)
			if err != nil {
				http.Error(w, "not hex", http.StatusBadRequest)
				return
			}
			tx := dcrwire.NewMsgTx()
			if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
				http.Error(w, "not a transaction: "+err.Error(), http.StatusBadRequest)
				return
			}
			h.mu.Lock()
			for _, in := range tx.TxIn {
				if h.spent[in.PreviousOutPoint.String()] {
					// Already taken. A real node refuses this, and a
					// test that let it through would let two peers
					// both pay the table out.
					h.mu.Unlock()
					http.Error(w, "already spent", http.StatusForbidden)
					return
				}
			}
			for _, in := range tx.TxIn {
				h.spent[in.PreviousOutPoint.String()] = true
			}
			h.sent = append(h.sent, tx)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"txid": tx.TxHash().String()})
			return
		}
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
		if kind, ok := frameKind(req.Frame); ok && h.swallow[kind] > 0 {
			h.swallow[kind]--
			h.lost[kind]++
			h.mu.Unlock()
			return
		}
		targets := make([]*plugin, 0, len(h.peers))
		if !h.muted[token] {
			for tok, p := range h.peers {
				if tok != token && !h.muted[tok] {
					targets = append(targets, p)
				}
			}
		}
		h.mu.Unlock()

		// Deliver out of band. A member receiving a frame usually sends
		// one of its own, and doing that on this call's stack would
		// make the fan-out reentrant in a way the real host is not.
		for _, p := range targets {
			h.inflight.Add(1)
			go func(p *plugin) {
				defer h.inflight.Done()
				p.router.HandleGCMessage(req.GCID, token, req.Frame, time.Now())
			}(p)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hub) join(t *testing.T, name string) *plugin {
	t.Helper()
	dir := t.TempDir()
	// Take this peer off the wire and let its deliveries finish before the
	// directory it writes to is removed. Registered after TempDir so it runs
	// before TempDir's own cleanup: a frame still in flight would otherwise
	// recreate a file under a directory being deleted.
	t.Cleanup(func() {
		h.mu.Lock()
		h.muted[name] = true
		h.mu.Unlock()
		h.inflight.Wait()
	})
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	// A seat costs a bond, so a peer with none can join nothing.
	h.bond(t, id, fmt.Sprintf("%02x", len(h.peers)+1))
	p, err := newPlugin(context.Background(), h.srv.URL+"/gaming", name, id, newStore(dir), testParams)
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
		Until:      900000,
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
	deadline := time.Now().Add(45 * time.Second)
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
	if _, left := p.tables.leave("0123456789abcdef"); !left {
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

	creds := h.lend(t, "cc")
	terms := membership.Terms{
		Game: schema.Game, GameVer: schema.Version, SID: "0123456789abcdef",
		BuyInAtoms: 10_000_000, Seats: 2, CSVBlocks: 64, Until: 900000,
	}
	j, err := membership.SignJoin(terms, creds)
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

	out := p.tables.deliver(context.Background(), transport.Delivery{
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
func deliverJoin(t *testing.T, p *plugin, terms membership.Terms, creds membership.Credentials) {
	t.Helper()
	j, err := membership.SignJoin(terms, creds)
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
	p.tables.deliver(context.Background(), transport.Delivery{GCID: testGC, Sender: "them", SID: terms.SID, Msg: msg})
}

func deliverCommit(t *testing.T, p *plugin, terms membership.Terms, creds membership.Credentials, roster [32]byte) {
	t.Helper()
	c, err := membership.SignCommit(terms, roster, creds.Session)
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	blob, err := schema.Encode(schema.KindCommit, terms.SID, schema.CommitFrom(c))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p.tables.deliver(context.Background(), transport.Delivery{GCID: testGC, Sender: "them", SID: terms.SID, Msg: msg})
}

func deliverKind(t *testing.T, p *plugin, terms membership.Terms, kind schema.Kind, body any) []outgoing {
	t.Helper()
	blob, err := schema.Encode(kind, terms.SID, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p.tables.deliver(context.Background(), transport.Delivery{GCID: testGC, Sender: "them", SID: terms.SID, Msg: msg})
}

// rosterHashOf reads back the membership a table bound itself to.
func rosterHashOf(t *testing.T, matchID string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(matchID)
	if err != nil || len(raw) != 32 {
		t.Fatalf("match id %q is not a roster hash", matchID)
	}
	var out [32]byte
	copy(out[:], raw)
	return out
}

func inviteTerms(inv schema.Invite) membership.Terms {
	return membership.Terms{
		Game: inv.Game, GameVer: schema.Version, SID: inv.SID,
		BuyInAtoms: inv.BuyInAtoms, Seats: inv.Seats, CSVBlocks: inv.CSVBlocks, Until: inv.Until,
	}
}

// restart builds a second process over the same data directory, which is what
// makes it the same player: the seed is there, so the session keys it derives
// are the ones it held before.
func (h *hub) restart(t *testing.T, dir, token string) *plugin {
	t.Helper()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if id.bondDeposit() == "" {
		h.bond(t, id, "aa")
	}
	p, err := newPlugin(context.Background(), h.srv.URL+"/gaming", token, id, newStore(dir), testParams)
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
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)

	other := h.lend(t, "dd")

	before := h.restart(t, dir, "tok")
	acceptInvite(t, before, inv)
	deliverJoin(t, before, terms, other)

	// Nobody else is here to agree, so it is the deadline that settles it.
	before.tables.tick(int64(terms.Until) + 1)

	first := before.tables.snapshots()[0]
	if first.State != membership.Committed.String() && first.State != membership.Settled.String() {
		t.Fatalf("state is %s, want it bound", first.State)
	}
	if first.MatchID == "" {
		t.Fatal("bound with no membership")
	}

	// Same directory, so the same player comes back.
	after := h.restart(t, dir, "tok")
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

// A table that settled and was seated has to come back that way.
//
// Settling needs a commit from every member. Ours reproduces itself, because
// binding again is deterministic - but theirs arrived as a message nobody will
// send a second time, so a peer that recorded only its own came back one
// signature short of a table it had already agreed and waited for something
// that was never coming. The seating has to survive for the same reason: it is
// drawn once, from a block chosen so nobody could predict it.
func TestARestartComesBackSettledAndSeated(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)

	other := h.lend(t, "dd")

	before := h.restart(t, dir, "tok")
	acceptInvite(t, before, inv)
	deliverJoin(t, before, terms, other)

	// Nobody else is here to agree, so it is the deadline that binds us.
	before.tables.tick(int64(terms.Until) + 1)

	first := before.tables.snapshots()[0]
	deliverCommit(t, before, terms, other, rosterHashOf(t, first.MatchID))

	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 1)
	}
	before.tables.seat(terms.SID, beacon)

	first = before.tables.snapshots()[0]
	if first.State != membership.Settled.String() {
		t.Fatalf("state is %s, want it settled before the restart", first.State)
	}
	if !before.tables.m[terms.SID].form.Seated() {
		t.Fatal("not seated before the restart")
	}

	// Same directory, so the same player comes back.
	after := h.restart(t, dir, "tok")
	acceptInvite(t, after, inv)

	second := after.tables.snapshots()[0]
	if second.State != membership.Settled.String() {
		t.Fatalf("a settled table came back as %s", second.State)
	}
	if second.MatchID != first.MatchID {
		t.Fatalf("came back at membership %s, had settled on %s", second.MatchID, first.MatchID)
	}
	resumed := after.tables.m[terms.SID].form
	if !resumed.Seated() {
		t.Fatal("came back settled but unseated, so it would draw its seats again")
	}
	if got := hex.EncodeToString(resumed.Beacon()); got != hex.EncodeToString(beacon) {
		t.Fatalf("came back seated from block %s, was seated from %s", got, hex.EncodeToString(beacon))
	}
}

// The cure for a table stuck one signature short.
//
// Formation messages are published once. A peer whose stream was down while
// somebody committed holds every join and still cannot settle, and no amount of
// waiting helps because nothing will send that commit again. Asking is what
// gets it back.
func TestResyncSettlesATableThatMissedACommit(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	other := h.lend(t, "dd")

	p := h.restart(t, t.TempDir(), "tok")
	acceptInvite(t, p, inv)
	deliverJoin(t, p, terms, other)
	p.tables.tick(int64(terms.Until) + 1)

	stuck := p.tables.snapshots()[0]
	if stuck.State != membership.Committed.String() {
		t.Fatalf("state is %s, want committed with the other commit still missing", stuck.State)
	}

	// The commit that was sent once, while this peer was not listening.
	c, err := membership.SignCommit(terms, rosterHashOf(t, stuck.MatchID), other.Session)
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	deliverKind(t, p, terms, schema.KindResyncReply, schema.ResyncReply{
		Commits: []schema.Commit{schema.CommitFrom(c)},
	})

	healed := p.tables.snapshots()[0]
	if healed.State != membership.Settled.String() {
		t.Fatalf("state is %s after a resync carrying the missing commit", healed.State)
	}
	if healed.MatchID != stuck.MatchID {
		t.Fatalf("healed onto membership %s, was bound to %s", healed.MatchID, stuck.MatchID)
	}
}

// A resync answer carries the difference, not the table.
func TestResyncAnswersOnlyWhatTheAskerLacks(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	other := h.lend(t, "dd")

	p := h.restart(t, t.TempDir(), "tok")
	acceptInvite(t, p, inv)
	deliverJoin(t, p, terms, other)
	p.tables.tick(int64(terms.Until) + 1)

	form := p.tables.m[terms.SID].form

	// An asker holding nothing is told everything this table holds.
	out := deliverKind(t, p, terms, schema.KindResync, schema.Resync{})
	if len(out) != 1 {
		t.Fatalf("answered with %d messages, want one", len(out))
	}
	reply, ok := out[0].body.(schema.ResyncReply)
	if !ok {
		t.Fatalf("answered with %T, want a resync reply", out[0].body)
	}
	if len(reply.Joins) != len(form.Joins()) {
		t.Fatalf("offered %d joins, holds %d", len(reply.Joins), len(form.Joins()))
	}
	if len(reply.Commits) != len(form.Commits()) {
		t.Fatalf("offered %d commits, holds %d", len(reply.Commits), len(form.Commits()))
	}

	// An asker already in step is answered with silence, so a table that is
	// healthy does not read itself back to every peer that reconnects.
	ask := schema.Resync{}
	for _, j := range form.Joins() {
		ask.Joins = append(ask.Joins, hex.EncodeToString(j.Key))
	}
	for _, c := range form.Commits() {
		ask.Commits = append(ask.Commits, hex.EncodeToString(c.Signer))
	}
	if out := deliverKind(t, p, terms, schema.KindResync, ask); len(out) != 0 {
		t.Fatalf("answered a peer that was already in step with %d messages", len(out))
	}
}

// A table short of a commit keeps asking.
//
// The first ask can go out while nobody is listening - both live peers did
// exactly that, each asking while the other was still starting - and a question
// asked once into a lossy channel is the fault resync exists to cure.
func TestATableShortOfACommitKeepsAsking(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	other := h.lend(t, "dd")

	p := h.restart(t, t.TempDir(), "tok")
	acceptInvite(t, p, inv)
	deliverJoin(t, p, terms, other)

	at := int64(terms.Until) + 1
	p.tables.tick(at)

	if got := p.tables.snapshots()[0].State; got != membership.Committed.String() {
		t.Fatalf("state is %s, want committed and waiting on a commit", got)
	}

	// Same height, so nothing new to say.
	if out := p.tables.tick(at); len(asksIn(out)) != 0 {
		t.Fatal("asked twice at one height")
	}

	// A new block, so ask again.
	if out := p.tables.tick(at + 1); len(asksIn(out)) != 1 {
		t.Fatal("a table still short of a commit stopped asking")
	}

	// Settled, so there is nothing left to ask for.
	c, err := membership.SignCommit(terms, rosterHashOf(t, p.tables.snapshots()[0].MatchID), other.Session)
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	deliverKind(t, p, terms, schema.KindResyncReply, schema.ResyncReply{
		Commits: []schema.Commit{schema.CommitFrom(c)},
	})
	if out := p.tables.tick(at + 2); len(asksIn(out)) != 0 {
		t.Fatal("a settled table is still asking to be caught up")
	}
}

// Two peers joining seconds apart each drop the other's join - it names a table
// they have not accepted yet - so a join published once leaves both of them
// holding one join until the deadline kills a table both of them wanted.
func TestATableStillShortOfJoinsSaysItselfAgain(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)

	p := h.restart(t, t.TempDir(), "tok")
	acceptInvite(t, p, inv)

	at := int64(terms.Until) - 10
	out := p.tables.tick(at)
	if len(joinsIn(out)) != 1 {
		t.Fatalf("a table short of joins republished its own %d times, want once", len(joinsIn(out)))
	}
	if len(asksIn(out)) != 1 {
		t.Fatal("republished a join without asking for the ones it is missing")
	}
	if out := p.tables.tick(at); len(joinsIn(out)) != 0 {
		t.Fatal("republished twice at one height")
	}
	if out := p.tables.tick(at + 1); len(joinsIn(out)) != 1 {
		t.Fatal("stopped republishing while still short of a table")
	}

	// Once the table is full there is nothing left to announce.
	deliverJoin(t, p, terms, h.lend(t, "dd"))
	if out := p.tables.tick(at + 2); len(joinsIn(out)) != 0 {
		t.Fatal("kept republishing a join after the table filled")
	}
}

func joinsIn(out []outgoing) []outgoing {
	var j []outgoing
	for _, o := range out {
		if o.kind == schema.KindJoin {
			j = append(j, o)
		}
	}
	return j
}

func asksIn(out []outgoing) []outgoing {
	var asks []outgoing
	for _, o := range out {
		if o.kind == schema.KindResync {
			asks = append(asks, o)
		}
	}
	return asks
}

// A resync answer is checked, not believed. It arrives from whoever felt like
// replying, so keys nobody joined with and commits nobody signed have to be
// refused exactly as they would be if published directly.
func TestResyncCannotInjectAMembership(t *testing.T) {
	h := newHub(t)
	inv := testInvite(2)
	terms := inviteTerms(inv)
	other := h.lend(t, "dd")
	stranger := h.lend(t, "ee")

	p := h.restart(t, t.TempDir(), "tok")
	acceptInvite(t, p, inv)
	deliverJoin(t, p, terms, other)
	p.tables.tick(int64(terms.Until) + 1)

	bound := p.tables.snapshots()[0]

	// A commit over this table's roster, signed by somebody who never joined.
	c, err := membership.SignCommit(terms, rosterHashOf(t, bound.MatchID), stranger.Session)
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	deliverKind(t, p, terms, schema.KindResyncReply, schema.ResyncReply{
		Commits: []schema.Commit{schema.CommitFrom(c)},
	})

	after := p.tables.snapshots()[0]
	if after.State == membership.Settled.String() {
		t.Fatal("settled on a commit from a key that never joined this table")
	}
	if after.MatchID != bound.MatchID {
		t.Fatalf("membership moved to %s, was %s", after.MatchID, bound.MatchID)
	}
}

// Aborted is terminal, and has to stay terminal across a restart: a commit
// arriving after everyone else gave up would otherwise put this process back
// into a membership nobody is bound to.
func TestARestartWillNotRejoinASessionThatEnded(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)

	p := h.restart(t, dir, "tok")
	acceptInvite(t, p, inv)

	// Two more answer a two seat table, arriving together in somebody's
	// assertion, so the table is over-full before anyone binds.
	joins := make([]*membership.Join, 0, 2)
	for i := range 2 {
		j, err := membership.SignJoin(terms, h.lend(t, fmt.Sprintf("e%d", i)))
		if err != nil {
			t.Fatalf("sign join: %v", err)
		}
		joins = append(joins, j)
	}
	blob, err := schema.Encode(schema.KindRoster, terms.SID,
		schema.RosterFrom(terms, map[uint32][]byte{}, joins, nil))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	p.tables.deliver(context.Background(), transport.Delivery{GCID: testGC, Sender: "them", SID: terms.SID, Msg: msg})
	if got := p.tables.snapshots()[0].State; got != membership.Aborted.String() {
		t.Fatalf("state is %s, want aborted", got)
	}

	link, _ := inv.String()
	body, _ := json.Marshal(map[string]string{"invite": link, "gcid": testGC})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/table/join", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer tok")
	h.restart(t, dir, "tok").routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a session that ended was rejoined after a restart: %s", rec.Body.String())
	}
}

// A table that fills but has nobody to agree with settles when its deadline
// passes. That is the path that makes forming deterministic; agreement is only
// the shortcut when everyone happens to be present.
func TestADeadlineBindsATableThatNobodyElseConfirmed(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)

	p := h.restart(t, dir, "tok")
	acceptInvite(t, p, inv)

	deliverJoin(t, p, terms, h.lend(t, "ff"))

	if got := p.tables.snapshots()[0].State; got != membership.Formed.String() {
		t.Fatalf("state is %s, want formed and waiting", got)
	}

	// Short of the deadline nothing moves.
	p.tables.tick(int64(terms.Until) - 1)
	if got := p.tables.snapshots()[0].State; got != membership.Formed.String() {
		t.Fatalf("state is %s before the deadline, want formed", got)
	}

	p.tables.tick(int64(terms.Until) + 1)
	if got := p.tables.snapshots()[0].State; got != membership.Committed.String() {
		t.Fatalf("state is %s past the deadline, want committed", got)
	}
}

// A table still short of its seats when admission shuts cannot form. Waiting
// longer would only be hoping, and the deadline is what turns that into an
// answer.
func TestADeadlineEndsATableThatNeverFilled(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(3)

	p := h.restart(t, dir, "tok")
	acceptInvite(t, p, inv)

	p.tables.tick(int64(inviteTerms(inv).Until) + 1)

	s := p.tables.snapshots()[0]
	if s.State != membership.Aborted.String() {
		t.Fatalf("state is %s, want aborted", s.State)
	}
	if s.Reason == "" {
		t.Fatal("aborted without saying why")
	}
}

// The point of the whole exercise. A join whose deposit is not locked coin on
// chain is a free seat, and a free seat is a stranger filling a table or
// spoiling it as often as they like.
func TestAJoinWhoseBondIsNotOnChainIsRefused(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)

	p := h.restart(t, dir, "tok")
	acceptInvite(t, p, inv)
	before := p.tables.snapshots()[0].Joined

	// A bond the chain has never heard of: the script and the proof of
	// possession are genuine, so this passes everything that can be checked
	// without asking, and fails on the one thing that matters.
	session, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	bond, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate bond key: %v", err)
	}
	script, err := escrow.BondScript(bond.PubKey().SerializeCompressed(), escrow.MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	logKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate log key: %v", err)
	}
	deliverJoin(t, p, terms, membership.Credentials{
		Session:      session,
		Log:          logKey,
		Bond:         bond,
		BondOutpoint: strings.Repeat("9", 64) + ":0",
		BondScript:   script,
	})

	if got := p.tables.snapshots()[0].Joined; got != before {
		t.Fatalf("a join with no deposit behind it was admitted (%d joins, was %d)", got, before)
	}
	if p.tables.snapshots()[0].State != membership.Joining.String() {
		t.Fatalf("state is %s, want still joining", p.tables.snapshots()[0].State)
	}
}

// A table cannot be joined at all without a bond of this player's own. Nothing
// else here is worth anything if the process can seat itself for free.
func TestThisPlayerCannotJoinWithoutItsOwnBond(t *testing.T) {
	dir := t.TempDir()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	p, err := newPlugin(context.Background(), "http://host/gaming", "tok", id, newStore(dir), testParams)
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}

	link, _ := testInvite(2).String()
	body, _ := json.Marshal(map[string]string{"invite": link, "gcid": testGC})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/table/join", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer tok")
	p.routes().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("a table was joined with no bond: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bond") {
		t.Fatalf("the refusal should say what is missing: %s", rec.Body.String())
	}
}

// Tables are listed in one order, and it is not the map's.
//
// snapshots() is built by ranging a map, which Go randomises per call. A panel
// asking twice a second therefore got its tables back shuffled every time and
// drew a row of buttons that swapped places continuously - which is not merely
// ugly, because those buttons pick which table a person is about to act on.
func TestTablesAreListedNewestFirstAndDoNotReorder(t *testing.T) {
	h := newHub(t)
	p := h.restart(t, t.TempDir(), "tok")

	// Three tables, deliberately accepted oldest first so that insertion
	// order and the wanted order disagree.
	for i, until := range []uint32{900000, 900010, 900005} {
		inv := testInvite(2)
		inv.SID = fmt.Sprintf("%015x%d", 0, i)
		inv.Until = until
		acceptInvite(t, p, inv)
	}

	first := p.tables.snapshots()
	if len(first) != 3 {
		t.Fatalf("holding %d tables, joined 3", len(first))
	}
	if got := []uint32{first[0].Until, first[1].Until, first[2].Until}; got[0] != 900010 ||
		got[1] != 900005 || got[2] != 900000 {
		t.Fatalf("listed in %v, want the newest admission deadline first", got)
	}

	// The failure this guards against is intermittent by nature, so ask
	// enough times that a map's randomness would have shown itself.
	want := make([]string, len(first))
	for i, s := range first {
		want[i] = s.SID
	}
	for range 50 {
		got := p.tables.snapshots()
		for i, s := range got {
			if s.SID != want[i] {
				t.Fatalf("asked again and table %d became %s, was %s", i, s.SID, want[i])
			}
		}
	}
}

// A payout address announced before there was a seat to sign it against.
//
// The address is set when the host mints a panel session, and a panel is opened
// to accept an invitation - which is before the seating is drawn. announcePayout
// can only sign against a seat, so that first telling produced nothing, and
// nothing ever said it again: resync carries joins and commits, and dealing is
// gated on stakes and bonds rather than on this. The table therefore dealt, played
// to the end, and only there found it could not build a payout at all.
//
// Seen live at table cb2b558c, where one box happened to have its panel reopened
// after seating and the other did not.
func TestAPayoutSaidBeforeSeatingIsSaidAgainAfterIt(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	inv := testInvite(2)
	terms := inviteTerms(inv)
	other := h.lend(t, "dd")

	p := h.restart(t, dir, "tok")
	acceptInvite(t, p, inv)
	deliverJoin(t, p, terms, other)
	p.tables.tick(int64(terms.Until) + 1)
	deliverCommit(t, p, terms, other, rosterHashOf(t, p.tables.snapshots()[0].MatchID))

	// The host says where to pay, exactly as it does when a panel opens: the
	// table is settled, but nobody has been seated yet.
	addr := payoutAddress(t, p)
	if err := p.id.setPayout(addr, testParams); err != nil {
		t.Fatalf("payout address: %v", err)
	}
	if out := p.tables.announcePayouts(addr); len(out) != 0 {
		t.Fatalf("announced a payout for a seat that does not exist yet: %d frames", len(out))
	}

	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 1)
	}

	// Drawing the seating is the moment it becomes sayable, so it is said.
	said := p.tables.seat(terms.SID, beacon)
	if len(said) != 1 || said[0].kind != schema.KindPayout {
		t.Fatalf("seating a table said %d frames, want one payout", len(said))
	}

	// And it keeps being said, because our own record of it says nothing
	// about whether the other seat ever received it.
	again := p.tables.tick(int64(terms.Until) + 2)
	if !saysPayout(again) {
		t.Fatal("a seated table stopped repeating where to pay it")
	}
}

func saysPayout(out []outgoing) bool {
	for _, o := range out {
		if o.kind == schema.KindPayout {
			return true
		}
	}
	return false
}
