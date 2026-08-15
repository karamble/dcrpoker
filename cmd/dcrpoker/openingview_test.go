package main

import (
	"encoding/json"
	"testing"
)

// The board's opening progress keeps the names the interface reads.
//
// TestSnapshotNamesItsFields pins snapshot and seatView; handView had no such
// test, so a rename here would reach the interface as a card that never opens
// rather than as a failure.
func TestAnOpeningNamesItsFields(t *testing.T) {
	blob, err := json.Marshal(handView{Opening: []openingView{
		{Index: 1, Arrived: 1, Needed: 2, Open: false},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	slots, ok := got["opening"].([]any)
	if !ok {
		t.Fatal("a hand no longer carries \"opening\"")
	}
	if len(slots) != 1 {
		t.Fatalf("one slot in, %d out", len(slots))
	}
	first, _ := slots[0].(map[string]any)
	for _, key := range []string{"index", "arrived", "needed", "open"} {
		if _, ok := first[key]; !ok {
			t.Errorf("an opening slot no longer carries %q", key)
		}
	}

	// open is false here, so a key dropped by omitempty would read as a card
	// that has opened. It must be on the wire either way.
	if v, _ := first["open"].(bool); v {
		t.Error("a slot that has not opened reports that it has")
	}

	// And a hand with nothing opening says nothing rather than an empty list.
	bare, err := json.Marshal(handView{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var none map[string]any
	if err := json.Unmarshal(bare, &none); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := none["opening"]; ok {
		t.Error("a hand with no street turned still carries \"opening\"")
	}
}
