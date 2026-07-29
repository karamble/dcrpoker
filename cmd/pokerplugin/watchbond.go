package main

import (
	"context"
	"log"
)

// Answering a claim nobody told us about.
//
// answerClaim otherwise runs only when a claim frame arrives over the group chat,
// so an opponent who sends nothing accuses us in silence and takes the bond
// uncontested. The claim itself cannot be hidden, though: it has to reach the
// network to be worth anything.
//
// The host reports an outpoint as absent whether it is spent, unconfirmed or never
// existed, so "is it gone" is not a useful question. Asking twice is: a confirmed
// output that a mempool transaction is spending is still in the confirmed set, so
// the confirmed-only view finds it while the mempool-aware view does not. That
// disagreement is somebody's claim, waiting to be mined.
//
// It is not a guarantee. A claim that goes straight into a block is seen too late,
// and winning the race that follows depends on being asked first. The answer to
// that is a lock that runs from the accusation rather than from the bond - see
// escrow.TableBondScript - and until that exists this is what makes an unannounced
// claim contested rather than free.
func (p *plugin) watchOwnBond(ctx context.Context) {
	type ask struct {
		sid      string
		outpoint string
	}
	var asks []ask
	p.tables.mu.Lock()
	for sid, tbl := range p.tables.m {
		seat, ok := tbl.form.OurSeat()
		if !ok || tbl.finished {
			continue
		}
		outpoint := tbl.bondedAt[seat]
		if outpoint == "" {
			outpoint = tbl.bonded[seat]
		}
		if outpoint == "" {
			continue
		}
		asks = append(asks, ask{sid: sid, outpoint: outpoint})
	}
	p.tables.mu.Unlock()

	for _, a := range asks {
		txid, vout, err := splitOutpoint(a.outpoint)
		if err != nil {
			continue
		}
		settled, err := p.bridge.Outpoint(ctx, txid, vout)
		if err != nil || !settled.Found {
			// Either the chain cannot be read, or the bond is already
			// spent and confirmed. Neither is a claim to answer: the
			// first is not an answer to anything, and the second is
			// over.
			continue
		}
		pending, err := p.bridge.UnconfirmedOutpoint(ctx, txid, vout)
		if err != nil || pending.Found {
			continue
		}

		log.Printf("pokerplugin: table %s: something is spending our table bond at %s; answering",
			a.sid, a.outpoint)
		p.tables.mu.Lock()
		tbl := p.tables.m[a.sid]
		if tbl != nil {
			tbl.answerClaim(ctx)
		}
		p.tables.mu.Unlock()
	}
}
