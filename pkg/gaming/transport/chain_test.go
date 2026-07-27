package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func chainHost(t *testing.T, handler http.HandlerFunc) *Bridge {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	b, err := NewBridge(srv.URL+"/gaming", "tok-abc", nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	return b
}

func TestChainTipComesBackFromTheHost(t *testing.T) {
	b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gaming/chain/tip" {
			t.Errorf("asked for %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok-abc" {
			t.Errorf("Authorization was %q", got)
		}
		_ = json.NewEncoder(w).Encode(ChainTip{Height: 912345, Hash: "abc"})
	})

	tip, err := b.ChainTip(context.Background())
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
	if tip.Height != 912345 || tip.Hash != "abc" {
		t.Fatalf("got %+v", tip)
	}
}

func TestAnOutpointIsAskedForByTxidAndVout(t *testing.T) {
	b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("txid") != "beef" || q.Get("vout") != "3" {
			t.Errorf("asked for txid=%q vout=%q", q.Get("txid"), q.Get("vout"))
		}
		_ = json.NewEncoder(w).Encode(Outpoint{
			Found: true, ValueAtoms: 10_000_000, PkScriptHex: "a914", Confirmations: 7,
		})
	})

	out, err := b.Outpoint(context.Background(), "beef", 3)
	if err != nil {
		t.Fatalf("outpoint: %v", err)
	}
	if !out.Found || out.ValueAtoms != 10_000_000 || out.Confirmations != 7 {
		t.Fatalf("got %+v", out)
	}
}

// An outpoint that is not there is an answer, not a failure: spent, missing and
// unconfirmed all mean there is no coin everyone can see.
func TestAnAbsentOutpointIsAnAnswer(t *testing.T) {
	b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Outpoint{})
	})

	out, err := b.Outpoint(context.Background(), "beef", 0)
	if err != nil {
		t.Fatalf("an absent outpoint should not be an error: %v", err)
	}
	if out.Found {
		t.Fatal("nothing was there, but it came back found")
	}
}

// A node that is not up is worth telling apart from a question that was wrong:
// only one of them is worth asking again.
func TestNoChainIsDistinctFromABadQuestion(t *testing.T) {
	unavailable := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "chain is not available", http.StatusServiceUnavailable)
	})
	err := mustFail(t, func() error { _, e := unavailable.ChainTip(context.Background()); return e })
	if got := err.Error(); got != "the host has no chain to read yet" {
		t.Fatalf("got %q", got)
	}

	refused := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "txid must be 64 hex characters", http.StatusBadRequest)
	})
	err = mustFail(t, func() error { _, e := refused.Outpoint(context.Background(), "no", 0); return e })
	if err.Error() == "the host has no chain to read yet" {
		t.Fatal("a bad question was reported as a missing chain")
	}
}

// An unrecognised token answers 404, the same as a gaming section switched off.
func TestTheChainRoutesNeedTheGamesToken(t *testing.T) {
	b := chainHost(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	err := mustFail(t, func() error { _, e := b.ChainTip(context.Background()); return e })
	if err.Error() == "" {
		t.Fatal("a refusal carried no reason")
	}
}

func mustFail(t *testing.T, fn func() error) error {
	t.Helper()
	err := fn()
	if err == nil {
		t.Fatal("expected a failure")
	}
	return err
}
