package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/companyzero/bisonrelay/zkidentity"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/stretchr/testify/require"
	testenv "github.com/vctt94/pokerbisonrelay/e2e/internal/testenv"
	"github.com/vctt94/pokerbisonrelay/pkg/client"
	"github.com/vctt94/pokerbisonrelay/pkg/rpc/grpc/pokerrpc"
	"github.com/vctt94/pokerbisonrelay/pkg/server"
)

func settlementTestBuyIn() uint64 {
	// Ensure at least two seats cover the fixed settlement fee with some buffer.
	return server.DefaultSettlementFeeAtoms/2 + 1_000
}

// openRosterEscrows opens one escrow per seated player and returns them with
// their deposit scripts filled in.
//
// The escrow script names every seat at the table, so no deposit address exists
// until the last player has opened. Everyone but that last player therefore
// gets an escrow id and nothing else on the first pass, and asks again once the
// roster has closed - which is what a client does in the field, and what the
// second loop here stands in for.
func openRosterEscrows(t *testing.T, ctx context.Context, tableID string, amount uint64, csvBlocks uint32, refs []*client.RefereeClient, pubs [][]byte) []*pokerrpc.OpenEscrowResponse {
	t.Helper()

	out := make([]*pokerrpc.OpenEscrowResponse, len(refs))
	for i, ref := range refs {
		resp, err := ref.OpenEscrow(ctx, tableID, "", amount, csvBlocks, pubs[i])
		require.NoError(t, err, "seat %d should be able to open an escrow", i)
		require.NotEmpty(t, resp.EscrowId)
		out[i] = resp
	}
	for i, ref := range refs {
		if out[i].GetRosterReady() {
			continue
		}
		resp, err := ref.OpenEscrow(ctx, tableID, "", amount, csvBlocks, pubs[i])
		require.NoError(t, err)
		require.True(t, resp.GetRosterReady(),
			"roster should be closed for seat %d once every seat has opened", i)
		require.Equal(t, out[i].EscrowId, resp.EscrowId,
			"reopening a seat's escrow must return the same escrow, not a new one")
		require.NotEmpty(t, resp.DepositAddr)
		out[i] = resp
	}
	return out
}

// TestRefereePresignFlow exercises the client referee helper through the UI stubs:
// - two players login, open escrow with session key, bind to match/table/seat via SettlementHello,
// - presign completes for both branches.
func TestRefereePresignFlow(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	defer env.Close()

	ctx := context.Background()

	buyIn := settlementTestBuyIn()

	// Create table (2 players) using alice as host.
	tableID := env.CreateTableWithBuyIn(ctx, "alice", 2, 2, int64(buyIn))

	// Ensure auth sessions with payout addresses for alice and bob.
	alicePayout := "TsRnk22spGQJTpKFcRBc281rmfNFpywh337"
	bobPayout := "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5"

	aliceToken := env.EnsureTestSession(ctx, "alice", "alice")
	bobToken := env.EnsureTestSession(ctx, "bob", "bob")

	var aliceUID zkidentity.ShortID
	_ = aliceUID.FromString(testenv.PlayerIDToShortIDString("alice"))
	env.PokerSrv.TestSeedSession(aliceToken, aliceUID, alicePayout, "alice")

	var bobUID zkidentity.ShortID
	_ = bobUID.FromString(testenv.PlayerIDToShortIDString("bob"))
	env.PokerSrv.TestSeedSession(bobToken, bobUID, bobPayout, "bob")

	// Create PokerClients on the same conn.
	logBackend := testenv.NewLogBackend()
	pcAlice, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: alicePayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)
	pcBob, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: bobPayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)

	// Stub login tokens: use existing ResumeSession which returns nil token; set manually.
	pcAliceToken := aliceToken
	pcBobToken := bobToken

	// Generate session keys for escrow.
	alicePriv, _ := secp256k1.GeneratePrivateKey()
	alicePub := alicePriv.PubKey().SerializeCompressed()
	bobPriv, _ := secp256k1.GeneratePrivateKey()
	bobPub := bobPriv.PubKey().SerializeCompressed()

	// Ensure both players are seated at the table via normal lobby flow.
	_, err = env.JoinTable(ctx, "bob", tableID)
	require.NoError(t, err, "bob should be able to join table")

	// Open escrows (no referee binding yet).
	refAlice := pcAlice.Referee(pcAliceToken)
	refBob := pcBob.Referee(pcBobToken)
	amount := buyIn // Must match table buy-in for referee binding
	escrows := openRosterEscrows(t, ctx, tableID, amount, 64,
		[]*client.RefereeClient{refAlice, refBob}, [][]byte{alicePub, bobPub})
	escrowA, escrowB := escrows[0], escrows[1]

	// Manually mark escrows as funded/bound (chainwatcher not exercised in test).
	env.PokerSrv.TestBindEscrowFunding(escrowA.EscrowId, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 0, amount)
	env.PokerSrv.TestBindEscrowFunding(escrowB.EscrowId, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 0, amount)

	// Bind and presign for a match (poker: matchID == tableID).
	matchID := tableID

	// Run presign concurrently for both players.
	errCh := make(chan error, 2)
	runPresign := func(ref *client.RefereeClient, seat uint32, escrowID string, pub []byte, privHex string) {
		const retries = 10
		var err error
		for i := 0; i < retries; i++ {
			err = ref.StartPresign(ctx, matchID, tableID, escrowID, pub, privHex)
			if err == nil {
				errCh <- nil
				return
			}
			if strings.Contains(err.Error(), "match seats not filled") {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("presign retries exhausted: %w", err)
	}
	go runPresign(refAlice, 0, escrowA.EscrowId, alicePub, hex.EncodeToString(alicePriv.Serialize()))
	go runPresign(refBob, 1, escrowB.EscrowId, bobPub, hex.EncodeToString(bobPriv.Serialize()))

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("presign timed out")
	}
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("presign timed out (second)")
	}

	expectedBranch, err := env.PokerSrv.BranchIndexForSeat(matchID, 0)
	require.NoError(t, err)

	// Winner (alice) fetches finalize bundle for seat 0.
	bundle, err := refAlice.GetFinalizeBundle(ctx, matchID, 0)
	require.NoError(t, err)
	require.Equal(t, expectedBranch, bundle.Branch)
	assertFinalizeBundle(t, bundle, matchID, expectedBranch, []string{"TsRnk22spGQJTpKFcRBc281rmfNFpywh337", "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5"}, amount, 2)
}

