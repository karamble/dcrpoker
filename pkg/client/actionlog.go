package client

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
)

// actionSigner is what this client signs its actions with at one table.
//
// The key is the session key the table's escrow is bound to, not a separate
// identity. That is the point: a signature on an action then comes from the
// same key that owns the stake, so the record of who acted and the record of
// whose money is at risk cannot come apart.
type actionSigner struct {
	seat uint32
	priv *secp256k1.PrivateKey
}

// SetActionSigner tells the client which seat it holds and which session key to
// sign its actions with. privHex is the same key used for the table's escrow -
// see GenerateSessionKey and DeriveSessionKeyAt.
func (pc *PokerClient) SetActionSigner(seat uint32, privHex string) error {
	raw, err := hex.DecodeString(strings.TrimSpace(privHex))
	if err != nil || len(raw) == 0 {
		return fmt.Errorf("bad session key")
	}
	pc.Lock()
	pc.signer = &actionSigner{seat: seat, priv: secp256k1.PrivKeyFromBytes(raw)}
	pc.Unlock()
	return nil
}

// ClearActionSigner forgets the signing key, for leaving a table.
func (pc *PokerClient) ClearActionSigner() {
	pc.Lock()
	pc.signer = nil
	pc.Unlock()
}

// signAction builds the signed log entry that accompanies an action.
//
// It returns nil, nil when there is nothing to sign against - no key set, or a
// table the server keeps no log for. Silence is right there: a table playing
// for nothing has no escrow roster and therefore no keys to check signatures
// with, and refusing to play on one would be a strange way to protect a pot
// that does not exist.
func (pc *PokerClient) signAction(action gamelog.Action, amount int64) (*pokerrpc.SignedAction, error) {
	pc.RLock()
	signer := pc.signer
	pc.RUnlock()
	if signer == nil {
		return nil, nil
	}

	// Where the chain stands comes from the last game update. Every entry
	// chains to the one before, so a client that has not heard from the
	// table yet has nothing to chain to.
	last := pc.lastKnownGameUpdate()
	if last == nil || len(last.GetLogHead()) != 32 {
		return nil, nil
	}

	e := &gamelog.Entry{
		Version: gamelog.Version,
		Seq:     last.GetLogSeq() + 1,
		Hand:    last.GetLogHand(),
		Street:  gamelog.Street(last.GetLogStreet()),
		Seat:    signer.seat,
		Action:  action,
		Amount:  amount,
	}
	copy(e.PrevHash[:], last.GetLogHead())

	if err := e.Sign(signer.priv); err != nil {
		return nil, fmt.Errorf("sign %s: %w", action, err)
	}
	return actionToProto(e), nil
}

func actionToProto(e *gamelog.Entry) *pokerrpc.SignedAction {
	return &pokerrpc.SignedAction{
		Version:  uint32(e.Version),
		PrevHash: e.PrevHash[:],
		Seq:      e.Seq,
		Hand:     e.Hand,
		Street:   uint32(e.Street),
		Seat:     e.Seat,
		Signer:   e.Signer,
		Action:   string(e.Action),
		Amount:   e.Amount,
		Sig:      e.Sig,
	}
}
