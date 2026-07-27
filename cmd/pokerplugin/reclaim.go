package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
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
	return p.bridge.Broadcast(ctx, hex.EncodeToString(raw))
}

// handleTableRefund takes this player's stake back out of a table.
func (p *plugin) handleTableRefund(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SID      string `json:"sid"`
		DestAddr string `json:"destAddr"`
		FeeAtoms int64  `json:"feeAtoms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sid := strings.ToLower(strings.TrimSpace(req.SID))

	seat, dep, terms, stake, err := p.tables.ourDeposit(sid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if stake == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("nothing was ever paid into seat %d of %s", seat, sid))
		return
	}
	redeem, err := hex.DecodeString(dep.RedeemScriptHex)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("this seat's script: %w", err))
		return
	}
	key, err := p.id.sessionKey(sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	txid, err := p.reclaim(r.Context(), stake, redeem, key, terms.CSVBlocks,
		escrow.RefundSigScript, req.DestAddr, req.FeeAtoms)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Forget the stake only once it is on its way, and forget it before
	// replying: a table still citing an output it has spent would announce
	// a stake that is no longer there.
	p.tables.forgetStake(sid, seat)
	log.Printf("pokerplugin: table %s: reclaimed seat %d's stake in %s", sid, seat, txid)
	writeJSON(w, map[string]any{"sid": sid, "seat": seat, "txid": txid})
}

// handleBondSweep takes this player's fidelity bond back.
func (p *plugin) handleBondSweep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DestAddr string `json:"destAddr"`
		FeeAtoms int64  `json:"feeAtoms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	outpoint := p.id.bondDeposit()
	if outpoint == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("this player holds no bond"))
		return
	}
	script, err := p.id.bondScript()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	key, err := p.id.bondKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// The lock is not looked up because it is not a choice: bondScript
	// always builds the minimum, so the bond it derives is the bond it
	// derives.
	txid, err := p.reclaim(r.Context(), outpoint, script, key, escrow.MinBondBlocks,
		escrow.BondSigScript, req.DestAddr, req.FeeAtoms)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if err := p.id.setBondDeposit(""); err != nil {
		// The coin is already moving. Saying so is all that is left, and
		// it matters: an identity still citing a spent bond will have its
		// joins refused by everyone.
		log.Printf("pokerplugin: swept the bond in %s but could not forget it: %v", txid, err)
	}
	log.Printf("pokerplugin: swept the bond into %s", txid)
	writeJSON(w, map[string]any{"txid": txid})
}
