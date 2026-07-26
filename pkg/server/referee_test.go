package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/companyzero/bisonrelay/zkidentity"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/stretchr/testify/require"
	"github.com/vctt94/pokerbisonrelay/pkg/chainwatcher"
	"github.com/vctt94/pokerbisonrelay/pkg/escrow"
	"github.com/vctt94/pokerbisonrelay/pkg/poker"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
	"github.com/vctt94/pokerbisonrelay/pkg/server/internal/db"
)

// mockDB satisfies Database for tests that don't exercise persistence paths.
type mockDB struct{}

func (m *mockDB) Close() error { return nil }

func (m *mockDB) UpsertSnapshot(context.Context, db.Snapshot) error               { return nil }
func (m *mockDB) UpsertMatchCheckpoint(context.Context, db.MatchCheckpoint) error { return nil }
func (m *mockDB) GetSnapshot(context.Context, string) (*db.Snapshot, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockDB) GetMatchCheckpoint(context.Context, string) (*db.MatchCheckpoint, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockDB) DeleteMatchCheckpoint(context.Context, string) error   { return nil }
func (m *mockDB) UpsertTable(context.Context, *poker.TableConfig) error { return nil }
func (m *mockDB) GetTable(context.Context, string) (*db.Table, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockDB) DeleteTable(context.Context, string) error      { return nil }
func (m *mockDB) ListTableIDs(context.Context) ([]string, error) { return nil, nil }
func (m *mockDB) ActiveParticipants(context.Context, string) ([]db.Participant, error) {
	return nil, nil
}
func (m *mockDB) SeatPlayer(context.Context, string, string, int) error { return nil }
func (m *mockDB) UnseatPlayer(context.Context, string, string) error    { return nil }
func (m *mockDB) SetReady(context.Context, string, string, bool) error  { return nil }
func (m *mockDB) UpsertAuthUser(context.Context, string, string) error  { return nil }
func (m *mockDB) GetAuthUserByNickname(context.Context, string) (*db.AuthUser, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockDB) GetAuthUserByUserID(context.Context, string) (*db.AuthUser, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockDB) UpdateAuthUserLastLogin(context.Context, string) error { return nil }
func (m *mockDB) UpdateAuthUserPayoutAddress(context.Context, string, string) error {
	return nil
}
func (m *mockDB) ListAllAuthUsers(context.Context) ([]db.AuthUser, error) {
	return nil, nil
}

// seedRosterTable registers a table whose seats are the escrow roster, since an
// escrow cannot be opened without one.
func seedRosterTable(t *testing.T, s *Server, tableID string, seats int, buyIn uint64) *poker.Table {
	t.Helper()
	table := poker.NewTable(poker.TableConfig{
		ID:         tableID,
		BuyIn:      int64(buyIn),
		MinPlayers: seats,
		MaxPlayers: seats,
		Log:        s.log,
	})
	s.tables.Store(tableID, table)
	return table
}

// seatPlayer sits a player at the table so OpenEscrow accepts them as a member
// of the roster.
func seatPlayer(t *testing.T, table *poker.Table, uidStr string, seat uint32) {
	t.Helper()
	user := poker.NewUser(uidStr, table, &poker.AddUserOptions{Seat: int(seat)})
	require.NoError(t, table.AddUser(user))
}

