package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/decred/dcrd/wire"

	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	gwire "github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
)

// Taking a bond nobody answered for.
//
// An accusation only moves a bond into the claimed bond. Nothing is forfeited by
// it: the accused has the window to spend that back with its own key, and only
// once the window has closed can the rest of the table take it.
//
// This half needs no agreeing in advance, unlike the accusation. The seats taking
// it are all willing at the time, which is what being the accusers means - so it
// is proposed, co-signed and broadcast the way the old claim was, and the only new
// condition is that the chain agrees the window has passed.

// takeClaimed proposes taking the claimed bonds whose windows have closed.
//
// Called from the chain poll, because the only thing that can say the window has
// passed is a block count.
func (p *plugin) takeClaimed(ctx context.Context) {
	type ask struct {
		sid      string
		seat     uint32
		outpoint string
		claimed  []byte
	}
	var asks []ask
	p.tables.mu.Lock()
	for sid, tbl := range p.tables.m {
		mine, ok := tbl.form.OurSeat()
		if !ok || tbl.finished {
			continue
		}
		for duty, c := range tbl.claims {
			if !c.done || c.taken || duty.Seat == int(mine) {
				continue
			}
			claimed, err := escrow.AccuseDraft{Bond: c.bond}.ClaimedScript()
			if err != nil {
				continue
			}
			asks = append(asks, ask{sid: sid, seat: uint32(duty.Seat),
				outpoint: fmt.Sprintf("%s:0", c.tx.TxHash()), claimed: claimed})
		}
	}
	p.tables.mu.Unlock()

	for _, a := range asks {
		txid, vout, err := splitOutpoint(a.outpoint)
		if err != nil {
			continue
		}
		out, err := p.bridge.Outpoint(ctx, txid, vout)
		if err != nil || !out.Found {
			// Not confirmed, or already spent - and already spent is the
			// ordinary case, because it is what answering looks like.
			continue
		}
		if out.Confirmations < int64(escrow.ClaimBlocks) {
			// The window is open and the accused may still answer. Waiting
			// is the whole point of this branch.
			continue
		}

		p.tables.mu.Lock()
		tbl := p.tables.m[a.sid]
		if tbl != nil {
			p.publish(ctx, tbl.proposeTake(a.seat, a.outpoint, a.claimed, out.ValueAtoms))
		}
		p.tables.mu.Unlock()
	}
}

// proposeTake builds the forfeiture and asks the other accusers to co-sign it.
func (tbl *table) proposeTake(against uint32, outpoint string, claimed []byte, value int64) []outgoing {
	if tbl.takes == nil {
		tbl.takes = map[string]*take{}
	}
	if tbl.takes[outpoint] != nil {
		return nil
	}
	terms, err := escrow.ParseClaimedBond(claimed)
	if err != nil {
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}

	// One payout per taking seat, in the claimed bond's own canonical order,
	// so every co-signer builds the same bytes.
	pays := make([][]byte, 0, len(terms.Others))
	for _, key := range terms.Others {
		seat, ok := seatOf(seats, key)
		if !ok {
			return nil
		}
		addr := tbl.payouts[seat]
		if addr == "" {
			log.Printf("pokerplugin: table %s: seat %d has not said where to pay it, "+
				"so a forfeiture cannot be built", tbl.terms.SID, seat)
			return nil
		}
		pay, err := payScriptFor(addr, tbl.netParams)
		if err != nil {
			return nil
		}
		pays = append(pays, pay)
	}
	prevout, err := outpointOf(outpoint)
	if err != nil {
		return nil
	}
	tx, err := escrow.BuildTake(escrow.TakeDraft{
		Claimed:    claimed,
		Prevout:    prevout,
		ValueAtoms: value,
		PayScripts: pays,
		FeeAtoms:   claimFee,
	})
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot build a forfeiture: %v", tbl.terms.SID, err)
		return nil
	}
	raw, err := tx.Bytes()
	if err != nil {
		return nil
	}

	t := &take{seat: against, claimed: claimed, tx: tx, sigs: map[string][]byte{}}
	tbl.takes[outpoint] = t
	log.Printf("pokerplugin: table %s: seat %d never answered, proposing to take its bond",
		tbl.terms.SID, against)

	out := []outgoing{tbl.frame(schema.KindTake, schema.Take{
		Seat:     against,
		Outpoint: outpoint,
		Claimed:  hex.EncodeToString(claimed),
		Tx:       hex.EncodeToString(raw),
	}, gwire.ClassState)}
	return append(out, tbl.signTake(outpoint)...)
}

// take is one proposed forfeiture and the signatures gathered for it.
type take struct {
	seat    uint32
	claimed []byte
	tx      *wire.MsgTx
	sigs    map[string][]byte
	done    bool
}

