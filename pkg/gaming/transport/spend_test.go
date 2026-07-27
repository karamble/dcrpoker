package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAskingToSpendReturnsBeforeAnybodyHasDecided(t *testing.T) {
	b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gaming/spend" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var req struct {
			Address     string `json:"address"`
			AmountAtoms int64  `json:"amountAtoms"`
			Reason      string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Address != "Tsaddr" || req.AmountAtoms != 10_000_000 {
			t.Errorf("asked to pay %d to %q", req.AmountAtoms, req.Address)
		}
		if req.Reason == "" {
			t.Error("a person is going to read this; it should say what it is for")
		}
		_ = json.NewEncoder(w).Encode(Spend{ID: "abc", State: SpendPending})
	})

	spend, err := b.RequestSpend(context.Background(), "Tsaddr", 10_000_000, "fund a bond")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if spend.ID != "abc" || spend.Settled() {
		t.Fatalf("got %+v, want a pending request", spend)
	}
}

// Policy refusals carry their reason, because a cap that will never be got
// under reads differently from one that resets tomorrow.
func TestAPolicyRefusalSaysWhy(t *testing.T) {
	b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "spend refused by policy: over the per-table cap", http.StatusForbidden)
	})

	_, err := b.RequestSpend(context.Background(), "Tsaddr", 1<<40, "buy in")
	if err == nil {
		t.Fatal("a refused spend came back as success")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "per-table cap") {
		t.Fatalf("the refusal lost its reason: %q", got)
	}
}

// A game waits on a person, and the wait has to end by itself - the host
// expires anything nobody answered.
func TestWaitingOnAPersonEndsWhenTheyDecide(t *testing.T) {
	var polls int32
	b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&polls, 1)
		state := SpendPending
		if n >= 2 {
			state = SpendApproved
		}
		_ = json.NewEncoder(w).Encode(Spend{ID: "abc", State: state, TxID: "deadbeef"})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spend, err := b.AwaitSpend(ctx, "abc")
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if spend.State != SpendApproved || spend.TxID != "deadbeef" {
		t.Fatalf("got %+v", spend)
	}
	if atomic.LoadInt32(&polls) < 2 {
		t.Fatal("it stopped waiting before anybody had answered")
	}
}

// Silence and refusal arrive as different states, and neither of them is money.
func TestARefusedSpendIsSettledNotPending(t *testing.T) {
	for _, state := range []SpendState{SpendDenied, SpendExpired, SpendFailed} {
		b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(Spend{ID: "abc", State: state})
		})
		spend, err := b.AwaitSpend(context.Background(), "abc")
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if !spend.Settled() || spend.TxID != "" {
			t.Fatalf("%s came back as %+v", state, spend)
		}
	}
}
