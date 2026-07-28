package forfeit

import (
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
)

// The mechanism has exactly two failure modes and they point opposite ways.
//
// If it leaks when it should not, an honest player loses their bond for playing
// correctly, which is worse than having no bond at all. If it fails to leak
// when it should, the whole thing is decoration. So the tests come in pairs:
// every "this leaks" has a "this does not", and the ones that must not leak are
// the ones worth writing first.

const match = "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9"

func key(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k
}

func digest(s string) []byte {
	h := blake256.Sum256([]byte(s))
	return h[:]
}

func at(seq uint64) Position {
	return Position{Match: match, Domain: DomainEntry, Seq: seq}
}

// A signature made this way is an ordinary Decred Schnorr signature. If this
// fails, nothing downstream can verify a log at all.
func TestASignatureVerifiesWithTheStockVerifier(t *testing.T) {
	k := key(t)
	m := digest("seat 1 folds")

	sig, err := Sign(k, at(4), m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if len(sig) != SigLen {
		t.Fatalf("signature is %d bytes, want %d", len(sig), SigLen)
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Verify(m, k.PubKey()) {
		t.Fatal("a signature made with a positional nonce did not verify")
	}
}

// The whole point: two entries at one position hand over the key.
func TestEquivocationPublishesTheKey(t *testing.T) {
	k := key(t)
	folded := digest("seat 1 folds")
	raised := digest("seat 1 raises 500")

	a, err := Sign(k, at(4), folded)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	b, err := Sign(k, at(4), raised)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Both are individually valid, which is what makes the cheat work at
	// all - each recipient sees a perfectly good signature.
	for _, tc := range []struct {
		sig  []byte
		hash []byte
	}{{a, folded}, {b, raised}} {
		p, err := schnorr.ParseSignature(tc.sig)
		if err != nil || !p.Verify(tc.hash, k.PubKey()) {
			t.Fatal("an equivocating signature was not individually valid")
		}
	}

	got, err := Recover(k.PubKey(), folded, a, raised, b)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.Key.Equals(&k.Key) {
		t.Fatal("recovered a key that is not the one that signed")
	}
}

// Playing correctly must never leak. These are the tests that protect the
// honest player, and they matter more than the one above.
func TestPlayingHonestlyDoesNotLeak(t *testing.T) {
	k := key(t)

	t.Run("different positions", func(t *testing.T) {
		a, _ := Sign(k, at(4), digest("folds"))
		b, _ := Sign(k, at(5), digest("raises"))
		if string(a[:32]) == string(b[:32]) {
			t.Fatal("two different sequence numbers produced the same nonce")
		}
		if _, err := Recover(k.PubKey(), digest("folds"), a, digest("raises"), b); err == nil {
			t.Fatal("signatures at different positions exposed the key")
		}
	})

	// The trap this design is most likely to fall into: a seat signs a log
	// entry at seq 5 and a head attestation at seq 5, both legitimately.
	// Without domain separation those share a nonce and the seat publishes
	// its own key by behaving perfectly.
	t.Run("an entry and a head attestation at one sequence", func(t *testing.T) {
		entry := Position{Match: match, Domain: DomainEntry, Seq: 5}
		head := Position{Match: match, Domain: DomainHead, Seq: 5}

		a, _ := Sign(k, entry, digest("seat 1 calls"))
		b, _ := Sign(k, head, digest("head is abcdef"))
		if string(a[:32]) == string(b[:32]) {
			t.Fatal("an entry and a head attestation at one sequence shared a nonce")
		}
		if _, err := Recover(k.PubKey(), digest("seat 1 calls"), a, digest("head is abcdef"), b); err == nil {
			t.Fatal("signing an entry and a head attestation at one sequence exposed the key")
		}
	})

	t.Run("the same message twice", func(t *testing.T) {
		m := digest("seat 1 folds")
		a, _ := Sign(k, at(4), m)
		b, _ := Sign(k, at(4), m)
		if string(a) != string(b) {
			t.Fatal("signing the same thing twice was not deterministic")
		}
		if _, err := Recover(k.PubKey(), m, a, m, b); err == nil {
			t.Fatal("resending an identical signature exposed the key")
		}
	})

	t.Run("the same position in different matches", func(t *testing.T) {
		here := Position{Match: match, Domain: DomainEntry, Seq: 4}
		there := Position{Match: "0000", Domain: DomainEntry, Seq: 4}
		a, _ := Sign(k, here, digest("folds"))
		b, _ := Sign(k, there, digest("raises"))
		if string(a[:32]) == string(b[:32]) {
			t.Fatal("the same position at two matches shared a nonce")
		}
	})

	t.Run("a whole honest hand", func(t *testing.T) {
		sigs := make([][]byte, 0, 40)
		hashes := make([][]byte, 0, 40)
		for seq := uint64(0); seq < 20; seq++ {
			for _, d := range []Domain{DomainEntry, DomainHead} {
				m := digest(string(d) + string(rune('a'+seq)))
				s, err := Sign(k, Position{Match: match, Domain: d, Seq: seq}, m)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				sigs = append(sigs, s)
				hashes = append(hashes, m)
			}
		}
		for i := range sigs {
			for j := i + 1; j < len(sigs); j++ {
				if string(sigs[i][:32]) == string(sigs[j][:32]) {
					t.Fatalf("signatures %d and %d reused a nonce during an honest hand", i, j)
				}
				if _, err := Recover(k.PubKey(), hashes[i], sigs[i], hashes[j], sigs[j]); err == nil {
					t.Fatalf("an honest hand exposed the key via signatures %d and %d", i, j)
				}
			}
		}
	})
}

// Different keys at one position must not be confusable either.
func TestAnotherPlayersSignatureDoesNotLeakAnything(t *testing.T) {
	a, b := key(t), key(t)
	ma, mb := digest("a folds"), digest("b raises")

	sa, _ := Sign(a, at(4), ma)
	sb, _ := Sign(b, at(4), mb)

	if string(sa[:32]) == string(sb[:32]) {
		t.Fatal("two different keys produced the same nonce at one position")
	}
	if _, err := Recover(a.PubKey(), ma, sa, mb, sb); err == nil {
		t.Fatal("two players' signatures at one position exposed a key")
	}
}

// Recover must refuse anything it cannot actually solve, rather than returning
// a plausible scalar.
func TestRecoverRefusesWhatItCannotSolve(t *testing.T) {
	k := key(t)
	m := digest("folds")
	sig, _ := Sign(k, at(4), m)

	for _, tc := range []struct {
		name        string
		pub         *secp256k1.PublicKey
		hashA, sigA []byte
		hashB, sigB []byte
	}{
		{"no key", nil, m, sig, m, sig},
		{"a short signature", k.PubKey(), m, sig[:63], m, sig},
		{"a short digest", k.PubKey(), m[:31], sig, m, sig},
		{"the wrong public key", key(t).PubKey(), m, sig, digest("raises"), mustSign(t, k, at(4), digest("raises"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Recover(tc.pub, tc.hashA, tc.sigA, tc.hashB, tc.sigB); err == nil {
				t.Fatal("Recover accepted something it could not have solved")
			}
		})
	}
}

func mustSign(t *testing.T, k *secp256k1.PrivateKey, p Position, m []byte) []byte {
	t.Helper()
	s, err := Sign(k, p, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// A position with no domain or no match is a programming error, not a
// signature - the separation is what keeps honest play safe.
func TestSigningRefusesAnUnqualifiedPosition(t *testing.T) {
	k := key(t)
	for _, p := range []Position{
		{Match: match, Seq: 1},
		{Domain: DomainEntry, Seq: 1},
		{},
	} {
		if _, err := Sign(k, p, digest("x")); err == nil {
			t.Fatalf("signed at an unqualified position %+v", p)
		}
	}
}

// The forfeit key: neither side can spend alone, both together can.
func TestAForfeitKeyNeedsBothHalves(t *testing.T) {
	log, punisher := key(t), key(t)

	forfeit, err := ForfeitKey(log.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}

	// Neither half alone is the forfeit key, so neither side can sign for it.
	if forfeit.IsEqual(log.PubKey()) || forfeit.IsEqual(punisher.PubKey()) {
		t.Fatal("the forfeit key is one of its halves")
	}

	// Together they are.
	both, err := ForfeitPrivKey(log, punisher)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if !both.PubKey().IsEqual(forfeit) {
		t.Fatal("the two halves did not reconstruct the forfeit key")
	}

	// And it really does sign for it.
	m := digest("take the bond")
	sig, err := schnorr.Sign(both, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !sig.Verify(m, forfeit) {
		t.Fatal("the reconstructed key does not sign for the forfeit key")
	}
}

// End to end: a player equivocates, the other one ends up holding the key to
// the punishment branch, and nobody else does.
func TestEquivocationHandsTheBondToTheWrongedPlayer(t *testing.T) {
	cheat, err := NewLogKey(match)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	punisher, err := PunishmentKey()
	if err != nil {
		t.Fatalf("punishment key: %v", err)
	}

	forfeit, err := ForfeitKey(cheat.Public(), punisher.PubKey())
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}

	// Before any cheating, the wronged player cannot spend the branch: they
	// have one half and no way to the other.
	if _, err := Recover(cheat.Public(), digest("a"), make([]byte, 64), digest("b"), make([]byte, 64)); err == nil {
		t.Fatal("a key was recovered from nothing")
	}

	// The cheat tells one player it folded and another that it raised.
	folded, raised := digest("seat 0 folds"), digest("seat 0 raises 2000")
	a, err := cheat.Sign(DomainEntry, 11, folded)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	b, err := cheat.Sign(DomainEntry, 11, raised)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Either recipient can now compute the cheat's log key...
	leaked, err := Recover(cheat.Public(), folded, a, raised, b)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	// ...but only the player holding the punishment key can spend the bond.
	spend, err := ForfeitPrivKey(leaked, punisher)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if !spend.PubKey().IsEqual(forfeit) {
		t.Fatal("the wronged player cannot spend the punishment branch")
	}

	// A bystander who saw the same equivocation holds the leaked key and
	// their own key, and that combination is not the branch.
	bystander, err := PunishmentKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	theirs, err := ForfeitPrivKey(leaked, bystander)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if theirs.PubKey().IsEqual(forfeit) {
		t.Fatal("a bystander could spend the punishment branch")
	}
}

// A log key has to be tied to the money, or a forfeited bond belongs to nobody.
func TestALogKeyIsBoundToTheSessionThatHoldsTheStake(t *testing.T) {
	session := key(t)
	log, err := NewLogKey(match)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}

	sig, err := Bind(session, match, log.Public())
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := VerifyBinding(session.PubKey(), log.Public(), match, sig); err != nil {
		t.Fatalf("an honest binding did not verify: %v", err)
	}

	t.Run("not at another table", func(t *testing.T) {
		if err := VerifyBinding(session.PubKey(), log.Public(), "0000", sig); err == nil {
			t.Fatal("a binding made at one table verified at another")
		}
	})
	t.Run("not for another log key", func(t *testing.T) {
		other, _ := NewLogKey(match)
		if err := VerifyBinding(session.PubKey(), other.Public(), match, sig); err == nil {
			t.Fatal("a binding covered a log key it was not made for")
		}
	})
	t.Run("not by another session", func(t *testing.T) {
		if err := VerifyBinding(key(t).PubKey(), log.Public(), match, sig); err == nil {
			t.Fatal("one session's binding verified as another's")
		}
	})
	// The mistake that would put the stake itself at risk of forfeiture.
	t.Run("and never the session key itself", func(t *testing.T) {
		if _, err := Bind(session, match, session.PubKey()); err == nil {
			t.Fatal("the session key was accepted as its own log key")
		}
		bad, _ := schnorr.Sign(session, func() []byte { d := bindDigest(match, session.PubKey()); return d[:] }())
		if err := VerifyBinding(session.PubKey(), session.PubKey(), match, bad.Serialize()); err == nil {
			t.Fatal("a binding of the session key to itself verified")
		}
	})
}
