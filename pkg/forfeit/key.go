package forfeit

import (
	"fmt"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// LogKey is a per-match key whose only job is signing the log.
//
// Separate from the session key on purpose, and the separation is the whole
// safety argument. The session key is named in the escrow script and holds the
// stake; if it were the key that leaked, an equivocating player would not be
// forfeiting a bond to the player they cheated - they would be publishing the
// key to their own stake, for anyone at all to take first. The penalty has to
// be bounded and it has to be directed at the person wronged.
//
// One per match, and never reused across matches: the key is expected to become
// public the moment its owner misbehaves, and a key reused at another table
// would take that table down with it.
type LogKey struct {
	priv  *secp256k1.PrivateKey
	match string

	// book is what this key has signed and where, so a second, different
	// message at one position is refused instead of publishing the key. That
	// mistake is reachable by accident: a table whose hand counter starts
	// over signs hand one again with a different deck.
	mu   sync.Mutex
	book Book
}

// Book remembers what a key signed at each position. One that does not outlive
// the process is a key with no memory of what it has already put its name to.
//
// A Record that fails means the position was not remembered, and a signature
// over an unremembered position is exactly what the book exists to prevent - so
// the failure refuses the signature rather than being noted somewhere.
type Book interface {
	Used(p Position) ([32]byte, bool)
	Record(p Position, digest [32]byte) error
}

// memoryBook catches the mistake within one process, and no further.
type memoryBook struct{ at map[Position][32]byte }

func (b *memoryBook) Used(p Position) ([32]byte, bool) {
	d, ok := b.at[p]
	return d, ok
}

func (b *memoryBook) Record(p Position, digest [32]byte) error {
	if b.at == nil {
		b.at = map[Position][32]byte{}
	}
	b.at[p] = digest
	return nil
}

// Remember gives this key a book that outlives the process.
func (k *LogKey) Remember(b Book) {
	if b == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.book = b
}

// NewLogKey draws a log key for one match.
func NewLogKey(match string) (*LogKey, error) {
	if match == "" {
		return nil, fmt.Errorf("a log key needs a match to belong to")
	}
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &LogKey{priv: priv, match: match}, nil
}

// LogKeyFrom wraps an existing private key, for restoring one from storage.
func LogKeyFrom(priv *secp256k1.PrivateKey, match string) (*LogKey, error) {
	if priv == nil {
		return nil, fmt.Errorf("no key")
	}
	if match == "" {
		return nil, fmt.Errorf("a log key needs a match to belong to")
	}
	return &LogKey{priv: priv, match: match}, nil
}

// Public is the key others verify against and build the forfeit key from.
func (k *LogKey) Public() *secp256k1.PublicKey { return k.priv.PubKey() }

// Match is the match this key belongs to.
func (k *LogKey) Match() string { return k.match }

// Sign signs one message at one position in this match's log.
//
// Calling it twice at one position with different messages publishes the key.
// That is not a hazard to be guarded against here - it is what the key is for -
// but it does mean a caller must be sure that a position means one message. The
// sequence number comes from the chain, which enforces exactly that.
func (k *LogKey) Sign(d Domain, seq uint64, hash []byte) ([]byte, error) {
	p := Position{Match: k.match, Domain: d, Seq: seq}
	if len(hash) != 32 {
		return nil, fmt.Errorf("message digest is %d bytes, want 32", len(hash))
	}
	var digest [32]byte
	copy(digest[:], hash)

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.book == nil {
		k.book = &memoryBook{}
	}
	if was, ok := k.book.Used(p); ok && was != digest {
		return nil, fmt.Errorf("%s/%d was already signed over a different message; "+
			"signing it again would publish this key", d, seq)
	}
	// Recorded before it is signed, never after. A failure between the two
	// leaves a position remembered and nothing signed, which costs nothing:
	// the same digest may be signed again. The other order can return a
	// signature the book never heard of.
	if err := k.book.Record(p, digest); err != nil {
		return nil, fmt.Errorf("%s/%d cannot be recorded, so it will not be signed: %w", d, seq, err)
	}
	return Sign(k.priv, p, hash)
}

// SignCommitted signs content its position does not determine. Two different
// messages at one position are two signatures and no disclosure.
func (k *LogKey) SignCommitted(d Domain, seq uint64, hash []byte) ([]byte, error) {
	return SignCommitted(k.priv, Position{Match: k.match, Domain: d, Seq: seq}, hash)
}

// ForfeitKey is the public key a bond's punishment branch pays to.
//
//	F = L + P
//
// where L is the cheat's log key and P is the wronged player's own punishment
// key. Neither side can spend it alone, which is the point in both directions:
//
//   - The bond's owner knows the secret behind L and can compute nothing
//     without the secret behind P, so they cannot take their own bond back
//     early through the punishment branch.
//   - The other player knows the secret behind P and cannot compute anything
//     without the secret behind L, so they cannot take a bond from somebody who
//     has not cheated. There is no accusation to make and nothing to grief.
//
// The moment the owner equivocates, L is public arithmetic and the other player
// holds both halves. This is Lightning's revocation trick, doing here what it
// does there: turning "prove they cheated" into "they handed you the key".
//
// One branch per opponent, because the punishment must be directed. A branch
// keyed to L alone would be spendable by any bystander who noticed.
func ForfeitKey(logPub, punisherPub *secp256k1.PublicKey) (*secp256k1.PublicKey, error) {
	if logPub == nil || punisherPub == nil {
		return nil, fmt.Errorf("a forfeit key needs both halves")
	}
	if logPub.IsEqual(punisherPub) {
		return nil, fmt.Errorf("a forfeit key cannot be built from one key twice")
	}
	var a, b, sum secp256k1.JacobianPoint
	logPub.AsJacobian(&a)
	punisherPub.AsJacobian(&b)
	secp256k1.AddNonConst(&a, &b, &sum)
	sum.ToAffine()
	if (sum.X.IsZero() && sum.Y.IsZero()) || sum.Z.IsZero() {
		return nil, fmt.Errorf("these keys sum to the point at infinity")
	}
	return secp256k1.NewPublicKey(&sum.X, &sum.Y), nil
}

// ForfeitPrivKey is the spending key for a punishment branch, once the cheat's
// log key has been recovered from their own equivocation.
func ForfeitPrivKey(recovered, punisher *secp256k1.PrivateKey) (*secp256k1.PrivateKey, error) {
	if recovered == nil || punisher == nil {
		return nil, fmt.Errorf("a forfeit key needs both halves")
	}
	d := new(secp256k1.ModNScalar).Set(&recovered.Key).Add(&punisher.Key)
	if d.IsZero() {
		return nil, fmt.Errorf("these keys sum to zero")
	}
	return secp256k1.NewPrivateKey(d), nil
}

// PunishmentKey draws a fresh key for punishing one opponent at one match.
//
// One per opponent per match. Reuse across opponents would let one cheat's
// leaked key be combined with a punishment key another branch also names, which
// is not exploitable today but is the kind of sharing that becomes one later.
func PunishmentKey() (*secp256k1.PrivateKey, error) {
	return secp256k1.GeneratePrivateKey()
}
