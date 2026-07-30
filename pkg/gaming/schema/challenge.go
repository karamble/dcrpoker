package schema

import (
	"encoding/hex"
	"fmt"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
)

// A challenge and what answers it.
//
// A challenge names a settled hand and demands it be recomputed from everything
// everybody knew. It carries nothing else, because there is nothing else to say
// - the position is the whole content, which is why it signs by position. The
// secrets that answer it were drawn fresh, so their signature commits to the
// bytes.

// Challenge is one seat demanding a settled hand be recomputed.
type Challenge struct {
	Seat uint32 `json:"seat"`
	Hand uint64 `json:"hand"`
	Sig  string `json:"sig"`
}

// ChallengeFrom renders a challenge.
func ChallengeFrom(seat uint32, hand uint64, sig []byte) Challenge {
	return Challenge{Seat: seat, Hand: hand, Sig: hex.EncodeToString(sig)}
}

// Into reads a challenge back.
func (c Challenge) Into() (seat uint32, hand uint64, sig []byte, err error) {
	sig, err = hex.DecodeString(c.Sig)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("challenge signature: %w", err)
	}
	return c.Seat, c.Hand, sig, nil
}

// Secrets is one seat giving a challenged hand's secrets away: the per-hand
// card key, the permutation, and the blinding factors. Worthless by now to
// anybody but an auditor, which is the point.
type Secrets struct {
	Seat uint32 `json:"seat"`
	Hand uint64 `json:"hand"`
	Key  string `json:"key"`
	Pi   []int  `json:"pi"`
	Beta string `json:"beta"`
	Sig  string `json:"sig"`
}

const scalarLen = 32

// scalarBytes renders one scalar.
func scalarBytes(s kyber.Scalar, what string) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("no %s", what)
	}
	b, err := s.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if len(b) != scalarLen {
		return nil, fmt.Errorf("%s is %d bytes, want %d", what, len(b), scalarLen)
	}
	return b, nil
}

func readScalar(b []byte, what string) (kyber.Scalar, error) {
	if len(b) != scalarLen {
		return nil, fmt.Errorf("%s is %d bytes, want %d", what, len(b), scalarLen)
	}
	s := deck.Suite().Scalar()
	if err := s.UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return s, nil
}

// SecretsFrom renders a seat's secrets for one hand.
func SecretsFrom(seat uint32, hand uint64, s *deck.Secrets, sig []byte) (Secrets, error) {
	if s == nil || s.Shuffle == nil {
		return Secrets{}, fmt.Errorf("no secrets to send")
	}
	kb, err := scalarBytes(s.Key, "card key")
	if err != nil {
		return Secrets{}, err
	}
	if len(s.Shuffle.Pi) != deck.Size {
		return Secrets{}, fmt.Errorf("a permutation of %d, want %d", len(s.Shuffle.Pi), deck.Size)
	}
	if len(s.Shuffle.Beta) != deck.Size {
		return Secrets{}, fmt.Errorf("%d blinding factors, want %d", len(s.Shuffle.Beta), deck.Size)
	}
	beta := make([]byte, 0, deck.Size*scalarLen)
	for i, b := range s.Shuffle.Beta {
		bb, err := scalarBytes(b, fmt.Sprintf("blinding factor %d", i))
		if err != nil {
			return Secrets{}, err
		}
		beta = append(beta, bb...)
	}
	return Secrets{
		Seat: seat,
		Hand: hand,
		Key:  b64(kb),
		Pi:   append([]int(nil), s.Shuffle.Pi...),
		Beta: b64(beta),
		Sig:  hex.EncodeToString(sig),
	}, nil
}

// Into reads a seat's secrets back, pinning every length. Whether they are the
// truth is the auditor's question, not the decoder's.
func (s Secrets) Into() (seat uint32, hand uint64, sec *deck.Secrets, sig []byte, err error) {
	kb, err := unb64(s.Key, "card key")
	if err != nil {
		return 0, 0, nil, nil, err
	}
	key, err := readScalar(kb, "card key")
	if err != nil {
		return 0, 0, nil, nil, err
	}
	if len(s.Pi) != deck.Size {
		return 0, 0, nil, nil, fmt.Errorf("a permutation of %d, want %d", len(s.Pi), deck.Size)
	}
	raw, err := unb64(s.Beta, "blinding factors")
	if err != nil {
		return 0, 0, nil, nil, err
	}
	if len(raw) != deck.Size*scalarLen {
		return 0, 0, nil, nil, fmt.Errorf("blinding factors are %d bytes, want %d",
			len(raw), deck.Size*scalarLen)
	}
	beta := make([]kyber.Scalar, deck.Size)
	for i := range beta {
		beta[i], err = readScalar(raw[i*scalarLen:(i+1)*scalarLen], fmt.Sprintf("blinding factor %d", i))
		if err != nil {
			return 0, 0, nil, nil, err
		}
	}
	sig, err = hex.DecodeString(s.Sig)
	if err != nil {
		return 0, 0, nil, nil, fmt.Errorf("secrets signature: %w", err)
	}
	return s.Seat, s.Hand, &deck.Secrets{
		Key:     key,
		Shuffle: &deck.ShuffleSecret{Pi: append([]int(nil), s.Pi...), Beta: beta},
	}, sig, nil
}

