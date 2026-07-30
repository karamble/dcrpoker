package main

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
)

// Where the money is, and what happened to it.
//
// Everything here already existed and was reachable by nobody. A claim was
// proposed, co-signed, broadcast or refused entirely inside log.Printf; a
// settlement was assembled and sent the same way; and the one case that costs a
// player real coin - claimed against while holding no answer - was a single line
// on stdout in a container with no shell.
//
// So this file adds no mechanism. It reports what the table already decided, and
// it answers before the table deals, which is the whole difference from
// /table/hand: the cards are one question and the money is another, and the
// second one is interesting long before the first has an answer.

// ledgerView is one table's money, as this peer understands it.
type ledgerView struct {
	SID    string `json:"sid"`
	Match  string `json:"match,omitempty"`
	Height int64  `json:"height,omitempty"`

	// Settled is the last boundary every seat signed and Live is what the
	// table thinks now. See snapshot for why both.
	Settled *settledBoundary `json:"settled,omitempty"`
	Live    []int64          `json:"live,omitempty"`
	Roster  []seatView       `json:"roster,omitempty"`

	// PayoutsMissing is every seat that has not said where to pay it. While
	// it is non-empty no claim at this table can be built at all, which was
	// previously a failure visible only as a refusal to co-sign.
	PayoutsMissing []uint32 `json:"payoutsMissing,omitempty"`

	// Accused reports that every seat's accusation has been
	// gathered and is held. A table dealing without this cannot answer a
	// claim, because the signatures an answer needs include the accusers'
	// and they will not give them once they have started accusing.
	Accused bool `json:"accused"`

	Claims     []claimView  `json:"claims,omitempty"`
	Settlement *settleView  `json:"settlement,omitempty"`
	Events     []chainEvent `json:"events,omitempty"`

	// Challenges is every challenged hand: who doubted it, who has revealed,
	// and - once the audit ran - what it found. Cards carry the whole
	// recomputed hand, muck included, which is what a completed audit has
	// already made computable by everyone at the table.
	Challenges []challengeView `json:"challenges,omitempty"`

	// Disputes is every refused shuffle and its verdict. Judged the moment
	// the complaint is read, so unlike a challenge there is no open state
	// worth showing - only who disputed what, and who was named for it.
	Disputes []disputeView `json:"disputes,omitempty"`
}

// disputeView is one shuffle dispute as the panel sees it.
type disputeView struct {
	Hand     uint64 `json:"hand"`
	Round    uint32 `json:"round"`
	By       uint32 `json:"by"`
	Shuffler uint32 `json:"shuffler"`
	// Verdict names the outcome, and Named the seat it went against.
	Verdict string `json:"verdict"`
	Named   uint32 `json:"named"`
}

// challengeView is one challenge as the panel sees it.
type challengeView struct {
	Hand     uint64   `json:"hand"`
	By       uint32   `json:"by"`
	Open     bool     `json:"open"`
	Revealed []uint32 `json:"revealed,omitempty"`
	Needs    int      `json:"needs"`
	// Verdict is empty while open, then "clean" or "cheat".
	Verdict string `json:"verdict,omitempty"`
	// CheatSeat is who the audit named, when it named anybody.
	CheatSeat *uint32   `json:"cheatSeat,omitempty"`
	Cards     []audCard `json:"cards,omitempty"`
}

// audCard is one recomputed card of an audited hand, rendered the way every
// other card in the interface is.
type audCard struct {
	Slot int    `json:"slot"`
	Card string `json:"card"`
	// Seat is whose hole card this slot was; absent for the board and the
	// muck that never belonged to anybody.
	Seat  *uint32 `json:"seat,omitempty"`
	Board bool    `json:"board,omitempty"`
}

// claimView is one proposal to take a bond, and how far it has got.
type claimView struct {
	Duty driver.Duty `json:"duty"`
	Says string      `json:"says"`
	// Against is the seat whose bond this would take. Ours says this peer
	// proposed or co-signed it; Mine says it is against this peer.
	Against uint32 `json:"against"`
	Ours    bool   `json:"ours,omitempty"`
	Mine    bool   `json:"mine,omitempty"`

	Signed int    `json:"signed"`
	Needs  int    `json:"needs"`
	Done   bool   `json:"done,omitempty"`
	Bond   string `json:"bond,omitempty"`
	// AtomsEach is what one claiming seat would be paid if this lands.
	AtomsEach int64 `json:"atomsEach,omitempty"`
}

// settleView is the payout and how much of it has been agreed.
type settleView struct {
	Hand   uint64   `json:"hand"`
	Signed []uint32 `json:"signed,omitempty"`
	Needs  int      `json:"needs"`
	Done   bool     `json:"done,omitempty"`
	Pays   []payAt  `json:"pays,omitempty"`
	// Blocked is why this table cannot be paid out yet, in the words the
	// draft itself used. Reported rather than translated: the reasons name
	// specific seats and specific missing facts, and a summary would lose
	// the only part anybody can act on.
	Blocked string `json:"blocked,omitempty"`
}

