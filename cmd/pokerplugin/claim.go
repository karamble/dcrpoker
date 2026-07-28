package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"

	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	gwire "github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// Taking a bond from a seat that has stopped.
//
// Nobody decides anything here, which is the point. A claim proposes a
// transaction and names the obligation the log says its target owes; every other
// seat checks that against its own copy of the log and its own arithmetic before
// signing; and what finally settles it is neither the message nor the
// signatures, but a race on chain that the accused wins by answering.
//
// So the whole of this file is: notice, propose, check, sign, assemble. There is
// no vote and nothing to be convinced of. A peer that disagrees does not sign, a
// peer that never received it loses nothing, and a peer that has simply not
// caught up refuses - which costs the claimant a retry and costs an honest seat
// nothing at all.

// claimAfter is how long an obligation must stand before this peer will propose
// taking somebody's bond over it.
//
// Longer than the on-chain window, deliberately. The accused gets ClaimBlocks
// more after the claim confirms, so this is the part that says "you are not
// coming back" rather than "you are slow" - and the cost of being wrong is
// somebody's bond, while the cost of waiting is a few more minutes.
const claimAfter uint32 = 2 * escrow.ClaimBlocks

// claimFee is left for the miner out of a forfeited bond.
const claimFee int64 = 10_000

// proposeClaims opens a claim against any seat that has stopped, at any table.
//
// Called as the chain moves, because that is the only clock here. Nothing
// happens at most tables most of the time, so this answers "is there anything to
// do" cheaply and almost always says no.
func (t *tables) proposeClaims() []outgoing {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []outgoing
	for _, tbl := range t.m {
		out = append(out, tbl.proposeClaim(t.params)...)
	}
	return out
}

func (tbl *table) proposeClaim(params stdaddr.AddressParams) []outgoing {
	if tbl.play == nil || tbl.play.Over() {
		return nil
	}
	duty, ok := tbl.play.Claimable(claimAfter)
	if !ok {
		return nil
	}
	seat, ours := tbl.form.OurSeat()
	if !ours || int(seat) == duty.Seat {
		// Nobody proposes a claim against themselves. If this peer is the
		// one holding the table up, the answer is to act.
		return nil
	}
	if tbl.claims[duty] != nil {
		// Already proposed. Saying it again would only produce a second
		// transaction for the same thing, and signatures collected against
		// one of them would not fit the other.
		return nil
	}

	draft, bond, err := tbl.claimDraft(duty.Seat, params)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot build a claim: %v", tbl.terms.SID, err)
		return nil
	}
	tx, err := escrow.BuildClaim(draft)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot build a claim: %v", tbl.terms.SID, err)
		return nil
	}
	raw, err := tx.Bytes()
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}

	c := &claim{duty: duty, tx: tx, bond: bond, sigs: map[string][]byte{}}
	tbl.claims[duty] = c
	log.Printf("pokerplugin: table %s: proposing a claim - %s", tbl.terms.SID, duty)
	tbl.note(eventProposed, fmt.Sprintf("proposed taking seat %d's bond: %s", duty.Seat, duty),
		"", seatp(duty.Seat))

	return []outgoing{tbl.frame(schema.KindClaim, schema.Claim{
		Seat:         uint32(duty.Seat),
		Duty:         duty,
		BondOutpoint: tbl.bonded[uint32(duty.Seat)],
		BondScript:   hex.EncodeToString(bond),
		Tx:           hex.EncodeToString(raw),
	}, gwire.ClassState)}
}

// claim is one proposal and the signatures gathered for it.
type claim struct {
	duty driver.Duty
	tx   *wire.MsgTx
	bond []byte
	// sigs is each claiming seat's signature, by its key in hex.
	sigs map[string][]byte
	done bool
}

