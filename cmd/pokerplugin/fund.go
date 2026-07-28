package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// Funding a table is roster-first, and it has to be.
//
// A deposit script names every member, so the address a seat pays depends on
// the whole membership: a late arrival would change the address of everyone who
// had already paid. That is why nothing here can happen before the table has
// settled and been seated, and why no address exists to hand out before then.
//
// The stake is checked the way a bond is - the rule is local, the facts come
// from the chain through the host - with one difference that matters. A bond is
// judged with a confirmed-only lookup because it is somebody else's claim; our
// own payment, seconds after it was broadcast, is in no block yet and has to be
// found in the mempool. Using the wrong one of those cost 0.1 DCR the first
// time it was written, so the two lookups are named apart deliberately.

// fundWait bounds how long the plugin waits for a person to answer.
//
// The host expires an unanswered request on its own - the default is two
// minutes - so this only has to outlast that rather than outlast a person's
// attention. Waiting far longer, as the bond path first did, does not extend
// anything: it just means the plugin is still holding a request the host has
// already given up on.
const fundWait = 5 * time.Minute

// deposit is where one seat's stake has to be paid.
//
// Derived rather than remembered. Every peer computes all of them from the
// membership it already agreed, so there is no address here that anyone was
// told, and nothing to keep in step with anyone else.
func (tbl *table) deposit(seat uint32, params stdaddr.AddressParams) (membership.Deposit, error) {
	all, err := tbl.form.Deposits(params)
	if err != nil {
		return membership.Deposit{}, err
	}
	for _, d := range all {
		if d.Seat == seat {
			return d, nil
		}
	}
	return membership.Deposit{}, fmt.Errorf("this table has no seat %d", seat)
}

// checkStake asks the chain whether a seat's stake is really there.
//
// Confirmed only. An unconfirmed output can still be replaced, and this is the
// question of whether to play a hand against somebody's money - so the answer
// has to be one that cannot be withdrawn while the hand is under way.
func checkStake(ctx context.Context, chain *transport.Bridge, outpoint, wantPkScript string, buyIn uint64) error {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return err
	}
	out, err := chain.Outpoint(ctx, txid, vout)
	if err != nil {
		return fmt.Errorf("could not check the stake: %w", err)
	}
	switch {
	case !out.Found:
		// Never existed, already spent, or not yet confirmed - the same
		// answer to the only question being asked.
		return fmt.Errorf("%s holds no coin anyone can see", outpoint)
	case !strings.EqualFold(out.PkScriptHex, wantPkScript):
		return fmt.Errorf("%s does not pay this seat's deposit script", outpoint)
	case out.ValueAtoms < int64(buyIn):
		return fmt.Errorf("%s holds %d atoms, and the buy-in is %d", outpoint, out.ValueAtoms, buyIn)
	case out.Confirmations < int64(escrow.StakeConfirmations):
		return fmt.Errorf("%s has %d confirmations, and a stake needs %d",
			outpoint, out.Confirmations, escrow.StakeConfirmations)
	}
	return nil
}

