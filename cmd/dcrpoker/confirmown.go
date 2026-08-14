package main

import (
	"context"
	"log"
)

// Checking our own money against the chain.
//
// A seat's stake and bond are written down here the moment the payment is
// broadcast, and that is deliberate: the outpoint is what gets announced, and
// it is what stops a second payment being made for something already paid for.
// What it is not is a fact yet. Every other seat's payment is held to its
// confirmations before it counts, and the two accept paths do that with
// checkStake and checkTableBond. This is the same question asked about ours.
//
// Getting it wrong is quiet rather than loud. The box that paid deals while
// every other correctly waits, so no hand can progress and the chain catches up
// a block or two later - but in the meantime that box has told its player, its
// log and its interface that a table is dealing on the strength of a
// transaction that may never confirm.

// ourMoneyConfirmed reports whether the chain has agreed to our own payments.
func (tbl *table) ourMoneyConfirmed() bool {
	seat, ok := tbl.form.OurSeat()
	if !ok {
		// No seat, so nothing of ours is in either map to be ahead of the
		// chain.
		return true
	}
	if tbl.funded[seat] != "" && !tbl.ourStakeSeen {
		return false
	}
	if tbl.bonded[seat] != "" && !tbl.ourBondSeen {
		return false
	}
	return true
}

// confirmOurPayments asks the chain about our own stake and bond, and starts
// dealing when it agrees.
//
// Shaped like learnBondValues: gather under the lock, ask the chain without it
// so one slow answer cannot stall every other table, then apply. Once a block
// rather than once a poll - confirmations only arrive with a block, so asking
// again at the same height re-asks a question whose answer cannot have
// changed, and a payment that never confirms would otherwise be asked about
// every poll forever.
func (p *plugin) confirmOurPayments(ctx context.Context, height int64) {
	type ask struct {
		sid     string
		seat    uint32
		stake   string
		stakePk string
		buyIn   uint64
		bond    string
		bondPk  string
	}
	var asks []ask

	p.tables.mu.Lock()
	for sid, tbl := range p.tables.m {
		seat, ok := tbl.form.OurSeat()
		if !ok || tbl.finished {
			continue
		}
		if height > 0 && height <= tbl.confirmAskedAt {
			continue
		}
		a := ask{sid: sid, seat: seat, buyIn: tbl.terms.BuyInAtoms}
		if out := tbl.funded[seat]; out != "" && !tbl.ourStakeSeen {
			if want, err := tbl.deposit(seat, p.tables.params); err == nil {
				a.stake, a.stakePk = out, want.PkScriptHex
			}
		}
		if out := tbl.bonded[seat]; out != "" && !tbl.ourBondSeen {
			if want, err := tbl.bond(seat, p.tables.params); err == nil {
				a.bond, a.bondPk = out, want.PkScriptHex
			}
		}
		if a.stake == "" && a.bond == "" {
			continue
		}
		tbl.confirmAskedAt = height
		asks = append(asks, a)
	}
	p.tables.mu.Unlock()

	for _, a := range asks {
		var stakeSeen, bondSeen bool
		var bondValue int64

		if a.stake != "" {
			if err := checkStake(ctx, p.tables.chain, a.stake, a.stakePk, a.buyIn); err != nil {
				log.Printf("pokerplugin: table %s: our own stake: %v", a.sid, err)
			} else {
				stakeSeen = true
			}
		}
		if a.bond != "" {
			value, err := checkTableBond(ctx, p.tables.chain, a.bond, a.bondPk)
			if err != nil {
				log.Printf("pokerplugin: table %s: our own bond: %v", a.sid, err)
			} else {
				bondSeen, bondValue = true, value
			}
		}
		if !stakeSeen && !bondSeen {
			continue
		}

		p.tables.mu.Lock()
		tbl := p.tables.m[a.sid]
		if tbl == nil {
			p.tables.mu.Unlock()
			continue
		}
		if stakeSeen && !tbl.ourStakeSeen {
			tbl.ourStakeSeen = true
			log.Printf("pokerplugin: table %s: the chain has our own stake at %s",
				a.sid, a.stake)
		}
		if bondSeen && !tbl.ourBondSeen {
			tbl.ourBondSeen = true
			if bondValue > 0 {
				tbl.bondValue[a.seat] = bondValue
			}
			log.Printf("pokerplugin: table %s: the chain has our own bond at %s",
				a.sid, a.bond)
		}
		out := tbl.startPlaying()
		p.tables.mu.Unlock()
		p.publish(ctx, out)
	}
}

// forgetSpentStakes clears a stake the chain has already paid out.
//
// The other half of what keeps a receipt alive. The release path cannot clear
// the bond and nothing at all clears the stake: a settlement is broadcast,
// confirms, pays the winner - and funded[seat] still names the outpoint, so
// holdsOurs stays true and the receipt is resurrected at every boot, saying
// nothing and holding nothing. Asked only about tables that are finished or
// whose game is over, because dealing waited for the stake to confirm - so for
// these tables "absent from both views" can only mean a mined spend took it.
//
// The one residual: a table finished before it ever dealt, whose stake
// transaction was evicted unmined. That is absent from both views too, and
// clearing it forgets a payment that never happened - which loses nothing,
// because an unmined payment's inputs never left this player's wallet.
//
// Once a block, like confirmOurPayments and for the same reason: an outpoint
// only leaves the chain when a block arrives, and a receipt whose coin never
// moves would otherwise be asked about every poll for as long as it is held.
func (p *plugin) forgetSpentStakes(ctx context.Context, height int64) {
	type ask struct {
		sid   string
		seat  uint32
		stake string
	}
	var asks []ask

	p.tables.mu.Lock()
	for sid, tbl := range p.tables.m {
		if !tbl.finished && (tbl.play == nil || !tbl.play.Over()) {
			continue
		}
		if height > 0 && height <= tbl.forgetAskedAt {
			continue
		}
		seat, ok := tbl.form.OurSeat()
		if !ok || tbl.funded[seat] == "" {
			continue
		}
		tbl.forgetAskedAt = height
		asks = append(asks, ask{sid: sid, seat: seat, stake: tbl.funded[seat]})
	}
	p.tables.mu.Unlock()

	for _, a := range asks {
		confirmed, err := p.onChain(ctx, a.stake)
		if err != nil || confirmed {
			continue
		}
		seen, err := p.inMempoolView(ctx, a.stake)
		if err != nil || seen {
			continue
		}

		p.tables.mu.Lock()
		tbl := p.tables.m[a.sid]
		if tbl == nil || tbl.funded[a.seat] != a.stake {
			p.tables.mu.Unlock()
			continue
		}
		delete(tbl.funded, a.seat)
		seat := a.seat
		tbl.note(eventSettled, "the stake left the chain", "", &seat)
		log.Printf("pokerplugin: table %s: our stake at %s left the chain; forgetting it",
			a.sid, a.stake)
		if tbl.finished && !tbl.holdsOurs() {
			log.Printf("pokerplugin: table %s holds nothing of ours any more; letting it go",
				a.sid)
			p.tables.drop(a.sid, tbl)
		} else {
			p.tables.persist(tbl)
		}
		p.tables.mu.Unlock()
	}
}