// claimDraft works out what a claim against a seat would pay, and to whom.
func (tbl *table) claimDraft(against int, params stdaddr.AddressParams) (escrow.ClaimDraft, []byte, error) {
	outpoint := tbl.bonded[uint32(against)]
	if outpoint == "" {
		return escrow.ClaimDraft{}, nil, fmt.Errorf("seat %d has no bond on the chain", against)
	}
	b, err := tbl.bond(uint32(against), params)
	if err != nil {
		return escrow.ClaimDraft{}, nil, err
	}
	script, err := hex.DecodeString(b.ScriptHex)
	if err != nil {
		return escrow.ClaimDraft{}, nil, err
	}
	terms, err := escrow.ParseTableBond(script)
	if err != nil {
		return escrow.ClaimDraft{}, nil, err
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return escrow.ClaimDraft{}, nil, fmt.Errorf("this table has no seating")
	}

	// One payout per claiming seat, in the bond's own canonical order -
	// which is what every co-signer will derive too, so they all build the
	// same bytes.
	pays := make([][]byte, 0, len(terms.Others))
	for _, key := range terms.Others {
		seat, ok := seatOf(seats, key)
		if !ok {
			return escrow.ClaimDraft{}, nil, fmt.Errorf("a bond names a key not at this table")
		}
		addr := tbl.payouts[seat]
		if addr == "" {
			return escrow.ClaimDraft{}, nil, fmt.Errorf(
				"seat %d has not said where to pay it, so a claim cannot be built", seat)
		}
		pay, err := payScriptFor(addr, params)
		if err != nil {
			return escrow.ClaimDraft{}, nil, fmt.Errorf("seat %d payout: %w", seat, err)
		}
		pays = append(pays, pay)
	}

	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return escrow.ClaimDraft{}, nil, err
	}
	var h chainhash.Hash
	if err := chainhash.Decode(&h, txid); err != nil {
		return escrow.ClaimDraft{}, nil, fmt.Errorf("bond outpoint: %w", err)
	}
	prevout := wire.OutPoint{Hash: h, Index: vout, Tree: wire.TxTreeRegular}
	return escrow.ClaimDraft{
		Bond:       script,
		Prevout:    prevout,
		ValueAtoms: int64(escrow.MinBondAtoms),
		PayScripts: pays,
		FeeAtoms:   claimFee,
	}, script, nil
}

func seatOf(seats map[uint32][]byte, key []byte) (uint32, bool) {
	for seat, k := range seats {
		if hex.EncodeToString(k) == hex.EncodeToString(key) {
			return seat, true
		}
	}
	return 0, false
}