// TestRefereePresignFlowSixPlayers exercises presign/finalize with a full 6-max table.
func TestRefereePresignFlowSixPlayers(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	defer env.Close()

	ctx := context.Background()

	players := []string{"p1", "p2", "p3", "p4", "p5", "p6"}
	payouts := []string{
		"TsnjFNHhZ17TKTLtSdXh9Z91TRHNsEp6N1d",
		"TsoxGYvsyhVooBMazDcntmjFq3ZpQCWMNCc",
		"Tsmy2RLwSbTsqmmSrmf6ma8Vsea8UAoZxUX",
		"TscMmNEjrniey3KukDh2ZDfVaVVVB6V6kYX",
		"TshxcBJTirEyYMZzL3ggP7jos8C16S64g2t",
		"TshjJ9kX7of5Jc1MihARYftaYqMp9dwnifW",
	}

	buyIn := settlementTestBuyIn()
	tableID := env.CreateTableWithBuyIn(ctx, "p1", 6, 6, int64(buyIn))
	// Poker: matchID == tableID (no session suffix).
	matchID := tableID

	logBackend := testenv.NewLogBackend()
	amount := buyIn // Must match table buy-in for referee binding
	type seatClient struct {
		ref      *client.RefereeClient
		pub      []byte
		privHex  string
		escrowID string
		seat     uint32
	}
	var seats []seatClient

	// Seed sessions with payout addresses and join all players to the table.
	for i, p := range players {
		token := env.EnsureTestSession(ctx, p, p)
		shortIDStr := testenv.PlayerIDToShortIDString(p)
		var uidShort zkidentity.ShortID
		_ = uidShort.FromString(shortIDStr)
		env.PokerSrv.TestSeedSession(token, uidShort, payouts[i], p)

		_, err := env.JoinTable(ctx, p, tableID)
		require.NoError(t, err, "player %s should be able to join table", p)
	}

	for i, p := range players {
		pc, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
			Datadir:       t.TempDir(),
			PayoutAddress: payouts[i],
			LogBackend:    logBackend,
			Notifications: client.NewNotificationManager(),
		}, env.DialTarget(), env.DialOptions()...)
		require.NoError(t, err)

		priv, _ := secp256k1.GeneratePrivateKey()
		pub := priv.PubKey().SerializeCompressed()
		token := env.EnsureTestSession(ctx, p, p)
		ref := pc.Referee(token)

		seats = append(seats, seatClient{
			ref:     ref,
			pub:     pub,
			privHex: hex.EncodeToString(priv.Serialize()),
			seat:    uint32(i),
		})
	}

	// Escrows open as a roster: nobody has a deposit address until the last
	// seat has opened, so funding only starts once they all have.
	refs := make([]*client.RefereeClient, len(seats))
	pubs := make([][]byte, len(seats))
	for i, sc := range seats {
		refs[i], pubs[i] = sc.ref, sc.pub
	}
	for i, esc := range openRosterEscrows(t, ctx, tableID, amount, 64, refs, pubs) {
		seats[i].escrowID = esc.EscrowId
		env.PokerSrv.TestBindEscrowFunding(esc.EscrowId, fmt.Sprintf("%064x", i+1), 0, amount)
	}

	errCh := make(chan error, len(seats))
	runPresign := func(sc seatClient) {
		const retries = 20
		var err error
		for i := 0; i < retries; i++ {
			err = sc.ref.StartPresign(ctx, matchID, tableID, sc.escrowID, sc.pub, sc.privHex)
			if err == nil {
				errCh <- nil
				return
			}
			if strings.Contains(err.Error(), "match seats not filled") {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("presign retries exhausted: %w", err)
	}
	for _, sc := range seats {
		go runPresign(sc)
	}
	for i := 0; i < len(seats); i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("presign timed out (%d)", i)
		}
	}

	winnerSeat := int32(3)
	expectedBranch, err := env.PokerSrv.BranchIndexForSeat(matchID, winnerSeat)
	require.NoError(t, err)

	// Winner seat 3 fetches finalize bundle.
	bundle, err := seats[winnerSeat].ref.GetFinalizeBundle(ctx, matchID, winnerSeat)
	require.NoError(t, err)
	require.Equal(t, expectedBranch, bundle.Branch)
	assertFinalizeBundle(t, bundle, matchID, expectedBranch, payouts, amount, len(seats))
}

