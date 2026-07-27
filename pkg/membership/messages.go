package membership

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
)

// SigLen is the length of a consensus Schnorr signature. Nothing here is
// spending an output, so there is no trailing hash-type byte.
const SigLen = 64

// Domain separation, so a signature made for one purpose can never be read as
// another. Without it a join could be replayed as a commit, or either replayed
// at a different table by the same key.
var (
	termsTag  = []byte("gaming/table/terms/v1")
	joinTag   = []byte("gaming/table/join/v1")
	rosterTag = []byte("gaming/table/roster/v1")
	assertTag = []byte("gaming/table/assert/v1")
	commitTag = []byte("gaming/table/commit/v1")
)

// Terms are what an invitation states and every join commits to.
//
// They are signed rather than merely agreed because an invitation is ordinary
// chat text, so whoever forwards it can hand one player one buy-in and another
// a different one. Winner-take-all only divides a pot fairly across equal
// stakes, so two players who read different invitations must fail to form a
// table rather than form one and discover it at settlement.
type Terms struct {
	Game       string
	GameVer    int
	SID        string
	BuyInAtoms uint64
	Seats      uint32
	// CSVBlocks is the relative timelock on every member's refund branch.
	// It is in the terms because each member builds their own branch, so a
	// member whose refund matured in one block could pull their stake
	// mid-hand; everyone has to be able to check everyone else's.
	CSVBlocks uint32
	// Until is the block height after which no more joins are admitted.
	//
	// It is a height rather than a time because it has to be a fact every
	// peer can check and nobody can be shown to have read wrong. Clocks
	// disagree, and a deadline each machine reads differently is one that
	// decides membership by whose clock ran fast.
	Until uint32
}

// Validate reports whether the terms could describe a table at all.
//
// The session id is checked only for being present. What shape it must take is
// the wire's business, and an invitation that carries a bad one should be
// refused when it is read rather than here.
func (t Terms) Validate() error {
	if strings.TrimSpace(t.Game) == "" {
		return fmt.Errorf("terms name no game")
	}
	if strings.TrimSpace(t.SID) == "" {
		return fmt.Errorf("terms name no session")
	}
	if t.Seats < 2 || int(t.Seats) > escrow.MaxMembers {
		return fmt.Errorf("a table of %d seats is outside 2..%d", t.Seats, escrow.MaxMembers)
	}
	if t.CSVBlocks == 0 {
		return fmt.Errorf("terms state no refund timelock")
	}
	if t.Until == 0 {
		// Without a deadline there is no moment at which "no more joins
		// are coming" becomes a fact, and a table can only ever be
		// formed by guessing.
		return fmt.Errorf("terms state no admission deadline")
	}
	return nil
}

// Hash is the digest joins and commits bind to.
func (t Terms) Hash() ([32]byte, error) {
	if err := t.Validate(); err != nil {
		return [32]byte{}, err
	}
	var b bytes.Buffer
	b.Write(termsTag)
	writeString(&b, t.Game)
	_ = binary.Write(&b, binary.BigEndian, int64(t.GameVer))
	writeString(&b, t.SID)
	_ = binary.Write(&b, binary.BigEndian, t.BuyInAtoms)
	_ = binary.Write(&b, binary.BigEndian, t.Seats)
	_ = binary.Write(&b, binary.BigEndian, t.CSVBlocks)
	_ = binary.Write(&b, binary.BigEndian, t.Until)
	return blake256.Sum256(b.Bytes()), nil
}

// Join is one player's claim to a seat, signed by the key it announces.
//
// The signature is what lets a join be relayed. A peer that missed one learns
// it from anyone who did, and can check it rather than believe them - which is
// the difference between healing a lossy channel and letting any member inject
// whatever keys they like.
//
// It carries no identity. The sender's Bison Relay uid is the authenticated
// identity, and a relayed join arrives from a third party, so a uid inside it
// would be an unverifiable claim.
type Join struct {
	Key  []byte // compressed session pubkey
	Bond Bond
	Sig  []byte
}

