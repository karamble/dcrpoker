package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
func checkTableBond(ctx context.Context, chain *transport.Bridge, outpoint, wantPkScript string) error {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return err
	}
	out, err := chain.Outpoint(ctx, txid, vout)
	if err != nil {
		return fmt.Errorf("could not check the bond: %w", err)
	}
	switch {
	case !out.Found:
		return fmt.Errorf("%s holds no coin anyone can see", outpoint)
	case !strings.EqualFold(out.PkScriptHex, wantPkScript):
		return fmt.Errorf("%s does not pay this seat's bond script", outpoint)
	case out.ValueAtoms < int64(escrow.MinBondAtoms):
		return fmt.Errorf("%s holds %d atoms, and a bond is at least %d",
			outpoint, out.ValueAtoms, escrow.MinBondAtoms)
	case out.Confirmations < int64(escrow.BondConfirmations):
		return fmt.Errorf("%s has %d confirmations, and a bond needs %d",
			outpoint, out.Confirmations, escrow.BondConfirmations)
	}
	return nil
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

	if err := checkTableBond(ctx, t.chain, bn.Outpoint, want.PkScriptHex); err != nil {
		log.Printf("pokerplugin: table %s: seat %d's bond: %v", d.SID, bn.Seat, err)
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	tbl = t.m[d.SID]
	if tbl == nil {
		return nil
	}
	tbl.bonded[bn.Seat] = bn.Outpoint
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
	var req struct {
		SID string `json:"sid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		writeJSON(w, map[string]any{"seat": seat, "outpoint": already, "bonded": true})
		return
	}

	spend, err := p.bridge.RequestSpend(r.Context(), bond.Address, int64(escrow.MinBondAtoms),
		fmt.Sprintf("bond seat %d at table %s, forfeit to the others if you walk out mid-hand, "+
			"yours again after the lock", seat, sid))
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), fundWait)
	defer cancel()
	settled, err := p.bridge.AwaitSpend(ctx, spend.ID)
	if err != nil {
		writeErr(w, http.StatusGatewayTimeout, err)
		return
	}
	if settled.State != transport.SpendApproved {
		writeErr(w, http.StatusForbidden,
			fmt.Errorf("the host %s the bond: %s", settled.State, settled.Error))
		return
	}
	outpoint, err := p.findDepositOutput(ctx, settled.TxID, bond.PkScriptHex)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	out, err := p.recordOwnBond(sid, seat, outpoint)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	p.publish(ctx, out)
	writeJSON(w, map[string]any{"seat": seat, "outpoint": outpoint, "bonded": true})
}
