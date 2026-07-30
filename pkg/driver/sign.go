package driver

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
)

// Who dealt this.
//
// A card key, a shuffle and a leaving each name a seat, and each is signed by
// that seat's log key so the name means something. The signature is checked
// against the roster the escrow committed to, which is the only seat-to-key
// mapping here that decides anything.
//
// A shuffle proof cannot do this job. Its witness is the permutation and the
// masking, with no secret key in it, so a valid proof establishes that a deck was
// permuted honestly by somebody and never by whom - anybody can produce one under
// anybody's card key. A card key needs the signature more urgently still: the
// joint key is the sum of every seat's, so a key accepted for the wrong seat masks
// every card at the table to a secret that seat does not hold. And a leaving needs
// it because once every seat is marked leaving the table settles, so an unsigned
// one lets anybody who can reach the table end somebody else's game.
//
// The position is the hand, because a seat does each of these exactly once in a
// hand. Signing one of them twice at one hand is therefore equivocation rather
// than anything an honest peer can do, and equivocation publishes the key - see
// pkg/forfeit. Saying the same frame again costs nothing: the nonce comes from the
// position rather than the message, so a repeat is byte-for-byte the signature
// sent the first time, which is what the repair discipline needs.

var (
	// v2: the digest gained the possession proof, so the signature covers
	// it - a Pop can be neither stripped from a signed announcement nor
	// swapped for another. A tag names what it hashes.
	cardKeyTag   = []byte("dcrpoker/deal/cardkey/v2")
	shuffleTag   = []byte("dcrpoker/deal/shuffle/v1")
	leavingTag   = []byte("dcrpoker/deal/leaving/v1")
	challengeTag = []byte("dcrpoker/deal/challenge/v1")
	secretsTag   = []byte("dcrpoker/deal/secrets/v1")
)

// field writes a length-prefixed field, so no two different frames can hash the
// same way by running their parts together.
func field(h io.Writer, b []byte) {
	_ = binary.Write(h, binary.BigEndian, uint32(len(b)))
	_, _ = h.Write(b)
}

func num(h io.Writer, v uint64) {
	_ = binary.Write(h, binary.BigEndian, v)
}

