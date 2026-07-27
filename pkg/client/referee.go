package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
	"github.com/decred/slog"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
	"google.golang.org/grpc/metadata"
)

var schnorrV0ExtraTag = func() [32]byte {
	const tagHex = "0b75f97b60e8a5762876c004829ee9b926fa6f0d2eeaec3a4fd1446a768331cb"
	b, _ := hex.DecodeString(tagHex)
	var out [32]byte
	copy(out[:], b)
	return out
}()

// DefaultMaxSettlementFeeAtoms bounds the fee a client will accept in a
// server-proposed settlement draft. Keep in sync with the server's
// DefaultSettlementFeeAtoms.
const DefaultMaxSettlementFeeAtoms uint64 = 10_000

// PresignPolicy is what a client demands of a server-proposed settlement draft
// before it will pre-sign it. A zero MaxFeeAtoms means
// DefaultMaxSettlementFeeAtoms.
type PresignPolicy struct {
	PayoutAddress string
	MaxFeeAtoms   uint64
}

func (p PresignPolicy) maxFee() uint64 {
	if p.MaxFeeAtoms == 0 {
		return DefaultMaxSettlementFeeAtoms
	}
	return p.MaxFeeAtoms
}

// RefereeClient wraps PokerReferee RPCs with presign helpers.
type RefereeClient struct {
	rc     pokerrpc.PokerRefereeClient
	log    slog.Logger
	token  string
	policy PresignPolicy
	// owner is the client this referee client belongs to, when there is
	// one. Binding an escrow is the moment a player's seat and their
	// session key are both known, which is what arming action signing
	// needs, so the two are joined here rather than left to every caller
	// to remember.
	owner *PokerClient
}

// RefereeOption customizes a RefereeClient.
type RefereeOption func(*RefereeClient)

// WithPresignPolicy sets what the client requires of settlement drafts.
func WithPresignPolicy(p PresignPolicy) RefereeOption {
	return func(c *RefereeClient) { c.policy = p }
}

