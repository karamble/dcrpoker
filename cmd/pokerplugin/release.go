package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"

	"github.com/decred/dcrd/wire"

	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	gwire "github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
)

// Handing the bonds back when a table ends properly.
//
// A table bond has three ways out and this is the one that should happen every
// time. The backstop returns it to its owner alone after a week, and exists for
// tables that dissolve rather than end. The claim branch takes it from somebody
// who walked out. This one - the alive branch - needs every member's signature
// and carries no timelock, so a table that finishes with everybody still present
// returns every bond at once.
//
// Without it the ordinary, cooperative, nothing-went-wrong case was the slowest:
// play a hand, settle, and then wait a week to get a bond back that nobody ever
// disputed.
//
// One transaction per bond, which is not a choice. escrow.SignBondSpend signs
// input zero and FinishAlive sets one signature script, because a bond is a
// single output under a script naming the whole table - so releasing n bonds is
// n transactions, each signed by everybody. Batching them would mean changing
// signed-money code to save a few hundred bytes of fee.
//
// Safe to run while a claim is open, and that is worth stating rather than
// guarding: the alive branch needs *every* member's signature, the accuser's
// included, so a release cannot be used to dodge a claim. Somebody accusing
// simply does not sign, and the bond stays where it is.

// releaseFee is what the miner gets for handing a bond back. One input and one
// output, so a few hundred bytes; generous rather than calculated, for the same
// reason the reclaim fee is.
const releaseFee = 20_000

// The states a release is simply not ready in yet. All three are ordinary and
// come back on their own, so they are values rather than log lines.
var (
	errNoBond      = errors.New("this seat has no bond on the chain")
	errNoPayout    = errors.New("this seat has not said where to pay it")
	errNoBondValue = errors.New("this peer has not seen what that bond holds")
)

// release is one bond's release and the signatures gathered for it.
type release struct {
	tx    *wire.MsgTx
	draft escrow.AliveDraft
	// sigs is each member's signature, by its key in hex. The alive branch
	// wants them in the order the script lists the members, which is why they
	// are kept by key rather than appended.
	sigs map[string][]byte
	done bool
}