// acceptClaim is what this peer does when somebody else proposes taking a bond.
//
// Three checks and then a signature. That the log really says the accused owes
// what the claim names, against this peer's own copy. That the transaction is
// the one this peer would have built, byte for byte. And that this peer is
// actually named in the bond, because signing for a bond that cannot pay it is
// signing somebody else's business.
//
// A refusal is silent. There is nothing to argue about and nobody to argue with:
// the claimant will try again, and if it was wrong it will keep being refused.
func (tbl *table) acceptClaim(body schema.Claim, params stdaddr.AddressParams) []outgoing {
	if err := body.Validate(); err != nil {
		log.Printf("pokerplugin: table %s: claim: %v", tbl.terms.SID, err)
		return nil
	}
	if tbl.play == nil {
		return nil
	}
	seat, ours := tbl.form.OurSeat()
	if !ours || int(seat) == body.Duty.Seat {
		// A claim against this peer is answered, not co-signed.
		return nil
	}
	// Does this peer's own log say the same thing?
	if err := tbl.play.Agrees(body.Duty); err != nil {
		log.Printf("pokerplugin: table %s: not co-signing a claim: %v", tbl.terms.SID, err)
		tbl.note(eventRefused, fmt.Sprintf("did not co-sign a claim against seat %d: %v",
			body.Duty.Seat, err), "", seatp(body.Duty.Seat))
		return nil
	}
	// Is it the transaction this peer would have built?
	raw, err := hex.DecodeString(body.Tx)
	if err != nil {
		return nil
	}
	tx := wire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		log.Printf("pokerplugin: table %s: claim transaction: %v", tbl.terms.SID, err)
		return nil
	}
	draft, bond, err := tbl.claimDraft(body.Duty.Seat, params)
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}
	if err := escrow.CheckClaimDraft(tx, draft); err != nil {
		log.Printf("pokerplugin: table %s: not co-signing a claim: %v", tbl.terms.SID, err)
		tbl.note(eventRefused, fmt.Sprintf("a claim against seat %d was not the transaction this peer would have built: %v",
			body.Duty.Seat, err), "", seatp(body.Duty.Seat))
		return nil
	}
	// And is this peer one of the seats the bond can pay?
	terms, err := escrow.ParseTableBond(bond)
	if err != nil {
		return nil
	}
	seats, _ := tbl.form.Seats()
	if _, err := escrow.MemberIndex(terms, seats[seat]); err != nil {
		log.Printf("pokerplugin: table %s: not co-signing a bond that cannot pay this seat", tbl.terms.SID)
		tbl.note(eventRefused, fmt.Sprintf("did not co-sign a claim against seat %d: its bond cannot pay this seat",
			body.Duty.Seat), "", seatp(body.Duty.Seat))
		return nil
	}

	priv := tbl.session
	if priv == nil {
		log.Printf("pokerplugin: table %s: no session key to sign a claim with", tbl.terms.SID)
		return nil
	}
	sig, err := escrow.SignBondSpend(tx, bond, priv)
	if err != nil {
		log.Printf("pokerplugin: table %s: signing a claim: %v", tbl.terms.SID, err)
		return nil
	}

	c := tbl.claims[body.Duty]
	if c == nil {
		c = &claim{duty: body.Duty, tx: tx, bond: bond, sigs: map[string][]byte{}}
		tbl.claims[body.Duty] = c
	}
	c.sigs[hex.EncodeToString(seats[seat])] = sig
	tbl.note(eventCosigned, fmt.Sprintf("co-signed taking seat %d's bond: %s", body.Duty.Seat, body.Duty),
		"", seatp(body.Duty.Seat))

	return []outgoing{tbl.frame(schema.KindClaim, schema.Claim{
		Seat:         uint32(body.Duty.Seat),
		Duty:         body.Duty,
		BondOutpoint: body.BondOutpoint,
		BondScript:   body.BondScript,
		Tx:           body.Tx,
		Signer:       hex.EncodeToString(seats[seat]),
		Sig:          hex.EncodeToString(sig),
	}, gwire.ClassState)}
}

// collectClaimSig records a co-signer's signature and broadcasts once they are
// all in.
func (tbl *table) collectClaimSig(ctx context.Context, chain broadcaster, body schema.Claim) {
	c := tbl.claims[body.Duty]
	if c == nil || c.done {
		return
	}
	sig, err := hex.DecodeString(body.Sig)
	if err != nil || len(sig) == 0 {
		return
	}
	terms, err := escrow.ParseTableBond(c.bond)
	if err != nil {
		return
	}
	signer, err := hex.DecodeString(body.Signer)
	if err != nil {
		return
	}
	if _, err := escrow.MemberIndex(terms, signer); err != nil {
		// A signature from somebody the bond cannot pay. Not evidence of
		// anything, and not usable.
		return
	}
	c.sigs[body.Signer] = sig

	sigs := make([][]byte, 0, len(terms.Others))
	for _, key := range terms.Others {
		s, ok := c.sigs[hex.EncodeToString(key)]
		if !ok {
			return // still short of somebody
		}
		sigs = append(sigs, s)
	}

	done, err := escrow.FinishClaim(c.tx, c.bond, sigs, tbl.netParams)
	if err != nil {
		log.Printf("pokerplugin: table %s: a fully signed claim did not satisfy the bond: %v",
			tbl.terms.SID, err)
		tbl.note(eventBlocked, fmt.Sprintf("a fully signed claim against seat %d did not satisfy the bond: %v",
			c.duty.Seat, err), "", seatp(c.duty.Seat))
		return
	}
	raw, err := done.Bytes()
	if err != nil {
		return
	}
	c.done = true
	txid, err := chain.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		// The window is what matters, not this attempt. Another peer holds
		// the same signatures and will broadcast the same transaction.
		log.Printf("pokerplugin: table %s: could not broadcast a claim: %v", tbl.terms.SID, err)
		tbl.note(eventBlocked, fmt.Sprintf("this peer could not send a claim against seat %d; another holds the same one: %v",
			c.duty.Seat, err), "", seatp(c.duty.Seat))
		return
	}
	log.Printf("pokerplugin: table %s: claimed seat %d's bond in %s - %s",
		tbl.terms.SID, c.duty.Seat, txid, c.duty)
	tbl.note(eventClaimed, fmt.Sprintf("took seat %d's bond: %s", c.duty.Seat, c.duty),
		txid, seatp(c.duty.Seat))
}