// NewRefereeClient constructs a referee client using an existing gRPC conn.
func NewRefereeClient(conn pokerrpc.PokerRefereeClient, log slog.Logger, token string, opts ...RefereeOption) *RefereeClient {
	c := &RefereeClient{rc: conn, log: log, token: token}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Referee returns a RefereeClient bound to this PokerClient's connection/token.
// The presign policy is seeded from the client's configured payout address so
// drafts are checked against where this player expects to be paid.
func (pc *PokerClient) Referee(token string) *RefereeClient {
	c := NewRefereeClient(pokerrpc.NewPokerRefereeClient(pc.conn), pc.log, token,
		WithPresignPolicy(PresignPolicy{PayoutAddress: pc.PayoutAddress()}))
	c.owner = pc
	return c
}

// SetPayoutAddress verifies a signed code and binds the payout address to the current session/user.
func (c *RefereeClient) SetPayoutAddress(ctx context.Context, address, signature, code string) (string, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	req := &pokerrpc.SetPayoutAddressRequest{
		Token:     c.token,
		Address:   address,
		Signature: signature,
		Code:      code,
	}
	resp, err := c.rc.SetPayoutAddress(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Ok {
		return "", fmt.Errorf("set payout address failed: %s", resp.Error)
	}
	return resp.Address, nil
}

func (c *RefereeClient) GetEscrowStatus(ctx context.Context, escrowID string) (*pokerrpc.GetEscrowStatusResponse, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	return c.rc.GetEscrowStatus(ctx, &pokerrpc.GetEscrowStatusRequest{
		EscrowId: escrowID,
	})
}

// OpenEscrow opens a Schnorr escrow for a table/session seat using the caller's
// token.
//
// The deposit address is the hash of a script naming every seat's session key,
// so it does not exist until the last seat has opened. Until then the response
// carries RosterReady false, SeatsPending, and no address; callers repeat the
// request, which is idempotent per seat, until the roster closes.
//
// Once it is ready the script is rebuilt here from the roster the referee
// reported and checked against the address it handed back, so funding never
// rests on the referee having been honest about either.
func (c *RefereeClient) OpenEscrow(ctx context.Context, tableID, sessionID string, amountAtoms uint64, csvBlocks uint32, compPubkey []byte) (*pokerrpc.OpenEscrowResponse, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	resp, err := c.rc.OpenEscrow(ctx, &pokerrpc.OpenEscrowRequest{
		AmountAtoms: amountAtoms,
		CsvBlocks:   csvBlocks,
		CompPubkey:  compPubkey,
		TableId:     tableID,
		SessionId:   sessionID,
	})
	if err != nil {
		return nil, err
	}
	if !resp.GetRosterReady() {
		return resp, nil
	}
	if err := VerifyEscrowRoster(resp, compPubkey, csvBlocks); err != nil {
		return nil, err
	}
	return resp, nil
}

// VerifyEscrowRoster rebuilds the deposit script from the roster the referee
// reported and checks that it yields both the script and the address the
// referee handed back.
//
// This is what makes a roster-bound escrow safe to fund. A referee that
// substituted a key of its own, dropped a member, or pointed the address at a
// script the client never agreed to fails here - before any funds move - rather
// than at settlement, when the only remedy left is the CSV refund.
func VerifyEscrowRoster(resp *pokerrpc.OpenEscrowResponse, compPubkey []byte, csvBlocks uint32) error {
	members := resp.GetMemberPubkeys()
	if len(members) == 0 {
		return fmt.Errorf("escrow roster reported ready but carries no member keys")
	}

	want, err := escrow.RedeemScript(compPubkey, members, csvBlocks)
	if err != nil {
		return fmt.Errorf("rebuild redeem script from roster: %w", err)
	}
	got, err := hex.DecodeString(resp.GetRedeemScriptHex())
	if err != nil {
		return fmt.Errorf("decode redeem script: %w", err)
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf("redeem script does not match the roster the referee reported")
	}

	// The address is only worth funding if it is the P2SH of that exact
	// script, so check the hash rather than taking the address on trust.
	pkScript, err := hex.DecodeString(resp.GetPkScriptHex())
	if err != nil {
		return fmt.Errorf("decode pk script: %w", err)
	}
	wantPk, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_HASH160).
		AddData(dcrutil.Hash160(want)).
		AddOp(txscript.OP_EQUAL).
		Script()
	if err != nil {
		return fmt.Errorf("build pk script: %w", err)
	}
	if !bytes.Equal(wantPk, pkScript) {
		return fmt.Errorf("pk script is not the P2SH of the roster redeem script")
	}

	addrScript, err := paymentScriptForAddress(resp.GetDepositAddr())
	if err != nil {
		return fmt.Errorf("decode deposit address: %w", err)
	}
	if !bytes.Equal(addrScript, pkScript) {
		return fmt.Errorf("deposit address does not pay the roster escrow script")
	}
	return nil
}

// BindEscrow binds escrow funding (txid:vout) to a table/session seat.
func (c *RefereeClient) BindEscrow(ctx context.Context, tableID, sessionID, matchID string, seatIndex uint32, outpoint string, redeemScriptHex string, csvBlocks uint32) (*pokerrpc.BindEscrowResponse, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	req := &pokerrpc.BindEscrowRequest{
		TableId:         tableID,
		SessionId:       sessionID,
		MatchId:         matchID,
		SeatIndex:       seatIndex,
		Outpoint:        outpoint,
		RedeemScriptHex: redeemScriptHex,
		CsvBlocks:       csvBlocks,
	}
	resp, err := c.rc.BindEscrow(ctx, req)
	if err != nil {
		return nil, err
	}
	c.armActionSigning(resp)
	return resp, nil
}

// armActionSigning teaches the client to sign its actions for the seat it has
// just bound an escrow to.
//
// This is the moment both halves exist: the referee has just told us which
// seat we hold, and the escrow we opened records which session key index it
// was derived from. Signing with that same key is what ties the record of who
// acted to the record of whose money is at stake.
//
// Failure is logged rather than returned. The bind itself succeeded, and the
// consequence surfaces immediately and loudly on the next action - a table
// that keeps a log refuses an unsigned one - which is far better than failing
// a bind that actually worked.
func (c *RefereeClient) armActionSigning(resp *pokerrpc.BindEscrowResponse) {
	if c.owner == nil || resp.GetEscrowId() == "" {
		return
	}
	info, err := c.owner.GetEscrowById(resp.GetEscrowId())
	if err != nil {
		c.logf("cannot arm action signing: no cached escrow %s: %v", resp.GetEscrowId(), err)
		return
	}
	idx, ok := info["key_index"].(float64)
	if !ok {
		c.logf("cannot arm action signing: escrow %s records no session key index", resp.GetEscrowId())
		return
	}
	privHex, _, err := c.owner.DeriveSessionKeyAt(uint64(idx))
	if err != nil {
		c.logf("cannot arm action signing: derive session key %d: %v", uint64(idx), err)
		return
	}
	if err := c.owner.SetActionSigner(resp.GetSeatIndex(), privHex); err != nil {
		c.logf("cannot arm action signing: %v", err)
		return
	}
	c.logf("signing actions for seat %d with session key %d", resp.GetSeatIndex(), uint64(idx))
}

func (c *RefereeClient) logf(format string, args ...interface{}) {
	if c.log != nil {
		c.log.Warnf(format, args...)
	}
}

// StartPresign runs the SettlementStream presign flow for a match/escrow.
// xPrivHex is the session private scalar (hex) corresponding to compPubkey.
// Seat is resolved server-side; seatIndex is not sent by callers.
func (c *RefereeClient) StartPresign(ctx context.Context, matchID, tableID string, escrowID string, compPubkey []byte, xPrivHex string) error {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	stream, err := c.rc.SettlementStream(ctx)
	if err != nil {
		return err
	}

	hello := &pokerrpc.SettlementHello{
		MatchId:    matchID,
		TableId:    tableID,
		EscrowId:   escrowID,
		CompPubkey: compPubkey,
		Token:      c.token,
	}
	if err := stream.Send(&pokerrpc.SettlementStreamMessage{Msg: &pokerrpc.SettlementStreamMessage_Hello{Hello: hello}}); err != nil {
		return err
	}

	// Track which branches have been acknowledged by VerifyOk. The branch set
	// is pinned to the draft itself — one branch per input, since every escrow
	// is one possible winner — rather than discovered from whatever the server
	// chooses to send. A server that withholds a branch cannot make this seat
	// believe presigning is complete.
	branches := make(map[int32]bool) // branch -> acked
	totalBranches := 0
	abortSigned := false
	abortAcked := false

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if need := msg.GetNeedPreSigs(); need != nil {
			if need.GetKind() == pokerrpc.DraftKind_DRAFT_KIND_ABORT {
				sigs, err := SignAbortDraft(xPrivHex, need, c.policy)
				if err != nil {
					return err
				}
				resp := &pokerrpc.ProvidePreSigs{
					MatchId: need.MatchId,
					Kind:    pokerrpc.DraftKind_DRAFT_KIND_ABORT,
					Cosigs:  sigs,
				}
				if err := stream.Send(&pokerrpc.SettlementStreamMessage{Msg: &pokerrpc.SettlementStreamMessage_ProvidePreSigs{ProvidePreSigs: resp}}); err != nil {
					return err
				}
				abortSigned = true
				continue
			}
			n, err := draftBranchCount(need)
			if err != nil {
				return err
			}
			switch {
			case totalBranches == 0:
				totalBranches = n
			case totalBranches != n:
				return fmt.Errorf("branch count changed mid-stream: %d then %d", totalBranches, n)
			}
			if need.Branch < 0 || int(need.Branch) >= totalBranches {
				return fmt.Errorf("branch %d out of range for %d branches", need.Branch, totalBranches)
			}
			if _, ok := branches[need.Branch]; !ok {
				branches[need.Branch] = false
			}
			pres, err := BuildVerifyOk(xPrivHex, need, c.policy)
			if err != nil {
				return err
			}
			resp := &pokerrpc.ProvidePreSigs{
				MatchId: need.MatchId,
				Branch:  need.Branch,
				Presigs: pres.PreSigs,
				Cosigs:  pres.CoSigs,
			}
			if err := stream.Send(&pokerrpc.SettlementStreamMessage{Msg: &pokerrpc.SettlementStreamMessage_ProvidePreSigs{ProvidePreSigs: resp}}); err != nil {
				return err
			}
			continue
		}
		if errMsg := msg.GetError(); errMsg != nil {
			return fmt.Errorf("referee error: %s", errMsg.Error)
		}
		if ok := msg.GetVerifyOk(); ok != nil {
			if ok.GetKind() == pokerrpc.DraftKind_DRAFT_KIND_ABORT {
				abortAcked = true
			} else {
				branches[ok.Branch] = true
			}

			// Presigning is complete only once every branch of the draft has
			// been seen and acknowledged, not merely those the server sent,
			// and once the unwind draft is agreed too - a table that skipped
			// it can only be recovered one CSV timeout at a time.
			//
			// Either kind of ack can be the last to arrive, so this is checked
			// on both.
			allAcked := totalBranches > 0 && len(branches) == totalBranches && abortSigned && abortAcked
			for i := 0; allAcked && i < totalBranches; i++ {
				if !branches[int32(i)] {
					allAcked = false
				}
			}
			if allAcked {
				// Presign finished for all branches for this seat.
				// Close the send direction to signal EOF to the server.
				_ = stream.CloseSend()
				return nil
			}
		}
	}
}