// assertFinalizeBundle verifies structural correctness of the finalize bundle.
func assertFinalizeBundle(t *testing.T, bundle *pokerrpc.GetFinalizeBundleResponse, matchID string, winnerSeat int32, payoutAddrs []string, perSeatAmt uint64, seats int) {
	t.Helper()

	require.Equal(t, matchID, bundle.MatchId)
	require.Equal(t, winnerSeat, bundle.Branch)
	require.NotEmpty(t, bundle.DraftTxHex)
	require.NotEmpty(t, bundle.GammaHex)
	require.Len(t, bundle.Inputs, seats)

	draftBytes, err := hex.DecodeString(bundle.DraftTxHex)
	require.NoError(t, err, "decode draft hex")
	var tx wire.MsgTx
	require.NoError(t, tx.Deserialize(bytes.NewReader(draftBytes)), "deserialize draft tx")
	require.Len(t, tx.TxIn, seats)
	require.Len(t, tx.TxOut, 1)

	scripts := make(map[string][]byte)
	for _, pa := range payoutAddrs {
		addr, err := stdaddr.DecodeAddress(pa, chaincfg.TestNet3Params())
		require.NoError(t, err)
		_, payScript := addr.PaymentScript()
		scripts[pa] = payScript
	}
	var matched bool
	for _, ps := range scripts {
		if bytes.Equal(ps, tx.TxOut[0].PkScript) {
			matched = true
			break
		}
	}
	require.True(t, matched, "tx output not paying any expected payout address")

	totalIn := perSeatAmt * uint64(seats)
	require.EqualValues(t, int64(totalIn-server.DefaultSettlementFeeAtoms), tx.TxOut[0].Value)

	inputByIdx := make(map[uint32]*pokerrpc.FinalizeInput, len(bundle.Inputs))
	for _, in := range bundle.Inputs {
		require.NotEmpty(t, in.InputId)
		require.NotEmpty(t, in.RPrimeCompactHex)
		require.NotEmpty(t, in.SPrimeHex)
		require.NotEmpty(t, in.RedeemScriptHex)
		inputByIdx[in.InputIndex] = in
	}
	require.Len(t, inputByIdx, seats)

	for i, txIn := range tx.TxIn {
		in, ok := inputByIdx[uint32(i)]
		require.True(t, ok, "missing input %d", i)
		require.Equal(t, txIn.PreviousOutPoint.String(), in.InputId)
	}

	require.EqualValues(t, perSeatAmt*uint64(seats), totalIn)
}

