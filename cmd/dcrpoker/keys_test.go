package main

import (
	"encoding/hex"
	"testing"
)

// The seed is the one secret here that nothing can regenerate, and it lives in
// a single file in a single volume. Being able to copy it out and put it back
// is the difference between a deleted volume costing a reinstall and costing
// the bond permanently, since the bond script names one key and no other.
func TestAnIdentityCanBeBackedUpAndPutBack(t *testing.T) {
	saved, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	seedHex, _ := saved.backup()
	want, err := saved.bondScript()
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}

	// A different volume is a different player.
	fresh, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	other, err := fresh.bondScript()
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	if hex.EncodeToString(other) == hex.EncodeToString(want) {
		t.Fatal("two fresh identities derived the same bond")
	}

	if err := fresh.restore(seedHex, ""); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := fresh.bondScript()
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatal("a restored identity derives a different bond, so it could not reclaim it")
	}

	// And it has to survive being read back, or the next start is a
	// different player again.
	reloaded, err := loadIdentity(fresh.dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if again, _ := reloaded.backup(); again != seedHex {
		t.Fatal("the restored seed was not written down")
	}
}

// Restoring over a player that holds a bond would put that coin beyond reach:
// the script names the old key, and the old key would be gone.
func TestRestoreWillNotStrandABond(t *testing.T) {
	saved, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	seedHex, _ := saved.backup()

	holder, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := holder.setBondDeposit("1def0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab:1"); err != nil {
		t.Fatalf("set bond: %v", err)
	}
	if err := holder.restore(seedHex, ""); err == nil {
		t.Fatal("restored over an identity holding a bond")
	}
	if now, _ := holder.backup(); now == seedHex {
		t.Fatal("the refused restore was applied anyway")
	}
}

func TestRestoreRefusesSomethingThatIsNotASeed(t *testing.T) {
	id, err := loadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	before, _ := id.backup()

	for _, bad := range []string{"", "nonsense", "abcd", hex.EncodeToString(make([]byte, 31))} {
		if err := id.restore(bad, ""); err == nil {
			t.Fatalf("accepted %q as a seed", bad)
		}
	}
	if after, _ := id.backup(); after != before {
		t.Fatal("a refused restore changed the identity")
	}
}

// A player that has sat at a table cannot be restored over either: the records
// name session keys derived from the seed that is there now.
func TestAStoreKnowsWhetherThePlayerHasPlayed(t *testing.T) {
	st := newStore(t.TempDir())

	used, err := st.used()
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	if used {
		t.Fatal("a store with no sessions reports a player that has played")
	}

	if err := st.save("beef01", &record{}); err != nil {
		t.Fatalf("save: %v", err)
	}
	used, err = st.used()
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	if !used {
		t.Fatal("a store holding a session reports a player that never played")
	}
}
