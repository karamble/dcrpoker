package main

import (
	"encoding/hex"
	"testing"

	"github.com/vctt94/dcrpoker/pkg/gaming/schema"
	"github.com/vctt94/dcrpoker/pkg/membership"
)

// settledRecord builds a dealt, seated record on disk the way a table that has
// played would have left one, with whatever boundary the caller wants on it.
func settledRecord(t *testing.T, h *hub, dir string, at *settledBoundary) (*plugin, string) {
	t.Helper()
	inv := testInvite(2)
	terms := inviteTerms(inv)
	p := h.restart(t, dir, "tok")

	creds, err := p.id.credentials(terms.SID)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	other := h.lend(t, "bb")
	f, err := membership.NewFormation(terms, creds)
	if err != nil {
		t.Fatalf("formation: %v", err)
	}
	j1, err := membership.SignJoin(terms, other)
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	if err := f.AddJoin(j1); err != nil {
		t.Fatalf("add join: %v", err)
	}
	c0, err := f.Bind()
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	c1, err := membership.SignCommit(terms, c0.Roster, other.Session)
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	if err := f.AddCommit(c1); err != nil {
		t.Fatalf("add commit: %v", err)
	}
	beacon := make([]byte, 32)
	for i := range beacon {
		beacon[i] = byte(i + 3)
	}
	if err := f.SetBeacon(beacon); err != nil {
		t.Fatalf("seat: %v", err)
	}

	rec := &record{
		Terms:    schema.TermsFrom(terms),
		GCID:     testGC,
		Bound:    true,
		Roster:   hex.EncodeToString(c0.Roster[:]),
		Beacon:   hex.EncodeToString(beacon),
		Funded:   map[uint32]string{0: "aa11:0", 1: "bb22:0"},
		Bonded:   map[uint32]string{0: "cc33:0", 1: "dd44:0"},
		Dealt:    true,
		Finished: true,
		Settled:  at,
	}
	for _, j := range f.Joins() {
		rec.Joins = append(rec.Joins, schema.JoinFrom(j))
	}
	for _, c := range f.Commits() {
		rec.Commits = append(rec.Commits, schema.CommitFrom(c))
	}
	if err := p.tables.store.save(terms.SID, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	return p, terms.SID
}

// A table's result outlives the process that played it.
//
// The driver holds the only copy while a table is live and dies with it, and
// the transcript carries the actions but no checkpoint - so without this a
// person cannot be told afterwards what their own table paid them.
func TestAResultOutlivesTheProcessThatPlayedIt(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	_, sid := settledRecord(t, h, dir, &settledBoundary{Hand: 1, Stacks: []int64{0, 200000}})

	back := h.restart(t, dir, "tok")
	tbl := back.tables.m[sid]
	if tbl == nil {
		t.Fatal("the receipt did not come back at all")
	}
	if tbl.boundary == nil {
		t.Fatal("the receipt came back without the result, so nobody can be told who won")
	}
	if tbl.boundary.Hand != 1 {
		t.Fatalf("the result came back at hand %d, not the hand it settled at", tbl.boundary.Hand)
	}
	if len(tbl.boundary.Stacks) != 2 || tbl.boundary.Stacks[0] != 0 || tbl.boundary.Stacks[1] != 200000 {
		t.Fatalf("the stacks came back as %v, which is not what every seat signed", tbl.boundary.Stacks)
	}

	var found bool
	for _, s := range back.tables.snapshots() {
		if s.SID != sid {
			continue
		}
		found = true
		if s.Settled == nil {
			t.Fatal("the snapshot hides the result, so the interface still cannot show it")
		}
		if s.Settled.Stacks[1] != 200000 {
			t.Fatalf("the snapshot reports stacks %v", s.Settled.Stacks)
		}
		if !s.Over {
			t.Fatal("a table with a signed result and no driver is over, and saying otherwise " +
				"leaves the interface waiting for a hand that will never come")
		}
		if s.Dealing {
			t.Fatal("a receipt reported itself as dealing; there is no driver to deal with")
		}
	}
	if !found {
		t.Fatalf("no snapshot for %s", sid)
	}
}

// A table that never dealt has no result, and must not be given one.
//
// Absent is not zero: a table that never played still holds its buy-in, and
// reporting a settled boundary of nothing would read as having lost it.
func TestATableThatNeverDealtIsNotReportedAsAResult(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	_, sid := settledRecord(t, h, dir, nil)

	back := h.restart(t, dir, "tok")
	tbl := back.tables.m[sid]
	if tbl == nil {
		t.Fatal("the receipt did not come back")
	}
	if tbl.boundary != nil {
		t.Fatalf("a record with no result produced one: %+v", tbl.boundary)
	}
	for _, s := range back.tables.snapshots() {
		if s.SID != sid {
			continue
		}
		if s.Settled != nil {
			t.Fatalf("a table that never settled reports stacks %v, which reads as a loss",
				s.Settled.Stacks)
		}
		if s.Over {
			t.Fatal("a table with no result was reported as over on the strength of nothing")
		}
	}
}

// Saving a receipt again keeps the result.
//
// A receipt is persisted whenever anything about it moves - a bond clearing,
// an outpoint going - and record() is what builds those writes. If it does not
// carry the boundary forward, the first such save after a restart erases the
// result that the restart had just survived.
func TestSavingAReceiptAgainKeepsTheResult(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	_, sid := settledRecord(t, h, dir, &settledBoundary{Hand: 3, Stacks: []int64{50000, 150000}})

	back := h.restart(t, dir, "tok")
	tbl := back.tables.m[sid]
	if tbl == nil {
		t.Fatal("the receipt did not come back")
	}

	// What the next ordinary save would write.
	rec := tbl.record()
	if rec.Settled == nil {
		t.Fatal("re-saving a receipt drops the result, so it survives a restart and dies at " +
			"the next write")
	}
	if rec.Settled.Hand != 3 || rec.Settled.Stacks[1] != 150000 {
		t.Fatalf("the rebuilt record carries %+v, not what was signed", rec.Settled)
	}

	// And through the store, which is what actually happens.
	if err := back.tables.store.save(sid, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	again, err := back.tables.store.load(sid)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if again.Settled == nil || again.Settled.Hand != 3 {
		t.Fatalf("the result did not survive a second round trip: %+v", again.Settled)
	}
}

// A record written before this field existed still loads.
//
// Every record already on disk is one of these, and refusing one would drop a
// receipt that is the only place an outpoint is written down.
func TestARecordWithoutTheFieldStillLoads(t *testing.T) {
	h := newHub(t)
	dir := t.TempDir()
	p, sid := settledRecord(t, h, dir, nil)

	// Read it back the way the loader does, from the bytes on disk.
	rec, err := p.tables.store.load(sid)
	if err != nil {
		t.Fatalf("a record without the field could not be read: %v", err)
	}
	if rec.Settled != nil {
		t.Fatal("an absent field did not decode as absent")
	}
	if !rec.Dealt {
		t.Fatal("the rest of the record did not survive the round trip")
	}
}