// TestGetFinalizeBundleForWinner tests that a winner can retrieve the finalize bundle
// with gamma after presign is complete for all branches.
// This verifies the settlement flow works correctly for different winner seats.
func TestGetFinalizeBundleForWinner(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	defer env.Close()

	ctx := context.Background()

	buyIn := settlementTestBuyIn()

	// Create table (2 players).
	tableID := env.CreateTableWithBuyIn(ctx, "alice", 2, 2, int64(buyIn))

	// Seed auth sessions with tokens and payout addresses using consistent ShortIDs.
	alicePayout := "TsRnk22spGQJTpKFcRBc281rmfNFpywh337"
	bobPayout := "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5"

	aliceToken := env.EnsureTestSession(ctx, "alice", "alice")
	bobToken := env.EnsureTestSession(ctx, "bob", "bob")

	var aliceUID zkidentity.ShortID
	_ = aliceUID.FromString(testenv.PlayerIDToShortIDString("alice"))
	env.PokerSrv.TestSeedSession(aliceToken, aliceUID, alicePayout, "alice")

	var bobUID zkidentity.ShortID
	_ = bobUID.FromString(testenv.PlayerIDToShortIDString("bob"))
	env.PokerSrv.TestSeedSession(bobToken, bobUID, bobPayout, "bob")

	// Create PokerClients.
	logBackend := testenv.NewLogBackend()
	pcAlice, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: alicePayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)
	pcBob, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: bobPayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)

	pcAliceToken := aliceToken
	pcBobToken := bobToken

	// Generate session keys for escrow.
	alicePriv, _ := secp256k1.GeneratePrivateKey()
	alicePub := alicePriv.PubKey().SerializeCompressed()
	bobPriv, _ := secp256k1.GeneratePrivateKey()
	bobPub := bobPriv.PubKey().SerializeCompressed()

	// Seat both players first: the escrow roster is the table's seats, so a
	// player has to hold one before they can escrow against it.
	_, err = env.JoinTable(ctx, "bob", tableID)
	require.NoError(t, err, "bob should be able to join table")

	// Open escrows.
	refAlice := pcAlice.Referee(pcAliceToken)
	refBob := pcBob.Referee(pcBobToken)
	escrows := openRosterEscrows(t, ctx, tableID, buyIn, 64,
		[]*client.RefereeClient{refAlice, refBob}, [][]byte{alicePub, bobPub})
	escrowA, escrowB := escrows[0], escrows[1]

	// Manually mark escrows as funded/bound.
	env.PokerSrv.TestBindEscrowFunding(escrowA.EscrowId, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 0, buyIn)
	env.PokerSrv.TestBindEscrowFunding(escrowB.EscrowId, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 0, buyIn)

	// Poker: matchID == tableID.
	matchID := tableID

	// Ensure both players are seated at the table via normal lobby flow.
	_, err = env.JoinTable(ctx, "bob", tableID)
	require.NoError(t, err, "bob should be able to join table")

	// Run presign concurrently for both players.
	errCh := make(chan error, 2)
	runPresign := func(ref *client.RefereeClient, seat uint32, escrowID string, pub []byte, privHex string) {
		const retries = 15
		var err error
		for i := 0; i < retries; i++ {
			err = ref.StartPresign(ctx, matchID, tableID, escrowID, pub, privHex)
			if err == nil {
				errCh <- nil
				return
			}
			if strings.Contains(err.Error(), "match seats not filled") {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("presign retries exhausted: %w", err)
	}
	go runPresign(refAlice, 0, escrowA.EscrowId, alicePub, hex.EncodeToString(alicePriv.Serialize()))
	go runPresign(refBob, 1, escrowB.EscrowId, bobPub, hex.EncodeToString(bobPriv.Serialize()))

	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "presign failed")
		case <-time.After(5 * time.Second):
			t.Fatal("presign timed out")
		}
	}
	t.Log("✓ Presign completed for both players")

	// Test: Alice wins (seat 0)
	t.Run("AliceWins", func(t *testing.T) {
		winnerSeat := int32(0)
		bundle, err := refAlice.GetFinalizeBundle(ctx, matchID, winnerSeat)
		require.NoError(t, err, "GetFinalizeBundle should succeed for winner seat 0")

		// Verify finalize bundle structure.
		require.Equal(t, matchID, bundle.MatchId)
		expectedBranch, err := env.PokerSrv.BranchIndexForSeat(matchID, winnerSeat)
		require.NoError(t, err, "BranchIndexForSeat should succeed")
		require.Equal(t, expectedBranch, bundle.Branch)
		require.NotEmpty(t, bundle.DraftTxHex, "DraftTxHex should be present")
		require.NotEmpty(t, bundle.GammaHex, "GammaHex (adaptor secret) should be present")
		require.Len(t, bundle.Inputs, 2, "Should have presigs for both inputs")

		// Verify gamma is 32 bytes hex (64 chars).
		gammaBytes, err := hex.DecodeString(bundle.GammaHex)
		require.NoError(t, err, "GammaHex should be valid hex")
		require.Len(t, gammaBytes, 32, "Gamma should be 32 bytes")

		// Verify each input has presig data.
		for i, in := range bundle.Inputs {
			require.NotEmpty(t, in.InputId, "Input %d should have InputId", i)
			require.NotEmpty(t, in.RPrimeCompactHex, "Input %d should have R'", i)
			require.NotEmpty(t, in.SPrimeHex, "Input %d should have s'", i)
			require.NotEmpty(t, in.RedeemScriptHex, "Input %d should have redeem script", i)
		}

		t.Logf("✓ Alice (seat 0) can retrieve finalize bundle with gamma: %s...", bundle.GammaHex[:16])
	})

	// Test: Bob wins (seat 1)
	t.Run("BobWins", func(t *testing.T) {
		winnerSeat := int32(1)
		bundle, err := refBob.GetFinalizeBundle(ctx, matchID, winnerSeat)
		require.NoError(t, err, "GetFinalizeBundle should succeed for winner seat 1")

		// Verify finalize bundle structure.
		require.Equal(t, matchID, bundle.MatchId)
		expectedBranch, err := env.PokerSrv.BranchIndexForSeat(matchID, winnerSeat)
		require.NoError(t, err, "BranchIndexForSeat should succeed")
		require.Equal(t, expectedBranch, bundle.Branch)
		require.NotEmpty(t, bundle.DraftTxHex, "DraftTxHex should be present")
		require.NotEmpty(t, bundle.GammaHex, "GammaHex (adaptor secret) should be present")
		require.Len(t, bundle.Inputs, 2, "Should have presigs for both inputs")

		// Verify gamma is 32 bytes hex (64 chars).
		gammaBytes, err := hex.DecodeString(bundle.GammaHex)
		require.NoError(t, err, "GammaHex should be valid hex")
		require.Len(t, gammaBytes, 32, "Gamma should be 32 bytes")

		// Verify each input has presig data.
		for i, in := range bundle.Inputs {
			require.NotEmpty(t, in.InputId, "Input %d should have InputId", i)
			require.NotEmpty(t, in.RPrimeCompactHex, "Input %d should have R'", i)
			require.NotEmpty(t, in.SPrimeHex, "Input %d should have s'", i)
			require.NotEmpty(t, in.RedeemScriptHex, "Input %d should have redeem script", i)
		}

		t.Logf("✓ Bob (seat 1) can retrieve finalize bundle with gamma: %s...", bundle.GammaHex[:16])
	})

	// Verify different gammas for different branches (important for security)
	t.Run("DifferentGammasPerBranch", func(t *testing.T) {
		bundleA, err := refAlice.GetFinalizeBundle(ctx, matchID, 0)
		require.NoError(t, err)
		bundleB, err := refBob.GetFinalizeBundle(ctx, matchID, 1)
		require.NoError(t, err)

		require.NotEqual(t, bundleA.GammaHex, bundleB.GammaHex,
			"Different branches should have different gamma values")
		require.NotEqual(t, bundleA.DraftTxHex, bundleB.DraftTxHex,
			"Different branches should have different draft transactions")

		t.Log("✓ Each branch has unique gamma and draft tx")
	})
}

