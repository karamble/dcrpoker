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
// An accusation is agreed while the table is still
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

// presignAccusations agrees every accusation the table may need, in advance.
//
// Repeated until every seat can be accused rather than sent once, because this
// channel loses messages and what is being agreed is the answer to somebody who
// stops. Repeats are byte-identical, so a peer that already holds one keeps it.
//
// It does not gate dealing, and that leaves a window: a seat that goes quiet
// between posting its bond and signing the accusations against itself cannot be
// accused at all, and its bond waits for the backstop. Closing it means a table
// that will not deal until the set is complete, which is the right answer and needs
// the dealing path to drive the exchange rather than only the clock. An accusation needs every member's signature including the
// seat it accuses, so a seat that went quiet before this was agreed could never be
// accused at all. Playing a hand first would be playing with no answer to somebody
// who stops, which is the one thing the bond exists to provide.
//
// Called once the bonds are all posted, which is the moment there is something
// to answer for and everybody is still talking. A peer signs every seat's
// answer, including its own: the branch needs the whole table, so a seat that
// signed only its neighbours' would leave its own unanswerable.
func (tbl *table) presignAccusations() []outgoing {
	if tbl.accusationsReady() {
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok || tbl.session == nil {
		return nil
	}
	mine, _ := tbl.form.OurSeat()
	var out []outgoing
	for seat := range seats {
		chain, bond, err := tbl.accuseChain(seat)
		if err != nil {
			// Usually a seat whose bond is not on the chain yet, which
			// gets its turn when it is. Said out loud regardless: a table
			// with no accusations agreed has no answer to somebody who
			// stops, and that is not something to discover later.
			log.Printf("pokerplugin: table %s: no accusations agreed against seat %d: %v",
				tbl.terms.SID, seat, err)
			continue
		}
		terms, err := escrow.ParseTableBond(bond)
		if err != nil {
			continue
		}
		for _, tx := range chain {
			sig, err := escrow.SignBondSpend(tx, bond, tbl.session)
			if err != nil {
				log.Printf("pokerplugin: table %s: pre-signing an accusation: %v",
					tbl.terms.SID, err)
				continue
			}
			// Held whatever happens, so this peer can complete the set from
			// its own side.
			tbl.holdAccusation(seat, tx, bond, seats[mine], sig)
			if tbl.fullySigned(tx, terms) {
				// Everybody has signed this one. Saying it again would
				// only add traffic to a channel the hand is using.
				continue
			}
			raw, err := tx.Bytes()
			if err != nil {
				continue
			}
			out = append(out, tbl.frame(schema.KindAccusation, schema.Accusation{
				Seat:   seat,
				Tx:     hex.EncodeToString(raw),
				Signer: hex.EncodeToString(seats[mine]),
				Sig:    hex.EncodeToString(sig),
			}, gwire.ClassState))
		}
	}
	// Said again every tick until every seat can be accused, and stopped by
	// that rather than by having said it once. A signature lost on the way is
	// a table that never deals, and nothing else would ever send it again.
	// Repeats are byte-identical, so a peer that already holds one keeps it.
	tbl.accused = tbl.accusationsReady()
	return out
}

// accuseChain builds every accusation against a seat that the table may need.
//
// Cached on the outpoint it starts from, which is the only thing that changes it:
// deriving a chain costs a script and a transaction per entry, and this is asked
// every tick by everything that wants to know whether a seat can be accused. When
// the bond moves the key moves with it, so there is nothing to invalidate.
func (tbl *table) accuseChain(seat uint32) ([]*wire.MsgTx, []byte, error) {
	d, bond, err := tbl.accuseDraft(seat)
	if err != nil {
		return nil, nil, err
	}
	key := fmt.Sprintf("%d/%s", seat, d.Prevout)
	if held, ok := tbl.chains[key]; ok {
		return held, bond, nil
	}
	terms, err := escrow.ParseTableBond(bond)
	if err != nil {
		return nil, nil, err
	}
	if escrow.AffordableDepth(d.ValueAtoms, d.FeeAtoms, len(terms.Others)) < 1 {
		return nil, nil, fmt.Errorf(
			"a bond of %d cannot fund one accusation at a fee of %d paying %d seats",
			d.ValueAtoms, d.FeeAtoms, len(terms.Others))
	}
	// One, against where the bond sits now. A deeper chain was agreed up front
	// when the answer needed signatures gathered in advance; it does not any
	// more. Answering moves the bond, every peer re-derives the accusation
	// against the new output, and the seat that answered signs it - which it
	// will, having just proved it is there.
	chain, err := escrow.BuildAccuseChain(d, 1)
	if err != nil {
		return nil, nil, err
	}
	if tbl.chains == nil {
		tbl.chains = map[string][]*wire.MsgTx{}
	}
	tbl.chains[key] = chain
	return chain, bond, nil
}

// accuseDraft describes an accusation: a seat's bond moved into the claimed bond,
// where that seat has the window to answer.
//
// Nothing about who gets paid, because an accusation pays nobody - which also
// means it can be built at a table where somebody has not yet said where to pay
// them, unlike the claim it replaces.
//
// The value is read off the ladder, not assumed: every answer costs the bond a
// fee, so a bond that has moved holds less than it was posted with, and an
// accusation naming the posted amount would spend more than is there.
func (tbl *table) accuseDraft(seat uint32) (escrow.AccuseDraft, []byte, error) {
	outpoint := tbl.bondedAt[seat]
	if outpoint == "" {
		outpoint = tbl.bonded[seat]
	}
	if outpoint == "" {
		return escrow.AccuseDraft{}, nil, fmt.Errorf("seat %d has no bond on the chain", seat)
	}
	prevout, err := outpointOf(outpoint)
	if err != nil {
		return escrow.AccuseDraft{}, nil, err
	}
	ladder, script, err := tbl.bondLadder(seat)
	if err != nil {
		return escrow.AccuseDraft{}, nil, err
	}
	for _, a := range ladder {
		if a.TxIn[0].PreviousOutPoint != prevout {
			continue
		}
		return escrow.AccuseDraft{
			Bond:       script,
			Prevout:    prevout,
			ValueAtoms: a.TxIn[0].ValueIn,
			FeeAtoms:   claimFee,
			Params:     tbl.netParams,
		}, script, nil
	}
	return escrow.AccuseDraft{}, nil, fmt.Errorf(
		"seat %d's bond sits at %s, which no agreed accusation reaches", seat, outpoint)
}

// bondLadder is every position a seat's bond can occupy, from where it was
// first posted.
//
// Derivable by every peer alone: an accusation's identity is fixed before it is
// signed and so is the answer's, so the whole ladder of outpoints follows from
// the first. Entry i spends position i and creates the claimed bond whose answer
// is position i+1. Walking it against the chain is how a peer finds a bond that
// moved while nobody said so - its own after a restart, or anybody's.
func (tbl *table) bondLadder(seat uint32) ([]*wire.MsgTx, []byte, error) {
	origin := tbl.bonded[seat]
	if origin == "" {
		return nil, nil, fmt.Errorf("seat %d has no bond on the chain", seat)
	}
	b, err := tbl.bond(seat, tbl.netParams)
	if err != nil {
		return nil, nil, err
	}
	script, err := hex.DecodeString(b.ScriptHex)
	if err != nil {
		return nil, nil, err
	}
	key := fmt.Sprintf("ladder/%d/%s", seat, origin)
	if held, ok := tbl.chains[key]; ok {
		return held, script, nil
	}
	prevout, err := outpointOf(origin)
	if err != nil {
		return nil, nil, err
	}
	d := escrow.AccuseDraft{
		Bond:       script,
		Prevout:    prevout,
		ValueAtoms: int64(escrow.MinBondAtoms),
		FeeAtoms:   claimFee,
		Params:     tbl.netParams,
	}
	terms, err := escrow.ParseTableBond(script)
	if err != nil {
		return nil, nil, err
	}
	depth := escrow.AffordableDepth(d.ValueAtoms, d.FeeAtoms, len(terms.Others))
	if depth < 1 {
		return nil, nil, fmt.Errorf(
			"a bond of %d cannot fund one accusation at a fee of %d paying %d seats",
			d.ValueAtoms, d.FeeAtoms, len(terms.Others))
	}
	ladder, err := escrow.BuildAccuseChain(d, depth)
	if err != nil {
		return nil, nil, err
	}
	if tbl.chains == nil {
		tbl.chains = map[string][]*wire.MsgTx{}
	}
	tbl.chains[key] = ladder
	return ladder, script, nil
}

// holdAccusation keeps a signature on one accusation, filed by the output it spends.
//
// By outpoint rather than by seat, because a seat has a chain of them and the
// one it needs is decided by where its bond actually sits - which a peer that
// restarted can look up rather than having to remember how many it has used.
func (tbl *table) holdAccusation(seat uint32, tx *wire.MsgTx, bond []byte, signer, sig []byte) {
	key := tx.TxIn[0].PreviousOutPoint.String()
	r := tbl.accuse[key]
	if r == nil {
		r = &accusation{seat: seat, tx: tx, bond: bond, sigs: map[string][]byte{}}
		tbl.accuse[key] = r
	}
	r.sigs[hex.EncodeToString(signer)] = sig
}

// accusation is one pre-agreed accusation and the signatures gathered for it.
type accusation struct {
	seat uint32
	tx   *wire.MsgTx
	bond []byte
	sigs map[string][]byte
}

// adoptAccusation keeps somebody's signature on an accusation, having checked what
// it signs.
//
// The check matters more here than anywhere else, because this is agreed long
// before anybody looks at it again: anything other than a payment back into the
// same bond is that seat taking its bond out early, and a signature given now is
// one that cannot be taken back.
func (tbl *table) adoptAccusation(body schema.Accusation) error {
	raw, err := hex.DecodeString(body.Tx)
	if err != nil {
		return fmt.Errorf("accusation transaction: %w", err)
	}
	tx := wire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("accusation transaction: %w", err)
	}
	chain, bond, err := tbl.accuseChain(body.Seat)
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
		// Worth saying out loud, not only returning. A peer whose
		// accusations are being refused will have nothing to accuse with the
		// day somebody stops, and that is a thing to learn now.
		tbl.note(eventRefused, fmt.Sprintf(
			"an accusation against seat %d was not one this peer derived, and was not signed",
			body.Seat), "", seatp(int(body.Seat)))
		return fmt.Errorf("an accusation against seat %d that this peer did not derive", body.Seat)
	}
	if err := escrow.CheckAccuseDraft(tx, escrow.AccuseDraft{
		Bond:       bond,
		Prevout:    want.TxIn[0].PreviousOutPoint,
		ValueAtoms: want.TxIn[0].ValueIn,
		FeeAtoms:   claimFee,
		Params:     tbl.netParams,
	}); err != nil {
		tbl.note(eventRefused, fmt.Sprintf(
			"an accusation against seat %d was refused: %v", body.Seat, err),
			"", seatp(int(body.Seat)))
		return fmt.Errorf("not keeping an accusation that %w", err)
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
		return fmt.Errorf("a signature on an accusation from somebody not at this table")
	}
	key := want.TxIn[0].PreviousOutPoint.String()
	_, knew := tbl.accuse[key].sigsOf()[hex.EncodeToString(signer)]
	tbl.holdAccusation(body.Seat, want, bond, signer, sig)
	if knew {
		// Nothing learned, so nothing to say back. That is what stops this
		// from going round for ever.
		return nil
	}
	// Learned something, so answer with ours. A broadcast that everybody
	// repeats until their own view looks complete deadlocks the moment one
	// peer is satisfied before another - so this converges by replying rather
	// than by insisting.
	tbl.replyWith = append(tbl.replyWith, tbl.ourAccusation(body.Seat, want, bond)...)
	return nil
}

