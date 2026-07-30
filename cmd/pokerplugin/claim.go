package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

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
		// Nobody accuses themselves. If this peer is the one holding the
		// table up, the answer is to act.
		return nil
	}
	if tbl.claims[duty] != nil {
		return nil
	}

	// The accusation was agreed while everybody was still talking, and this
	// is where it gets used. Nothing is gathered now: an accusation needs
	// every member including the accused, who would not sign today.
	at := tbl.bondedAt[uint32(duty.Seat)]
	if at == "" {
		at = tbl.bonded[uint32(duty.Seat)]
	}
	a := tbl.accuse[at]
	if a == nil {
		log.Printf("pokerplugin: table %s: no agreed accusation against seat %d at %s",
			tbl.terms.SID, duty.Seat, at)
		tbl.note(eventBlocked, fmt.Sprintf(
			"seat %d has stopped, and this peer holds no agreed accusation against its bond at %s",
			duty.Seat, at), "", seatp(duty.Seat))
		return nil
	}
	terms, err := escrow.ParseTableBond(a.bond)
	if err != nil {
		return nil
	}
	sigs := make([][]byte, 0, len(terms.Members))
	for _, m := range terms.Members {
		sig, ok := a.sigs[hex.EncodeToString(m)]
		if !ok {
			log.Printf("pokerplugin: table %s: the accusation against seat %d is short a signature",
				tbl.terms.SID, duty.Seat)
			tbl.note(eventBlocked, fmt.Sprintf(
				"seat %d has stopped, and the agreed accusation is short a signature", duty.Seat),
				"", seatp(duty.Seat))
			return nil
		}
		sigs = append(sigs, sig)
	}
	done, err := escrow.FinishAlive(a.tx, a.bond, sigs, params)
	if err != nil {
		log.Printf("pokerplugin: table %s: the agreed accusation does not satisfy the bond: %v",
			tbl.terms.SID, err)
		return nil
	}
	raw, err := done.Bytes()
	if err != nil {
		return nil
	}

	c := &claim{duty: duty, tx: done, bond: a.bond, sigs: a.sigs, done: true}
	tbl.claims[duty] = c
	log.Printf("pokerplugin: table %s: accusing - %s", tbl.terms.SID, duty)
	tbl.note(eventProposed, fmt.Sprintf("accused seat %d of leaving: %s", duty.Seat, duty),
		"", seatp(duty.Seat))

	// Said out loud as well as broadcast. The chain is what decides, and the
	// accused watches it - but a peer that is listening should not have to
	// wait for a confirmation to learn it is being accused.
	return []outgoing{tbl.frame(schema.KindClaim, schema.Claim{
		Seat:         uint32(duty.Seat),
		Duty:         duty,
		BondOutpoint: at,
		BondScript:   hex.EncodeToString(a.bond),
		Tx:           hex.EncodeToString(raw),
	}, gwire.ClassState)}
}

// claim is one proposal and the signatures gathered for it.
type claim struct {
	duty driver.Duty
	tx   *wire.MsgTx
	bond []byte
	// sigs is each signature the accusation carried, by its key in hex.
	sigs map[string][]byte
	done bool
	// taken records that the claimed bond this produced has been forfeited,
	// so the forfeiture is not proposed twice.
	taken bool
}

func seatOf(seats map[uint32][]byte, key []byte) (uint32, bool) {
	for seat, k := range seats {
		if hex.EncodeToString(k) == hex.EncodeToString(key) {
			return seat, true
		}
	}
	return 0, false
}

// acceptClaim is what this peer does when somebody says they have accused a seat.
//
// Nothing is co-signed and nothing is weighed. The accusation was agreed before
// the dispute and the chain decides it, so this message only saves the accused a
// wait: if it is about us, answer now rather than when the accusation confirms.
// The chain is watched as well, precisely because this can be withheld.
func (tbl *table) acceptClaim(body schema.Claim, params stdaddr.AddressParams) []outgoing {
	seat, ours := tbl.form.OurSeat()
	if !ours {
		return nil
	}
	if int(seat) != body.Duty.Seat {
		// Somebody else is accused. Worth recording, since a table where a
		// seat is being accused is a table about to end one way or another.
		tbl.note(eventProposed, fmt.Sprintf("seat %d is accused of leaving: %s",
			body.Duty.Seat, body.Duty), "", seatp(body.Duty.Seat))
		return nil
	}
	log.Printf("pokerplugin: table %s: accused of leaving, and answering", tbl.terms.SID)
	tbl.answerClaim(context.Background())
	return nil
}

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

// payoutFrame signs where this seat wants to be paid and frames it.
//
// Deliberately without the "we already recorded this" guard that announcePayout
// has. Recording and saying are two different things, and conflating them is
// what made this address impossible to repeat: our own record says nothing about
// whether the peer holds it, and the peer that still needs to hear it is by
// definition the one whose view differs from ours.
//
// Nil before the seating is drawn, because a payout is signed against a seat and
// there is no seat yet. That is correct here and is exactly why the caller has
// to come back afterwards - see announcePayoutAgain.
func (tbl *table) payoutFrame(addr string) []outgoing {
	seat, ok := tbl.form.OurSeat()
	if !ok || addr == "" || tbl.session == nil {
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

// announcePayout says where this player wants to be paid at a table, when that
// is news. The repeat that makes sure it arrives is announcePayoutAgain.
func (tbl *table) announcePayout(addr string) []outgoing {
	if seat, ok := tbl.form.OurSeat(); ok && tbl.payouts[seat] == addr {
		return nil
	}
	return tbl.payoutFrame(addr)
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
