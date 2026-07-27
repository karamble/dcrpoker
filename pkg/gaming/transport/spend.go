package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Spending is the one thing a game asks the host for that the host judges.
//
// Everywhere else the bridge carries frames whole and forms no opinion, because
// a host that understood a fold would be a party to the game. Money is the
// exception and the reason the split exists at all: which account may be spent
// from, how much at once, how much in a day, and whether a person agreed are
// all the host's to decide, and none of them are the game's.
//
// So a game asks, and waits. It never learns the passphrase, never holds a key
// to the wallet, and cannot tell the difference between a person who said no
// and a person who was not there - which is correct, because for a game those
// are the same answer.

// SpendState is what became of a request.
type SpendState string

const (
	SpendPending  SpendState = "pending"
	SpendApproved SpendState = "approved"
	SpendDenied   SpendState = "denied"
	SpendExpired  SpendState = "expired"
	SpendFailed   SpendState = "failed"
)

// Spend is a request to move money, and what was decided about it.
type Spend struct {
	ID          string     `json:"id"`
	Game        string     `json:"game"`
	Address     string     `json:"address"`
	AmountAtoms int64      `json:"amountAtoms"`
	Reason      string     `json:"reason,omitempty"`
	State       SpendState `json:"state"`
	TxID        string     `json:"txid,omitempty"`
	Error       string     `json:"error,omitempty"`
	RequestedAt int64      `json:"requestedAt"`
	DecidedAt   int64      `json:"decidedAt,omitempty"`
	ExpiresAt   int64      `json:"expiresAt"`
}

// Settled reports whether the request has stopped being able to change.
func (s Spend) Settled() bool { return s.State != SpendPending }

// RequestSpend asks the host to pay an address, and returns as soon as the
// request is recorded - not when it is paid.
//
// The reason travels because a person is going to read it. "Fund a bond" and
// "buy into a table" are the same amount to a cap and entirely different things
// to somebody deciding.
func (b *Bridge) RequestSpend(ctx context.Context, address string, amountAtoms int64, reason string) (Spend, error) {
	body, err := json.Marshal(map[string]any{
		"address":     address,
		"amountAtoms": amountAtoms,
		"reason":      reason,
	})
	if err != nil {
		return Spend{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/spend", bytes.NewReader(body))
	if err != nil {
		return Spend{}, fmt.Errorf("build spend request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.Token)

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return Spend{}, fmt.Errorf("ask to spend: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		// Policy. The host says why, and the reason is the useful part:
		// a cap that will never be got under reads differently from one
		// that resets tomorrow.
		return Spend{}, fmt.Errorf("host refused to spend: %s", readError(resp))
	case http.StatusNotFound:
		return Spend{}, fmt.Errorf("host does not recognise this game's token; check it is still installed")
	default:
		return Spend{}, fmt.Errorf("ask to spend: host returned %s: %s", resp.Status, readError(resp))
	}

	var out Spend
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Spend{}, fmt.Errorf("read the host's answer: %w", err)
	}
	return out, nil
}

// SpendStatus reports what became of a request.
func (b *Bridge) SpendStatus(ctx context.Context, id string) (Spend, error) {
	var out Spend
	err := b.getJSON(ctx, "/spend/status", url.Values{"id": {id}}, &out)
	return out, err
}

// spendPoll is how often the host is asked whether somebody has answered. A
// person is being waited on, so this is patient by design.
const spendPoll = 3 * time.Second

// AwaitSpend waits for a person to decide, and reports what they decided.
//
// The wait ends by itself: the host expires a request nobody answered, so this
// cannot hang on somebody who has gone to bed. A refusal and a silence arrive
// as different states, though a game has no reason to treat them differently -
// neither of them is money.
func (b *Bridge) AwaitSpend(ctx context.Context, id string) (Spend, error) {
	ticker := time.NewTicker(spendPoll)
	defer ticker.Stop()

	for {
		spend, err := b.SpendStatus(ctx, id)
		if err != nil {
			return Spend{}, err
		}
		if spend.Settled() {
			return spend, nil
		}
		select {
		case <-ctx.Done():
			return Spend{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
