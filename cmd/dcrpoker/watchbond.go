package main

import (
	"context"
	"log"
)

// Where every bond sits now, asked of the chain.
//
// A bond moves without anybody saying so: an accusation spends it into the
// claimed bond, and an answer spends that back into a fresh bond. Every position
// it can occupy is derivable in advance - see bondLadder - so this walks the
// ladder from where each bond was last believed to be until it finds the coin.
//
// Three things fall out of the one walk. A mined accusation against this seat is
// found as the claimed bond it created, and answered - the window opens when the
// accusation confirms, so a spend seen only once mined is the window opening,
// not the window missed. A bond that moved while nobody said so - this seat's
// own after a restart, or another seat's whose answer this peer never heard
// about - is found where it now sits, so later accusations and releases build
// against the coin rather than against history. And a spend still waiting in
// the mempool shows as the confirmed view holding an output the mempool view
// says is gone, which is answered before it confirms.
func (p *plugin) watchBonds(ctx context.Context) {
	type ask struct {
		sid       string
		seat      uint32
		ours      bool
		start     int
		positions []string // positions[i] is where the bond sits before accusation i
		claimed   []string // claimed[i] is the output accusation i creates
	}
	var asks []ask
	p.tables.mu.Lock()
	for sid, tbl := range p.tables.m {
		// Finished tables are walked too, on purpose. A table that comes back
		// from a restart is finished - it can never deal again - but its bond
		// is still on the chain and still claimable, and the bond outliving
		// the table is the whole reason the answer needs only the seed.
		mine, ok := tbl.form.OurSeat()
		if !ok {
			continue
		}
		seats, ok := tbl.form.Seats()
		if !ok {
			continue
		}
		for seat := range seats {
			if tbl.bonded[seat] == "" {
				continue
			}
			r, err := tbl.bondLadder(seat)
			if err != nil {
				continue
			}
			believed := tbl.bondedAt[seat]
			if believed == "" {
				believed = tbl.bonded[seat]
			}
			a := ask{sid: sid, seat: seat, ours: seat == mine, start: -1,
				positions: r.positions, claimed: r.claimed}
			for i, pos := range a.positions {
				if pos == believed {
					a.start = i
				}
			}
			if a.start < 0 {
				// Wherever this peer believes the bond is, no agreed
				// accusation spends it. Nothing here can move it on.
				continue
			}
			asks = append(asks, a)
		}
	}
	p.tables.mu.Unlock()

	for _, a := range asks {
		// landed says the walk found the bond rather than running out of
		// ladder. Only a walk that found it may write down where it is: a
		// bond that is at no rung at all has been released, forfeited or
		// swept, and recording a position for it would be inventing one.
		at, answer, landed := a.start, false, false
		for ; at < len(a.positions); at++ {
			found, err := p.onChain(ctx, a.positions[at])
			if err != nil {
				at = -1 // the chain could not be read; the next poll asks again
				break
			}
			if found {
				landed = true
				// The bond is here. For our own, a mempool transaction
				// spending it may be an accusation to answer before it
				// confirms - or the cooperative release, which spends the
				// same branch. Only the accusation creates the claimed
				// output, so that appearing in the mempool view is what
				// tells them apart, and answering a release would be
				// broadcasting a spend of an output that will never exist.
				// Only where an accusation exists for this rung. On the last
				// one nothing spends the bond, so a mempool spend of it can
				// only be the release.
				if a.ours && at < len(a.claimed) {
					if gone, err := p.leavingMempool(ctx, a.positions[at]); err == nil && gone {
						if seen, err := p.inMempoolView(ctx, a.claimed[at]); err == nil && seen {
							answer = true
						}
					}
				}
				break
			}
			if at >= len(a.claimed) {
				// The last rung, and the coin is not on it. Nothing spends
				// this position, so there is nowhere further to walk.
				break
			}
			claimed, err := p.onChain(ctx, a.claimed[at])
			if err != nil {
				at = -1
				break
			}
			if claimed {
				landed = true
				// A mined accusation, and the window is counting. Ours is
				// answered - unless the answer is already on its way, which
				// looks like the claimed output leaving the mempool.
				if a.ours {
					if gone, err := p.leavingMempool(ctx, a.claimed[at]); err == nil && !gone {
						answer = true
					}
				}
				break
			}
			// Both gone: the accusation here was answered, and the bond sits
			// one position on. A forfeited or released bond matches nothing
			// further, and the walk ends having moved nothing.
		}
		if at < 0 {
			continue
		}
		// A walk that landed nowhere found the coin at no rung and no
		// claimed output - and every position was asked of the confirmed
		// set, so whatever spent it is mined. Only three spends leave the
		// ladder at all: the cooperative release, the backstop sweep and
		// the forfeiture, and each is the end of this bond's story. That is
		// what finally lets the maps forget it, which nothing on the
		// release path can do - both peers broadcast the release, one of
		// them is refused as a duplicate, and only the chain knows the
		// truth both can see.
		//
		// One thing looks the same and is not: a bond broadcast and never
		// mined sits at rung 0 of the confirmed set's view of nothing. The
		// mempool-aware view still sees it, so ask that view before calling
		// coin gone - clearing an unmined bond would forget real money.
		gone := !landed
		if gone {
			seen, err := p.inMempoolView(ctx, a.positions[a.start])
			if err != nil || seen {
				continue
			}
		}

		p.tables.mu.Lock()
		tbl := p.tables.m[a.sid]
		if tbl == nil {
			p.tables.mu.Unlock()
			continue
		}
		// The walk ran unlocked, and something else - an answer, a frame -
		// may have moved the belief meanwhile. A walk that started from a
		// belief nobody holds any more concluded nothing about the current
		// one, so it writes nothing; the next poll walks from wherever the
		// belief now is.
		believed := tbl.bondedAt[a.seat]
		if believed == "" {
			believed = tbl.bonded[a.seat]
		}
		if believed != a.positions[a.start] {
			p.tables.mu.Unlock()
			continue
		}
		if gone {
			delete(tbl.bonded, a.seat)
			delete(tbl.bondedAt, a.seat)
			delete(tbl.bondValue, a.seat)
			seat := a.seat
			tbl.note(eventSettled, "the bond left the chain", "", &seat)
			log.Printf("pokerplugin: table %s: seat %d's bond left the chain; forgetting it",
				a.sid, a.seat)
			if tbl.finished && !tbl.holdsOurs() {
				// The receipt was only ever kept for this. See drop:
				// with nothing of ours left it deletes rather than
				// marks, and the emptied record on disk is what stops
				// the next boot resurrecting it.
				log.Printf("pokerplugin: table %s holds nothing of ours any more; letting it go",
					a.sid)
				p.tables.drop(a.sid, tbl)
			} else {
				p.tables.persist(tbl)
			}
			p.tables.mu.Unlock()
			continue
		}
		if landed && at > a.start {
			tbl.bondedAt[a.seat] = a.positions[at]
			// The value shrank by two fees per answer, so what the chain
			// says is there is re-read rather than remembered.
			delete(tbl.bondValue, a.seat)
			log.Printf("pokerplugin: table %s: seat %d's bond moved while nobody said so; it sits at %s",
				a.sid, a.seat, a.positions[at])
		}
		if answer {
			log.Printf("pokerplugin: table %s: our bond at %s is claimed against; answering",
				a.sid, a.positions[at])
			tbl.answerClaim()
		}
		p.tables.mu.Unlock()
	}
	p.dispatchAnswers(ctx)
}

// onChain reports whether an output is in the confirmed UTXO set. Absent says
// nothing about why: spent, unconfirmed and never-existed all look the same.
func (p *plugin) onChain(ctx context.Context, outpoint string) (bool, error) {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return false, err
	}
	out, err := p.bridge.Outpoint(ctx, txid, vout)
	if err != nil {
		return false, err
	}
	return out.Found, nil
}

// leavingMempool reports whether a confirmed output is being spent by a mempool
// transaction: still in the confirmed set, and gone from the mempool-aware view.
func (p *plugin) leavingMempool(ctx context.Context, outpoint string) (bool, error) {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return false, err
	}
	out, err := p.bridge.UnconfirmedOutpoint(ctx, txid, vout)
	if err != nil {
		return false, err
	}
	return !out.Found, nil
}

// inMempoolView reports whether the mempool-aware view can see an output - which
// an output created by a transaction still in the mempool is.
func (p *plugin) inMempoolView(ctx context.Context, outpoint string) (bool, error) {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return false, err
	}
	out, err := p.bridge.UnconfirmedOutpoint(ctx, txid, vout)
	if err != nil {
		return false, err
	}
	return out.Found, nil
}