// GetFinalizeBundle fetches the winning draft + presigs for a branch.
func (c *RefereeClient) GetFinalizeBundle(ctx context.Context, matchID string, winnerSeat int32) (*pokerrpc.GetFinalizeBundleResponse, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	return c.rc.GetFinalizeBundle(ctx, &pokerrpc.GetFinalizeBundleRequest{
		MatchId:    matchID,
		WinnerSeat: winnerSeat,
	})
}

// SignedBranch is what a client contributes for one settlement branch.
//
// The escrow's settlement branch needs a signature from every table member, so
// a client signs every input of the draft — but in two different ways. Its own
// input gets an adaptor pre-signature, which only completes once the referee
// reveals that branch's secret, and that is what keeps a branch unspendable
// until it is the branch that won. Everyone else's inputs get ordinary
// signatures: branch selection is already gated by the owner's presig, so a
// co-signature only has to prove this player agreed to the draft.
type SignedBranch struct {
	PreSigs []*pokerrpc.PreSignature
	CoSigs  []*pokerrpc.CoSignature
}

func buildPresigs(xPrivHex string, need *pokerrpc.NeedPreSigs, ownPub []byte) (*SignedBranch, error) {
	privB, err := hex.DecodeString(xPrivHex)
	if err != nil || len(privB) == 0 {
		return nil, fmt.Errorf("bad x priv")
	}
	out := &SignedBranch{}
	for _, in := range need.Inputs {
		if len(in.SighashHex) != 64 {
			return nil, fmt.Errorf("bad sighash for %s", in.InputId)
		}

		// An input with no stated owner comes from a server that sends only
		// the caller's own inputs, so it is ours by construction.
		if len(in.OwnerPubkey) != 0 && !bytes.Equal(in.OwnerPubkey, ownPub) {
			sig, err := signSchnorrV0(xPrivHex, in.SighashHex)
			if err != nil {
				return nil, fmt.Errorf("cosign %s: %w", in.InputId, err)
			}
			out.CoSigs = append(out.CoSigs, &pokerrpc.CoSignature{
				InputId:      in.InputId,
				SignerPubkey: append([]byte(nil), ownPub...),
				SigHex:       hex.EncodeToString(sig),
			})
			continue
		}

		if len(in.AdaptorPointHex) != 66 {
			return nil, fmt.Errorf("bad adaptor point for %s", in.InputId)
		}
		rComp, sPrime, err := computePreSig(privB, in.SighashHex, in.AdaptorPointHex)
		if err != nil {
			return nil, fmt.Errorf("compute presig %s: %w", in.InputId, err)
		}
		out.PreSigs = append(out.PreSigs, &pokerrpc.PreSignature{
			InputId:          in.InputId,
			RPrimeCompactHex: rComp,
			SPrimeHex:        sPrime,
		})
	}
	if len(out.PreSigs) == 0 {
		return nil, fmt.Errorf("draft carries no input owned by this player")
	}
	return out, nil
}