// payAt is one seat's share of a settlement.
type payAt struct {
	Seat    uint32 `json:"seat"`
	Atoms   int64  `json:"atoms"`
	Address string `json:"address,omitempty"`
}

// handleLedger reports where a table's money is.
//
// Unlike /table/hand this answers 200 before the table deals. A table that has
// not dealt still has stakes, bonds, payout addresses and a funding deadline,
// and every one of them is something somebody may need to act on.
func (p *plugin) handleLedger(w http.ResponseWriter, r *http.Request) {
	sid := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sid")))
	view, err := p.tables.Ledger(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, view)
}

// Ledger reports one table's money.
func (t *tables) Ledger(sid string) (*ledgerView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return nil, fmt.Errorf("not at a table under session %s", sid)
	}
	return tbl.ledger(t.height, t.names), nil
}

// ledger is the report itself. Requires the registry lock.
func (tbl *table) ledger(height int64, names map[string]string) *ledgerView {
	v := &ledgerView{
		SID:     tbl.terms.SID,
		Height:  height,
		Roster:  tbl.seatViews(names),
		Accused: tbl.accused,
		Events:  append([]chainEvent(nil), tbl.events...),
	}
	v.Challenges = tbl.challengeViews()
	v.Disputes = tbl.disputeViews()
	if id, ok := tbl.form.MatchID(); ok {
		v.Match = id
	}
	if tbl.play != nil {
		v.Live = tbl.play.Stacks()
		at, stacks := tbl.play.Settled()
		v.Settled = &settledBoundary{Hand: at, Stacks: stacks}
	}
	for _, s := range v.Roster {
		if s.Payout == "" {
			v.PayoutsMissing = append(v.PayoutsMissing, s.Seat)
		}
	}
	v.Claims = tbl.claimViews()
	v.Settlement = tbl.settleView()
	return v
}

// claimViews is every claim in flight at this table.
func (tbl *table) claimViews() []claimView {
	if len(tbl.claims) == 0 {
		return nil
	}
	mine, ours := tbl.form.OurSeat()
	seats, _ := tbl.form.Seats()

	out := make([]claimView, 0, len(tbl.claims))
	for duty, c := range tbl.claims {
		v := claimView{
			Duty:    duty,
			Says:    duty.String(),
			Against: uint32(duty.Seat),
			Mine:    ours && int(mine) == duty.Seat,
			Signed:  len(c.sigs),
			Done:    c.done,
			Bond:    tbl.bonded[uint32(duty.Seat)],
		}
		if terms, err := escrow.ParseTableBond(c.bond); err == nil {
			v.Needs = len(terms.Others)
			if v.Needs > 0 {
				v.AtomsEach = (int64(escrow.MinBondAtoms) - claimFee) / int64(v.Needs)
			}
			// Ours means this peer's signature is on it, which is the
			// honest reading of "we are part of this" - proposing and
			// co-signing are the same commitment.
			if ours {
				if _, on := c.sigs[hex.EncodeToString(seats[mine])]; on {
					v.Ours = true
				}
			}
		}
		out = append(out, v)
	}
	sortClaims(out)
	return out
}

// sortClaims puts claims in a stable order, so a caller polling this does not
// see them shuffle. Map iteration would otherwise reorder them every read and
// make a quiet table look busy.
func sortClaims(v []claimView) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && claimBefore(v[j], v[j-1]); j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func claimBefore(a, b claimView) bool {
	if a.Duty.Hand != b.Duty.Hand {
		return a.Duty.Hand < b.Duty.Hand
	}
	if a.Against != b.Against {
		return a.Against < b.Against
	}
	if a.Duty.Kind != b.Duty.Kind {
		return a.Duty.Kind < b.Duty.Kind
	}
	return a.Duty.At < b.Duty.At
}

// settleView reports the payout, and why there is not one yet when there is not.
func (tbl *table) settleView() *settleView {
	if tbl.play == nil {
		return nil
	}
	hand, _ := tbl.play.Settled()
	v := &settleView{Hand: hand}
	seats, ok := tbl.form.Seats()
	if !ok {
		return v
	}
	v.Needs = len(seats)

	if tbl.settle != nil {
		v.Done = tbl.settle.done
		for key := range tbl.settle.sigs {
			raw, err := hex.DecodeString(key)
			if err != nil {
				continue
			}
			if seat, at := seatOf(seats, raw); at {
				v.Signed = append(v.Signed, seat)
			}
		}
		sortSeats(v.Signed)
		for i, atoms := range tbl.settle.draft.Amounts {
			v.Pays = append(v.Pays, payAt{
				Seat:    uint32(i),
				Atoms:   atoms,
				Address: tbl.payouts[uint32(i)],
			})
		}
		return v
	}

	// No settlement yet. Say why, but only once the table is over - before
	// that "no hand has been agreed yet" is not an obstacle, it is the game
	// being played.
	if tbl.play.Over() {
		if _, err := tbl.settleDraft(); err != nil {
			v.Blocked = err.Error()
		}
	}
	return v
}

func sortSeats(s []uint32) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