// Bond is the deposit that makes a seat cost something.
//
// Without one a join is free, and free joins make every rule above them
// decorative: a stranger can fill a table's seats, or answer it once too often
// so nobody can form it, at no price and as many times as they like. zkidentity
// keys are free to generate, so the cost has to be locked coin rather than
// anything about who is asking.
//
// The script travels rather than just the outpoint, because it is what says the
// coin is locked at all - and PoP is what says this player controls it. A
// deposit anyone can name proves nothing on its own.
type Bond struct {
	Outpoint string
	Script   []byte
	// PoP is a signature by the bond's owner key over this join's session
	// key. It binds the two, so a bond cited in public cannot be lifted
	// into somebody else's join.
	PoP []byte
}

// Credentials are what a player sits down with: the key it plays under, and the
// bond that makes the seat cost something.
//
// They travel together because neither is any use alone. A session key with no
// bond is a free seat, and a bond nobody can tie to a session key is somebody
// else's deposit being cited.
type Credentials struct {
	// Session is the key this player's actions and commitments are signed
	// with, and the key the escrow scripts name.
	Session *secp256k1.PrivateKey
	// Bond is the key the deposit pays out to. It signs nothing but the
	// proof of possession.
	Bond         *secp256k1.PrivateKey
	BondOutpoint string
	BondScript   []byte
}

// Valid reports whether these could join anything.
func (c Credentials) Valid() error {
	switch {
	case c.Session == nil:
		return fmt.Errorf("no session key")
	case c.Bond == nil:
		return fmt.Errorf("no bond key")
	case strings.TrimSpace(c.BondOutpoint) == "":
		return fmt.Errorf("no bond deposit")
	case len(c.BondScript) == 0:
		return fmt.Errorf("no bond script")
	}
	return nil
}

// SignJoin makes this player's claim to a seat at the table these terms
// describe.
//
// The proof of possession names the session key, so what is published cannot be
// lifted and presented by anybody else.
func SignJoin(t Terms, c Credentials) (*Join, error) {
	if err := c.Valid(); err != nil {
		return nil, err
	}
	priv, bondOutpoint, bondScript := c.Session, c.BondOutpoint, c.BondScript
	key := priv.PubKey().SerializeCompressed()
	pop, err := escrow.SignBondPoP(bondOutpoint, key, c.Bond)
	if err != nil {
		return nil, err
	}

	j := &Join{
		Key:  key,
		Bond: Bond{Outpoint: bondOutpoint, Script: bondScript, PoP: pop},
	}
	digest, err := j.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign join: %w", err)
	}
	j.Sig = sig.Serialize()
	return j, nil
}

func (j *Join) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(j.Key) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("join key is %d bytes, want %d", len(j.Key), escrow.PubKeyLen)
	}
	var b bytes.Buffer
	b.Write(joinTag)
	b.Write(th[:])
	b.Write(j.Key)
	// The bond is signed over as well, so a relayed join cannot have its
	// deposit swapped for another on the way.
	writeString(&b, j.Bond.Outpoint)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(j.Bond.Script)))
	b.Write(j.Bond.Script)
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks that the key really claimed a seat at this table. Binding the
// terms is what stops a join collected for one table being relayed into
// another, which would enrol a key in a roster it never agreed to.
func (j *Join) Verify(t Terms) error {
	digest, err := j.digest(t)
	if err != nil {
		return err
	}
	if err := verifySig(j.Key, j.Sig, digest); err != nil {
		return err
	}

	// The bond, as far as it can be checked without a chain. Whether the
	// deposit exists is somebody else's question - see CheckBond - but
	// whether it is a bond at all, on terms worth anything, and whether
	// this player controls it, are all answerable here.
	terms, err := escrow.ParseBond(j.Bond.Script)
	if err != nil {
		return fmt.Errorf("bond script: %w", err)
	}
	if terms.LockBlocks < escrow.MinBondBlocks {
		return fmt.Errorf("bond is locked for %d blocks, under the %d minimum",
			terms.LockBlocks, escrow.MinBondBlocks)
	}
	if err := escrow.VerifyBondPoP(j.Bond.Script, j.Bond.Outpoint, j.Key, j.Bond.PoP); err != nil {
		return fmt.Errorf("bond: %w", err)
	}
	return nil
}

