package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/decred/dcrd/txscript/v4/stdaddr"

	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// The bond a table can take, as opposed to the one that buys a seat.
//
// There are two and they answer different things. The standing bond posted at
// registration makes identities cost something, and it is deliberately not
// forfeitable: at registration there is no roster to name, so there is nobody it
// could be forfeited to. This one is posted once the seating is drawn, names the
// whole table, and is what a seat loses for holding everyone up or walking out
// of a hand.
//
// A table deals only when every seat has both. Dealing without bonds would mean
// playing with no answer to somebody who stops, which is the situation the whole
// claim mechanism exists to avoid.

// bond returns where this table's bond for a seat must be paid.
func (tbl *table) bond(seat uint32, params stdaddr.AddressParams) (membership.TableBond, error) {
	all, err := tbl.form.TableBonds(params)
	if err != nil {
		return membership.TableBond{}, err
	}
	for _, b := range all {
		if b.Seat == seat {
			return b, nil
		}
	}
	return membership.TableBond{}, fmt.Errorf("this table has no seat %d", seat)
}

// checkTableBond asks the chain whether a seat's bond is really there.
//
// Confirmed, and paying the script this peer derived itself from the roster it
// already agreed - not one the announcement supplied. A bond nobody else can
// claim looks exactly like a real one from the outside, and the difference is
// the entire point of having it.
// Returns what the output holds, because releasing this bond later needs the
// amount and this is the one place that has already looked it up.
func checkTableBond(ctx context.Context, chain *transport.Bridge, outpoint, wantPkScript string) (int64, error) {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return 0, err
	}
	out, err := chain.Outpoint(ctx, txid, vout)
	if err != nil {
		return 0, fmt.Errorf("could not check the bond: %w", err)
	}
	switch {
	case !out.Found:
		return 0, fmt.Errorf("%s holds no coin anyone can see", outpoint)
	case !strings.EqualFold(out.PkScriptHex, wantPkScript):
		return 0, fmt.Errorf("%s does not pay this seat's bond script", outpoint)
	case out.ValueAtoms < int64(escrow.MinBondAtoms):
		return 0, fmt.Errorf("%s holds %d atoms, and a bond is at least %d",
			outpoint, out.ValueAtoms, escrow.MinBondAtoms)
	case out.Confirmations < int64(escrow.BondConfirmations):
		return 0, fmt.Errorf("%s has %d confirmations, and a bond needs %d",
			outpoint, out.Confirmations, escrow.BondConfirmations)
	}
	return out.ValueAtoms, nil
}

// acceptBond records another seat's bond, once the chain agrees it is there.
//
// Same shape as a stake and for the same reason: the announcement establishes
// only who said it, and everything that matters is read from the ledger. The
// lookup happens with the registry unlocked, because one slow answer must not
// stall every other table.
func (t *tables) acceptBond(ctx context.Context, d transport.Delivery) []outgoing {
	var body schema.Bonded
	if err := d.Msg.Into(&body); err != nil {
		log.Printf("pokerplugin: table %s: bonded: %v", d.SID, err)
		return nil
	}
	bn, err := body.Into()
	if err != nil {
		log.Printf("pokerplugin: table %s: bonded: %v", d.SID, err)
		return nil
	}

	t.mu.Lock()
	tbl := t.m[d.SID]
	if tbl == nil || tbl.gcID != d.GCID {
		t.mu.Unlock()
		return nil
	}
	terms := tbl.terms
	if err := bn.Verify(terms); err != nil {
		t.mu.Unlock()
		log.Printf("pokerplugin: table %s: bonded: %v", d.SID, err)
		return nil
	}
	seats, seated := tbl.form.Seats()
	if !seated {
		t.mu.Unlock()
		return nil
	}
	if key, ok := seats[bn.Seat]; !ok || hex.EncodeToString(key) != hex.EncodeToString(bn.Signer) {
		t.mu.Unlock()
		log.Printf("pokerplugin: table %s: a key that does not hold seat %d says it bonded it",
			d.SID, bn.Seat)
		return nil
	}
	if have := tbl.bonded[bn.Seat]; have == bn.Outpoint {
		t.mu.Unlock()
		return nil
	}
	want, err := tbl.bond(bn.Seat, t.params)
	t.mu.Unlock()
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", d.SID, err)
		return nil
	}

	value, err := checkTableBond(ctx, t.chain, bn.Outpoint, want.PkScriptHex)
	if err != nil {
		log.Printf("pokerplugin: table %s: seat %d's bond: %v", d.SID, bn.Seat, err)
		// A bond needs two confirmations, so the first telling is always
		// refused and a person is owed the difference between "waiting" and
		// "never arrived".
		t.noteWaiting(d.SID, bn.Seat, true,
			look(ctx, t.chain, bn.Outpoint, int64(escrow.BondConfirmations)))
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	tbl = t.m[d.SID]
	if tbl == nil {
		return nil
	}
	if held, ok := tbl.bonded[bn.Seat]; ok && held != bn.Outpoint {
		// The first one stands, as with a stake: a seat naming two bonds is
		// naming two, and a claim can only be built against one.
		log.Printf("pokerplugin: table %s: seat %d announced a second bond at %s "+
			"while %s stands; keeping the first", d.SID, bn.Seat, bn.Outpoint, held)
		return nil
	}
	tbl.bonded[bn.Seat] = bn.Outpoint
	tbl.bondValue[bn.Seat] = value
	tbl.uids[bn.Seat] = d.Sender
	delete(tbl.bondWaiting, bn.Seat)
	t.persist(tbl)
	log.Printf("pokerplugin: table %s: seat %d is bonded at %s (%d of %d seats)",
		d.SID, bn.Seat, bn.Outpoint, len(tbl.bonded), tbl.terms.Seats)

	// The last bond, like the last stake, is what starts the dealing.
	return tbl.startPlaying()
}

