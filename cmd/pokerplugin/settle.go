package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/wire"

	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	gwire "github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
)

// The two transactions that were designed and never assembled.
//
// A refresh is a seat's answer to a claim, gathered while the table is still
// cooperating because it cannot be gathered afterwards. A settlement is the
// table paying out what it ended holding, which is the only reason any of the
// rest of this exists.
//
// They have the same shape - build a draft everybody derives identically, check
// it, sign it, collect, assemble - and they are here together because the shape
// is the point. Neither decides anything. Every peer builds the same bytes from
// facts it already holds, and a peer that reads something different simply does
// not sign.

// settleFee is left for the miner out of a table's final balance.
const settleFee int64 = 20_000

// presignRefreshes agrees every seat's answer to a claim, in advance.
//
// Called once the bonds are all posted, which is the moment there is something
// to answer for and everybody is still talking. A peer signs every seat's
// answer, including its own: the branch needs the whole table, so a seat that
// signed only its neighbours' would leave its own unanswerable.
func (tbl *table) presignRefreshes() []outgoing {
	if tbl.play == nil || tbl.refreshed {
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok || tbl.session == nil {
		return nil
	}
	mine, _ := tbl.form.OurSeat()
	var out []outgoing
	for seat := range seats {
		chain, bond, err := tbl.refreshChain(seat)
		if err != nil {
			// A seat whose bond is not yet on the chain has nothing to
			// answer for. It gets its turn when it does.
			continue
		}
		for _, tx := range chain {
			sig, err := escrow.SignBondSpend(tx, bond, tbl.session)
			if err != nil {
				log.Printf("pokerplugin: table %s: pre-signing an answer: %v",
					tbl.terms.SID, err)
				continue
			}
			raw, err := tx.Bytes()
			if err != nil {
				continue
			}
			tbl.holdRefresh(seat, tx, bond, seats[mine], sig)
			out = append(out, tbl.frame(schema.KindRefresh, schema.Refresh{
				Seat:   seat,
				Tx:     hex.EncodeToString(raw),
				Signer: hex.EncodeToString(seats[mine]),
				Sig:    hex.EncodeToString(sig),
			}, gwire.ClassState))
		}
	}
	if len(out) > 0 {
		tbl.refreshed = true
	}
	return out
}

// refreshChain builds every answer a seat may need, in order.
func (tbl *table) refreshChain(seat uint32) ([]*wire.MsgTx, []byte, error) {
	d, bond, err := tbl.refreshDraft(seat)
	if err != nil {
		return nil, nil, err
	}
	chain, err := escrow.BuildRefreshChain(d, escrow.RefreshDepth)
	return chain, bond, err
}

// refreshDraft describes a seat's answer: its bond respent into the same bond.
func (tbl *table) refreshDraft(seat uint32) (escrow.RefreshDraft, []byte, error) {
	outpoint := tbl.bondedAt[seat]
	if outpoint == "" {
		outpoint = tbl.bonded[seat]
	}
	if outpoint == "" {
		return escrow.RefreshDraft{}, nil, fmt.Errorf("seat %d has no bond on the chain", seat)
	}
	b, err := tbl.bond(seat, tbl.netParams)
	if err != nil {
		return escrow.RefreshDraft{}, nil, err
	}
	script, err := hex.DecodeString(b.ScriptHex)
	if err != nil {
		return escrow.RefreshDraft{}, nil, err
	}
	prevout, err := outpointOf(outpoint)
	if err != nil {
		return escrow.RefreshDraft{}, nil, err
	}
	return escrow.RefreshDraft{
		Bond:       script,
		Prevout:    prevout,
		ValueAtoms: int64(escrow.MinBondAtoms),
		FeeAtoms:   claimFee,
		Params:     tbl.netParams,
	}, script, nil
}

// holdRefresh keeps a signature on one answer, filed by the output it spends.
//
// By outpoint rather than by seat, because a seat has a chain of them and the
// one it needs is decided by where its bond actually sits - which a peer that
// restarted can look up rather than having to remember how many it has used.
func (tbl *table) holdRefresh(seat uint32, tx *wire.MsgTx, bond []byte, signer, sig []byte) {
	key := tx.TxIn[0].PreviousOutPoint.String()
	r := tbl.refresh[key]
	if r == nil {
		r = &refresh{seat: seat, tx: tx, bond: bond, sigs: map[string][]byte{}}
		tbl.refresh[key] = r
	}
	r.sigs[hex.EncodeToString(signer)] = sig
}

// refresh is one seat's answer and the signatures gathered for it.
type refresh struct {
	seat uint32
	tx   *wire.MsgTx
	bond []byte
	sigs map[string][]byte
}

// adoptRefresh keeps somebody's signature on an answer, having checked what it
// signs.
//
// The check matters more here than anywhere else, because this is agreed long
// before anybody looks at it again: anything other than a payment back into the
// same bond is that seat taking its bond out early, and a signature given now is
// one that cannot be taken back.
func (tbl *table) adoptRefresh(body schema.Refresh) error {
	raw, err := hex.DecodeString(body.Tx)
	if err != nil {
		return fmt.Errorf("refresh transaction: %w", err)
	}
	tx := wire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("refresh transaction: %w", err)
	}
	chain, bond, err := tbl.refreshChain(body.Seat)
	if err != nil {
		return err
	}
	// It has to be one of the answers this peer derived itself. Anything
	// else is a signature given months early on a transaction nobody agreed.
	var want *wire.MsgTx
	for _, c := range chain {
		if c.TxHash() == tx.TxHash() {
			want = c
			break
		}
	}
	if want == nil {
		return fmt.Errorf("an answer for seat %d that this peer did not derive", body.Seat)
	}
	if err := escrow.CheckRefreshDraft(tx, escrow.RefreshDraft{
		Bond:       bond,
		Prevout:    want.TxIn[0].PreviousOutPoint,
		ValueAtoms: want.TxIn[0].ValueIn,
		FeeAtoms:   claimFee,
		Params:     tbl.netParams,
	}); err != nil {
		return fmt.Errorf("not keeping an answer that %w", err)
	}
	signer, err := hex.DecodeString(body.Signer)
	if err != nil {
		return err
	}
	sig, err := hex.DecodeString(body.Sig)
	if err != nil {
		return err
	}
	terms, err := escrow.ParseTableBond(bond)
	if err != nil {
		return err
	}
	if _, err := escrow.MemberIndex(terms, signer); err != nil {
		return fmt.Errorf("a signature on an answer from somebody not at this table")
	}
	tbl.holdRefresh(body.Seat, want, bond, signer, sig)
	return nil
}