// BondFacts is what a chain says about a bond's deposit. A peer gets these from
// its own node - or, in a sandbox, from the host's - and hands them here.
type BondFacts struct {
	Found         bool
	ValueAtoms    int64
	PkScriptHex   string
	Confirmations int64
}

// CheckBond decides whether a join's bond is real, given what the chain says.
//
// It is separate from Verify because it needs facts nobody can sign for. Keeping
// the rule here rather than in whatever fetched them means every peer applies
// the same one, and that it can be checked without a chain at all.
func CheckBond(j *Join, facts BondFacts, params stdaddr.AddressParams) error {
	if j == nil {
		return fmt.Errorf("no join")
	}
	if !facts.Found {
		// Never existed, already spent, or not yet confirmed. For a
		// deposit that is supposed to be locked coin everyone can see,
		// those are the same answer.
		return fmt.Errorf("bond deposit %s is not unspent coin on chain", j.Bond.Outpoint)
	}

	_, pkScript, err := escrow.BondAddress(j.Bond.Script, params)
	if err != nil {
		return fmt.Errorf("bond address: %w", err)
	}
	if !strings.EqualFold(facts.PkScriptHex, hex.EncodeToString(pkScript)) {
		// The deposit pays something else. Citing an outpoint that
		// happens to hold coin is not the same as having locked any.
		return fmt.Errorf("bond deposit %s does not pay the bond script", j.Bond.Outpoint)
	}
	if facts.ValueAtoms < int64(escrow.MinBondAtoms) {
		return fmt.Errorf("bond holds %d atoms, under the %d minimum",
			facts.ValueAtoms, escrow.MinBondAtoms)
	}
	if facts.Confirmations < int64(escrow.BondConfirmations) {
		return fmt.Errorf("bond has %d confirmations, needs %d",
			facts.Confirmations, escrow.BondConfirmations)
	}
	return nil
}

// Assertion is a peer saying which membership it currently holds.
//
// It is signed, and that is not decoration. Everyone asserting the same thing
// is what lets a table form before its deadline, so an unsigned one would let
// anybody manufacture agreement - telling each peer that everyone concurs with
// whatever that peer happens to hold, and so driving peers with different join
// sets to bind different memberships.
//
// It is still revisable, which is the whole difference from a commit: a peer
// that learns another join says so and moves on. Nothing irreversible rests on
// one, so two conflicting assertions are not evidence of anything.
type Assertion struct {
	Roster [32]byte
	Signer []byte
	Sig    []byte
}

// SignAssertion says which membership this key currently holds.
func SignAssertion(t Terms, roster [32]byte, priv *secp256k1.PrivateKey) (*Assertion, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	a := &Assertion{Roster: roster, Signer: priv.PubKey().SerializeCompressed()}
	digest, err := a.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign assertion: %w", err)
	}
	a.Sig = sig.Serialize()
	return a, nil
}

func (a *Assertion) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(a.Signer) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("assertion signer is %d bytes, want %d", len(a.Signer), escrow.PubKeyLen)
	}
	var b bytes.Buffer
	b.Write(assertTag)
	b.Write(th[:])
	b.Write(a.Roster[:])
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks the assertion's signature against the key it names.
func (a *Assertion) Verify(t Terms) error {
	digest, err := a.digest(t)
	if err != nil {
		return err
	}
	return verifySig(a.Signer, a.Sig, digest)
}