// acceptFunding records another seat's stake, once the chain agrees it is there.
//
// The announcement establishes only who said it. Everything that matters -
// that the output exists, pays this seat's script and holds the buy-in - is
// read from the chain, so a member announcing somebody else's output, an old
// one, or nothing at all is refused by the ledger rather than by an opinion.
//
// The chain lookup happens with the registry unlocked, like a bond's, because
// one slow answer must not stall every other table.
func (t *tables) acceptFunding(ctx context.Context, d transport.Delivery) []outgoing {
	var body schema.Funded
	if err := d.Msg.Into(&body); err != nil {
		log.Printf("pokerplugin: table %s: funded: %v", d.SID, err)
		return nil
	}
	fn, err := body.Into()
	if err != nil {
		log.Printf("pokerplugin: table %s: funded: %v", d.SID, err)
		return nil
	}

	t.mu.Lock()
	tbl := t.m[d.SID]
	if tbl == nil || tbl.gcID != d.GCID {
		t.mu.Unlock()
		return nil
	}
	terms := tbl.terms
	if err := fn.Verify(terms); err != nil {
		t.mu.Unlock()
		log.Printf("pokerplugin: table %s: funded: %v", d.SID, err)
		return nil
	}
	seats, seated := tbl.form.Seats()
	if !seated {
		// No seating means no deposit scripts, so there is nothing this
		// could be checked against yet.
		t.mu.Unlock()
		return nil
	}
	if key, ok := seats[fn.Seat]; !ok || hex.EncodeToString(key) != hex.EncodeToString(fn.Signer) {
		t.mu.Unlock()
		log.Printf("pokerplugin: table %s: a key that does not hold seat %d says it funded it", d.SID, fn.Seat)
		return nil
	}
	if have := tbl.funded[fn.Seat]; have == fn.Outpoint {
		t.mu.Unlock()
		return nil
	}
	want, err := tbl.deposit(fn.Seat, t.params)
	t.mu.Unlock()
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", d.SID, err)
		return nil
	}

	if err := checkStake(ctx, t.chain, fn.Outpoint, want.PkScriptHex, terms.BuyInAtoms); err != nil {
		log.Printf("pokerplugin: table %s: seat %d: %v", d.SID, fn.Seat, err)
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	tbl = t.m[d.SID]
	if tbl == nil {
		return nil
	}
	tbl.funded[fn.Seat] = fn.Outpoint
	t.persist(tbl)
	log.Printf("pokerplugin: table %s: seat %d is funded at %s (%d of %d seats)",
		d.SID, fn.Seat, fn.Outpoint, len(tbl.funded), tbl.terms.Seats)

	// The last stake to arrive is what starts the dealing.
	return tbl.startPlaying()
}

// handleFund pays this player's own stake into the table's escrow.
func (p *plugin) handleFund(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sid := strings.ToLower(strings.TrimSpace(req.SID))

	seat, dep, terms, already, err := p.tables.ourDeposit(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if already != "" {
		// Paid once already. The bond path learned this the hard way:
		// without it, a retried request is a second payment.
		writeJSON(w, map[string]any{"seat": seat, "outpoint": already, "funded": true})
		return
	}
	if open, ok := p.spends.openFor(purposeStake, sid, seat); ok {
		writeOpenSpend(w, open)
		return
	}

	spend, err := p.bridge.RequestSpend(r.Context(), dep.DepositAddr, int64(terms.BuyInAtoms),
		fmt.Sprintf("buy into table %s at seat %d, refundable by you alone after the timelock", sid, seat))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	pending := &pendingSpend{
		ID: spend.ID, Purpose: purposeStake, SID: sid, Seat: seat,
		PkScript: dep.PkScriptHex, AtAtoms: int64(terms.BuyInAtoms), State: "pending",
	}
	p.spends.put(pending)

	if wantsDetach(body) {
		// The caller is a page, and a page cannot hold a request open
		// for five minutes while somebody decides. The work is
		// identical; only the waiting moves.
		p.detach(pending, fundWait)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"seat": seat, "spendId": spend.ID, "state": "pending"})
		return
	}

	// Detached from the request context: a person answering takes as long as
	// it takes, and the caller hanging up must not lose the outcome.
	done, err := p.awaitSpend(context.Background(), pending, fundWait)
	if err != nil {
		writeErr(w, spendErrStatus(done), err)
		return
	}
	writeJSON(w, map[string]any{"seat": seat, "outpoint": done.Outpoint, "funded": true})
}

// paysOurDeposit checks that an outpoint somebody named really does pay this
// seat's own deposit script.
//
// The one thing standing between "take this back" and "sign away anything I am
// pointed at". Shared by the route that records a stake and the route that
// takes one back, because both are told an outpoint by a caller.
func (p *plugin) paysOurDeposit(ctx context.Context, outpoint, wantScript string) error {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return err
	}
	// Confirmations are not required here, for the reason /bond/set does not
	// require them: a peer will refuse a stake it cannot see yet, and that
	// resolves itself as blocks arrive. The reclaim path checks maturity
	// against the chain itself regardless.
	out, err := p.bridge.UnconfirmedOutpoint(ctx, txid, vout)
	if err != nil {
		return err
	}
	switch {
	case !out.Found:
		return fmt.Errorf("%s holds no coin", outpoint)
	case !strings.EqualFold(out.PkScriptHex, wantScript):
		return fmt.Errorf("%s does not pay this seat's deposit script", outpoint)
	}
	return nil
}