// sigsOf is the signatures held for an accusation, or none.
func (a *accusation) sigsOf() map[string][]byte {
	if a == nil {
		return nil
	}
	return a.sigs
}

// ourAccusation is this peer's own signature on one accusation, as a frame.
func (tbl *table) ourAccusation(seat uint32, tx *wire.MsgTx, bond []byte) []outgoing {
	if tbl.session == nil {
		return nil
	}
	seats, ok := tbl.form.Seats()
	if !ok {
		return nil
	}
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	sig, err := escrow.SignBondSpend(tx, bond, tbl.session)
	if err != nil {
		return nil
	}
	raw, err := tx.Bytes()
	if err != nil {
		return nil
	}
	tbl.holdAccusation(seat, tx, bond, seats[mine], sig)
	return []outgoing{tbl.frame(schema.KindAccusation, schema.Accusation{
		Seat:   seat,
		Tx:     hex.EncodeToString(raw),
		Signer: hex.EncodeToString(seats[mine]),
		Sig:    hex.EncodeToString(sig),
	}, gwire.ClassState)}
}

// readyAnswer is an answer built and signed, waiting to leave.
//
// Built under the registry lock, where the table's state is, and broadcast
// outside it by dispatchAnswers - the same split deliver makes for frames,
// because a broadcast is an RPC and one that hangs under the lock stalls every
// frame for every table.
type readyAnswer struct {
	sid   string
	seat  uint32
	raw   string
	from  string // the position the answered accusation spends
	next  string // where the answer puts the bond
	chain broadcaster
}