// answerClaim broadcasts this seat's pre-agreed answer.
//
// What it spends is the output the claim is against, so the claim dies whatever
// anybody thinks about it. Nobody is asked and nobody is convinced: the accused
// simply spends its own bond back into an identical bond, which it could have
// done at any time and which gains it nothing.
func (tbl *table) answerClaim(ctx context.Context) {
	seat, ok := tbl.form.OurSeat()
	if !ok {
		return
	}
	// The answer whose input is where the bond actually sits. Answering once
	// moves it on, and the next claim is against the new output.
	at := tbl.bondedAt[seat]
	if at == "" {
		at = tbl.bonded[seat]
	}
	prevout, err := outpointOf(at)
	if err != nil {
		return
	}
	r := tbl.refresh[prevout.String()]
	if r == nil {
		log.Printf("pokerplugin: table %s: claimed against and holding no answer for %s",
			tbl.terms.SID, at)
		return
	}
	terms, err := escrow.ParseTableBond(r.bond)
	if err != nil {
		return
	}
	sigs := make([][]byte, 0, len(terms.Members))
	for _, m := range terms.Members {
		sig, ok := r.sigs[hex.EncodeToString(m)]
		if !ok {
			log.Printf("pokerplugin: table %s: claimed against and short of an answer's signatures",
				tbl.terms.SID)
			return
		}
		sigs = append(sigs, sig)
	}
	done, err := escrow.FinishAlive(r.tx, r.bond, sigs, tbl.netParams)
	if err != nil {
		log.Printf("pokerplugin: table %s: the answer held does not satisfy the bond: %v",
			tbl.terms.SID, err)
		return
	}
	raw, err := done.Bytes()
	if err != nil {
		return
	}
	txid, err := tbl.chain.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		log.Printf("pokerplugin: table %s: could not answer a claim: %v", tbl.terms.SID, err)
		return
	}
	// The bond has moved, so every future claim - and every future answer -
	// is against the new output. Recorded here and announced, because a peer
	// still naming the old one would build a claim nobody can sign.
	tbl.bondedAt[seat] = fmt.Sprintf("%s:0", done.TxHash())
	log.Printf("pokerplugin: table %s: answered a claim in %s; the bond is posted again at %s",
		tbl.terms.SID, txid, tbl.bondedAt[seat])
}