// spendErrStatus turns what went wrong into a status a caller can act on.
func spendErrStatus(s pendingSpend) int {
	switch {
	case s.State == "unanswered", s.State == "unknown":
		return http.StatusGatewayTimeout
	case s.TxID != "":
		// The money moved and something after that failed. The /set
		// routes are the way back.
		return http.StatusBadGateway
	default:
		return http.StatusForbidden
	}
}

// handleDepositSet records a stake that was paid but never written down.
//
// The same recovery /bond/set is: a payment can be approved after the caller
// that asked for it has gone, and money that moved with nothing pointing at it
// is worse than money that has not moved. The claim is checked against the
// script this seat is required to pay, so it can only ever name our own stake.
func (p *plugin) handleDepositSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SID      string `json:"sid"`
		Outpoint string `json:"outpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sid := strings.ToLower(strings.TrimSpace(req.SID))

	seat, dep, _, already, err := p.tables.ourDeposit(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if already != "" {
		writeJSON(w, map[string]any{"seat": seat, "outpoint": already, "funded": true})
		return
	}

	if err := p.paysOurDeposit(r.Context(), req.Outpoint, dep.PkScriptHex); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	outgoing, err := p.recordOwnStake(sid, seat, strings.TrimSpace(req.Outpoint))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	p.publish(p.ctx, outgoing)
	writeJSON(w, map[string]any{"seat": seat, "outpoint": strings.TrimSpace(req.Outpoint), "funded": true})
}

// ourDeposit reports which seat this peer holds and where its stake must go.
func (t *tables) ourDeposit(sid string) (seat uint32, dep membership.Deposit, terms membership.Terms, already string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return 0, membership.Deposit{}, membership.Terms{}, "", fmt.Errorf("not at table %q", sid)
	}
	if tbl.form.State() != membership.Settled {
		return 0, membership.Deposit{}, membership.Terms{}, "",
			fmt.Errorf("table %s has not settled, so it has no deposit script yet", sid)
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return 0, membership.Deposit{}, membership.Terms{}, "",
			fmt.Errorf("table %s has not been seated yet", sid)
	}
	dep, err = tbl.deposit(seat, t.params)
	if err != nil {
		return 0, membership.Deposit{}, membership.Terms{}, "", err
	}
	return seat, dep, tbl.terms, tbl.funded[seat], nil
}

// recordOwnStake writes down what this peer paid, and says so to the table.
//
// Written before it is announced, and announced from what was written: the
// announcement is a pointer to money that has already moved, and losing track
// of that is the failure worth avoiding.
func (t *tables) recordOwnStakeLocked(tbl *table, seat uint32, outpoint string) {
	tbl.funded[seat] = outpoint
	t.persist(tbl)
}

func (p *plugin) recordOwnStake(sid string, seat uint32, outpoint string) ([]outgoing, error) {
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()

	tbl := p.tables.m[sid]
	if tbl == nil {
		return nil, fmt.Errorf("not at table %q", sid)
	}
	p.tables.recordOwnStakeLocked(tbl, seat, outpoint)

	fn, err := p.tables.signFunding(tbl.terms, seat, outpoint)
	if err != nil {
		return nil, err
	}
	log.Printf("pokerplugin: table %s: paid seat %d's stake into %s", sid, seat, outpoint)
	return []outgoing{tbl.frame(schema.KindFunded, schema.FundedFrom(fn), wire.ClassState)}, nil
}

// findDepositOutput locates which output of a payment we just made pays the
// deposit script.
//
// It matches on the script rather than trusting an index, so it can only ever
// find our own stake - and it looks in the mempool, because a transaction
// broadcast seconds ago is in no block. That distinction is the whole of a bug
// that once spent a bond it could not then cite.
func (p *plugin) findDepositOutput(ctx context.Context, txid, wantPkScript string) (string, error) {
	for vout := uint32(0); vout < maxFundingVout; vout++ {
		out, err := p.bridge.UnconfirmedOutpoint(ctx, txid, vout)
		if err != nil {
			return "", err
		}
		if out.Found && strings.EqualFold(out.PkScriptHex, wantPkScript) {
			return fmt.Sprintf("%s:%d", txid, vout), nil
		}
	}
	return "", fmt.Errorf("none of the first %d outputs of %s pay this seat's deposit", maxFundingVout, txid)
}
