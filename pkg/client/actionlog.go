package client

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
)

// actionSigner is what this client signs its actions with at one table.
//
// The key is a per-match log key and emphatically *not* the session key the
// escrow is bound to. It used to be the session key, on the reasoning that the
// record of who acted and the record of whose money is at risk should not come
// apart - which is right, and is now done by binding the two with a signature
// instead of by using one key for both (forfeit.Bind).
//
// They had to be separated because of what the log key now is. Its signatures
// take their nonce from the position in the log rather than from the message,
// so signing two different things at one position publishes it: that is how
// equivocation is punished without anybody adjudicating anything. A key that
// both owns the stake and is published on misbehaviour would mean a cheat hands
// out the key to their own escrow, to whoever is watching, rather than
// forfeiting a bond to the player they lied to. The penalty has to be bounded
// and it has to be directed.
type actionSigner struct {
	seat  uint32
	key   *forfeit.LogKey
	match string
}

// SetActionSigner tells the client which seat it holds and which log key to
// sign its actions with.
//
// privHex must be the match's log key - see DeriveLogKey - and not a session
// key. There is no way to check that here, which is why DeriveLogKey exists and
// why nothing else should be passed.
func (pc *PokerClient) SetActionSigner(seat uint32, match, privHex string) error {
	raw, err := hex.DecodeString(strings.TrimSpace(privHex))
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("bad log key")
	}
	key, err := forfeit.LogKeyFrom(secp256k1.PrivKeyFromBytes(raw), strings.TrimSpace(match))
	if err != nil {
		return err
	}
	pc.Lock()
	pc.signer = &actionSigner{seat: seat, key: key, match: strings.TrimSpace(match)}
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

	if err := e.Sign(signer.key); err != nil {
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