// settleDraft is what this table would pay out, from the last boundary every
// seat signed.
func (tbl *table) settleDraft() (escrow.SettleDraft, error) {
	if tbl.play == nil {
		return escrow.SettleDraft{}, fmt.Errorf("this table never dealt")
	}
	hand, stacks := tbl.play.Settled()
	if hand == 0 {
		return escrow.SettleDraft{}, fmt.Errorf("no hand has been agreed yet")
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return escrow.SettleDraft{}, fmt.Errorf("this table has no seating")
	}
	deposits, err := tbl.form.Deposits(tbl.netParams)
	if err != nil {
		return escrow.SettleDraft{}, err
	}

	d := escrow.SettleDraft{FeeAtoms: settleFee}
	for seat := range uint32(len(seats)) {
		var dep string
		for _, x := range deposits {
			if x.Seat == seat {
				dep = x.RedeemScriptHex
			}
		}
		redeem, err := hex.DecodeString(dep)
		if err != nil || len(redeem) == 0 {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d has no escrow script", seat)
		}
		outpoint := tbl.funded[seat]
		if outpoint == "" {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d's stake is not on the chain", seat)
		}
		prevout, err := outpointOf(outpoint)
		if err != nil {
			return escrow.SettleDraft{}, err
		}
		addr := tbl.payouts[seat]
		if addr == "" {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d has not said where to pay it", seat)
		}
		pay, err := payScriptFor(addr, tbl.netParams)
		if err != nil {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d payout: %w", seat, err)
		}
		if int(seat) >= len(stacks) {
			return escrow.SettleDraft{}, fmt.Errorf("the checkpoint has no stack for seat %d", seat)
		}
		d.Inputs = append(d.Inputs, escrow.SettleInput{
			Redeem:     redeem,
			Prevout:    prevout,
			ValueAtoms: int64(tbl.terms.BuyInAtoms),
		})
		d.Pays = append(d.Pays, pay)
		d.Amounts = append(d.Amounts, stacks[seat])
	}
	return d, nil
}

// proposeSettlement pays the table out, once it is over.
func (tbl *table) proposeSettlement() []outgoing {
	if tbl.play == nil || !tbl.play.Over() || tbl.settled {
		return nil
	}
	d, err := tbl.settleDraft()
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot settle yet: %v", tbl.terms.SID, err)
		return nil
	}
	tx, err := escrow.BuildSettlement(d)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot settle: %v", tbl.terms.SID, err)
		return nil
	}
	return tbl.signSettlement(tx, d)
}

func (tbl *table) signSettlement(tx *wire.MsgTx, d escrow.SettleDraft) []outgoing {
	seats, ok := tbl.form.Seats()
	if !ok || tbl.session == nil {
		return nil
	}
	mine, _ := tbl.form.OurSeat()
	sigs, err := escrow.SignSettlement(tx, d, tbl.session)
	if err != nil {
		log.Printf("pokerplugin: table %s: signing a settlement: %v", tbl.terms.SID, err)
		return nil
	}
	raw, err := tx.Bytes()
	if err != nil {
		return nil
	}
	hand, _ := tbl.play.Settled()
	if tbl.settle == nil {
		tbl.settle = &settlement{tx: tx, draft: d, sigs: map[string][][]byte{}}
	}
	tbl.settle.sigs[hex.EncodeToString(seats[mine])] = sigs
	tbl.settled = true

	hexed := make([]string, 0, len(sigs))
	for _, s := range sigs {
		hexed = append(hexed, hex.EncodeToString(s))
	}
	return []outgoing{tbl.frame(schema.KindSettle, schema.Settle{
		Hand:   hand,
		Tx:     hex.EncodeToString(raw),
		Signer: hex.EncodeToString(seats[mine]),
		Sigs:   hexed,
	}, gwire.ClassState)}
}

