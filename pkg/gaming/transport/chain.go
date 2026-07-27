package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// A game runs where there is no chain to read. It needs one anyway: a bond is
// coin at an outpoint, so a player can only be asked to post one if the others
// can check it; a deadline is only agreed if everyone reads the same height;
// and a dealer drawn from a block hash is only unpredictable if nobody supplies
// the hash themselves.
//
// So the host answers, and the division is the same one that holds everywhere
// else here, seen from the other side. The host carries frames whole and forms
// no opinion about them, because a host that judged a fold would be a party to
// the game. What arrives here is not a judgement about a table - it is what a
// public ledger contains, which anybody could read for themselves given a node.
//
// Trusting the host for that is not a concession. It is the user's own node.
// The parties a player must not have to trust are the other players, and the
// host is not one of them.

// ChainTip is where the chain is now.
type ChainTip struct {
	Height int64  `json:"height"`
	Hash   string `json:"hash"`
}

// Outpoint is what the chain says about one transaction output.
//
// Found is false for an outpoint that never existed, one already spent, and one
// still unconfirmed alike. For the question a game asks - is there coin here
// that everyone can see - those are the same answer.
type Outpoint struct {
	Found         bool   `json:"found"`
	ValueAtoms    int64  `json:"valueAtoms"`
	PkScriptHex   string `json:"pkScriptHex"`
	Confirmations int64  `json:"confirmations"`
	Coinbase      bool   `json:"coinbase"`
}

// ChainTip reports the best block the host's node knows.
func (b *Bridge) ChainTip(ctx context.Context) (ChainTip, error) {
	var tip ChainTip
	err := b.getJSON(ctx, "/chain/tip", nil, &tip)
	return tip, err
}

// BlockHash reports the hash of the block at a height.
//
// By height rather than by asking for the tip, because a table draws its
// seating from one: the tip moves, and two peers reading it a second apart
// would seat the same table differently.
func (b *Bridge) BlockHash(ctx context.Context, height uint32) (string, error) {
	var out struct {
		Height int64  `json:"height"`
		Hash   string `json:"hash"`
	}
	err := b.getJSON(ctx, "/chain/blockhash", url.Values{
		"height": {strconv.FormatUint(uint64(height), 10)},
	}, &out)
	return out.Hash, err
}

// Outpoint reports what is at txid:vout, once it is in a block.
//
// Confirmed only, because this is what decides whether somebody else's deposit
// is worth staking against and an unconfirmed transaction can still be
// replaced.
func (b *Bridge) Outpoint(ctx context.Context, txid string, vout uint32) (Outpoint, error) {
	return b.outpoint(ctx, txid, vout, false)
}

// UnconfirmedOutpoint reports what is at txid:vout, whether or not it is in a
// block yet.
//
// It answers a different question, which is why it has its own name: finding
// which output of a payment you have just made is yours cannot wait for a
// block, because there is not one. Never use it to judge another player's
// bond - that is the case Outpoint exists for.
func (b *Bridge) UnconfirmedOutpoint(ctx context.Context, txid string, vout uint32) (Outpoint, error) {
	return b.outpoint(ctx, txid, vout, true)
}

// Broadcast puts a transaction this game signed itself onto the network.
//
// The other way coin moves is RequestSpend, where the host builds the
// transaction and a person approves it with a passphrase this process never
// sees. That is for paying money in. It cannot be used for taking money back
// out, because a bond, and a stake behind its refund timelock, are spendable
// only with a key this game holds - so the game builds and signs, and the host
// relays.
//
// Nobody approves this and the host does not ask, because the coin is already
// ours to move. What the host does instead is refuse anything of the wrong
// shape: a transaction taking coin that is not ours, or sending ours anywhere
// but back to the person who funded it, is not relayed. So a refusal here is
// about the transaction, not about permission.
func (b *Bridge) Broadcast(ctx context.Context, rawTxHex string) (string, error) {
	body, err := json.Marshal(map[string]any{"rawTxHex": rawTxHex})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/chain/broadcast", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build broadcast request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.Token)

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return "", fmt.Errorf("host refused to relay: %s", readError(resp))
	case http.StatusNotFound:
		return "", fmt.Errorf("host does not recognise this game's token; check it is still installed")
	default:
		return "", fmt.Errorf("broadcast: host returned %s: %s", resp.Status, readError(resp))
	}

	var out struct {
		TxID string `json:"txid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("read the host's answer: %w", err)
	}
	if out.TxID == "" {
		return "", fmt.Errorf("the host relayed it but named no transaction")
	}
	return out.TxID, nil
}

func (b *Bridge) outpoint(ctx context.Context, txid string, vout uint32, includeMempool bool) (Outpoint, error) {
	q := url.Values{
		"txid": {strings.TrimSpace(txid)},
		"vout": {strconv.FormatUint(uint64(vout), 10)},
	}
	if includeMempool {
		q.Set("mempool", "1")
	}
	var out Outpoint
	err := b.getJSON(ctx, "/chain/outpoint", q, &out)
	return out, err
}

func (b *Bridge) getJSON(ctx context.Context, path string, query url.Values, into any) error {
	endpoint := b.BaseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("ask the host: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		// The node is not there. Worth telling apart from a bad
		// question: this one is worth asking again.
		return fmt.Errorf("the host has no chain to read yet")
	case http.StatusNotFound:
		return fmt.Errorf("host does not recognise this game's token; check it is still installed")
	default:
		return fmt.Errorf("host returned %s: %s", resp.Status, readError(resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("read the host's answer: %w", err)
	}
	return nil
}
