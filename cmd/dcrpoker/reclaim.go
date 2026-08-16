package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// Taking our own coin back out.
//
// Two things end up locked behind a timelock with this player's key on it: the
// fidelity bond, which is the standing cost of being somebody, and a stake in a
// table that did not finish. Both are spendable by nobody else, ever, and by us
// only once the lock matures - so getting them back is a transaction this
// process builds and signs itself, and the host merely relays.
//
// Neither is a settlement. A refund is the escape hatch that exists precisely
// so recovering your own money never depends on anyone else being online, and
// it is the only way out of a table that died until the abort transaction is
// built. Slow, unilateral, and always available, which is the right trade for
// something whose whole job is to still work when nothing else does.

// defaultReclaimFee is what is left for the miner when the caller says nothing.
//
// One input and one output is a few hundred bytes, so this is generous rather
// than calculated. It is worth being generous: the alternative to a transaction
// that confirms slowly is coin that sits behind a matured lock nobody swept.
const defaultReclaimFee = 20_000

// payScriptFor turns the address the host named into the script that pays it.
//
// The address comes from the caller and never from this process. That is not a
// convenience - the coin being reclaimed came out of the user's wallet, and a
// game that chose where its refunds landed could simply choose itself. The host
// names the destination, and refuses to relay anything paying elsewhere.
func payScriptFor(addr string, params stdaddr.AddressParams) ([]byte, error) {
	a, err := stdaddr.DecodeAddress(strings.TrimSpace(addr), params)
	if err != nil {
		return nil, fmt.Errorf("destination address: %w", err)
	}
	_, script := a.PaymentScript()
	return script, nil
}

// reclaim builds, checks and relays a spend of one timelocked output.
//
// The maturity check is here rather than in the builder because it is the one
// thing a script engine cannot see: whether a branch is spendable depends on
// how many blocks sit on top of the output, which is a fact about the chain and
// not about the transaction. Left out, this produces a transaction that
// verifies perfectly and that the network then refuses.
func (p *plugin) reclaim(ctx context.Context, outpoint string, script []byte, key *secp256k1.PrivateKey,
	csvBlocks uint32, sigScript func(script, sig []byte) ([]byte, error),
	destAddr string, feeAtoms int64) (string, error) {

	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return "", err
	}
	// Confirmed only: the age of this output is the whole question.
	out, err := p.bridge.Outpoint(ctx, txid, vout)
	if err != nil {
		return "", fmt.Errorf("could not read the output: %w", err)
	}
	switch {
	case !out.Found:
		return "", fmt.Errorf("%s holds no coin - it may already have been spent", outpoint)
	case out.Confirmations < int64(csvBlocks):
		return "", fmt.Errorf("%s has %d confirmations and the lock is %d, so it is not spendable for another %d blocks",
			outpoint, out.Confirmations, csvBlocks, int64(csvBlocks)-out.Confirmations)
	}

	// The script has to be the one this output was paid into. The engine check
	// inside BuildTimelockedSpend derives its pkScript from the script it was
	// handed, so it cannot see a script that is simply the wrong one, and dcrd
	// reports that as a stack failure long after we signed.
	_, want, err := escrow.Address(script, p.params)
	if err != nil {
		return "", fmt.Errorf("derive the script's address: %w", err)
	}
	if got := hex.EncodeToString(want); !strings.EqualFold(got, out.PkScriptHex) {
		return "", fmt.Errorf("%s pays %s, and this key derives %s - so this is not the script that "+
			"output was paid into; nothing was signed and the coin is untouched",
			outpoint, out.PkScriptHex, got)
	}

	payScript, err := payScriptFor(destAddr, p.params)
	if err != nil {
		return "", err
	}
	if feeAtoms <= 0 {
		feeAtoms = defaultReclaimFee
	}

	prevHash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return "", fmt.Errorf("outpoint txid: %w", err)
	}
	tx, err := escrow.BuildTimelockedSpend(escrow.Spend{
		Key:        key,
		Script:     script,
		Prevout:    wire.OutPoint{Hash: *prevHash, Index: vout, Tree: wire.TxTreeRegular},
		ValueAtoms: out.ValueAtoms,
		CSVBlocks:  csvBlocks,
		PayScript:  payScript,
		FeeAtoms:   feeAtoms,
		SigScript:  sigScript,
		Params:     p.params,
	})
	if err != nil {
		return "", err
	}

	raw, err := tx.Bytes()
	if err != nil {
		return "", fmt.Errorf("serialise: %w", err)
	}
	txhex, err := p.bridge.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		return "", err
	}
	p.noteSweeping(outpoint)
	return txhex, nil
}

// noteSweeping records that this process broadcast a spend of an outpoint.
//
// dcrd's gettxout ignores mempool spends even with includemempool set, so an
// output whose reclaim is already broadcast still reads as unspent coin. Until
// a block carries it away, this is the only reliable answer to "is one already
// on its way", and it is the difference between offering a refund once and
// offering it twice.
func (p *plugin) noteSweeping(outpoint string) {
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	if p.sweeping == nil {
		p.sweeping = make(map[string]bool)
	}
	p.sweeping[outpoint] = true
}

// isSweeping reports whether this process has a spend of the outpoint out.
func (p *plugin) isSweeping(outpoint string) bool {
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	return p.sweeping[outpoint]
}

// doneSweeping forgets an outpoint the chain has stopped holding, so the map
// does not grow for the life of the process.
func (p *plugin) doneSweeping(outpoint string) {
	p.sweepMu.Lock()
	defer p.sweepMu.Unlock()
	delete(p.sweeping, outpoint)
}