// answerClaim builds the answer that spends the claimed bond back into a bond,
// with this seat's own key.
//
// One signature, nobody asked. That is the shape the claimed bond exists to
// provide: an accusation moves the bond somewhere its owner can take back
// immediately while the accusers wait out the window, so answering needs no
// agreement gathered in advance and no file kept since the table formed. A seat
// that has lost everything but its seed can still do this.
//
// Where to answer is derived rather than remembered. Every accusation against this
// seat is deterministic, so the one that was broadcast is one of the chain this peer
// can rebuild - and the claimed bond it created is its first output.
func (tbl *table) answerClaim() {
	seat, ok := tbl.form.OurSeat()
	if !ok || tbl.session == nil {
		return
	}
	chain, bond, err := tbl.accuseChain(seat)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot work out how to answer: %v", tbl.terms.SID, err)
		return
	}
	at := tbl.bondedAt[seat]
	if at == "" {
		at = tbl.bonded[seat]
	}
	if tbl.answerReady != nil && tbl.answerReady.from == at {
		return // built already, waiting to leave
	}
	prevout, err := outpointOf(at)
	if err != nil {
		return
	}
	// The accusation whose input is where the bond actually sits. Answering
	// moves it on, and the next accusation is against the new output.
	var accuse *wire.MsgTx
	for _, c := range chain {
		if c.TxIn[0].PreviousOutPoint == prevout {
			accuse = c
			break
		}
	}
	if accuse == nil {
		log.Printf("pokerplugin: table %s: claimed against at %s, which no agreed accusation spends",
			tbl.terms.SID, at)
		tbl.note(eventUnanswerable,
			fmt.Sprintf("claimed against at %s, and no accusation this peer agreed spends it", at),
			"", seatp(int(seat)))
		return
	}

	claimed, err := escrow.AccuseDraft{Bond: bond}.ClaimedScript()
	if err != nil {
		return
	}
	answer, err := escrow.BuildAnswer(escrow.AnswerDraft{
		Claimed:    claimed,
		Bond:       bond,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: accuse.TxOut[0].Value,
		FeeAtoms:   claimFee,
		Params:     tbl.netParams,
	})
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot build an answer: %v", tbl.terms.SID, err)
		return
	}
	sig, err := escrow.SignClaimedSpend(answer, claimed, tbl.session)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot sign an answer: %v", tbl.terms.SID, err)
		return
	}
	sigScript, err := escrow.AnswerSigScript(claimed, sig)
	if err != nil {
		log.Printf("pokerplugin: table %s: the answer does not satisfy the claimed bond: %v",
			tbl.terms.SID, err)
		return
	}
	answer.TxIn[0].SignatureScript = sigScript

	raw, err := answer.Bytes()
	if err != nil {
		return
	}
	tbl.answerReady = &readyAnswer{
		sid:   tbl.terms.SID,
		seat:  seat,
		raw:   hex.EncodeToString(raw),
		from:  at,
		next:  fmt.Sprintf("%s:0", answer.TxHash()),
		chain: tbl.chain,
	}
}