// signTake adds this peer's own signature and says so.
func (tbl *table) signTake(outpoint string) []outgoing {
	t := tbl.takes[outpoint]
	if t == nil || tbl.session == nil {
		return nil
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	sig, err := escrow.SignClaimedSpend(t.tx, t.claimed, tbl.session)
	if err != nil {
		return nil
	}
	t.sigs[hex.EncodeToString(seats[mine])] = sig

	raw, err := t.tx.Bytes()
	if err != nil {
		return nil
	}
	return []outgoing{tbl.frame(schema.KindTake, schema.Take{
		Seat:     t.seat,
		Outpoint: outpoint,
		Claimed:  hex.EncodeToString(t.claimed),
		Tx:       hex.EncodeToString(raw),
		Signer:   hex.EncodeToString(seats[mine]),
		Sig:      hex.EncodeToString(sig),
	}, gwire.ClassState)}
}

// acceptTake checks somebody else's proposed forfeiture and co-signs it.
//
// The checks are the same ones a claim needed: that it spends the claimed bond
// this peer derived, and that it pays this peer what the division says. What it
// does not check is whether the accused really left - nothing can, and a seat that
// believes otherwise simply does not sign.
func (tbl *table) acceptTake(body schema.Take) []outgoing {
	if tbl.takes == nil {
		tbl.takes = map[string]*take{}
	}
	claimed, err := hex.DecodeString(body.Claimed)
	if err != nil {
		return nil
	}
	terms, err := escrow.ParseClaimedBond(claimed)
	if err != nil {
		log.Printf("pokerplugin: table %s: a forfeiture against a script that is not a claimed bond: %v",
			tbl.terms.SID, err)
		return nil
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	if _, err := escrow.MemberIndexOf(terms.Others, seats[mine]); err != nil {
		// This peer is the accused, or not one of the seats it pays.
		return nil
	}

	raw, err := hex.DecodeString(body.Tx)
	if err != nil {
		return nil
	}
	tx := wire.NewMsgTx()
	if err := tx.FromBytes(raw); err != nil {
		return nil
	}

	if t := tbl.takes[body.Outpoint]; t != nil {
		if t.tx.TxHash() != tx.TxHash() {
			// Somebody else built a different one. Ours stands; theirs will
			// not gather signatures, and neither of us needs to argue.
			return nil
		}
	} else {
		tbl.takes[body.Outpoint] = &take{seat: body.Seat, claimed: claimed,
			tx: tx, sigs: map[string][]byte{}}
	}

	// Their signature, if this message carried one.
	if body.Sig != "" && body.Signer != "" {
		signer, err := hex.DecodeString(body.Signer)
		if err != nil {
			return nil
		}
		if _, err := escrow.MemberIndexOf(terms.Others, signer); err != nil {
			return nil
		}
		sig, err := hex.DecodeString(body.Sig)
		if err != nil {
			return nil
		}
		tbl.takes[body.Outpoint].sigs[body.Signer] = sig
	}
	if _, held := tbl.takes[body.Outpoint].sigs[hex.EncodeToString(seats[mine])]; held {
		return nil
	}
	return tbl.signTake(body.Outpoint)
}

// finishTake broadcasts a forfeiture once every taking seat has signed it.
func (tbl *table) finishTake(ctx context.Context, chain broadcaster, outpoint string) {
	t := tbl.takes[outpoint]
	if t == nil || t.done {
		return
	}
	terms, err := escrow.ParseClaimedBond(t.claimed)
	if err != nil {
		return
	}
	sigs := make([][]byte, 0, len(terms.Others))
	for _, key := range terms.Others {
		sig, ok := t.sigs[hex.EncodeToString(key)]
		if !ok {
			return // still short of somebody
		}
		sigs = append(sigs, sig)
	}
	sigScript, err := escrow.TakeSigScript(t.claimed, sigs)
	if err != nil {
		log.Printf("pokerplugin: table %s: a fully signed forfeiture does not satisfy the claimed bond: %v",
			tbl.terms.SID, err)
		return
	}
	t.tx.TxIn[0].SignatureScript = sigScript
	raw, err := t.tx.Bytes()
	if err != nil {
		return
	}
	t.done = true
	txid, err := chain.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		log.Printf("pokerplugin: table %s: could not broadcast a forfeiture: %v", tbl.terms.SID, err)
		return
	}
	log.Printf("pokerplugin: table %s: took seat %d's unanswered bond in %s",
		tbl.terms.SID, t.seat, txid)
	tbl.note(eventClaimed, fmt.Sprintf("seat %d never answered, and its bond was taken", t.seat),
		txid, seatp(int(t.seat)))
	// A reveal this seat was refusing has now been answered by its coin, and
	// the audit it blocked can never complete - so the settlement it was
	// holding up may proceed on the standing checkpoint.
	tbl.closeChallengesAfterTake(t.seat)
}