// Commit binds its signer to one roster, for one session, irrevocably.
//
// This is the message settlement rests on, and it is separate from the roster
// assertions that heal the channel for exactly that reason: an assertion is a
// claim its sender may still revise, and a peer that settled on one could find
// its counterparty had moved on. A commit cannot be revised, so two commits
// from one key at one session are a self-contained proof rather than a
// complaint - the same shape gamelog uses for conflicting chain heads.
type Commit struct {
	Roster [32]byte
	Signer []byte
	Sig    []byte
}

// SignCommit binds a key to a roster.
func SignCommit(t Terms, roster [32]byte, priv *secp256k1.PrivateKey) (*Commit, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	c := &Commit{Roster: roster, Signer: priv.PubKey().SerializeCompressed()}
	digest, err := c.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign commit: %w", err)
	}
	c.Sig = sig.Serialize()
	return c, nil
}

func (c *Commit) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(c.Signer) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("commit signer is %d bytes, want %d", len(c.Signer), escrow.PubKeyLen)
	}
	var b bytes.Buffer
	b.Write(commitTag)
	b.Write(th[:])
	b.Write(c.Roster[:])
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks the commit's signature against the key it names.
func (c *Commit) Verify(t Terms) error {
	digest, err := c.digest(t)
	if err != nil {
		return err
	}
	return verifySig(c.Signer, c.Sig, digest)
}

// ConflictingCommits is proof that one key bound itself to two rosters at one
// session. It is verifiable by anyone, offline, with no quorum and no vote.
type ConflictingCommits struct {
	A, B *Commit
}

// Verify checks that the pair really is a contradiction by its signer. Showing
// the same commit twice proves nothing, and neither does a pair whose
// signatures do not check.
func (p ConflictingCommits) Verify(t Terms) error {
	if p.A == nil || p.B == nil {
		return fmt.Errorf("a proof needs two commits")
	}
	if !bytes.Equal(p.A.Signer, p.B.Signer) {
		return fmt.Errorf("commits are by different keys, which is not a contradiction")
	}
	if p.A.Roster == p.B.Roster {
		return fmt.Errorf("both commits name the same roster")
	}
	if err := p.A.Verify(t); err != nil {
		return fmt.Errorf("first commit: %w", err)
	}
	if err := p.B.Verify(t); err != nil {
		return fmt.Errorf("second commit: %w", err)
	}
	return nil
}

// RosterHash is the digest a commit binds to: the terms, and the membership in
// the one canonical order every member derives independently.
func RosterHash(t Terms, canonical [][]byte) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	var b bytes.Buffer
	b.Write(rosterTag)
	b.Write(th[:])
	_ = binary.Write(&b, binary.BigEndian, uint32(len(canonical)))
	for _, k := range canonical {
		if len(k) != escrow.PubKeyLen {
			return [32]byte{}, fmt.Errorf("member key is %d bytes, want %d", len(k), escrow.PubKeyLen)
		}
		b.Write(k)
	}
	return blake256.Sum256(b.Bytes()), nil
}

func writeString(b *bytes.Buffer, s string) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(s)))
	b.WriteString(s)
}

func verifySig(key, sig []byte, digest [32]byte) error {
	if len(sig) != SigLen {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), SigLen)
	}
	pub, err := secp256k1.ParsePubKey(key)
	if err != nil {
		return fmt.Errorf("key: %w", err)
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	if !parsed.Verify(digest[:], pub) {
		return fmt.Errorf("signature does not verify")
	}
	return nil
}

// sortKeys orders compressed keys the way escrow.CanonicalMembers does, without
// its member-count limit. Selection has to be able to look at more keys than a
// table can seat in order to notice that it is oversubscribed.
func sortKeys(keys [][]byte) [][]byte {
	out := make([][]byte, len(keys))
	copy(out, keys)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

func keyID(k []byte) string { return hex.EncodeToString(k) }