// HandRecordView is a stored hand bundle: the transcript as this peer verified
// it, its own secrets, and whatever other seats have revealed. This is the file
// that answers a challenge after a restart, so it has to round-trip exactly.
type HandRecordView struct {
	Match string `json:"match"`
	Hand  uint64 `json:"hand"`
	// Pubs are the committed card keys in shuffling order.
	Pubs []string `json:"pubs"`
	// Steps carry each seat's published deck and proof, in the same order.
	Steps []StepView `json:"steps"`
	// Shown is every card this peer read, with the shares that opened it.
	Shown []ShownView `json:"shown,omitempty"`
	// Own is this peer's secrets; Revealed is everybody else's, as received.
	Own      *Secrets           `json:"own,omitempty"`
	Revealed map[uint32]Secrets `json:"revealed,omitempty"`
}

// StepView is one shuffle at rest.
type StepView struct {
	Deck  string `json:"deck"`
	Proof string `json:"proof"`
}

// ShownView is one opened card at rest. Shares line up with the seats, empty
// for a seat whose share this peer never verified.
type ShownView struct {
	Slot   int      `json:"slot"`
	Card   int      `json:"card"`
	Shares []string `json:"shares,omitempty"`
}

// HandRecordFrom renders a finished hand for storage.
func HandRecordFrom(r *driver.HandRecord, ownSeat uint32, ownSig []byte) (*HandRecordView, error) {
	if r == nil || r.Hand == nil {
		return nil, fmt.Errorf("no hand record")
	}
	v := &HandRecordView{Match: r.Hand.Match, Hand: r.Hand.Hand}
	for i, p := range r.Hand.Pubs {
		ph, err := pointHex(p)
		if err != nil {
			return nil, fmt.Errorf("card key %d: %w", i, err)
		}
		v.Pubs = append(v.Pubs, ph)
	}
	for i, st := range r.Hand.Steps {
		db, err := deckBytes(st.Deck)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		v.Steps = append(v.Steps, StepView{Deck: b64(db), Proof: b64(st.Proof)})
	}
	for _, sh := range r.Hand.Shown {
		sv := ShownView{Slot: sh.Slot, Card: int(sh.Card)}
		for _, p := range sh.Shares {
			if p == nil {
				sv.Shares = append(sv.Shares, "")
				continue
			}
			ph, err := pointHex(p)
			if err != nil {
				return nil, fmt.Errorf("share at slot %d: %w", sh.Slot, err)
			}
			sv.Shares = append(sv.Shares, ph)
		}
		v.Shown = append(v.Shown, sv)
	}
	if r.Secrets != nil && r.Secrets.Shuffle != nil {
		own, err := SecretsFrom(ownSeat, r.Hand.Hand, r.Secrets, ownSig)
		if err != nil {
			return nil, fmt.Errorf("own secrets: %w", err)
		}
		v.Own = &own
	}
	return v, nil
}

// Into reads a stored hand back: the transcript with each step attributed to
// its seat's committed key, and this peer's own secrets.
func (v *HandRecordView) Into() (*deck.Hand, *deck.Secrets, error) {
	if v == nil {
		return nil, nil, fmt.Errorf("no stored hand")
	}
	h := &deck.Hand{Match: v.Match, Hand: v.Hand}
	for i, ph := range v.Pubs {
		p, err := readPoint(ph, fmt.Sprintf("card key %d", i))
		if err != nil {
			return nil, nil, err
		}
		h.Pubs = append(h.Pubs, p)
	}
	if len(v.Steps) != len(h.Pubs) {
		return nil, nil, fmt.Errorf("%d steps for %d players", len(v.Steps), len(h.Pubs))
	}
	for i, sv := range v.Steps {
		raw, err := unb64(sv.Deck, fmt.Sprintf("step %d deck", i))
		if err != nil {
			return nil, nil, err
		}
		d, err := readDeck(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("step %d: %w", i, err)
		}
		prf, err := unb64(sv.Proof, fmt.Sprintf("step %d proof", i))
		if err != nil {
			return nil, nil, err
		}
		h.Steps = append(h.Steps, deck.Step{By: h.Pubs[i], Deck: d, Proof: prf})
	}
	for _, sv := range v.Shown {
		sh := deck.Shown{Slot: sv.Slot, Card: deck.Card(sv.Card)}
		for i, ph := range sv.Shares {
			if ph == "" {
				sh.Shares = append(sh.Shares, nil)
				continue
			}
			p, err := readPoint(ph, fmt.Sprintf("share %d at slot %d", i, sv.Slot))
			if err != nil {
				return nil, nil, err
			}
			sh.Shares = append(sh.Shares, p)
		}
		h.Shown = append(h.Shown, sh)
	}
	if v.Own == nil {
		return h, nil, nil
	}
	_, _, own, _, err := v.Own.Into()
	if err != nil {
		return nil, nil, fmt.Errorf("own secrets: %w", err)
	}
	return h, own, nil
}