// ourBond reports where this player's own bond for a table must be paid.
func (t *tables) ourBond(sid string) (seat uint32, b membership.TableBond, already string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return 0, membership.TableBond{}, "", fmt.Errorf("not at a table under session %s", sid)
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return 0, membership.TableBond{}, "", fmt.Errorf("this table has no seating yet")
	}
	b, err = tbl.bond(seat, t.params)
	if err != nil {
		return 0, membership.TableBond{}, "", err
	}
	return seat, b, tbl.bonded[seat], nil
}

// recordOwnBond remembers this player's own bond and announces it.
func (p *plugin) recordOwnBond(sid string, seat uint32, outpoint string) ([]outgoing, error) {
	p.tables.mu.Lock()
	tbl := p.tables.m[sid]
	if tbl == nil {
		p.tables.mu.Unlock()
		return nil, fmt.Errorf("not at a table under session %s", sid)
	}
	tbl.bonded[seat] = outpoint
	p.tables.persist(tbl)

	priv, err := p.id.sessionKey(tbl.terms.SID)
	if err != nil {
		p.tables.mu.Unlock()
		return nil, err
	}
	bn, err := membership.SignBonded(tbl.terms, seat, outpoint, priv)
	if err != nil {
		p.tables.mu.Unlock()
		return nil, err
	}
	out := []outgoing{tbl.frame(schema.KindBonded, schema.BondedFrom(bn), wire.ClassState)}
	out = append(out, tbl.startPlaying()...)
	p.tables.mu.Unlock()
	return out, nil
}

// announceOwnBond says again where this seat's bond already is.
//
// The same frame recordOwnBond sends, without the recording: the bond is on the
// chain and written down, and the only thing missing is that somebody else
// heard about it.
func (p *plugin) announceOwnBond(sid string) ([]outgoing, error) {
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()

	tbl := p.tables.m[sid]
	if tbl == nil {
		return nil, fmt.Errorf("not at a table under session %s", sid)
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return nil, fmt.Errorf("this table has no seating yet")
	}
	outpoint := tbl.bonded[seat]
	if outpoint == "" {
		return nil, fmt.Errorf("this seat has posted no bond")
	}
	if p.tables.signBonded == nil {
		return nil, fmt.Errorf("nothing here can sign a bond announcement")
	}
	bn, err := p.tables.signBonded(tbl.terms, seat, outpoint)
	if err != nil {
		return nil, err
	}
	return []outgoing{tbl.frame(schema.KindBonded, schema.BondedFrom(bn), wire.ClassState)}, nil
}