// proposeReleases signs this peer's own view of every bond release the table
// owes, once it is over.
//
// After the settlement rather than beside it. They are independent transactions
// and independent failures: a release that cannot be built must not stop the
// stakes being paid out, which is the part that actually holds the money.
func (tbl *table) proposeReleases() []outgoing {
	if tbl.play == nil || !tbl.play.Over() || tbl.session == nil {
		return nil
	}
	if tbl.resultInDoubt() {
		// Once a bond is back, refusing a challenge costs its owner nothing -
		// so no bond goes back while one is open, nor while a hand stands
		// disproved.
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	var out []outgoing
	for seat := range uint32(len(seats)) {
		if tbl.caughtCheating(seat) {
			// The audit named this seat. Its bond is not released by this
			// peer; the backstop is its way out, a week from now.
			continue
		}
		if tbl.releases[seat] != nil {
			continue
		}
		d, err := tbl.releaseDraft(seat)
		if err != nil {
			// Ordinary before every seat has said where to pay it, and
			// before a bond is on the chain. Both are states to come back
			// from, so this stays quiet and tries again next tick.
			continue
		}
		tx, err := escrow.BuildAlive(d)
		if err != nil {
			log.Printf("pokerplugin: table %s: cannot release seat %d's bond: %v",
				tbl.terms.SID, seat, err)
			continue
		}
		out = append(out, tbl.signRelease(seat, tx, d)...)
	}
	return out
}

// releaseDraft describes handing one seat's bond back to that seat.
//
// Derived entirely from what this peer already agreed - the roster gives the
// script, the chain gives the amount, and the seat itself said where to be paid.
// Nothing here comes from whoever proposed it, which is what makes the draft
// something every member computes identically and therefore something worth
// comparing against.
func (tbl *table) releaseDraft(seat uint32) (escrow.AliveDraft, error) {
	outpoint := tbl.bondedAt[seat]
	if outpoint == "" {
		outpoint = tbl.bonded[seat]
	}
	if outpoint == "" {
		return escrow.AliveDraft{}, errNoBond
	}
	b, err := tbl.bond(seat, tbl.netParams)
	if err != nil {
		return escrow.AliveDraft{}, err
	}
	script, err := hex.DecodeString(b.ScriptHex)
	if err != nil {
		return escrow.AliveDraft{}, err
	}
	addr := tbl.payouts[seat]
	if addr == "" {
		return escrow.AliveDraft{}, errNoPayout
	}
	pay, err := payScriptFor(addr, tbl.netParams)
	if err != nil {
		return escrow.AliveDraft{}, err
	}
	prevout, err := outpointOf(outpoint)
	if err != nil {
		return escrow.AliveDraft{}, err
	}
	// What the chain said, when this peer was the one who checked. Our own
	// bond was never checked by us - we paid it - so it falls back to the
	// minimum, which is what refreshDraft has always assumed and what every
	// bond posted here actually holds.
	//
	// A bond holding more than the minimum would make the two peers build
	// different transactions, and the effect of that is a release both of them
	// refuse rather than a payment either of them gets wrong. It falls back to
	// the backstop, slowly and correctly.
	value, ok := tbl.bondValue[seat]
	if !ok || value <= 0 {
		return escrow.AliveDraft{}, errNoBondValue
	}
	return escrow.AliveDraft{
		Bond:       script,
		Prevout:    prevout,
		ValueAtoms: value,
		PayScript:  pay,
		FeeAtoms:   releaseFee,
	}, nil
}

// signRelease adds this peer's signature and says so.
func (tbl *table) signRelease(seat uint32, tx *wire.MsgTx, d escrow.AliveDraft) []outgoing {
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	sig, err := escrow.SignBondSpend(tx, d.Bond, tbl.session)
	if err != nil {
		log.Printf("pokerplugin: table %s: signing seat %d's release: %v", tbl.terms.SID, seat, err)
		return nil
	}
	raw, err := tx.Bytes()
	if err != nil {
		return nil
	}
	r := tbl.releases[seat]
	if r == nil {
		r = &release{tx: tx, draft: d, sigs: map[string][]byte{}}
		tbl.releases[seat] = r
	}
	r.sigs[hex.EncodeToString(seats[mine])] = sig

	return []outgoing{tbl.frame(schema.KindRelease, schema.Release{
		Seat:   seat,
		Tx:     hex.EncodeToString(raw),
		Signer: hex.EncodeToString(seats[mine]),
		Sig:    hex.EncodeToString(sig),
	}, gwire.ClassState)}
}

// adoptRelease records a member's signature and broadcasts once they are all in.
//
// The transaction is rebuilt from this peer's own view and compared, never
// trusted. A release needs every member's signature, so a seat that signed what
// it was handed would be giving the sender a transaction paying wherever they
// liked with everybody's names on it - escrow.CheckAliveDraft is the whole
// defence and it is the reason the draft is derived rather than carried.
func (tbl *table) adoptRelease(ctx context.Context, body schema.Release) []outgoing {
	if tbl.play == nil {
		return nil
	}
	if tbl.resultInDoubt() {
		tbl.note(eventRefused,
			"a bond release arrived for a table with a hand in doubt, and was not signed", "", nil)
		return nil
	}
	if tbl.caughtCheating(body.Seat) {
		tbl.note(eventRefused, fmt.Sprintf(
			"seat %d's bond release was refused: the audit named it", body.Seat), "", seatp(int(body.Seat)))
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	signer, err := hex.DecodeString(body.Signer)
	if err != nil {
		return nil
	}
	// The signer has to hold a seat at this table, checked against the
	// membership rather than taken from the message.
	var known bool
	for _, key := range seats {
		if hex.EncodeToString(key) == body.Signer {
			known = true
		}
	}
	if !known {
		log.Printf("pokerplugin: table %s: a key that holds no seat signed a bond release",
			tbl.terms.SID)
		return nil
	}
	sig, err := hex.DecodeString(body.Sig)
	if err != nil || len(sig) == 0 {
		return nil
	}

	d, err := tbl.releaseDraft(body.Seat)
	if err != nil {
		// We cannot build it yet, so we cannot check it. Saying nothing is
		// right: our own next tick proposes it once we can.
		return nil
	}
	raw, err := hex.DecodeString(body.Tx)
	if err != nil {
		return nil
	}
	tx := wire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		log.Printf("pokerplugin: table %s: seat %d's release will not decode: %v",
			tbl.terms.SID, body.Seat, err)
		return nil
	}
	if err := escrow.CheckAliveDraft(tx, d); err != nil {
		log.Printf("pokerplugin: table %s: refusing seat %d's release: %v",
			tbl.terms.SID, body.Seat, err)
		return nil
	}

	var out []outgoing
	r := tbl.releases[body.Seat]
	if r == nil {
		// Somebody proposed it before we did. Signing it is how we join in,
		// and what we sign is the transaction we just rebuilt for ourselves.
		out = tbl.signRelease(body.Seat, tx, d)
		r = tbl.releases[body.Seat]
		if r == nil {
			return nil
		}
	}
	r.sigs[body.Signer] = sig
	_ = signer

	tbl.broadcastRelease(ctx, body.Seat)
	return out
}

// broadcastRelease sends a release once every member has signed it.
func (tbl *table) broadcastRelease(ctx context.Context, seat uint32) {
	r := tbl.releases[seat]
	if r == nil || r.done {
		return
	}
	terms, err := escrow.ParseTableBond(r.draft.Bond)
	if err != nil {
		return
	}
	sigs := make([][]byte, 0, len(terms.Members))
	for _, member := range terms.Members {
		sig, ok := r.sigs[hex.EncodeToString(member)]
		if !ok {
			// Still short. Nothing to do but wait for the rest.
			return
		}
		sigs = append(sigs, sig)
	}
	signed, err := escrow.FinishAlive(r.tx, r.draft.Bond, sigs, tbl.netParams)
	if err != nil {
		log.Printf("pokerplugin: table %s: seat %d's release will not assemble: %v",
			tbl.terms.SID, seat, err)
		return
	}
	raw, err := signed.Bytes()
	if err != nil {
		return
	}
	if tbl.chain == nil {
		return
	}
	txid, err := tbl.chain.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		// The others hold the same signatures, so somebody else's copy may
		// well land. Worth saying and not worth stopping for.
		log.Printf("pokerplugin: table %s: could not send seat %d's release: %v",
			tbl.terms.SID, seat, err)
		return
	}
	r.done = true
	at := seat
	tbl.note(eventSettled, "bond released", txid, &at)
	log.Printf("pokerplugin: table %s: seat %d's bond went back in %s", tbl.terms.SID, seat, txid)
}