// pubFromPrivHex derives the compressed session key for a private scalar, so a
// client can tell which draft inputs are its own without being told.
func pubFromPrivHex(xPrivHex string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(xPrivHex))
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("bad x priv")
	}
	return secp256k1.PrivKeyFromBytes(b).PubKey().SerializeCompressed(), nil
}

// computePreSig derives adaptor pre-signature for (x, m, T).
// Math (minus variant, DCRv0):
//
//	e  = BLAKE256(r_x || m) mod n
//	s' = k - e·x
//	R' = k·G + T   (we enforce even-Y on R')
func computePreSig(xb []byte, mHex, TCompHex string) (rCompHex string, sPrimeHex string, err error) {
	mb, err := hex.DecodeString(mHex)
	if err != nil || len(mb) != 32 {
		return "", "", fmt.Errorf("bad m")
	}
	Tb, err := hex.DecodeString(TCompHex)
	if err != nil {
		return "", "", err
	}

	var x secp256k1.ModNScalar
	if overflow := x.SetByteSlice(xb); overflow || x.IsZero() {
		return "", "", fmt.Errorf("bad x scalar")
	}
	Tpub, err := secp256k1.ParsePubKey(Tb)
	if err != nil {
		return "", "", err
	}

	extra := blake256.Sum256(append(schnorrV0ExtraTag[:], Tb...))

	for iter := uint32(0); ; iter++ {
		k := secp256k1.NonceRFC6979(xb, mb, extra[:], nil, iter)
		if k == nil || k.IsZero() {
			continue
		}

		var R secp256k1.JacobianPoint
		secp256k1.ScalarBaseMultNonConst(k, &R)

		var tJac secp256k1.JacobianPoint
		Tpub.AsJacobian(&tJac)
		secp256k1.AddNonConst(&R, &tJac, &R)
		if R.Z.IsZero() {
			continue
		}
		R.ToAffine()
		Rpub := secp256k1.NewPublicKey(&R.X, &R.Y)
		rComp := Rpub.SerializeCompressed()
		if len(rComp) != 33 || rComp[0] != 0x02 {
			continue
		}

		eBytes := hashSchnorr(rComp[1:], mb)
		var e secp256k1.ModNScalar
		if overflow := e.SetByteSlice(eBytes[:]); overflow || e.IsZero() {
			continue
		}

		var ex secp256k1.ModNScalar
		ex.Set(&e)
		ex.Mul(&x) // ex = e * x

		var s secp256k1.ModNScalar
		s.Set(k)    // s = k
		ex.Negate() // -e*x
		s.Add(&ex)  // s' = k - e*x
		if s.IsZero() {
			continue
		}
		sb := s.Bytes()
		return hex.EncodeToString(rComp), hex.EncodeToString(sb[:]), nil
	}
}

