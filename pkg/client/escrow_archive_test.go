package client

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vctt94/bisonbotkit/logging"
)

// The archive is what a CSV refund is built from: it is the only place the
// redeem script, timelock and funding outpoint survive once a match is over and
// the referee has forgotten the escrow. Losing a field here means losing the
// ability to recover the deposit at all.
func TestEscrowArchiveKeepsRefundMetadata(t *testing.T) {
	pc := newTestPokerClient(t)

	require.NoError(t, pc.CacheEscrowInfo(&EscrowInfo{
		EscrowID:        "escrow-1",
		DepositAddress:  "TsTestAddress",
		RedeemScriptHex: "5121aabb",
		PKScriptHex:     "a91400",
		CSVBlocks:       64,
		KeyIndex:        3,
		Status:          "opened",
	}))

	// Funding arrives later and must merge, not replace: an update carrying
	// only the outpoint cannot be allowed to drop the redeem script.
	require.NoError(t, pc.CacheEscrowInfo(&EscrowInfo{
		EscrowID:        "escrow-1",
		FundingTxid:     "live-txid",
		FundingVout:     2,
		FundedAmount:    10_000_000,
		ConfirmedHeight: 900_000,
		Status:          "funded",
	}))

	got, err := pc.GetEscrowById("escrow-1")
	require.NoError(t, err)
	require.Equal(t, "5121aabb", got["redeem_script_hex"])
	require.Equal(t, "a91400", got["pk_script_hex"])
	require.Equal(t, "TsTestAddress", got["deposit_address"])
	require.EqualValues(t, 64, got["csv_blocks"])
	require.EqualValues(t, 3, got["key_index"])
	require.Equal(t, "live-txid", got["funding_txid"])
	require.EqualValues(t, 2, got["funding_vout"])
	require.EqualValues(t, 10_000_000, got["funded_amount"])
	require.EqualValues(t, 900_000, got["confirmed_height"])
	require.Equal(t, "funded", got["status"], "status is the one field an update replaces")
}

// History lists confirmed escrows newest first, which is what refund tooling
// walks. An escrow with no confirmed height has nothing to refund yet.
func TestEscrowHistoryOrdersConfirmedNewestFirst(t *testing.T) {
	pc := newTestPokerClient(t)

	for _, e := range []*EscrowInfo{
		{EscrowID: "old", RedeemScriptHex: "51", CSVBlocks: 64, ConfirmedHeight: 100},
		{EscrowID: "new", RedeemScriptHex: "51", CSVBlocks: 64, ConfirmedHeight: 300},
		{EscrowID: "mid", RedeemScriptHex: "51", CSVBlocks: 64, ConfirmedHeight: 200},
		{EscrowID: "unconfirmed", RedeemScriptHex: "51", CSVBlocks: 64},
	} {
		require.NoError(t, pc.CacheEscrowInfo(e))
	}

	hist, err := pc.GetEscrowHistory()
	require.NoError(t, err)
	require.Len(t, hist, 3, "an unconfirmed escrow is not refundable and is left out")
	require.Equal(t, "new", hist[0]["escrow_id"])
	require.Equal(t, "mid", hist[1]["escrow_id"])
	require.Equal(t, "old", hist[2]["escrow_id"])

	require.NoError(t, pc.DeleteEscrowHistory("mid"))
	hist, err = pc.GetEscrowHistory()
	require.NoError(t, err)
	require.Len(t, hist, 2)

	_, err = pc.GetEscrowById("mid")
	require.ErrorContains(t, err, "not found")
}

func newTestPokerClient(t *testing.T) *PokerClient {
	t.Helper()

	logBackend, err := logging.NewLogBackend(logging.LogConfig{
		LogFile:        "",
		DebugLevel:     "error",
		MaxLogFiles:    1,
		MaxBufferLines: 100,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = logBackend.Close() })

	return &PokerClient{
		DataDir: t.TempDir(),
		log:     logBackend.Logger("PokerClientTest"),
	}
}
