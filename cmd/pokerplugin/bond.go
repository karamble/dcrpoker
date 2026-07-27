package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
	"github.com/vctt94/pokerbisonrelay/pkg/membership"
)

// bondCacheFor is how long a verified deposit is taken on trust before being
// looked up again. A bond can be spent, so this is not forever; a table forms in
// minutes, so it does not need to be short.
const bondCacheFor = 10 * time.Minute

// bonds remembers which deposits have been checked, so a join re-broadcast for
// healing does not mean asking the chain again every time.
type bonds struct {
	mu     sync.Mutex
	seen   map[string]time.Time
	chain  *transport.Bridge
	params stdaddr.AddressParams
}

func newBonds(bridge *transport.Bridge, params stdaddr.AddressParams) *bonds {
	return &bonds{seen: make(map[string]time.Time), chain: bridge, params: params}
}

// check decides whether a join's bond is really locked coin.
//
// The rule lives in pkg/membership so every peer applies the same one; what
// happens here is only fetching the facts it needs. The host answers, because
// the sandbox has no node - and what comes back is what a public ledger
// contains rather than anybody's opinion about a table.
func (b *bonds) check(ctx context.Context, j *membership.Join) error {
	if j == nil {
		return fmt.Errorf("no join")
	}
	key := j.Bond.Outpoint + "/" + hex.EncodeToString(j.Bond.Script)

	b.mu.Lock()
	if until, ok := b.seen[key]; ok && time.Now().Before(until) {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	txid, vout, err := splitOutpoint(j.Bond.Outpoint)
	if err != nil {
		return err
	}
	out, err := b.chain.Outpoint(ctx, txid, vout)
	if err != nil {
		// The chain could not be read. Refusing is right: admitting a
		// join whose bond nobody checked is admitting a free seat, and
		// the table's deadline will end it if this does not clear.
		return fmt.Errorf("could not check the bond: %w", err)
	}
	if err := membership.CheckBond(j, membership.BondFacts{
		Found:         out.Found,
		ValueAtoms:    out.ValueAtoms,
		PkScriptHex:   out.PkScriptHex,
		Confirmations: out.Confirmations,
	}, b.params); err != nil {
		return err
	}

	b.mu.Lock()
	b.seen[key] = time.Now().Add(bondCacheFor)
	b.mu.Unlock()
	return nil
}

func splitOutpoint(s string) (string, uint32, error) {
	txid, voutStr, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return "", 0, fmt.Errorf("outpoint %q is not txid:vout", s)
	}
	vout, err := strconv.ParseUint(voutStr, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("outpoint %q is not txid:vout", s)
	}
	return txid, uint32(vout), nil
}

// maxFundingVout bounds the search for which output of a funding transaction
// paid the bond. An ordinary send has a payment and a change output; this is
// generous rather than clever.
const maxFundingVout = 4

// handleBond reports this player's bond: where it must be paid, and what the
// chain says about it if it has been.
func (p *plugin) handleBond(w http.ResponseWriter, r *http.Request) {
	script, err := p.id.bondScript()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	addr, _, err := escrow.BondAddress(script, p.params)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	out := map[string]any{
		"address":    addr.String(),
		"scriptHex":  hex.EncodeToString(script),
		"outpoint":   p.id.bondDeposit(),
		"minAtoms":   escrow.MinBondAtoms,
		"minBlocks":  escrow.MinBondBlocks,
		"confsNeed":  escrow.BondConfirmations,
		"hasDeposit": p.id.bondDeposit() != "",
	}
	writeJSON(w, out)
}

// handleBondFund asks the host to lock this player's bond.
//
// It is the host that decides: the account, the caps and whether a person
// agreed are all its business, and this process holds no wallet key and never
// learns the passphrase. All it can do is ask, and wait.
func (p *plugin) handleBondFund(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if have := p.id.bondDeposit(); have != "" {
		writeJSON(w, map[string]any{"outpoint": have, "funded": true})
		return
	}

	script, err := p.id.bondScript()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	addr, pkScript, err := escrow.BondAddress(script, p.params)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	spend, err := p.bridge.RequestSpend(r.Context(), addr.String(), int64(escrow.MinBondAtoms),
		"lock a fidelity bond, so this player's seat costs something")
	if err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}

	// A person has to answer, and they may be a while.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	settled, err := p.bridge.AwaitSpend(ctx, spend.ID)
	if err != nil {
		writeErr(w, http.StatusGatewayTimeout, err)
		return
	}
	if settled.State != transport.SpendApproved {
		writeErr(w, http.StatusForbidden,
			fmt.Errorf("the bond was not paid: %s %s", settled.State, settled.Error))
		return
	}

	outpoint, err := p.findBondOutput(ctx, settled.TxID, hex.EncodeToString(pkScript))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := p.id.setBondDeposit(outpoint); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	log.Printf("pokerplugin: bond locked at %s", outpoint)
	writeJSON(w, map[string]any{"outpoint": outpoint, "funded": true, "txid": settled.TxID})
}

// findBondOutput works out which output of the funding transaction paid the
// bond. The host reports a transaction id and nothing about its shape, so this
// asks about each output until one pays the script this player's bond needs.
func (p *plugin) findBondOutput(ctx context.Context, txid, wantPkScript string) (string, error) {
	for vout := uint32(0); vout < maxFundingVout; vout++ {
		out, err := p.bridge.Outpoint(ctx, txid, vout)
		if err != nil {
			return "", fmt.Errorf("look up %s:%d: %w", txid, vout, err)
		}
		if out.Found && strings.EqualFold(out.PkScriptHex, wantPkScript) {
			return fmt.Sprintf("%s:%d", txid, vout), nil
		}
	}
	return "", fmt.Errorf("none of the first %d outputs of %s pay this bond", maxFundingVout, txid)
}

// paramsForNetwork picks the chain parameters a bond address is built for.
func paramsForNetwork(name string) (stdaddr.AddressParams, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "mainnet":
		return chaincfg.MainNetParams(), nil
	case "testnet", "testnet3":
		return chaincfg.TestNet3Params(), nil
	case "simnet":
		return chaincfg.SimNetParams(), nil
	}
	return nil, fmt.Errorf("unknown network %q", name)
}