// hashSchnorr computes e = H(rx || m) mod n.
func hashSchnorr(rx []byte, m []byte) [32]byte {
	h := blake256.New()
	h.Write(rx)
	h.Write(m)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// VerifyPreSig can be used by tests to validate stored presigs.
// Check: s'G + eX + T ?= R'
func VerifyPreSig(ctx *pokerrpc.NeedPreSigs, compPubkey []byte, ps *pokerrpc.PreSignature) error {
	if ctx == nil || ps == nil {
		return fmt.Errorf("nil args")
	}
	var target *pokerrpc.NeedPreSigsInput
	for _, in := range ctx.Inputs {
		if in.InputId == ps.InputId {
			target = in
			break
		}
	}
	if target == nil {
		return fmt.Errorf("input not found in ctx")
	}
	rb, err := hex.DecodeString(ps.RPrimeCompactHex)
	if err != nil || len(rb) != 33 || rb[0] != 0x02 {
		return fmt.Errorf("bad R'")
	}
	sb, err := hex.DecodeString(ps.SPrimeHex)
	if err != nil || len(sb) != 32 {
		return fmt.Errorf("bad s'")
	}
	tb, err := hex.DecodeString(target.AdaptorPointHex)
	if err != nil || len(tb) != 33 {
		return fmt.Errorf("bad T")
	}
	T, err := secp256k1.ParsePubKey(tb)
	if err != nil {
		return fmt.Errorf("parse T: %w", err)
	}
	R, err := secp256k1.ParsePubKey(rb)
	if err != nil {
		return fmt.Errorf("parse R': %w", err)
	}

	// Recompute e
	mBytes, err := hex.DecodeString(target.SighashHex)
	if err != nil || len(mBytes) != 32 {
		return fmt.Errorf("bad m")
	}
	e := hashSchnorr(R.X().Bytes(), mBytes)
	var es, s secp256k1.ModNScalar
	es.SetByteSlice(e[:])
	s.SetByteSlice(sb)

	// Check s'G + eX + T == R'
	X, err := secp256k1.ParsePubKey(compPubkey)
	if err != nil {
		return fmt.Errorf("parse comp pubkey: %w", err)
	}
	var sG secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(&s, &sG)

	var xJac secp256k1.JacobianPoint
	X.AsJacobian(&xJac)
	var exP secp256k1.JacobianPoint
	secp256k1.ScalarMultNonConst(&es, &xJac, &exP)

	secp256k1.AddNonConst(&sG, &exP, &sG)

	var tJac secp256k1.JacobianPoint
	T.AsJacobian(&tJac)
	secp256k1.AddNonConst(&sG, &tJac, &sG)

	sG.ToAffine()
	L := secp256k1.NewPublicKey(&sG.X, &sG.Y)

	if !L.IsEqual(R) {
		return fmt.Errorf("presig verification failed")
	}
	return nil
}

// validateNeedPreSigs ensures the server-provided draft and inputs are consistent
// with each other before we derive and return pre-signatures.
func validateNeedPreSigs(need *pokerrpc.NeedPreSigs, pol PresignPolicy, ownPub []byte) error {
	if need == nil {
		return fmt.Errorf("nil need presigs")
	}
	if len(need.Inputs) == 0 {
		return fmt.Errorf("no inputs in presign request")
	}
	rawTx, err := hex.DecodeString(need.DraftTxHex)
	if err != nil {
		return fmt.Errorf("decode draft tx: %w", err)
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
		return fmt.Errorf("deserialize draft tx: %w", err)
	}
	if tx.Version < 3 {
		return fmt.Errorf("draft tx version %d too low for schnorr", tx.Version)
	}
	if len(tx.TxOut) == 0 {
		return fmt.Errorf("draft tx has no outputs")
	}

	for _, in := range need.Inputs {
		if in.InputIndex >= uint32(len(tx.TxIn)) {
			return fmt.Errorf("input %s index %d out of range", in.InputId, in.InputIndex)
		}
		txIn := tx.TxIn[in.InputIndex]

		parts := strings.Split(in.InputId, ":")
		if len(parts) != 2 {
			return fmt.Errorf("input id %s malformed", in.InputId)
		}
		vout, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			return fmt.Errorf("parse vout for %s: %w", in.InputId, err)
		}
		var h chainhash.Hash
		if err := chainhash.Decode(&h, parts[0]); err != nil {
			return fmt.Errorf("parse txid for %s: %w", in.InputId, err)
		}
		if txIn.PreviousOutPoint.Index != uint32(vout) || txIn.PreviousOutPoint.Hash != h {
			return fmt.Errorf("draft input mismatch for %s", in.InputId)
		}

		redeem, err := hex.DecodeString(in.RedeemScriptHex)
		if err != nil {
			return fmt.Errorf("decode redeem for %s: %w", in.InputId, err)
		}
		sighash, err := txscript.CalcSignatureHash(redeem, txscript.SigHashAll, &tx, int(in.InputIndex), nil)
		if err != nil {
			return fmt.Errorf("calc sighash for %s: %w", in.InputId, err)
		}
		if !strings.EqualFold(hex.EncodeToString(sighash), in.SighashHex) {
			return fmt.Errorf("sighash mismatch for %s", in.InputId)
		}

		adaptB, err := hex.DecodeString(in.AdaptorPointHex)
		if err != nil {
			return fmt.Errorf("decode adaptor for %s: %w", in.InputId, err)
		}
		if len(adaptB) != 33 || (adaptB[0] != 0x02 && adaptB[0] != 0x03) {
			return fmt.Errorf("invalid adaptor encoding for %s", in.InputId)
		}
		if _, err := secp256k1.ParsePubKey(adaptB); err != nil {
			return fmt.Errorf("parse adaptor point for %s: %w", in.InputId, err)
		}
	}

	return validateDraftOutputs(&tx, need, pol, ownPub)
}

