package chainwatch

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
)

const watchMatch = "table1|sess1"

// table stands in for the server: it holds the authoritative chain and produces
// the messages a player would receive.
type watchTable struct {
	t      *testing.T
	chain  *gamelog.Chain
	privs  []*forfeit.LogKey
	roster map[uint32][]byte
}

func newWatchTable(t *testing.T, seats int) *watchTable {
	t.Helper()
	privs := make([]*forfeit.LogKey, seats)
	roster := make(map[uint32][]byte, seats)
	for i := range privs {
		priv, err := forfeit.NewLogKey(watchMatch)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		privs[i] = priv
		roster[uint32(i)] = priv.Public().SerializeCompressed()
	}
	chain, err := gamelog.NewChain(watchMatch, gamelog.Roster(roster))
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	return &watchTable{t: t, chain: chain, privs: privs, roster: roster}
}

// act appends an action to the authoritative chain and returns the message a
// player would receive for it.
func (w *watchTable) act(seat uint32, action gamelog.Action, amount int64) *schema.Message {
	w.t.Helper()
	e := w.chain.Next(seat, 1, gamelog.StreetPreFlop, action, amount)
	if err := e.Sign(w.privs[seat]); err != nil {
		w.t.Fatalf("sign: %v", err)
	}
	if err := w.chain.Append(e); err != nil {
		w.t.Fatalf("append: %v", err)
	}
	return w.message(e)
}

func (w *watchTable) message(e *gamelog.Entry) *schema.Message {
	w.t.Helper()
	blob, err := schema.Encode(schema.KindAction, watchMatch, schema.Action{
		Entry: gamelog.TranscriptEntry{
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
		},
	})
	if err != nil {
		w.t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		w.t.Fatalf("decode: %v", err)
	}
	return msg
}

// A player who receives the relayed entries reaches the same head as the table,
// having checked every signature rather than been told the answer.
func TestChainWatchReachesTheSameHead(t *testing.T) {
	table := newWatchTable(t, 3)
	w, err := New(watchMatch, table.roster)
	if err != nil {
		t.Fatalf("new watch: %v", err)
	}

	msgs := []*schema.Message{
		table.act(0, gamelog.ActionBet, 500),
		table.act(1, gamelog.ActionCall, 0),
		table.act(2, gamelog.ActionFold, 0),
	}
	for _, m := range msgs {
		if err := w.Apply(m); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	wantHash, wantSeq := table.chain.Head()
	if !w.AgreesWith(wantHash[:], wantSeq) {
		gotHash, gotSeq := w.Head()
		t.Fatalf("player head %x@%d, table head %x@%d", gotHash, gotSeq, wantHash, wantSeq)
	}
	if w.Waiting() != 0 {
		t.Fatalf("%d entries still waiting", w.Waiting())
	}
}

// Group chat delivery has no ordering, so entries arriving out of order is the
// ordinary case and must not break the chain.
func TestChainWatchToleratesArrivalOrder(t *testing.T) {
	table := newWatchTable(t, 2)
	w, _ := New(watchMatch, table.roster)

	msgs := []*schema.Message{
		table.act(0, gamelog.ActionBet, 500),
		table.act(1, gamelog.ActionCall, 0),
		table.act(0, gamelog.ActionCheck, 0),
	}

	// Last first: nothing can be applied until the first arrives.
	if err := w.Apply(msgs[2]); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := w.Apply(msgs[1]); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, seq := w.Head(); seq != 0 {
		t.Fatalf("chain advanced to %d with its first entry missing", seq)
	}
	if w.Waiting() != 2 {
		t.Fatalf("%d entries waiting, want 2", w.Waiting())
	}

	if err := w.Apply(msgs[0]); err != nil {
		t.Fatalf("apply: %v", err)
	}
	wantHash, wantSeq := table.chain.Head()
	if !w.AgreesWith(wantHash[:], wantSeq) {
		t.Fatal("the chain did not drain once the gap was filled")
	}
}

// The relay replays what was not acked, so the same entry arriving twice is
// normal and not a fault.
func TestChainWatchIgnoresRedelivery(t *testing.T) {
	table := newWatchTable(t, 2)
	w, _ := New(watchMatch, table.roster)

	m := table.act(0, gamelog.ActionBet, 500)
	for range 3 {
		if err := w.Apply(m); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	if _, seq := w.Head(); seq != 1 {
		t.Fatalf("redelivery advanced the chain to %d", seq)
	}
}

// This is what the whole exercise is for: an entry that does not verify is
// refused by the player, whoever relayed it.
func TestChainWatchRefusesAnEntryThatDoesNotVerify(t *testing.T) {
	table := newWatchTable(t, 2)
	w, _ := New(watchMatch, table.roster)

	m := table.act(0, gamelog.ActionBet, 500)

	// Raise the bet in transit, leaving the signature untouched.
	var body schema.Action
	if err := m.Into(&body); err != nil {
		t.Fatalf("into: %v", err)
	}
	body.Entry.Amount = 900
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	m.Body = raw

	if err := w.Apply(m); err == nil {
		t.Fatal("an altered entry must not be accepted")
	}
	if _, seq := w.Head(); seq != 0 {
		t.Fatal("an altered entry reached the chain")
	}
}

// A seat that is not at the table cannot write history, whatever the relay says.
func TestChainWatchRefusesAStranger(t *testing.T) {
	table := newWatchTable(t, 2)
	w, _ := New(watchMatch, table.roster)

	stranger, err := forfeit.NewLogKey(watchMatch)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	e := table.chain.Next(0, 1, gamelog.StreetPreFlop, gamelog.ActionBet, 500)
	if err := e.Sign(stranger); err != nil {
		t.Fatalf("sign: %v", err)
	}

	if err := w.Apply(table.message(e)); err == nil {
		t.Fatal("an entry signed by a stranger must not be accepted")
	}
}

// A build that has not been taught a message kind must stay in the hand.
func TestChainWatchIgnoresOtherKinds(t *testing.T) {
	table := newWatchTable(t, 2)
	w, _ := New(watchMatch, table.roster)

	blob, err := schema.Encode(schema.KindHead, watchMatch, schema.Head{Seq: 1})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := schema.Decode(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := w.Apply(msg); err != nil {
		t.Fatalf("another kind of message should be ignored, not refused: %v", err)
	}
	if err := w.Apply(nil); err != nil {
		t.Fatalf("a nil message should be ignored: %v", err)
	}
}

// Disagreement with the server is the point, and a player who is merely behind
// must be distinguishable from one who is being lied to.
func TestAgreesWithSeparatesBehindFromDivergent(t *testing.T) {
	table := newWatchTable(t, 2)
	w, _ := New(watchMatch, table.roster)

	first := table.act(0, gamelog.ActionBet, 500)
	table.act(1, gamelog.ActionCall, 0) // never delivered
	if err := w.Apply(first); err != nil {
		t.Fatalf("apply: %v", err)
	}

	serverHash, serverSeq := table.chain.Head()
	if w.AgreesWith(serverHash[:], serverSeq) {
		t.Fatal("a player one entry behind should not report agreement")
	}
	if w.Waiting() != 0 {
		t.Fatal("nothing should be waiting; the entry simply never arrived")
	}

	// Its own head is real and exportable regardless.
	blob, err := w.Transcript()
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	back, err := gamelog.Unmarshal(blob)
	if err != nil {
		t.Fatalf("the player's own transcript must verify: %v", err)
	}
	if back.Len() != 1 {
		t.Fatalf("transcript holds %d entries, want 1", back.Len())
	}
}