// deckDigestBytes lays a masked deck out for hashing.
//
// Taken from the points rather than from the encoded message: what is signed has
// to be what was meant, not how it happened to be written down, or a peer that
// encodes differently would produce a signature nobody could check.
func deckDigestBytes(d deck.Deck) ([]byte, error) {
	var buf bytes.Buffer
	for i, m := range d {
		for _, p := range []kyber.Point{m.C1, m.C2} {
			if p == nil {
				return nil, fmt.Errorf("card %d is not masked", i)
			}
			b, err := p.MarshalBinary()
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
	}
	return buf.Bytes(), nil
}

// cardKeyDigest is what a seat signs to say a deck key is its own - the key
// and its possession proof together, so neither can be replaced under the
// other's signature.
func cardKeyDigest(match string, hand uint64, seat int, key kyber.Point, pop []byte) ([32]byte, error) {
	if key == nil {
		return [32]byte{}, fmt.Errorf("no card key")
	}
	kb, err := key.MarshalBinary()
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	field(h, cardKeyTag)
	field(h, []byte(match))
	num(h, hand)
	num(h, uint64(seat))
	field(h, kb)
	field(h, pop)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// shuffleDigest is what a seat signs to say a shuffle is its own.
func shuffleDigest(match string, hand uint64, seat int, d deck.Deck, proof []byte) ([32]byte, error) {
	db, err := deckDigestBytes(d)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	field(h, shuffleTag)
	field(h, []byte(match))
	num(h, hand)
	num(h, uint64(seat))
	field(h, db)
	field(h, proof)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// leavingDigest is what a seat signs to say it is getting up.
func leavingDigest(match string, hand uint64, seat int) [32]byte {
	h := sha256.New()
	field(h, leavingTag)
	field(h, []byte(match))
	num(h, hand)
	num(h, uint64(seat))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ChallengeDigest is what a seat signs to demand a hand be recomputed.
//
// Exported, with SecretsDigest and VerifySeatSig below, because challenges are
// made and answered by the plugin - most often for a table whose driver no
// longer exists.
func ChallengeDigest(match string, hand uint64, seat int) [32]byte {
	h := sha256.New()
	field(h, challengeTag)
	field(h, []byte(match))
	num(h, hand)
	num(h, uint64(seat))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// SecretsDigest is what a seat signs to give a challenged hand's secrets away.
// From the decoded values, never the encoding.
func SecretsDigest(match string, hand uint64, seat int, s *deck.Secrets) ([32]byte, error) {
	if s == nil || s.Key == nil || s.Shuffle == nil {
		return [32]byte{}, fmt.Errorf("nothing to sign: the secrets are incomplete")
	}
	kb, err := s.Key.MarshalBinary()
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	field(h, secretsTag)
	field(h, []byte(match))
	num(h, hand)
	num(h, uint64(seat))
	field(h, kb)
	var pi bytes.Buffer
	for _, j := range s.Shuffle.Pi {
		_ = binary.Write(&pi, binary.BigEndian, uint32(j))
	}
	field(h, pi.Bytes())
	var beta bytes.Buffer
	for i, b := range s.Shuffle.Beta {
		bb, err := b.MarshalBinary()
		if err != nil {
			return [32]byte{}, fmt.Errorf("blinding factor %d: %w", i, err)
		}
		field(&beta, bb)
	}
	field(h, beta.Bytes())
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// VerifySeatSig checks a signature against the roster key a seat committed,
// which the caller looks up from the escrow roster and never from the message.
func VerifySeatSig(rosterKey []byte, digest [32]byte, sig []byte, seat int) error {
	return verifyDealt(rosterKey, digest, sig, seat)
}

// verifyDealt checks a dealing signature against the roster key for the seat
// that the frame claims to be from.
//
// The key comes from the seat, never from the message. A frame that named its
// own signer would be checking a claim against itself; the only key worth
// checking against is the one the escrow agreed sits in that chair.
func verifyDealt(want []byte, digest [32]byte, sig []byte, seat int) error {
	if len(sig) == 0 {
		return fmt.Errorf("seat %d's frame is unsigned", seat)
	}
	if len(sig) != forfeit.SigLen {
		return fmt.Errorf("seat %d's signature is %d bytes, want %d",
			seat, len(sig), forfeit.SigLen)
	}
	pub, err := secp256k1.ParsePubKey(want)
	if err != nil {
		return fmt.Errorf("seat %d's roster key: %w", seat, err)
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		return fmt.Errorf("seat %d's signature: %w", seat, err)
	}
	if !parsed.Verify(digest[:], pub) {
		return fmt.Errorf("seat %d did not sign this", seat)
	}
	return nil
}

// checkDealt verifies a dealing frame against the roster the escrow committed
// to, which the chain carries.
//
// The digest is built lazily so a frame that cannot even be encoded fails as a
// bad frame rather than as a bad signature.
func (d *Driver) checkDealt(seat int, sig []byte, digest func() ([32]byte, error)) error {
	want, ok := d.cfg.Chain.Signer(uint32(seat))
	if !ok {
		return fmt.Errorf("seat %d is not on this table's roster", seat)
	}
	sum, err := digest()
	if err != nil {
		return err
	}
	return verifyDealt(want, sum, sig, seat)
}

// checkDealt is the same check before a hand is running, where the table holds
// the roster itself.
func (t *Table) checkDealt(seat int, sig []byte, digest func() ([32]byte, error)) error {
	want, ok := t.cfg.Roster[uint32(seat)]
	if !ok {
		return fmt.Errorf("seat %d is not on this table's roster", seat)
	}
	sum, err := digest()
	if err != nil {
		return err
	}
	return verifyDealt(want, sum, sig, seat)
}
