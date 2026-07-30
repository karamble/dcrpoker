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
// so one slow answer cannot stall every other table, then apply.
func (p *plugin) confirmOurPayments(ctx context.Context) {
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