// handleTableBond pays this player's forfeitable bond for a table.
//
// A second approval after the stake, and it has to be: a spend pays one address,
// and these are two different scripts with two different jobs. The person is
// told which is which, because "this is what you lose for walking out" is not
// the same request as "this is what you are playing for".
func (p *plugin) handleTableBond(w http.ResponseWriter, r *http.Request) {
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

	seat, bond, already, err := p.tables.ourBond(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if already != "" {
		// Paid, so nothing is asked for again - but say where it is once
		// more on the way out. Somebody pressing this a second time is
		// somebody whose table has not started, and the likeliest reason
		// is a peer that never accepted this bond. Answering "yes, done"
		// and sending nothing is the one response that cannot help them.
		if out, err := p.announceOwnBond(sid); err != nil {
			log.Printf("pokerplugin: table %s: cannot say again where our bond is: %v", sid, err)
		} else {
			p.publish(r.Context(), out)
		}
		writeJSON(w, map[string]any{"seat": seat, "outpoint": already, "bonded": true, "announced": true})
		return
	}
	if open, ok := p.spends.openFor(purposeTableBond, sid, seat); ok {
		writeOpenSpend(w, open)
		return
	}

	spend, err := p.bridge.RequestSpend(r.Context(), bond.Address, int64(escrow.MinBondAtoms),
		fmt.Sprintf("bond seat %d at table %s, forfeit to the others if you walk out mid-hand, "+
			"yours again after the lock", seat, sid))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	pending := &pendingSpend{
		ID: spend.ID, Purpose: purposeTableBond, SID: sid, Seat: seat,
		PkScript: bond.PkScriptHex, AtAtoms: int64(escrow.MinBondAtoms), State: "pending",
	}
	p.spends.put(pending)

	if wantsDetach(body) {
		p.detach(pending, fundWait)
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"seat": seat, "spendId": spend.ID, "state": "pending"})
		return
	}

	done, err := p.awaitSpend(context.Background(), pending, fundWait)
	if err != nil {
		writeErr(w, spendErrStatus(done), err)
		return
	}
	writeJSON(w, map[string]any{"seat": seat, "outpoint": done.Outpoint, "bonded": true})
}

// ourTableBond reports where this seat's forfeitable bond sits and what script
// holds it.
//
// The outpoint is bondedAt before bonded, because answering a claim respends a
// bond into an identical one and the answer is what is on the chain afterwards.
// Reclaiming the outpoint the bond was first paid to would be reclaiming
// something already spent - the same fallback refreshDraft makes, for the same
// reason.
func (t *tables) ourTableBond(sid string) (seat uint32, bond membership.TableBond, outpoint string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return 0, membership.TableBond{}, "", fmt.Errorf("not at table %q", sid)
	}
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return 0, membership.TableBond{}, "", fmt.Errorf("table %s was never seated, so it has no bond of ours", sid)
	}
	bond, err = tbl.bond(seat, t.params)
	if err != nil {
		return 0, membership.TableBond{}, "", err
	}
	outpoint = tbl.bondedAt[seat]
	if outpoint == "" {
		outpoint = tbl.bonded[seat]
	}
	if outpoint == "" {
		return 0, membership.TableBond{}, "", fmt.Errorf("no bond of ours was ever seen at seat %d of %s", seat, sid)
	}
	return seat, bond, outpoint, nil
}

