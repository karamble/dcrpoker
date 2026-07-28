package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Handing over the log a table was played from.
//
// The signed chain is the only account of a hand that does not need anybody to
// be believed: every entry carries the signature of the seat that made it, and
// the whole thing hashes forward, so a transcript can be checked by somebody
// who was not there and trusts nobody who was. That is what makes it worth
// keeping rather than a curiosity - it is what a dispute is argued from, and
// what proves a settlement paid what the play produced.
//
// Two places it can come from, and both are needed. A table still in memory has
// the live chain, which is the most current answer. A table that is over has
// its transcript on disk, written when it stopped being played, because the
// entries live in memory and the coin behind them does not.

// handleTableLog hands over one table's signed log.
func (p *plugin) handleTableLog(w http.ResponseWriter, r *http.Request) {
	sid := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sid")))

	blob, err := p.tables.transcript(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if blob == nil {
		blob, err = p.store.loadTranscript(sid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if blob == nil {
		writeErr(w, http.StatusNotFound,
			fmt.Errorf("no log was kept for %s; a table that never dealt has none", sid))
		return
	}

	// Named, because this is a file somebody is saving rather than a body
	// something is parsing. A log called "log" among five of them is a log
	// nobody can tell apart later.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"poker-%s.json\"", sid))
	_, _ = w.Write(blob)
}

// transcript renders a table's live chain, if it is one this player is at.
//
// Nil with no error means the table is known and has nothing yet, which is not
// a failure: a table that never dealt has no log, and one read back from disk
// has its entries there rather than here.
func (t *tables) transcript(sid string) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return nil, fmt.Errorf("no table under session %s", sid)
	}
	return tbl.log()
}

// log renders whichever chain this table is actually writing.
//
// There are two and only one is live at a time. Before a table deals, entries
// arrive as messages and the watcher folds them in; once it is dealing the hand
// owns the log, because it appends and folds each entry itself and letting the
// watcher append too would refuse the second of the two as a repeat. Reading
// the wrong one gives an empty transcript for a table that has been playing
// for hours.
//
// Requires the registry lock.
func (tbl *table) log() ([]byte, error) {
	if tbl.play != nil {
		if c := tbl.play.Chain(); c != nil {
			return c.Marshal()
		}
	}
	if tbl.watch == nil {
		return nil, nil
	}
	return tbl.watch.Transcript()
}
