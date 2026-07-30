package schema

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
)

// Dealing, on the wire.
//
// These are the only messages in the protocol big enough for their encoding to
// matter. A shuffle carries a whole masked deck and its proof - about 23KB of
// binary - and every seat sends one per hand, so hex would put 47KB on the wire
// where base64 puts 31KB. Everything smaller stays hex to match the rest of the
// schema; the two blobs that dominate do not.
//
// The framing already chunks at 200KB and admits messages far larger, so none of
// this needs splitting. It is worth knowing rather than assuming: a protocol
// whose largest message is a third of a chunk has room, and one that discovered
// otherwise at a live table would not.

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(s, what string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return b, nil
}

// pointHex renders a group element.
func pointHex(p kyber.Point) (string, error) {
	if p == nil {
		return "", fmt.Errorf("no point")
	}
	b, err := p.MarshalBinary()
	if err != nil {
		return "", err
	}
	return b64(b), nil
}

func readPoint(s, what string) (kyber.Point, error) {
	b, err := unb64(s, what)
	if err != nil {
		return nil, err
	}
	p := deck.Suite().Point()
	if err := p.UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return p, nil
}

// CardKey is one seat's deck key for one hand.
//
// A new one every hand, because a card key becomes public when its hand ends.
//
// The signature is the seat saying the key is its own, checked against the roster
// the escrow committed to. It is not there to catch equivocation - a seat handing
// different keys to different players gives them different joint keys, so the
// first shuffle fails everywhere and the hand never starts. It is there to catch
// pre-emption: a key announced for a seat that has not spoken yet would otherwise
// be believed, and since the joint key is the sum of all of them, the table would
// mask every card to a secret the seat it is attributed to does not hold.
type CardKey struct {
	Seat uint32 `json:"seat"`
	Hand uint64 `json:"hand"`
	Key  string `json:"key"`
	// Pop is the possession proof: the announcer knows the key's discrete
	// log, so the joint key is a sum of secrets somebody holds, one each.
	Pop string `json:"pop"`
	Sig string `json:"sig"`
}

// CardKeyFrom renders a key announcement.
func CardKeyFrom(m driver.OutCardKey) (CardKey, error) {
	k, err := pointHex(m.Key)
	if err != nil {
		return CardKey{}, fmt.Errorf("card key: %w", err)
	}
	return CardKey{Seat: uint32(m.Seat), Hand: m.Hand, Key: k,
		Pop: b64(m.Pop), Sig: hex.EncodeToString(m.Sig)}, nil
}

// Into reads a key announcement back.
func (c CardKey) Into() (driver.InCardKey, error) {
	p, err := readPoint(c.Key, "card key")
	if err != nil {
		return driver.InCardKey{}, err
	}
	// Refused empty here; its exact length is the driver's question, since
	// the honest size is measured there against a real proof.
	if c.Pop == "" {
		return driver.InCardKey{}, fmt.Errorf("card key carries no possession proof")
	}
	pop, err := unb64(c.Pop, "possession proof")
	if err != nil {
		return driver.InCardKey{}, err
	}
	sig, err := hex.DecodeString(c.Sig)
	if err != nil {
		return driver.InCardKey{}, fmt.Errorf("card key signature: %w", err)
	}
	return driver.InCardKey{Seat: int(c.Seat), Hand: c.Hand, Key: p, Pop: pop, Sig: sig}, nil
}

// Shuffle is one seat's permutation of the deck, and the proof it did nothing
// else.
type Shuffle struct {
	Seat  uint32 `json:"seat"`
	Hand  uint64 `json:"hand"`
	Deck  string `json:"deck"`
	Proof string `json:"proof"`
	// Sig is the seat saying the shuffle is its own. The proof cannot say
	// it - a Neff proof's witness is the permutation and the masking, so it
	// establishes that the deck was permuted honestly by somebody and never
	// by whom.
	Sig string `json:"sig"`
}