// settlement is the payout and the signatures gathered for it.
type settlement struct {
	tx    *wire.MsgTx
	draft escrow.SettleDraft
	// sigs is each member's signature for every input, by its key in hex.
	sigs map[string][][]byte
	done bool
}

// adoptSettlement records a member's signatures and broadcasts once they are all
// in.
//
// The transaction is checked against the one this peer would have built from its
// own copy of the checkpoint, which covers the amounts, the addresses and the
// stakes at once. A peer that reads a different boundary refuses, and a peer
// that has not caught up refuses too - which costs a retry rather than the
// table's balance.
func (tbl *table) adoptSettlement(ctx context.Context, body schema.Settle) []outgoing {
	if tbl.play == nil {
		return nil
	}
	d, err := tbl.settleDraft()
	if err != nil {
		return nil
	}
	raw, err := hex.DecodeString(body.Tx)
	if err != nil {
		return nil
	}
	tx := wire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil
	}
	if err := escrow.CheckSettleDraft(tx, d); err != nil {
		log.Printf("pokerplugin: table %s: not signing a settlement: %v", tbl.terms.SID, err)
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
	if _, at := seatOf(seats, signer); !at {
		return nil
	}
	sigs := make([][]byte, 0, len(body.Sigs))
	for _, h := range body.Sigs {
		s, err := hex.DecodeString(h)
		if err != nil {
			return nil
		}
		sigs = append(sigs, s)
	}
	if len(sigs) != len(d.Inputs) {
		return nil
	}

	var out []outgoing
	if tbl.settle == nil {
		tbl.settle = &settlement{tx: tx, draft: d, sigs: map[string][][]byte{}}
	}
	tbl.settle.sigs[body.Signer] = sigs
	if !tbl.settled {
		// This peer had not signed yet, and it agrees, so it signs now.
		out = append(out, tbl.signSettlement(tx, d)...)
	}
	tbl.broadcastSettlement(ctx)
	return out
}

// broadcastSettlement assembles and sends, once every seat has signed.
func (tbl *table) broadcastSettlement(ctx context.Context) {
	s := tbl.settle
	if s == nil || s.done {
		return
	}
	members, err := escrow.Members(s.draft.Inputs[0].Redeem)
	if err != nil {
		return
	}
	byInput := make([][][]byte, len(s.draft.Inputs))
	for i := range byInput {
		for _, m := range members {
			sigs, ok := s.sigs[hex.EncodeToString(m)]
			if !ok || i >= len(sigs) {
				return // still short of somebody
			}
			byInput[i] = append(byInput[i], sigs[i])
		}
	}
	done, err := escrow.FinishSettlement(s.tx, s.draft, byInput, tbl.netParams)
	if err != nil {
		log.Printf("pokerplugin: table %s: a fully signed settlement did not satisfy the escrows: %v",
			tbl.terms.SID, err)
		return
	}
	raw, err := done.Bytes()
	if err != nil {
		return
	}
	s.done = true
	txid, err := tbl.chain.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		// Every other seat holds the same signatures and the same
		// transaction, so one of them will send it.
		log.Printf("pokerplugin: table %s: could not broadcast the settlement: %v",
			tbl.terms.SID, err)
		return
	}
	log.Printf("pokerplugin: table %s: settled in %s", tbl.terms.SID, txid)
}

func outpointOf(s string) (wire.OutPoint, error) {
	txid, vout, err := splitOutpoint(s)
	if err != nil {
		return wire.OutPoint{}, err
	}
	var h chainhash.Hash
	if err := chainhash.Decode(&h, txid); err != nil {
		return wire.OutPoint{}, fmt.Errorf("outpoint: %w", err)
	}
	return wire.OutPoint{Hash: h, Index: vout, Tree: wire.TxTreeRegular}, nil
}
