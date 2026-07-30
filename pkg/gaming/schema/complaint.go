package schema

import (
	"encoding/hex"
	"fmt"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
)

// A shuffle dispute, self-contained.
//
// A complaint says: this signed shuffle did not verify against this input
// deck. It carries the refused frame whole - deck, proof and the shuffler's
// signature - and the input the complainer verified against, which together
// are everything a verdict needs: check the input against the complainer's own
// signed upstream, then re-run the shuffle proof. Proof verifies, the
// complaint was false; proof fails, the shuffler signed an invalid frame.
// Nothing secret ever moves and nothing answers a complaint, because there is
// nothing left to ask anybody.

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

// ComplaintView is a stored dispute: the evidence a verdict is recomputed
// from, kept by the complaint machinery itself because a wedged hand never
// produced a hand record. Round-trips exactly, like HandRecordView, because it
// judges after a restart.
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
	Verdict   string           `json:"verdict,omitempty"`
}
// Exported encoding helpers, for the plugin's stored dispute state: the view
// is the single at-rest form, and the plugin decodes it with the same readers
// the wire uses rather than growing a second copy of them.

// B64 renders bytes the way every proof field here is rendered.
func B64(b []byte) string { return b64(b) }

// UnB64 reads them back.
func UnB64(s, what string) ([]byte, error) { return unb64(s, what) }

// DeckBytes lays a deck out for storage.
func DeckBytes(d deck.Deck) ([]byte, error) { return deckBytes(d) }

// ReadDeck reads one back.
func ReadDeck(b []byte) (deck.Deck, error) { return readDeck(b) }