// seedEscrow seeds an escrow session with funding + presign metadata for tests.
// The escrow only gets an address once every seat of tableID has opened one, so
// callers seed the whole roster before expecting a funded escrow.
func seedEscrow(t *testing.T, s *Server, table *poker.Table, uidStr, token, txid string, seat uint32, amount uint64) (*refereePreSignCtx, string) {
	t.Helper()

	var uid zkidentity.ShortID
	require.NoError(t, uid.FromString(uidStr))
	// register payout + token
	s.TestSeedSession(token, uid, "TsRnk22spGQJTpKFcRBc281rmfNFpywh337", uidStr)
	seatPlayer(t, table, uid.String(), seat)

	priv, _ := secp256k1.GeneratePrivateKey()
	compPub := priv.PubKey().SerializeCompressed()
	openResp, err := s.OpenEscrow(context.Background(), &pokerrpc.OpenEscrowRequest{
		AmountAtoms: amount,
		CsvBlocks:   64,
		Token:       token,
		CompPubkey:  compPub,
		TableId:     table.GetConfig().ID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, openResp.EscrowId)

	s.TestBindEscrowFunding(openResp.EscrowId, txid, 0, amount)

	psCtx := &refereePreSignCtx{
		InputID:            txid + ":0",
		AmountAtoms:        amount,
		RedeemScriptHex:    openResp.RedeemScriptHex,
		DraftHex:           "deadbeef",
		SighashHex:         "0102030405060708090a0b0c0d0e0f00102030405060708090a0b0c0d0e0f00",
		TCompressed:        []byte{0x02, 0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x4b},
		RPrimeCompressed:   []byte{0x02, 0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x7b},
		SPrime32:           bytesFromHex(t, "6c6f63616c2d73702d7363616c61722d746573742d30313233"),
		InputIndex:         seat,
		Branch:             0,
		SeatIndex:          seat,
		OwnerUID:           uid,
		WinnerCandidateUID: uid,
	}
	return psCtx, openResp.EscrowId
}

// applyRosterScripts copies each escrow's deposit script into its presign
// context. The scripts only exist once the last seat has opened, so tests seed
// the whole roster first and then pick them up here.
func applyRosterScripts(t *testing.T, s *Server, ctxs []*refereePreSignCtx, escrowIDs []string) {
	t.Helper()
	for i, id := range escrowIDs {
		s.referee.mu.RLock()
		es := s.referee.escrows[id]
		s.referee.mu.RUnlock()
		require.NotNil(t, es, "escrow %s missing", id)
		snap := es.snapshot()
		require.NotEmpty(t, snap.RedeemScriptHex, "escrow %s has no script; roster never closed", id)
		if ctxs[i] != nil {
			ctxs[i].RedeemScriptHex = snap.RedeemScriptHex
		}
	}
}

func bytesFromHex(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	require.NoError(t, err)
	return b
}

// newTestServerWithState creates a referee-ready server with preloaded maps.
func newTestServerWithState(t *testing.T) *Server {
	logBackend := createTestLogBackend()
	srv := &Server{
		log:           logBackend.Logger("SERVER"),
		logBackend:    logBackend,
		db:            &mockDB{},
		chainParams:   selectChainParams("testnet"),
		adaptorSecret: "",
	}
	srv.auth = newAuthState(srv.db)
	srv.referee = newSchnorrRefereeState(ServerConfig{})
	srv.referee.presigns = make(map[string]map[int32]map[string]*refereePreSignCtx)
	srv.referee.branchGamma = make(map[string]map[int32]string)
	srv.referee.matchEscrows = make(map[string]map[uint32]string)
	srv.referee.settlementEscrows = make(map[string]map[uint32]string)
	srv.referee.escrows = make(map[string]*refereeEscrowSession)
	return srv
}

// TestGetFinalizeBundleSuccess ensures a winning finalize bundle is returned when presigs are present.
func TestGetFinalizeBundleSuccess(t *testing.T) {
	const matchID = "table1|sess1"
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	srv.chainParams = selectChainParams("testnet")

	table := seedRosterTable(t, srv, matchID, 2, amount)
	ctx1, esc1 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000001", "tok1", "aaaa", 0, amount)
	ctx2, esc2 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000002", "tok2", "bbbb", 1, amount)
	applyRosterScripts(t, srv, []*refereePreSignCtx{ctx1, ctx2}, []string{esc1, esc2})

	// bind match escrows
	srv.referee.matchEscrows[matchID] = map[uint32]string{0: esc1, 1: esc2}
	srv.referee.escrows[esc1].SeatIndex = 0
	srv.referee.escrows[esc2].SeatIndex = 1

	// Determine which branch corresponds to table seat 0 using the same
	// logic as GetFinalizeBundle (via seatToBranchIndex).
	branchIndex, err := srv.seatToBranchIndex(matchID, 0)
	require.NoError(t, err)

	// Set up presigns for all branches to ensure the test works regardless of sorting.
	srv.referee.presigns[matchID] = map[int32]map[string]*refereePreSignCtx{
		0: {ctx1.InputID: ctx1, ctx2.InputID: ctx2},
		1: {ctx1.InputID: ctx1, ctx2.InputID: ctx2},
	}
	// Configure gamma so that the branch corresponding to seat 0 has a
	// distinctive value we can assert on, without assuming a specific
	// branch index.
	otherBranch := int32(0)
	if branchIndex == 0 {
		otherBranch = 1
	}
	srv.referee.branchGamma[matchID] = map[int32]string{
		branchIndex: "cafebabe", // branch that pays seat 0
		otherBranch: "gamma-other",
	}

	resp, err := srv.GetFinalizeBundle(context.Background(), &pokerrpc.GetFinalizeBundleRequest{
		MatchId:    matchID,
		WinnerSeat: 0,
	})
	require.NoError(t, err)
	require.Equal(t, matchID, resp.MatchId)
	// Returned branch must match the one mapped from table seat 0.
	require.Equal(t, branchIndex, resp.Branch)
	require.Equal(t, "cafebabe", resp.GammaHex)
	require.Equal(t, ctx1.DraftHex, resp.DraftTxHex)
	require.Len(t, resp.Inputs, 2)
}

// TestGetFinalizeBundleSuccessThreePlayers covers a 3-player match with winner seat 2.
func TestGetFinalizeBundleSuccessThreePlayers(t *testing.T) {
	const matchID = "table1|sess3"
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	srv.chainParams = selectChainParams("testnet")

	table := seedRosterTable(t, srv, matchID, 3, amount)
	ctx1, esc1 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000001", "tok1", "aaaa", 0, amount)
	ctx2, esc2 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000002", "tok2", "bbbb", 1, amount)
	ctx3, esc3 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000003", "tok3", "cccc", 2, amount)
	applyRosterScripts(t, srv, []*refereePreSignCtx{ctx1, ctx2, ctx3}, []string{esc1, esc2, esc3})

	// Set SeatIndex and add to escrows map
	srv.referee.matchEscrows[matchID] = map[uint32]string{0: esc1, 1: esc2, 2: esc3}
	srv.referee.escrows[esc1].SeatIndex = 0
	srv.referee.escrows[esc2].SeatIndex = 1
	srv.referee.escrows[esc3].SeatIndex = 2

	// Determine which branch corresponds to table seat 2 using the same
	// logic as GetFinalizeBundle (via seatToBranchIndex).
	branchIndex, err := srv.seatToBranchIndex(matchID, 2)
	require.NoError(t, err)
	require.GreaterOrEqual(t, branchIndex, int32(0))
	require.Less(t, branchIndex, int32(3))

	// Set up presigns for all branches to ensure the test works regardless of sorting.
	srv.referee.presigns[matchID] = map[int32]map[string]*refereePreSignCtx{
		0: {ctx1.InputID: ctx1, ctx2.InputID: ctx2, ctx3.InputID: ctx3},
		1: {ctx1.InputID: ctx1, ctx2.InputID: ctx2, ctx3.InputID: ctx3},
		2: {ctx1.InputID: ctx1, ctx2.InputID: ctx2, ctx3.InputID: ctx3},
	}
	// Configure gamma so that the branch corresponding to seat 2 has a
	// distinctive value we can assert on, without assuming a specific
	// branch index.
	bg := make(map[int32]string)
	for b := int32(0); b < 3; b++ {
		if b == branchIndex {
			bg[b] = "cafebabe" // branch that pays seat 2
		} else {
			bg[b] = fmt.Sprintf("gamma%d", b)
		}
	}
	srv.referee.branchGamma[matchID] = bg

	resp, err := srv.GetFinalizeBundle(context.Background(), &pokerrpc.GetFinalizeBundleRequest{
		MatchId:    matchID,
		WinnerSeat: 2,
	})
	require.NoError(t, err)
	require.Len(t, resp.Inputs, 3)

	// Verify that the returned branch corresponds to seat 2.
	require.Equal(t, branchIndex, resp.Branch)
	require.Equal(t, "cafebabe", resp.GammaHex, "Should return gamma for the branch that pays seat 2")
}

// TestGetFinalizeBundleUsesFrozenSettlementRoster ensures settlement still
// finalizes against the original presigned field even if the live table roster
// shrinks after a bust-out.
func TestGetFinalizeBundleUsesFrozenSettlementRoster(t *testing.T) {
	const matchID = "table1|sess3-frozen"
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	srv.chainParams = selectChainParams("testnet")

	table := seedRosterTable(t, srv, matchID, 3, amount)
	ctx1, esc1 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000011", "tok11", "aaaa", 0, amount)
	ctx2, esc2 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000012", "tok12", "bbbb", 1, amount)
	ctx3, esc3 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000013", "tok13", "cccc", 2, amount)
	applyRosterScripts(t, srv, []*refereePreSignCtx{ctx1, ctx2, ctx3}, []string{esc1, esc2, esc3})

	srv.referee.matchEscrows[matchID] = map[uint32]string{0: esc1, 1: esc2, 2: esc3}
	srv.referee.escrows[esc1].SeatIndex = 0
	srv.referee.escrows[esc2].SeatIndex = 1
	srv.referee.escrows[esc3].SeatIndex = 2
	srv.freezeSettlementEscrows(matchID, []*refereeEscrowSession{
		srv.referee.escrows[esc1],
		srv.referee.escrows[esc2],
		srv.referee.escrows[esc3],
	})

	expectedBranch, err := srv.seatToBranchIndex(matchID, 2)
	require.NoError(t, err)

	srv.referee.presigns[matchID] = map[int32]map[string]*refereePreSignCtx{
		0: {ctx1.InputID: ctx1, ctx2.InputID: ctx2, ctx3.InputID: ctx3},
		1: {ctx1.InputID: ctx1, ctx2.InputID: ctx2, ctx3.InputID: ctx3},
		2: {ctx1.InputID: ctx1, ctx2.InputID: ctx2, ctx3.InputID: ctx3},
	}
	srv.referee.branchGamma[matchID] = map[int32]string{
		0: "gamma0",
		1: "gamma1",
		2: "gamma2",
	}

	// Simulate the live table pruning the busted player before settlement.
	srv.referee.matchEscrows[matchID] = map[uint32]string{1: esc2, 2: esc3}

	resp, err := srv.GetFinalizeBundle(context.Background(), &pokerrpc.GetFinalizeBundleRequest{
		MatchId:    matchID,
		WinnerSeat: 2,
	})
	require.NoError(t, err)
	require.Equal(t, expectedBranch, resp.Branch)
	require.Len(t, resp.Inputs, 3)
}

// TestGetFinalizeBundleErrors covers common failure cases.
func TestGetFinalizeBundleErrors(t *testing.T) {
	const matchID = "table1|sess1"
	srv := newTestServerWithState(t)

	// Missing match
	_, err := srv.GetFinalizeBundle(context.Background(), &pokerrpc.GetFinalizeBundleRequest{MatchId: matchID, WinnerSeat: 0})
	require.Error(t, err)

	// Seed escrows but no presigs/gamma
	srv.referee.matchEscrows[matchID] = map[uint32]string{0: "esc1", 1: "esc2"}
	srv.referee.escrows["esc1"] = &refereeEscrowSession{EscrowID: "esc1", BoundUTXO: &chainwatcher.EscrowUTXO{Value: 1, Txid: "a", Vout: 0}, CompPubkey: []byte{1}}
	srv.referee.escrows["esc2"] = &refereeEscrowSession{EscrowID: "esc2", BoundUTXO: &chainwatcher.EscrowUTXO{Value: 1, Txid: "b", Vout: 0}, CompPubkey: []byte{1}}

	_, err = srv.GetFinalizeBundle(context.Background(), &pokerrpc.GetFinalizeBundleRequest{MatchId: matchID, WinnerSeat: 0})
	require.Error(t, err)

	// Presigs without gamma
	var uid zkidentity.ShortID
	_ = uid.FromString("0000000000000000000000000000000000000000000000000000000000000001")
	srv.referee.presigns[matchID] = map[int32]map[string]*refereePreSignCtx{0: {"a:0": {InputID: "a:0", DraftHex: "aa", RedeemScriptHex: "bb", TCompressed: []byte{1}, RPrimeCompressed: []byte{2}, SPrime32: []byte{3}, InputIndex: 0, AmountAtoms: 1, OwnerUID: uid, WinnerCandidateUID: uid}}}
	_, err = srv.GetFinalizeBundle(context.Background(), &pokerrpc.GetFinalizeBundleRequest{MatchId: matchID, WinnerSeat: 0})
	require.Error(t, err)
}

// mkEscrow constructs a refereeEscrowSession with the given bound UTXO + payout.
func mkEscrow(t *testing.T, uidStr, txid string, vout uint32, pkScriptHex, redeemHex, payout string, seat uint32) *refereeEscrowSession {
	t.Helper()
	var uid zkidentity.ShortID
	require.NoError(t, uid.FromString(uidStr))
	amount := uint64(1_000_000)
	// Create a 33-byte compressed pubkey (required by readyMatchEscrows)
	compPubkey := make([]byte, 33)
	compPubkey[0] = 0x02 // compressed pubkey prefix
	for i := 1; i < 33; i++ {
		compPubkey[i] = byte(i) // fill with test data
	}
	bound := &chainwatcher.EscrowUTXO{Txid: txid, Vout: vout, Value: amount, PkScriptHex: pkScriptHex}
	return &refereeEscrowSession{
		EscrowID:        txid,
		OwnerUID:        uid,
		SeatIndex:       seat,
		PayoutAddr:      payout,
		PkScriptHex:     pkScriptHex,
		RedeemScriptHex: redeemHex,
		AmountAtoms:     amount,
		BoundUTXO:       cloneUTXO(bound),
		LatestFunding: chainwatcher.DepositUpdate{
			PkScriptHex: pkScriptHex,
			Confs:       escrowRequiredConfirmations,
			UTXOCount:   1,
			OK:          true,
			UTXOs:       []*chainwatcher.EscrowUTXO{bound},
		},
		fundingState: escrowStateReady,
		HadFunding:   true,
		CompPubkey:   compPubkey,
	}
}

func TestBuildWTADraftsDeterministicAndPayout(t *testing.T) {
	s := &Server{
		referee:     newSchnorrRefereeState(ServerConfig{AdaptorSecret: "deadbeef"}),
		chainParams: selectChainParams("testnet"),
	}

	// Two escrows with differing pkScripts and txids; payout goes to seat 1.
	e1 := mkEscrow(t, "0000000000000000000000000000000000000000000000000000000000000001", "aaaa", 0, "51", "51", "TsRnk22spGQJTpKFcRBc281rmfNFpywh337", 0)
	e2 := mkEscrow(t, "0000000000000000000000000000000000000000000000000000000000000002", "bbbb", 1, "52", "52", "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5", 1)

	drafts, err := s.buildWTADrafts("m1", []*refereeEscrowSession{e2, e1}) // reverse order on purpose
	require.NoError(t, err)
	require.Len(t, drafts, 2)

	// Inputs should be sorted deterministically: by pkScript then txid/vout.
	require.Equal(t, "51", drafts[0].Inputs[0].RedeemScriptHex)
	require.Equal(t, "52", drafts[0].Inputs[1].RedeemScriptHex)
	require.Equal(t, uint32(0), drafts[0].Inputs[0].InputIndex)
	require.Equal(t, uint32(1), drafts[0].Inputs[1].InputIndex)

	// Winner seat 1 draft should pay seat 1's payout address.
	winner := drafts[1]
	require.Equal(t, e2.OwnerUID, winner.WinnerUID)
	raw, err := hex.DecodeString(winner.DraftHex)
	require.NoError(t, err)
	tx, err := decodeMsgTx(raw)
	require.NoError(t, err)
	require.Len(t, tx.TxOut, 1)
	// Sum(inputs)=2_000_000, fee=DefaultSettlementFeeAtoms -> output value must match.
	require.Equal(t, int64(2_000_000-DefaultSettlementFeeAtoms), tx.TxOut[0].Value)
}

// TestSettlementBranchIndexBug reproduces the settlement bug where the
// winner's table seat is mapped to a branch index and then treated as a
// table seat again when fetching the finalize bundle. Branches are indexed
// by sorted UTXO order (pkScript, txid, vout), not seat order, so this
// double mapping causes the wrong player to be paid when UTXO order differs
// from table seat order.
func TestSettlementBranchIndexBug(t *testing.T) {
	const matchID = "table1|sess1"
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	srv.referee.adaptorSecret = "deadbeef"
	srv.chainParams = selectChainParams("testnet")

	// Create 3 escrows with txids that will sort differently from seat order:
	// Seat 0: txid "cccc" -> will sort last
	// Seat 1: txid "aaaa" -> will sort first
	// Seat 2: txid "bbbb" -> will sort middle
	// After sorting: seat 1 (aaaa), seat 2 (bbbb), seat 0 (cccc)
	// So branch 0 = seat 1, branch 1 = seat 2, branch 2 = seat 0
	payoutAddrs := []string{
		"TsRnk22spGQJTpKFcRBc281rmfNFpywh337", // seat 0
		"TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5", // seat 1
		"TsnjFNHhZ17TKTLtSdXh9Z91TRHNsEp6N1d", // seat 2
	}

	// Use same pkScriptHex for all so sorting is by txid: aaaa < bbbb < cccc
	// This means after sort: seat 1 (aaaa), seat 2 (bbbb), seat 0 (cccc)
	e0 := mkEscrow(t, "0000000000000000000000000000000000000000000000000000000000000001", "cccc", 0, "51", "51", payoutAddrs[0], 0)
	e1 := mkEscrow(t, "0000000000000000000000000000000000000000000000000000000000000002", "aaaa", 0, "51", "51", payoutAddrs[1], 1)
	e2 := mkEscrow(t, "0000000000000000000000000000000000000000000000000000000000000003", "bbbb", 0, "51", "51", payoutAddrs[2], 2)

	// Bind escrows to match
	srv.referee.matchEscrows[matchID] = map[uint32]string{
		0: e0.EscrowID,
		1: e1.EscrowID,
		2: e2.EscrowID,
	}
	srv.referee.escrows[e0.EscrowID] = e0
	srv.referee.escrows[e1.EscrowID] = e1
	srv.referee.escrows[e2.EscrowID] = e2

	// Build drafts - this will sort escrows by txid: aaaa, bbbb, cccc
	// So branch 0 pays to seat 1, branch 1 pays to seat 2, branch 2 pays to seat 0
	allEscrows, err := srv.readyMatchEscrows(matchID)
	require.NoError(t, err)
	drafts, err := srv.buildWTADrafts(matchID, allEscrows)
	require.NoError(t, err)
	require.Len(t, drafts, 3)
	// Verify the sorting: after sort, order should be seat 1, seat 2, seat 0
	require.Equal(t, uint32(1), drafts[0].Inputs[0].SeatIndex, "first input after sort should be seat 1")
	require.Equal(t, uint32(2), drafts[0].Inputs[1].SeatIndex, "second input after sort should be seat 2")
	require.Equal(t, uint32(0), drafts[0].Inputs[2].SeatIndex, "third input after sort should be seat 0")

	// Verify branch 0 pays to seat 1, branch 1 pays to seat 2, branch 2 pays to seat 0
	require.Equal(t, e1.OwnerUID, drafts[0].WinnerUID, "branch 0 should pay seat 1")
	require.Equal(t, e2.OwnerUID, drafts[1].WinnerUID, "branch 1 should pay seat 2")
	require.Equal(t, e0.OwnerUID, drafts[2].WinnerUID, "branch 2 should pay seat 0")

	// Set up presigns for all branches
	srv.referee.presigns[matchID] = make(map[int32]map[string]*refereePreSignCtx)
	srv.referee.branchGamma[matchID] = make(map[int32]string)
	for branch := int32(0); branch < 3; branch++ {
		srv.referee.branchGamma[matchID][branch] = fmt.Sprintf("gamma%d", branch)
		presigns := make(map[string]*refereePreSignCtx)
		for _, in := range drafts[branch].Inputs {
			presigns[in.InputID] = &refereePreSignCtx{
				InputID:            in.InputID,
				DraftHex:           drafts[branch].DraftHex,
				RedeemScriptHex:    in.RedeemScriptHex,
				RPrimeCompressed:   []byte{0x02, 0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x7b},
				SPrime32:           []byte{0x6c, 0x6f, 0x63, 0x61, 0x6c, 0x2d, 0x73, 0x70, 0x2d, 0x73, 0x63, 0x61, 0x6c, 0x61, 0x72, 0x2d, 0x74, 0x65, 0x73, 0x74, 0x2d, 0x30, 0x31, 0x32, 0x33, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
				InputIndex:         in.InputIndex,
				Branch:             branch,
				SeatIndex:          in.SeatIndex,
				OwnerUID:           in.OwnerUID,
				WinnerCandidateUID: drafts[branch].WinnerUID,
			}
		}
		srv.referee.presigns[matchID][branch] = presigns
	}

	// Winner is at table seat 1. After UTXO sorting, branch 0 pays to seat 1.
	// The correct behavior for the settlement path is that a winner at table
	// seat 1 is ultimately paid via the branch that pays seat 1 (branch 0),
	// even when UTXO sort order differs from seat order.
	winnerTableSeat := int32(1)

	// Map table seat to branch index using the same logic as buildWTADrafts.
	branchIndex, err := srv.seatToBranchIndex(matchID, winnerTableSeat)
	require.NoError(t, err)
	// Given our txid ordering (aaaa, bbbb, cccc), seat 1 should map to branch 0.
	require.Equal(t, int32(0), branchIndex, "table seat 1 should map to branch 0")

	// Call GetFinalizeBundle with the winner's TABLE seat (not branch index).
	// This must internally map to the correct branch (0) and pay seat 1.
	bundle, err := srv.GetFinalizeBundle(context.Background(), &pokerrpc.GetFinalizeBundleRequest{
		MatchId:    matchID,
		WinnerSeat: winnerTableSeat,
	})
	require.NoError(t, err)

	// CORRECT behavior: settlement for a winner at table seat 1 must use the
	// branch that pays seat 1 (branch 0).
	require.Equal(t, int32(0), bundle.Branch,
		"winner at table seat 1 should ultimately use branch 0 (which pays seat 1)")

	// Decode the draft transaction to verify it pays to the correct winner.
	draftBytes, err := hex.DecodeString(bundle.DraftTxHex)
	require.NoError(t, err)
	tx, err := decodeMsgTx(draftBytes)
	require.NoError(t, err)
	require.Len(t, tx.TxOut, 1)

	// Get the payout script for seat 1 (the actual winner)
	winnerAddr, err := stdaddr.DecodeAddress(payoutAddrs[1], srv.chainParams)
	require.NoError(t, err)
	_, winnerScript := winnerAddr.PaymentScript()

	// CORRECT: Transaction should pay to seat 1 (the winner).
	// With the current buggy settlement path, this pays a different seat.
	require.Equal(t, winnerScript, tx.TxOut[0].PkScript,
		"transaction should pay to seat 1 (the winner), but currently pays to the wrong address")
}

// TestCleanupMatchState ensures match cleanup marks escrows spent and clears
// per-match bookkeeping so the same UTXO cannot be reused.
func TestCleanupMatchState(t *testing.T) {
	const matchID = "cleanup-match"
	const amount = uint64(2_000_000)

	srv := newTestServerWithState(t)

	table := seedRosterTable(t, srv, matchID, 2, amount)
	_, esc1 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000001", "tok1", "aaaa", 0, amount)
	_, esc2 := seedEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000002", "tok2", "bbbb", 1, amount)

	// Bind escrows to the match and seed presign metadata.
	srv.referee.matchEscrows[matchID] = map[uint32]string{0: esc1, 1: esc2}
	srv.referee.presigns[matchID] = map[int32]map[string]*refereePreSignCtx{
		0: {"aaaa:0": {InputID: "aaaa:0"}},
	}
	srv.referee.branchGamma[matchID] = map[int32]string{0: "gamma"}
	srv.referee.presignComplete[matchID] = map[uint32]bool{0: true, 1: true}

	// Record table metadata and watcher unsubscribe hooks so cleanup can clear them.
	es1 := srv.referee.escrows[esc1]
	es2 := srv.referee.escrows[esc2]
	es1.TableID = matchID
	es1.SeatIndex = 0
	es2.TableID = matchID
	es2.SeatIndex = 1

	var unsub1Called, unsub2Called bool
	es1.WatcherUnsub = func() { unsub1Called = true }
	es2.WatcherUnsub = func() { unsub2Called = true }

	srv.cleanupMatchState(matchID)

	_, ok := srv.referee.matchEscrows[matchID]
	require.False(t, ok, "match escrows should be cleared")
	_, ok = srv.referee.presigns[matchID]
	require.False(t, ok, "presigns should be cleared")
	_, ok = srv.referee.branchGamma[matchID]
	require.False(t, ok, "branch gamma should be cleared")
	_, ok = srv.referee.presignComplete[matchID]
	require.False(t, ok, "presign completion should be cleared")

	require.True(t, unsub1Called, "watcher unsubscribe should run for escrow 1")
	require.True(t, unsub2Called, "watcher unsubscribe should run for escrow 2")

	es1.mu.RLock()
	require.Empty(t, es1.TableID)
	require.Zero(t, es1.SeatIndex)
	require.Nil(t, es1.BoundUTXO)
	es1.mu.RUnlock()
	_, err := ensureBoundFunding(es1)
	require.ErrorContains(t, err, "spent")

	es2.mu.RLock()
	require.Empty(t, es2.TableID)
	require.Zero(t, es2.SeatIndex)
	require.Nil(t, es2.BoundUTXO)
	es2.mu.RUnlock()
	_, err = ensureBoundFunding(es2)
	require.ErrorContains(t, err, "spent")
}

// openRosterEscrow is the OpenEscrow call a seated player makes.
func openRosterEscrow(t *testing.T, s *Server, table *poker.Table, uidStr, token string, seat uint32, amount uint64, pub []byte) *pokerrpc.OpenEscrowResponse {
	t.Helper()
	var uid zkidentity.ShortID
	require.NoError(t, uid.FromString(uidStr))
	s.TestSeedSession(token, uid, "TsRnk22spGQJTpKFcRBc281rmfNFpywh337", uidStr)
	if table.GetUser(uid.String()) == nil {
		seatPlayer(t, table, uid.String(), seat)
	}
	resp, err := s.OpenEscrow(context.Background(), &pokerrpc.OpenEscrowRequest{
		AmountAtoms: amount,
		CsvBlocks:   64,
		Token:       token,
		CompPubkey:  pub,
		TableId:     table.GetConfig().ID,
	})
	require.NoError(t, err)
	return resp
}

func testSessionKey(t *testing.T) []byte {
	t.Helper()
	priv, err := secp256k1.GeneratePrivateKey()
	require.NoError(t, err)
	return priv.PubKey().SerializeCompressed()
}

// A deposit address commits to every seat's key, so it cannot exist before the
// last seat has published one. Handing one out early would let a player fund a
// script that a later arrival then changes out from under them.
func TestOpenEscrowWithholdsAddressUntilRosterCloses(t *testing.T) {
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	table := seedRosterTable(t, srv, "roster-table", 2, amount)

	pubA := testSessionKey(t)
	first := openRosterEscrow(t, srv, table,
		"0000000000000000000000000000000000000000000000000000000000000001", "tokA", 0, amount, pubA)
	require.False(t, first.GetRosterReady(), "roster cannot be ready with a seat still empty")
	require.EqualValues(t, 1, first.GetSeatsPending())
	require.Empty(t, first.GetDepositAddr(), "no address may be issued before the roster closes")
	require.Empty(t, first.GetRedeemScriptHex())
	require.Empty(t, first.GetPkScriptHex())
	require.NotEmpty(t, first.GetEscrowId())

	second := openRosterEscrow(t, srv, table,
		"0000000000000000000000000000000000000000000000000000000000000002", "tokB", 1, amount, testSessionKey(t))
	require.True(t, second.GetRosterReady(), "the last seat to open closes the roster")
	require.Zero(t, second.GetSeatsPending())
	require.NotEmpty(t, second.GetDepositAddr())
	require.Len(t, second.GetMemberPubkeys(), 2)

	// The roster closes for everyone at once, so the first player picks up the
	// same address by asking again rather than opening a second escrow.
	again := openRosterEscrow(t, srv, table,
		"0000000000000000000000000000000000000000000000000000000000000001", "tokA", 0, amount, pubA)
	require.Equal(t, first.GetEscrowId(), again.GetEscrowId(), "reopening must not mint a new escrow")
	require.True(t, again.GetRosterReady())
	require.NotEmpty(t, again.GetDepositAddr())

	// Each member funds a different address: the settlement branch is shared,
	// but the refund branch names only its owner.
	require.NotEqual(t, again.GetDepositAddr(), second.GetDepositAddr())
	require.Equal(t, again.GetMemberPubkeys(), second.GetMemberPubkeys(),
		"every member's script commits to the same roster")
}

// The script the referee hands back has to be the one the roster implies, or
// clients rebuilding it locally would reject it.
func TestOpenEscrowScriptMatchesRoster(t *testing.T) {
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	table := seedRosterTable(t, srv, "roster-script", 2, amount)

	pubA, pubB := testSessionKey(t), testSessionKey(t)
	openRosterEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000001", "tokA", 0, amount, pubA)
	resp := openRosterEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000002", "tokB", 1, amount, pubB)
	require.True(t, resp.GetRosterReady())

	want, err := escrow.RedeemScript(pubB, [][]byte{pubA, pubB}, 64)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want), resp.GetRedeemScriptHex())

	n, err := escrow.MemberCount(want)
	require.NoError(t, err)
	require.Equal(t, 2, n, "settlement must take a signature from every member")

	_, addr, err := pkScriptAndAddrFromRedeem(want, srv.chainParams)
	require.NoError(t, err)
	require.Equal(t, addr, resp.GetDepositAddr())
}