// validateDraftOutputs checks the part of a draft that decides where the money
// goes. Without it a client pre-signs whatever destination the server names.
//
// A settlement draft is winner-take-all: exactly one output, worth the sum of
// the inputs less a bounded fee. Branches are numbered by draft input index and
// branch b pays the owner of input b, so a client can recognize the branch that
// pays it and require that output to match its own payout address. Branches
// paying somebody else are checked for shape and fee only, because a client
// cannot know another player's address. That is still sufficient in aggregate:
// every branch is strictly checked by the player it pays, so a draft with a
// redirected payout can never collect a full set of presigs.
func validateDraftOutputs(tx *wire.MsgTx, need *pokerrpc.NeedPreSigs, pol PresignPolicy, ownPub []byte) error {
	if len(tx.TxOut) != 1 {
		return fmt.Errorf("draft tx has %d outputs, want exactly 1", len(tx.TxOut))
	}

	var totalIn int64
	for i, txIn := range tx.TxIn {
		if txIn.ValueIn <= 0 {
			return fmt.Errorf("draft input %d has non-positive value %d", i, txIn.ValueIn)
		}
		totalIn += txIn.ValueIn
	}

	payout := tx.TxOut[0].Value
	if payout <= 0 {
		return fmt.Errorf("draft payout %d is not positive", payout)
	}
	if payout > totalIn {
		return fmt.Errorf("draft payout %d exceeds inputs %d", payout, totalIn)
	}
	if fee := uint64(totalIn - payout); fee > pol.maxFee() {
		return fmt.Errorf("draft fee %d exceeds maximum %d", fee, pol.maxFee())
	}

	// Branch b pays the owner of input b, so this branch pays us when the
	// input at that index is ours. Ownership is stated by owner_pubkey; a
	// server that sends only our own inputs and names no owner leaves the
	// index match as the sole signal.
	paysUs := false
	for _, in := range need.Inputs {
		if int32(in.InputIndex) != need.Branch {
			continue
		}
		paysUs = len(in.OwnerPubkey) == 0 || bytes.Equal(in.OwnerPubkey, ownPub)
		break
	}
	if !paysUs {
		return nil
	}

	if strings.TrimSpace(pol.PayoutAddress) == "" {
		return fmt.Errorf("refusing to presign branch %d: no payout address configured", need.Branch)
	}
	want, err := paymentScriptForAddress(pol.PayoutAddress)
	if err != nil {
		return fmt.Errorf("payout address: %w", err)
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, want) {
		return fmt.Errorf("draft branch %d pays a script other than the configured payout address", need.Branch)
	}
	return nil
}