// TestSettlementMatchIDFromTable verifies that the table correctly provides
// the matchID for settlement when a game ends.
//
// For WTA poker, the tableID itself is the matchID (a random 16-byte hex string).
// This simplifies the design - no sessionID tracking needed.
func TestSettlementMatchIDFromTable(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	defer env.Close()

	ctx := context.Background()

	buyIn := settlementTestBuyIn()

	tableID := env.CreateTableWithBuyIn(ctx, "alice", 2, 2, int64(buyIn))

	// Verify tableID is now hex format (32 chars = 16 bytes)
	require.Len(t, tableID, 32, "tableID should be 32 hex chars (16 bytes)")
	t.Logf("Table created with hex ID: %s", tableID)

	// Ensure auth sessions with payout addresses for alice and bob.
	alicePayout := "TsRnk22spGQJTpKFcRBc281rmfNFpywh337"
	bobPayout := "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5"

	aliceToken := env.EnsureTestSession(ctx, "alice", "alice")
	bobToken := env.EnsureTestSession(ctx, "bob", "bob")

	var aliceUID zkidentity.ShortID
	_ = aliceUID.FromString(testenv.PlayerIDToShortIDString("alice"))
	env.PokerSrv.TestSeedSession(aliceToken, aliceUID, alicePayout, "alice")

	var bobUID zkidentity.ShortID
	_ = bobUID.FromString(testenv.PlayerIDToShortIDString("bob"))
	env.PokerSrv.TestSeedSession(bobToken, bobUID, bobPayout, "bob")

	logBackend := testenv.NewLogBackend()
	pcAlice, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: alicePayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)
	pcBob, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: bobPayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)

	alicePriv, _ := secp256k1.GeneratePrivateKey()
	alicePub := alicePriv.PubKey().SerializeCompressed()
	bobPriv, _ := secp256k1.GeneratePrivateKey()
	bobPub := bobPriv.PubKey().SerializeCompressed()

	// Seat both players via the normal lobby flow (alice is host; bob joins)
	// before opening escrows: the roster is the table's seats.
	_, err = env.JoinTable(ctx, "bob", tableID)
	require.NoError(t, err, "bob should be able to join table")

	refAlice := pcAlice.Referee(aliceToken)
	refBob := pcBob.Referee(bobToken)
	escrows := openRosterEscrows(t, ctx, tableID, buyIn, 64,
		[]*client.RefereeClient{refAlice, refBob}, [][]byte{alicePub, bobPub})
	escrowA, escrowB := escrows[0], escrows[1]

	env.PokerSrv.TestBindEscrowFunding(escrowA.EscrowId, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 0, buyIn)
	env.PokerSrv.TestBindEscrowFunding(escrowB.EscrowId, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 0, buyIn)

	// For WTA poker, matchID = tableID (no sessionID suffix needed)
	matchID := tableID

	errCh := make(chan error, 2)
	runPresign := func(ref *client.RefereeClient, seat uint32, escrowID string, pub []byte, privHex string) {
		const retries = 15
		var err error
		for i := 0; i < retries; i++ {
			// Use tableID as both matchID and tableID; sessionID can be empty
			err = ref.StartPresign(ctx, matchID, tableID, escrowID, pub, privHex)
			if err == nil {
				errCh <- nil
				return
			}
			if strings.Contains(err.Error(), "match seats not filled") {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("presign retries exhausted: %w", err)
	}
	go runPresign(refAlice, 0, escrowA.EscrowId, alicePub, hex.EncodeToString(alicePriv.Serialize()))
	go runPresign(refBob, 1, escrowB.EscrowId, bobPub, hex.EncodeToString(bobPriv.Serialize()))

	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "presign failed")
		case <-time.After(5 * time.Second):
			t.Fatal("presign timed out")
		}
	}
	t.Log("✓ Presign completed for both players")

	// The table's GetSettlementMatchID() should return just tableID
	table, ok := env.PokerSrv.GetTable(tableID)
	require.True(t, ok, "Table should exist")

	tableMatchID := table.GetSettlementMatchID()
	require.Equal(t, tableID, tableMatchID,
		"Table's GetSettlementMatchID() should return the tableID")

	// Verify this matchID works for GetFinalizeBundle
	bundle, err := refAlice.GetFinalizeBundle(ctx, tableMatchID, 0)
	require.NoError(t, err, "GetFinalizeBundle should work with table's matchID")
	require.NotEmpty(t, bundle.GammaHex)

	t.Log("✓ Table correctly provides matchID for settlement")
}