// dispatchAnswers broadcasts every answer the tables have built.
//
// Run wherever answerClaim may have run - after a delivery, and after the bond
// walk. An answer built and never dispatched is rebuilt by the next walk, so a
// crash between the two loses nothing.
func (p *plugin) dispatchAnswers(ctx context.Context) {
	p.tables.mu.Lock()
	var ready []*readyAnswer
	for _, tbl := range p.tables.m {
		if tbl.answerReady != nil {
			ready = append(ready, tbl.answerReady)
			tbl.answerReady = nil
		}
	}
	p.tables.mu.Unlock()

	for _, ra := range ready {
		txid, err := ra.chain.Broadcast(ctx, ra.raw)

		p.tables.mu.Lock()
		tbl := p.tables.m[ra.sid]
		if tbl == nil {
			p.tables.mu.Unlock()
			continue
		}
		if err != nil {
			log.Printf("pokerplugin: table %s: could not answer a claim: %v", ra.sid, err)
			tbl.note(eventUnanswerable,
				fmt.Sprintf("claimed against, and the answer could not be sent: %v", err),
				"", seatp(int(ra.seat)))
			p.tables.mu.Unlock()
			continue
		}
		// The bond has moved, so every future accusation is against the new
		// output - unless something else already moved the belief further
		// while the broadcast was in flight, in which case theirs stands.
		if cur := tbl.bondedAt[ra.seat]; cur == "" || cur == ra.from {
			tbl.bondedAt[ra.seat] = ra.next
		}
		log.Printf("pokerplugin: table %s: answered a claim in %s; the bond is posted again at %s",
			ra.sid, txid, ra.next)
		tbl.note(eventAnswered,
			fmt.Sprintf("claimed against, and the answer was sent from this seat's own key; "+
				"the bond is posted again at %s", ra.next), txid, seatp(int(ra.seat)))
		p.tables.mu.Unlock()
	}
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
	if tbl.resultInDoubt() {
		// A challenged hand is a result being checked, and a hand that failed
		// to reproduce is one already answered against. Neither is paid out.
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
	if tbl.resultInDoubt() {
		tbl.note(eventRefused,
			"a settlement arrived for a table with a hand in doubt, and was not signed", "", nil)
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
		tbl.note(eventRefused,
			fmt.Sprintf("a proposed payout was not the one this peer would have built: %v", err), "", nil)
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
		tbl.note(eventBlocked,
			fmt.Sprintf("a fully signed payout did not satisfy the escrows: %v", err), "", nil)
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
		tbl.note(eventBlocked,
			fmt.Sprintf("this peer could not send the payout; every other seat holds the same one: %v", err),
			"", nil)
		return
	}
	log.Printf("pokerplugin: table %s: settled in %s", tbl.terms.SID, txid)
	tbl.note(eventSettled, "the table paid out", txid, nil)
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

// accusationsReady reports whether every seat can be accused if it stops.
//
// The first accusation in each seat's chain has to carry every member's signature.
// The rest of the chain matters only if a seat answers one, and by then the table
// is talking again.
func (tbl *table) accusationsReady() bool {
	seats, ok := tbl.form.Seats()
	if !ok {
		return false
	}
	for seat := range seats {
		chain, bond, err := tbl.accuseChain(seat)
		if err != nil || len(chain) == 0 {
			return false
		}
		terms, err := escrow.ParseTableBond(bond)
		if err != nil {
			return false
		}
		a := tbl.accuse[chain[0].TxIn[0].PreviousOutPoint.String()]
		if a == nil {
			return false
		}
		for _, m := range terms.Members {
			if _, held := a.sigs[hex.EncodeToString(m)]; !held {
				return false
			}
		}
	}
	return true
}

// fullySigned reports whether every member has signed one accusation.
func (tbl *table) fullySigned(tx *wire.MsgTx, terms *escrow.TableBondTerms) bool {
	a := tbl.accuse[tx.TxIn[0].PreviousOutPoint.String()]
	if a == nil {
		return false
	}
	for _, m := range terms.Members {
		if _, held := a.sigs[hex.EncodeToString(m)]; !held {
			return false
		}
	}
	return true
}