// draftBranchCount reports how many branches a match has. Every escrow is one
// draft input and one possible winner, so the input count is the branch count.
func draftBranchCount(need *pokerrpc.NeedPreSigs) (int, error) {
	raw, err := hex.DecodeString(need.DraftTxHex)
	if err != nil {
		return 0, fmt.Errorf("decode draft tx: %w", err)
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return 0, fmt.Errorf("deserialize draft tx: %w", err)
	}
	if len(tx.TxIn) == 0 {
		return 0, fmt.Errorf("draft tx has no inputs")
	}
	return len(tx.TxIn), nil
}

// BuildVerifyOk validates the server-provided NeedPreSigs and derives adaptor
// pre-signatures for each input using the given private scalar (hex).
//
// Math (minus variant, DCRv0):
//
//	e  = BLAKE256(r_x || m) mod n
//	s' = k - e·x
//	R' = k·G + T   (even-Y enforced in computePreSig)
//
// Server check (equivalent form): s'G + eX + T ?= R'   // i.e. s'G ?= R' - eX - T
func BuildVerifyOk(xPrivHex string, need *pokerrpc.NeedPreSigs, pol PresignPolicy) (*SignedBranch, error) {
	ownPub, err := pubFromPrivHex(xPrivHex)
	if err != nil {
		return nil, err
	}
	if err := validateNeedPreSigs(need, pol, ownPub); err != nil {
		return nil, fmt.Errorf("server presign validation failed: %w", err)
	}
	return buildPresigs(xPrivHex, need, ownPub)
}

// SignAbortDraft validates the unwind draft and signs every one of its inputs.
//
// The abort is what saves a funded table that never starts from waiting out the
// CSV timeout, and signing it is safe precisely because it returns every seat
// its own stake: there is no branch to choose and nothing it can be misapplied
// to. That property is only worth anything if the draft really does pay this
// player back, which is what is checked here - a referee that proposed an
// "abort" paying somebody else would otherwise be handing itself the pot.
//
// The signer's own input is signed plainly too, unlike a settlement draft where
// that slot must stay adaptor-locked.
func SignAbortDraft(xPrivHex string, need *pokerrpc.NeedPreSigs, pol PresignPolicy) ([]*pokerrpc.CoSignature, error) {
	ownPub, err := pubFromPrivHex(xPrivHex)
	if err != nil {
		return nil, err
	}
	if err := validateAbortDraft(need, pol, ownPub); err != nil {
		return nil, err
	}

	xb, err := hex.DecodeString(strings.TrimSpace(xPrivHex))
	if err != nil || len(xb) == 0 {
		return nil, fmt.Errorf("bad x priv")
	}
	priv := secp256k1.PrivKeyFromBytes(xb)

	out := make([]*pokerrpc.CoSignature, 0, len(need.Inputs))
	for _, in := range need.Inputs {
		sighash, err := hex.DecodeString(in.SighashHex)
		if err != nil || len(sighash) != 32 {
			return nil, fmt.Errorf("input %s: bad sighash", in.InputId)
		}
		sig, err := schnorr.Sign(priv, sighash)
		if err != nil {
			return nil, fmt.Errorf("input %s: sign: %w", in.InputId, err)
		}
		out = append(out, &pokerrpc.CoSignature{
			InputId:      in.InputId,
			SignerPubkey: ownPub,
			SigHex:       hex.EncodeToString(append(sig.Serialize(), byte(txscript.SigHashAll))),
		})
	}
	return out, nil
}