// TestGameDoesNotStartWithoutPresign verifies that an escrow-backed table
// will not start the game until all players have completed presigning.
func TestGameDoesNotStartWithoutPresign(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	defer env.Close()

	ctx := context.Background()

	// Prepare two players with balances.
	// We need to create ShortIDs from the player names so that uid.String() matches the table's player ID.
	// Since ShortID.String() returns hex, we need to use the hex representation as the player ID.
	// Create ShortIDs from "alice" and "bob" by hashing them to get valid ShortID bytes.
	aliceBytes := chainhash.HashB([]byte("alice"))
	bobBytes := chainhash.HashB([]byte("bob"))
	var aliceUID, bobUID zkidentity.ShortID
	aliceUID.FromBytes(aliceBytes[:])
	bobUID.FromBytes(bobBytes[:])
	alicePlayerID := aliceUID.String()
	bobPlayerID := bobUID.String()

	buyIn := settlementTestBuyIn()

	// Seed auth sessions with tokens and payout addresses.
	env.PokerSrv.TestSeedSession("alice-token", aliceUID, "TsRnk22spGQJTpKFcRBc281rmfNFpywh337", "alice")
	env.PokerSrv.TestSeedSession("bob-token", bobUID, "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5", "bob")

	// Create table with buy-in (escrow required).
	// Use the ShortID string representation as the player ID to match BindEscrow's lookup.
	tableID := env.CreateTableWithBuyIn(ctx, alicePlayerID, 2, 2, int64(buyIn))

	// Create PokerClients.
	logBackend := testenv.NewLogBackend()
	pcAlice, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: "TsRnk22spGQJTpKFcRBc281rmfNFpywh337",
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)
	pcBob, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5",
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)

	pcAliceToken := "alice-token"
	pcBobToken := "bob-token"

	// Generate session keys for escrow.
	alicePriv, _ := secp256k1.GeneratePrivateKey()
	alicePub := alicePriv.PubKey().SerializeCompressed()
	bobPriv, _ := secp256k1.GeneratePrivateKey()
	bobPub := bobPriv.PubKey().SerializeCompressed()

	// Seat bob before escrows open: the roster is the table's seats, so a
	// player has to hold one before they can escrow against it.
	_, err = env.LobbyClient.JoinTable(ctx, &pokerrpc.JoinTableRequest{
		PlayerId: bobPlayerID,
		TableId:  tableID,
	})
	require.NoError(t, err)

	// Open escrows.
	refAlice := pcAlice.Referee(pcAliceToken)
	refBob := pcBob.Referee(pcBobToken)
	escrows := openRosterEscrows(t, ctx, tableID, buyIn, 64,
		[]*client.RefereeClient{refAlice, refBob}, [][]byte{alicePub, bobPub})
	escrowA, escrowB := escrows[0], escrows[1]

	// Mark escrows as funded.
	txidA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txidB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	env.PokerSrv.TestBindEscrowFunding(escrowA.EscrowId, txidA, 0, buyIn)
	env.PokerSrv.TestBindEscrowFunding(escrowB.EscrowId, txidB, 0, buyIn)

	// Bind escrows to the table/match using proper RPC calls (not test helpers).
	// For poker tables, matchID = tableID (no sessionID suffix needed).
	matchID := tableID
	outpointA := fmt.Sprintf("%s:0", txidA)
	outpointB := fmt.Sprintf("%s:0", txidB)

	// Bind Alice's escrow (seat will be auto-detected from her position at table).
	bindRespA, err := refAlice.BindEscrow(ctx, tableID, "", matchID, 0, outpointA, escrowA.RedeemScriptHex, 64)
	require.NoError(t, err, "BindEscrow for alice failed")
	require.Equal(t, escrowA.EscrowId, bindRespA.EscrowId)
	require.True(t, bindRespA.EscrowReady, "Alice's escrow should be ready after binding")

	// Bind Bob's escrow (seat will be auto-detected from his position at table).
	bindRespB, err := refBob.BindEscrow(ctx, tableID, "", matchID, 0, outpointB, escrowB.RedeemScriptHex, 64)
	require.NoError(t, err, "BindEscrow for bob failed")
	require.Equal(t, escrowB.EscrowId, bindRespB.EscrowId)
	require.True(t, bindRespB.EscrowReady, "Bob's escrow should be ready after binding")

	// Both players set ready.
	readyResp, err := env.LobbyClient.SetPlayerReady(ctx, &pokerrpc.SetPlayerReadyRequest{
		PlayerId: alicePlayerID,
		TableId:  tableID,
	})
	require.NoError(t, err)
	require.True(t, readyResp.Success)

	readyResp, err = env.LobbyClient.SetPlayerReady(ctx, &pokerrpc.SetPlayerReadyRequest{
		PlayerId: bobPlayerID,
		TableId:  tableID,
	})
	require.NoError(t, err)
	require.True(t, readyResp.Success)
	// The response should indicate all players are ready, but waiting for presigning.
	require.True(t, readyResp.AllPlayersReady)
	require.Contains(t, readyResp.Message, "Waiting for presigning")

	// Verify the game has NOT started (presigning incomplete).
	time.Sleep(100 * time.Millisecond) // Give any async start a chance
	gameState, err := env.PokerClient.GetGameState(ctx, &pokerrpc.GetGameStateRequest{
		TableId: tableID,
	})
	require.NoError(t, err)
	require.False(t, gameState.GameState.GameStarted, "Game should NOT start without presigning")
	t.Log("✓ Game correctly blocked from starting without presigning")

	// Now complete presigning for both players.
	errCh := make(chan error, 2)
	runPresign := func(ref *client.RefereeClient, seat uint32, escrowID string, pub []byte, privHex string) {
		const retries = 10
		var err error
		for i := 0; i < retries; i++ {
			err = ref.StartPresign(ctx, matchID, tableID, escrowID, pub, privHex)
			if err == nil {
				errCh <- nil
				return
			}
			if strings.Contains(err.Error(), "match seats not filled") {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			errCh <- err
			return
		}
		errCh <- fmt.Errorf("presign retries exhausted: %w", err)
	}
	go runPresign(refAlice, 0, escrowA.EscrowId, alicePub, hex.EncodeToString(alicePriv.Serialize()))
	go runPresign(refBob, 1, escrowB.EscrowId, bobPub, hex.EncodeToString(bobPriv.Serialize()))

	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "presign failed")
		case <-time.After(5 * time.Second):
			t.Fatal("presign timed out")
		}
	}
	t.Log("✓ Presigning completed for both players")

	// Wait for server to mark presigning as complete.
	require.Eventually(t, func() bool {
		complete, _, _ := env.PokerSrv.IsPresigningComplete(matchID)
		return complete
	}, 2*time.Second, 10*time.Millisecond, "Server should mark presigning as complete")

	// Now trigger the ready check again (simulate re-setting ready or a background check).
	// In practice, the server should auto-start when presigning completes and all are ready.
	// For this test, we set one player ready again to trigger the check.
	readyResp, err = env.LobbyClient.SetPlayerReady(ctx, &pokerrpc.SetPlayerReadyRequest{
		PlayerId: alicePlayerID,
		TableId:  tableID,
	})
	require.NoError(t, err)

	// Wait for game to start.
	var gameStarted bool
	for i := 0; i < 20; i++ {
		time.Sleep(50 * time.Millisecond)
		gameState, err = env.PokerClient.GetGameState(ctx, &pokerrpc.GetGameStateRequest{
			TableId: tableID,
		})
		require.NoError(t, err)
		if gameState.GameState.GameStarted {
			gameStarted = true
			break
		}
	}
	require.True(t, gameStarted, "Game should start after presigning is complete")
	t.Log("✓ Game started after presigning completed")
}

