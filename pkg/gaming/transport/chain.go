package transport

import (
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

// Outpoint reports what is at txid:vout.
func (b *Bridge) Outpoint(ctx context.Context, txid string, vout uint32) (Outpoint, error) {
	var out Outpoint
	err := b.getJSON(ctx, "/chain/outpoint", url.Values{
		"txid": {strings.TrimSpace(txid)},
		"vout": {strconv.FormatUint(uint64(vout), 10)},
	}, &out)
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