// deckBytes lays a masked deck out as C1,C2 pairs.
func deckBytes(d deck.Deck) ([]byte, error) {
	out := make([]byte, 0, len(d)*64)
	for i, m := range d {
		for _, p := range []kyber.Point{m.C1, m.C2} {
			if p == nil {
				return nil, fmt.Errorf("card %d is not masked", i)
			}
			b, err := p.MarshalBinary()
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
	}
	return out, nil
}

func readDeck(b []byte) (deck.Deck, error) {
	const point = 32
	if len(b) == 0 || len(b)%(2*point) != 0 {
		return nil, fmt.Errorf("a deck of %d bytes is not whole cards", len(b))
	}
	n := len(b) / (2 * point)
	if n != deck.Size {
		return nil, fmt.Errorf("a deck of %d cards, want %d", n, deck.Size)
	}
	out := make(deck.Deck, n)
	for i := range n {
		c1 := deck.Suite().Point()
		if err := c1.UnmarshalBinary(b[i*2*point : i*2*point+point]); err != nil {
			return nil, fmt.Errorf("card %d: %w", i, err)
		}
		c2 := deck.Suite().Point()
		if err := c2.UnmarshalBinary(b[i*2*point+point : (i+1)*2*point]); err != nil {
			return nil, fmt.Errorf("card %d: %w", i, err)
		}
		out[i] = deck.Masked{C1: c1, C2: c2}
	}
	return out, nil
}

// ShuffleFrom renders a shuffle.
func ShuffleFrom(m driver.OutShuffle, hand uint64) (Shuffle, error) {
	b, err := deckBytes(m.Deck)
	if err != nil {
		return Shuffle{}, err
	}
	return Shuffle{
		Seat:  uint32(m.Seat),
		Hand:  hand,
		Deck:  b64(b),
		Proof: b64(m.Proof),
		Sig:   hex.EncodeToString(m.Sig),
	}, nil
}

// Into reads a shuffle back.
//
// The proof is not checked here and must not be. Verifying needs the deck this
// one was shuffled *from*, which is state the reader holds and this message does
// not carry - so a check here could only ever be a weaker one that looked like
// the real thing.
func (s Shuffle) Into() (driver.InShuffle, error) {
	raw, err := unb64(s.Deck, "deck")
	if err != nil {
		return driver.InShuffle{}, err
	}
	d, err := readDeck(raw)
	if err != nil {
		return driver.InShuffle{}, err
	}
	prf, err := unb64(s.Proof, "shuffle proof")
	if err != nil {
		return driver.InShuffle{}, err
	}
	sig, err := hex.DecodeString(s.Sig)
	if err != nil {
		return driver.InShuffle{}, fmt.Errorf("shuffle signature: %w", err)
	}
	return driver.InShuffle{Seat: int(s.Seat), Deck: d, Proof: prf, Sig: sig}, nil
}

// Share is one seat's contribution to opening one card.
type Share struct {
	Seat  uint32 `json:"seat"`
	Hand  uint64 `json:"hand"`
	Slot  int    `json:"slot"`
	D     string `json:"d"`
	Proof string `json:"proof"`
}

// ShareFrom renders a decryption share.
func ShareFrom(m driver.OutShare, hand uint64) (Share, error) {
	if m.Share == nil {
		return Share{}, fmt.Errorf("no share")
	}
	d, err := pointHex(m.Share.D)
	if err != nil {
		return Share{}, fmt.Errorf("share: %w", err)
	}
	return Share{
		Seat:  uint32(m.Seat),
		Hand:  hand,
		Slot:  m.Slot,
		D:     d,
		Proof: b64(m.Share.Proof),
	}, nil
}

// Into reads a share back. Its proof is checked when it is added to an opening,
// against the card it claims to open - which is the only place the statement it
// proves is known.
func (s Share) Into() (driver.InShare, error) {
	d, err := readPoint(s.D, "share")
	if err != nil {
		return driver.InShare{}, err
	}
	prf, err := unb64(s.Proof, "share proof")
	if err != nil {
		return driver.InShare{}, err
	}
	return driver.InShare{
		Seat:  int(s.Seat),
		Slot:  s.Slot,
		Share: &deck.Share{D: d, Proof: prf},
	}, nil
}