// TestEscrowFundingAmountMismatchBug reproduces a bug where funding an escrow
// checks against the wrong escrow's amount when multiple escrows exist.
// Scenario:
// 1. Open escrow 1 with 0.01 BTC (1000000 satoshis) - not funded
// 2. Open escrow 2 with 0.1 BTC (10000000 satoshis)
// 3. Fund escrow 1 with 0.01 BTC (1000000 satoshis)
// 4. The system incorrectly checks against escrow 2's amount (0.1 BTC) instead of escrow 1's amount
//
// The bug is in TestBindEscrowFunding: it uses the 'amount' parameter instead of es.AmountAtoms.
// This test should FAIL when the bug exists, demonstrating the incorrect behavior.
func TestEscrowFundingAmountMismatchBug(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	defer env.Close()

	ctx := context.Background()

	// Seed auth session and payout for alice using consistent ShortID.
	alicePayout := "TsRnk22spGQJTpKFcRBc281rmfNFpywh337"
	aliceToken := env.EnsureTestSession(ctx, "alice", "alice")
	var aliceUID zkidentity.ShortID
	_ = aliceUID.FromString(testenv.PlayerIDToShortIDString("alice"))
	env.PokerSrv.TestSeedSession(aliceToken, aliceUID, alicePayout, "alice")

	// Create PokerClient
	logBackend := testenv.NewLogBackend()
	pcAlice, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: alicePayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)

	pcAliceToken := aliceToken

	// Seed bob too: the roster is the table's seats, so a two-seat table needs
	// both of them before any escrow gets an address.
	bobPayout := "TsgsQwSZTkbXPGdFBg5z3wthjkQs1EeKcJ5"
	bobToken := env.EnsureTestSession(ctx, "bob", "bob")
	var bobUID zkidentity.ShortID
	_ = bobUID.FromString(testenv.PlayerIDToShortIDString("bob"))
	env.PokerSrv.TestSeedSession(bobToken, bobUID, bobPayout, "bob")

	pcBob, err := client.NewPokerClientWithDialOptions(ctx, &client.ClientConfig{
		Datadir:       t.TempDir(),
		PayoutAddress: bobPayout,
		LogBackend:    logBackend,
		Notifications: client.NewNotificationManager(),
	}, env.DialTarget(), env.DialOptions()...)
	require.NoError(t, err)

	// Generate session keys for escrows
	priv1, _ := secp256k1.GeneratePrivateKey()
	pub1 := priv1.PubKey().SerializeCompressed()
	priv2, _ := secp256k1.GeneratePrivateKey()
	pub2 := priv2.PubKey().SerializeCompressed()

	alicePlayerID := "alice"
	amount1 := uint64(1_000_000)  // the table buy-in
	amount2 := uint64(10_000_000) // ten times it

	tableID := env.CreateTableWithBuyIn(ctx, alicePlayerID, 2, 2, int64(amount1))
	matchID := tableID
	_, err = env.JoinTable(ctx, alicePlayerID, tableID)
	require.NoError(t, err, "JoinTable should succeed")
	_, err = env.JoinTable(ctx, "bob", tableID)
	require.NoError(t, err, "bob should be able to join table")

	refAlice := pcAlice.Referee(pcAliceToken)
	refBob := pcBob.Referee(bobToken)

	// Winner-take-all splits the pot evenly only if every stake is the same,
	// so an escrow for the wrong amount is refused where it is opened rather
	// than surfacing later as a funding mismatch against some other escrow.
	_, err = refAlice.OpenEscrow(ctx, tableID, "", amount2, 64, pub2)
	require.Error(t, err, "an escrow that does not match the table buy-in must be refused")
	require.Contains(t, err.Error(), "buy-in")

	escrows := openRosterEscrows(t, ctx, tableID, amount1, 64,
		[]*client.RefereeClient{refAlice, refBob}, [][]byte{pub1, pub2})
	escrow1 := escrows[0]
	t.Logf("Opened escrow 1: %s with amount %d atoms", escrow1.EscrowId, amount1)

	// Fund escrow 1 and check the status reports its own amount. The bug this
	// covers had classifyEscrowFundingState judge an escrow against whatever
	// amount the caller passed rather than the escrow's own AmountAtoms, so a
	// second escrow of a different size could make this one look unfunded.
	txid1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	env.PokerSrv.TestBindEscrowFunding(escrow1.EscrowId, txid1, 0, amount1)

	status1, err := refAlice.GetEscrowStatus(ctx, escrow1.EscrowId)
	require.NoError(t, err)
	require.Equal(t, amount1, status1.GetAmountAtoms(), "Escrow 1 should have amount1")
	t.Logf("Escrow 1 status: OK=%v, Amount=%d, UTXOCount=%d", status1.GetOk(), status1.GetAmountAtoms(), status1.GetUtxoCount())

	outpoint1 := fmt.Sprintf("%s:0", txid1)
	bindResp, err := refAlice.BindEscrow(ctx, tableID, "", matchID, 0, outpoint1, escrow1.RedeemScriptHex, 64)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "expected funding 10000000 but found 1000000") ||
			strings.Contains(errMsg, "funding amount mismatch") ||
			strings.Contains(errMsg, "expected 10000000") ||
			strings.Contains(errMsg, "have 1000000 want 10000000") {
			t.Fatalf("BUG REPRODUCED: BindEscrow checked against another escrow's amount: %v", err)
		}
		require.NoError(t, err, "BindEscrow should succeed")
	}
	require.Equal(t, escrow1.EscrowId, bindResp.EscrowId)
}
