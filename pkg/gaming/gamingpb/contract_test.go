package gamingpb

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// contractSHA256 is the bridge contract this game speaks, byte for byte.
//
// The same file lives in dcrpulse, pinned there to the same number. That is the
// whole point of pinning it in two places: the wire is one artifact with two
// copies, and a copy that drifted would still compile on both sides and still
// generate stubs, then fail at a live table with a field nobody sent.
//
// Updating it is a deliberate act on both sides at once. If this test fails,
// the question is not "what is the new hash" but "which repo changed the wire,
// and has the other one been given the same change".
const contractSHA256 = "98681ea0d77a2a3f4e50f0ba58e641d7dd7e422809558b280e976193b843d485"

func TestTheWireContractMatchesTheBridge(t *testing.T) {
	raw, err := os.ReadFile("gaming_bridge.proto")
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != contractSHA256 {
		t.Fatalf("the contract in this repo is not the one dcrpulse serves.\n"+
			"  here: %s\n  bridge: %s\n"+
			"Copy dcrpulse's dashboard/internal/gamingpb/gaming_bridge.proto over this one and "+
			"regenerate, or take the change back to dcrpulse first - do not edit the hash.",
			hex.EncodeToString(sum[:]), contractSHA256)
	}
}