// broadcaster is what a claim needs from the chain, and nothing more.
type broadcaster interface {
	Broadcast(ctx context.Context, rawTxHex string) (string, error)
}

// adoptPayout records where a seat wants to be paid.
//
// Signed by the seat itself, because a claim is built by the others: without
// that, a member could announce an address on a neighbour's behalf and quietly
// take their share of every forfeit at the table. The address is decoded here as
// well, since one nobody can decode makes every claim built to pay it fail - and
// it would fail at the other seats, looking like they were refusing to pay.
func (tbl *table) adoptPayout(body schema.Payout) error {
	p, err := body.Into()
	if err != nil {
		return err
	}
	if err := p.Verify(tbl.terms); err != nil {
		return fmt.Errorf("payout: %w", err)
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	if key, at := seats[p.Seat], hex.EncodeToString(p.Signer); at != hex.EncodeToString(key) {
		return fmt.Errorf("a key that does not hold seat %d says where to pay it", p.Seat)
	}
	if _, err := payScriptFor(p.Address, tbl.netParams); err != nil {
		return fmt.Errorf("seat %d payout: %w", p.Seat, err)
	}
	tbl.payouts[p.Seat] = p.Address
	return nil
}

// announcePayout says where this player wants to be paid at a table.
func (tbl *table) announcePayout(addr string) []outgoing {
	seat, ok := tbl.form.OurSeat()
	if !ok || addr == "" || tbl.session == nil {
		return nil
	}
	if tbl.payouts[seat] == addr {
		return nil
	}
	p, err := membership.SignPayout(tbl.terms, seat, addr, tbl.session)
	if err != nil {
		log.Printf("pokerplugin: table %s: %v", tbl.terms.SID, err)
		return nil
	}
	tbl.payouts[seat] = addr
	return []outgoing{tbl.frame(schema.KindPayout, schema.PayoutFrom(p), gwire.ClassState)}
}

// handlePayoutSet records where this player wants to be paid, and tells every
// table it is at.
//
// A GET reads it back. Not decoration: until every seat at a table has said
// this, no claim there can be built at all, so whether it is set is an
// obligation somebody has to be able to check rather than infer from a claim
// that never appears.
func (p *plugin) handlePayoutSet(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"address": p.id.payoutAddress()})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := p.id.setPayout(req.Address, p.params); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p.publish(r.Context(), p.tables.announcePayouts(p.id.payoutAddress()))
	writeJSON(w, map[string]any{"address": p.id.payoutAddress()})
}

// announcePayouts tells every table this player is at where to pay it.
func (t *tables) announcePayouts(addr string) []outgoing {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []outgoing
	for _, tbl := range t.m {
		out = append(out, tbl.announcePayout(addr)...)
	}
	return out
}