// handleTableBondSweep takes this seat's forfeitable bond back on its own.
//
// The backstop branch: the owner alone, after the long lock. It is the way out
// of a table that dissolved rather than ended - one that never dealt, or one
// whose hand could not finish - where the cooperative release needs signatures
// from peers who are no longer there to give them.
//
// Slow on purpose. The lock has to outlast the table by enough that nobody can
// sit out their own claim window, so this is a week rather than the stake's few
// hours. That is the price of a branch that needs nobody's agreement, and it is
// the right trade for the one path that still works when nothing else does.
func (p *plugin) handleTableBondSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SID      string `json:"sid"`
		DestAddr string `json:"destAddr"`
		FeeAtoms int64  `json:"feeAtoms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sid := strings.ToLower(strings.TrimSpace(req.SID))

	seat, bond, outpoint, err := p.tables.ourTableBond(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	script, err := hex.DecodeString(bond.ScriptHex)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("this seat's bond script: %w", err))
		return
	}
	// The bond names the session key as its owner, because that is the key the
	// roster seats. The identity's own bond key holds the standing deposit and
	// could not spend this.
	key, err := p.id.sessionKey(sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	txid, err := p.reclaim(r.Context(), outpoint, script, key, membership.TableBondBlocks,
		escrow.BackstopSigScript, req.DestAddr, req.FeeAtoms)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	p.tables.forgetTableBond(sid, seat)
	log.Printf("pokerplugin: table %s: reclaimed seat %d's bond in %s", sid, seat, txid)
	writeJSON(w, map[string]any{"sid": sid, "seat": seat, "txid": txid})
}

// forgetTableBond stops a table citing a bond it has spent.
//
// Same discipline as forgetStake: a table still announcing an output that is
// gone is a table telling its peers something untrue about the chain.
func (t *tables) forgetTableBond(sid string, seat uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tbl := t.m[sid]
	if tbl == nil {
		return
	}
	delete(tbl.bonded, seat)
	delete(tbl.bondedAt, seat)
	t.persist(tbl)
}

// handleTableBonds reports every table bond this player holds and when each
// comes back.
//
// A list rather than a field on the table, because these outlive the table by a
// week: the lock has to outlast the game by enough that nobody can sit out their
// own claim window, so by the time one matures the table it belonged to is long
// finished and would not be the place anybody looks. This is what the host's
// Bonds screen is built from.
func (p *plugin) handleTableBonds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	type held struct {
		SID       string `json:"sid"`
		Seat      uint32 `json:"seat"`
		Outpoint  string `json:"outpoint"`
		Address   string `json:"address,omitempty"`
		Atoms     int64  `json:"atoms,omitempty"`
		MinBlocks uint32 `json:"minBlocks"`

		Confirmations int64  `json:"confirmations,omitempty"`
		Height        int64  `json:"height,omitempty"`
		MaturesAt     int64  `json:"maturesAt,omitempty"`
		BlocksLeft    int64  `json:"blocksLeft,omitempty"`
		Spendable     bool   `json:"spendable,omitempty"`
		Spent         bool   `json:"spent,omitempty"`
		ChainErr      string `json:"chainErr,omitempty"`
	}

	// The outpoints first, under the lock, then the chain without it: one slow
	// lookup must not stall every table.
	type want struct {
		sid      string
		seat     uint32
		outpoint string
		address  string
	}
	var wants []want
	p.tables.mu.Lock()
	for sid, tbl := range p.tables.m {
		seat, ok := tbl.form.OurSeat()
		if !ok {
			continue
		}
		outpoint := tbl.bondedAt[seat]
		if outpoint == "" {
			outpoint = tbl.bonded[seat]
		}
		if outpoint == "" {
			continue
		}
		w := want{sid: sid, seat: seat, outpoint: outpoint}
		if b, err := tbl.bond(seat, p.tables.params); err == nil {
			w.address = b.Address
		}
		wants = append(wants, w)
	}
	p.tables.mu.Unlock()

	sort.Slice(wants, func(i, j int) bool { return wants[i].sid < wants[j].sid })

	ctx := r.Context()
	tip, tipErr := p.bridge.ChainTip(ctx)
	out := make([]held, 0, len(wants))
	for _, w := range wants {
		h := held{SID: w.sid, Seat: w.seat, Outpoint: w.outpoint, Address: w.address,
			MinBlocks: membership.TableBondBlocks}
		txid, vout, err := splitOutpoint(w.outpoint)
		if err != nil {
			h.ChainErr = err.Error()
			out = append(out, h)
			continue
		}
		found, err := p.bridge.Outpoint(ctx, txid, vout)
		switch {
		case err != nil:
			h.ChainErr = err.Error()
		case !found.Found:
			// Never confirmed, or already taken back. Both mean the same
			// thing to somebody asking what is still locked up.
			h.Spent = true
		default:
			h.Atoms = found.ValueAtoms
			h.Confirmations = found.Confirmations
			if tipErr == nil {
				h.Height = tip.Height
				h.MaturesAt = tip.Height - found.Confirmations + 1 + int64(membership.TableBondBlocks)
			}
			if left := int64(membership.TableBondBlocks) - found.Confirmations; left > 0 {
				h.BlocksLeft = left
			} else {
				h.Spendable = true
			}
		}
		out = append(out, h)
	}
	writeJSON(w, map[string]any{"bonds": out})
}