// validateAbortDraft checks that an unwind draft returns this player's own
// stake to this player's own payout address.
func validateAbortDraft(need *pokerrpc.NeedPreSigs, pol PresignPolicy, ownPub []byte) error {
	if len(need.Inputs) == 0 {
		return fmt.Errorf("abort draft has no inputs")
	}
	if strings.TrimSpace(pol.PayoutAddress) == "" {
		return fmt.Errorf("refusing to sign an abort draft with no payout address to check it against")
	}

	raw, err := hex.DecodeString(need.DraftTxHex)
	if err != nil {
		return fmt.Errorf("decode abort draft: %w", err)
	}
	tx := wire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("deserialize abort draft: %w", err)
	}
	if len(tx.TxIn) != len(need.Inputs) {
		return fmt.Errorf("abort draft has %d inputs but %d were described", len(tx.TxIn), len(need.Inputs))
	}
	// One refund per stake: an extra output is somewhere else for the money
	// to go, and a missing one is a seat that does not get paid.
	if len(tx.TxOut) != len(need.Inputs) {
		return fmt.Errorf("abort draft has %d outputs for %d inputs", len(tx.TxOut), len(need.Inputs))
	}

	var ours *pokerrpc.NeedPreSigsInput
	for _, in := range need.Inputs {
		if bytes.Equal(in.OwnerPubkey, ownPub) {
			ours = in
			break
		}
	}
	if ours == nil {
		return fmt.Errorf("abort draft carries no input of ours to be refunded")
	}

	// The fee falls on the seats evenly, with the remainder on one of them, so
	// the most any single seat can be charged is its share plus that
	// remainder. Bounding our refund by the whole fee instead would let a
	// referee quietly charge us everyone else's share.
	seats := uint64(len(need.Inputs))
	ourFee := pol.maxFee()/seats + pol.maxFee()%seats
	if ours.AmountAtoms <= ourFee {
		return fmt.Errorf("our stake %d does not cover its share of the fee (%d)", ours.AmountAtoms, ourFee)
	}
	least := int64(ours.AmountAtoms - ourFee)

	payScript, err := paymentScriptForAddress(pol.PayoutAddress)
	if err != nil {
		return fmt.Errorf("decode our payout address: %w", err)
	}
	for _, out := range tx.TxOut {
		if bytes.Equal(out.PkScript, payScript) && out.Value >= least {
			return nil
		}
	}
	return fmt.Errorf("abort draft does not refund our stake of %d atoms to our payout address", ours.AmountAtoms)
}

// BondAddress derives the deposit address for a fidelity bond over the given
// session key and lock, along with the script the referee will be shown.
//
// A bond is the player's own coin, locked: nobody else can ever spend it, and
// the referee gains no claim on it. What it buys is a seat - registration has
// to cost something, because a zkidentity does not, and a seat held by someone
// who never funds keeps every other stake at the table waiting on its CSV.
func BondAddress(compPubkey []byte, lockBlocks uint32, network string) (addr string, bondScriptHex string, err error) {
	script, err := escrow.BondScript(compPubkey, lockBlocks)
	if err != nil {
		return "", "", err
	}
	params, err := chainParamsForNetwork(network)
	if err != nil {
		return "", "", err
	}
	a, _, err := escrow.BondAddress(script, params)
	if err != nil {
		return "", "", err
	}
	return a.String(), hex.EncodeToString(script), nil
}

// PostBond registers a funded bond with the referee.
//
// It proves the caller holds the key the bond pays out to. A bond is public, so
// citing an outpoint is not a claim on it - anyone can read the script and name
// the deposit. The proof names this player as well as the deposit, so it cannot
// be lifted and presented by somebody else.
func (c *RefereeClient) PostBond(ctx context.Context, outpoint, bondScriptHex string, ownerPriv *secp256k1.PrivateKey) (*pokerrpc.PostBondResponse, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(bondScriptHex))
	if err != nil {
		return nil, fmt.Errorf("bond script: %w", err)
	}
	if _, err := escrow.ParseBond(raw); err != nil {
		return nil, fmt.Errorf("bond script: %w", err)
	}

	uid := c.owner.ID
	pop, err := escrow.SignBondPoP(outpoint, uid[:], ownerPriv)
	if err != nil {
		return nil, err
	}
	return c.rc.PostBond(ctx, &pokerrpc.PostBondRequest{
		Token:         c.token,
		Outpoint:      outpoint,
		BondScriptHex: bondScriptHex,
		Pop:           pop,
	})
}

// GetBond reports the bond the caller holds, and the terms required if none.
func (c *RefereeClient) GetBond(ctx context.Context) (*pokerrpc.PostBondResponse, error) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "token", c.token)
	}
	return c.rc.GetBond(ctx, &pokerrpc.GetBondRequest{Token: c.token})
}
