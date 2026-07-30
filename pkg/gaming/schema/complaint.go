package schema

import (
	"encoding/hex"
	"fmt"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
)

// A shuffle dispute and what answers it.
//
// A complaint says: this signed shuffle did not verify against this input
// deck. It carries the refused frame whole, so a peer that lost the original
// still reaches the same verdict, and the claimed input, which is the whole of
// the first question - if the complainer's input differs from what the
// upstream shuffles it accepted actually produce, the complainer is wrong and
// no secret need move. The answer is the shuffle secret alone: the card key is
// deliberately not part of a dispute, because heads-up the permutations
// already expose everything and at three or more seats a card key is strictly
// more than a dispute needs to cost.

// ShuffleComplaint is one seat disputing another's shuffle.
type ShuffleComplaint struct {
	// Seat is the complainer; Shuffler is whose shuffle is disputed; Round
	// is which shuffle of the hand.
	Seat     uint32 `json:"seat"`
	Shuffler uint32 `json:"shuffler"`
	Hand     uint64 `json:"hand"`
	Round    uint32 `json:"round"`
	// Input is the deck the complainer verified against.
	Input string `json:"input"`
	// RefusedDeck, RefusedProof and RefusedSig are the disputed frame,
	// whole and still carrying its shuffler's signature.
	RefusedDeck  string `json:"refusedDeck"`
	RefusedProof string `json:"refusedProof"`
	RefusedSig   string `json:"refusedSig"`
	// Sig is the complainer's, over the claimed input and the digest of the
	// refused frame - position-signed, so shifting the story publishes the
	// complainer's key.
	Sig string `json:"sig"`
}

// ShuffleComplaintFrom renders a complaint.
func ShuffleComplaintFrom(seat, shuffler uint32, hand uint64, round uint32,
	input, refused deck.Deck, refusedProof, refusedSig, sig []byte) (ShuffleComplaint, error) {
	in, err := deckBytes(input)
	if err != nil {
		return ShuffleComplaint{}, fmt.Errorf("claimed input: %w", err)
	}
	rd, err := deckBytes(refused)
	if err != nil {
		return ShuffleComplaint{}, fmt.Errorf("refused deck: %w", err)
	}
	return ShuffleComplaint{
		Seat: seat, Shuffler: shuffler, Hand: hand, Round: round,
		Input:        b64(in),
		RefusedDeck:  b64(rd),
		RefusedProof: b64(refusedProof),
		RefusedSig:   hex.EncodeToString(refusedSig),
		Sig:          hex.EncodeToString(sig),
	}, nil
}

// Into reads a complaint back. Whether either deck is the truth is the
// verdict's question, not the decoder's.
func (c ShuffleComplaint) Into() (input, refused deck.Deck, refusedProof, refusedSig, sig []byte, err error) {
	inRaw, err := unb64(c.Input, "claimed input")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if input, err = readDeck(inRaw); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("claimed input: %w", err)
	}
	rdRaw, err := unb64(c.RefusedDeck, "refused deck")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if refused, err = readDeck(rdRaw); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("refused deck: %w", err)
	}
	if refusedProof, err = unb64(c.RefusedProof, "refused proof"); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if len(refusedProof) == 0 {
		return nil, nil, nil, nil, nil, fmt.Errorf("the complaint carries no refused proof")
	}
	if refusedSig, err = hex.DecodeString(c.RefusedSig); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("refused signature: %w", err)
	}
	if sig, err = hex.DecodeString(c.Sig); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("complaint signature: %w", err)
	}
	return input, refused, refusedProof, refusedSig, sig, nil
}

// ShuffleAnswer is the disputed shuffler's shuffle secret: the permutation and
// the blinding factors, and deliberately not the card key.
type ShuffleAnswer struct {
	Seat  uint32 `json:"seat"`
	Hand  uint64 `json:"hand"`
	Round uint32 `json:"round"`
	Pi    []int  `json:"pi"`
	Beta  string `json:"beta"`
	Sig   string `json:"sig"`
}

// ShuffleAnswerFrom renders an answer.
func ShuffleAnswerFrom(seat uint32, hand uint64, round uint32, s *deck.ShuffleSecret, sig []byte) (ShuffleAnswer, error) {
	if s == nil {
		return ShuffleAnswer{}, fmt.Errorf("no shuffle secret to send")
	}
	if len(s.Pi) != deck.Size {
		return ShuffleAnswer{}, fmt.Errorf("a permutation of %d, want %d", len(s.Pi), deck.Size)
	}
	if len(s.Beta) != deck.Size {
		return ShuffleAnswer{}, fmt.Errorf("%d blinding factors, want %d", len(s.Beta), deck.Size)
	}
	beta := make([]byte, 0, deck.Size*scalarLen)
	for i, b := range s.Beta {
		bb, err := scalarBytes(b, fmt.Sprintf("blinding factor %d", i))
		if err != nil {
			return ShuffleAnswer{}, err
		}
		beta = append(beta, bb...)
	}
	return ShuffleAnswer{
		Seat: seat, Hand: hand, Round: round,
		Pi:   append([]int(nil), s.Pi...),
		Beta: b64(beta),
		Sig:  hex.EncodeToString(sig),
	}, nil
}

// Into reads an answer back, pinning every length.
func (a ShuffleAnswer) Into() (sec *deck.ShuffleSecret, sig []byte, err error) {
	if len(a.Pi) != deck.Size {
		return nil, nil, fmt.Errorf("a permutation of %d, want %d", len(a.Pi), deck.Size)
	}
	raw, err := unb64(a.Beta, "blinding factors")
	if err != nil {
		return nil, nil, err
	}
	if len(raw) != deck.Size*scalarLen {
		return nil, nil, fmt.Errorf("blinding factors are %d bytes, want %d",
			len(raw), deck.Size*scalarLen)
	}
	beta := make([]kyber.Scalar, deck.Size)
	for i := range beta {
		beta[i], err = readScalar(raw[i*scalarLen:(i+1)*scalarLen], fmt.Sprintf("blinding factor %d", i))
		if err != nil {
			return nil, nil, err
		}
	}
	sig, err = hex.DecodeString(a.Sig)
	if err != nil {
		return nil, nil, fmt.Errorf("answer signature: %w", err)
	}
	return &deck.ShuffleSecret{Pi: append([]int(nil), a.Pi...), Beta: beta}, sig, nil
}

// ComplaintView is a stored dispute: the evidence a verdict is recomputed
// from, kept by the complaint machinery itself because a wedged hand never
// produced a hand record. Round-trips exactly, like HandRecordView, because it
// answers and judges after a restart.
type ComplaintView struct {
	Match string `json:"match"`
	Hand  uint64 `json:"hand"`
	Round uint32 `json:"round"`
	By    uint32 `json:"by"`
	// Pubs are the hand's committed card keys, as this peer verified them -
	// what the joint key and Fresh deck are recomputed from.
	Pubs []string `json:"pubs"`
	// Steps are the shuffles this peer accepted, through the round before
	// the disputed one.
	Steps     []StepView       `json:"steps"`
	Complaint ShuffleComplaint `json:"complaint"`
	Answer    *ShuffleAnswer   `json:"answer,omitempty"`
	Verdict   string           `json:"verdict,omitempty"`
}