// Once the roster is closed the keys in it are load-bearing: other members may
// already have funded scripts naming them, so they can no longer be swapped.
func TestOpenEscrowRejectsKeyChangeAfterRosterCloses(t *testing.T) {
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	table := seedRosterTable(t, srv, "roster-frozen", 2, amount)

	openRosterEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000001", "tokA", 0, amount, testSessionKey(t))
	openRosterEscrow(t, srv, table, "0000000000000000000000000000000000000000000000000000000000000002", "tokB", 1, amount, testSessionKey(t))

	_, err := srv.OpenEscrow(context.Background(), &pokerrpc.OpenEscrowRequest{
		AmountAtoms: amount,
		CsvBlocks:   64,
		Token:       "tokA",
		CompPubkey:  testSessionKey(t),
		TableId:     "roster-frozen",
	})
	require.ErrorContains(t, err, "closed roster")
}

func TestOpenEscrowRequiresSeatAndBuyIn(t *testing.T) {
	const amount = uint64(1_000_000)
	srv := newTestServerWithState(t)
	table := seedRosterTable(t, srv, "roster-gate", 2, amount)

	var uid zkidentity.ShortID
	require.NoError(t, uid.FromString("0000000000000000000000000000000000000000000000000000000000000009"))
	srv.TestSeedSession("tokX", uid, "TsRnk22spGQJTpKFcRBc281rmfNFpywh337", uid.String())

	// A player who holds no seat is not part of the roster.
	_, err := srv.OpenEscrow(context.Background(), &pokerrpc.OpenEscrowRequest{
		AmountAtoms: amount,
		CsvBlocks:   64,
		Token:       "tokX",
		CompPubkey:  testSessionKey(t),
		TableId:     "roster-gate",
	})
	require.ErrorContains(t, err, "not seated")

	// Winner-take-all is only fair across equal stakes.
	seatPlayer(t, table, uid.String(), 0)
	_, err = srv.OpenEscrow(context.Background(), &pokerrpc.OpenEscrowRequest{
		AmountAtoms: amount + 1,
		CsvBlocks:   64,
		Token:       "tokX",
		CompPubkey:  testSessionKey(t),
		TableId:     "roster-gate",
	})
	require.ErrorContains(t, err, "buy-in")

	// And an escrow has to name a table at all.
	_, err = srv.OpenEscrow(context.Background(), &pokerrpc.OpenEscrowRequest{
		AmountAtoms: amount,
		CsvBlocks:   64,
		Token:       "tokX",
		CompPubkey:  testSessionKey(t),
	})
	require.ErrorContains(t, err, "table_id required")
}