// learnBondValues asks the chain what this player's own bonds hold.
//
// Every other seat's value is learned by checking their announcement, which is
// the one place that already looks. Our own is never checked by us - we paid it -
// so without this the two peers build releases from different numbers and refuse
// each other's, which is safe and useless.
//
// It runs on the chain tick rather than at the moment the bond is paid, because
// at that moment the transaction has no confirmations and a confirmed-only
// lookup would find nothing. Here it simply succeeds on some later pass.
func (p *plugin) learnBondValues(ctx context.Context) {
	type ask struct {
		sid      string
		seat     uint32
		outpoint string
	}
	var asks []ask
	p.tables.mu.Lock()
	for sid, tbl := range p.tables.m {
		seats, ok := tbl.form.Seats()
		if !ok {
			continue
		}
		// Every seat, not only our own. A release names an amount for each
		// bond it returns, and the walk forgets what a bond held the moment
		// it finds it somewhere new - so a seat whose bond moved would have
		// no amount to build a release from, and only its owner would ever
		// go and ask. That leaves a bond nobody can release cooperatively.
		for seat := range uint32(len(seats)) {
			if tbl.bondValue[seat] > 0 {
				continue
			}
			outpoint := tbl.bondedAt[seat]
			if outpoint == "" {
				outpoint = tbl.bonded[seat]
			}
			if outpoint == "" {
				continue
			}
			asks = append(asks, ask{sid: sid, seat: seat, outpoint: outpoint})
		}
	}
	p.tables.mu.Unlock()

	for _, a := range asks {
		txid, vout, err := splitOutpoint(a.outpoint)
		if err != nil {
			continue
		}
		out, err := p.bridge.Outpoint(ctx, txid, vout)
		if err != nil || !out.Found || out.ValueAtoms <= 0 {
			continue
		}
		p.tables.mu.Lock()
		if tbl := p.tables.m[a.sid]; tbl != nil {
			tbl.bondValue[a.seat] = out.ValueAtoms
		}
		p.tables.mu.Unlock()
	}
}
