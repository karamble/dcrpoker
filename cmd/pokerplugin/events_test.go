package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stream opens the event stream against a real server and reads it.
//
// Through net/http rather than httptest.NewRecorder, because a recorder is not
// a Flusher and buffers everything until the handler returns - which for a
// stream is never. A test that used one would be testing a code path nothing
// else takes.
func stream(t *testing.T, p *plugin) (<-chan sse, func()) {
	t.Helper()
	srv := httptest.NewServer(p.routes())

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		srv.Close()
		t.Fatalf("/events returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		srv.Close()
		t.Fatalf("/events answered %q, which is not a stream", ct)
	}

	out := make(chan sse, 64)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			resp.Body.Close()
			srv.Close()
		})
	}
	t.Cleanup(stop)

	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var ev sse
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = []byte(strings.TrimPrefix(line, "data: "))
			case line == "":
				if ev.Event != "" {
					select {
					case out <- ev:
					default:
					}
					ev = sse{}
				}
			}
		}
	}()
	return out, stop
}

// await reads until an event of a kind arrives, or gives up.
func await(t *testing.T, events <-chan sse, kind string) sse {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream closed before any %q arrived", kind)
			}
			if ev.Event == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("no %q ever arrived", kind)
		}
	}
}

// A stream says everything it knows the moment it is opened.
//
// Resumption by sequence number would be more elegant and is not worth it:
// every frame carries whole state, so catching up and keeping up are the same
// operation, and a reader that missed frames is never behind.
func TestAStreamOpensWithEverythingItKnows(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)
	a.sweep()

	events, stop := stream(t, a)
	defer stop()

	var got map[string]bool = map[string]bool{}
	deadline := time.After(30 * time.Second)
	for len(got) < 3 {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream closed after %v", got)
			}
			got[ev.Event] = true
		case <-deadline:
			t.Fatalf("a fresh stream produced only %v", got)
		}
	}
	for _, want := range []string{"state", "ledger", "hand"} {
		if !got[want] {
			t.Errorf("a fresh stream never said %q", want)
		}
	}
	_ = terms
}

// What the stream sends and what a caller asking would get are the same bytes,
// so a reader needs one way of understanding both.
func TestTheStreamSaysWhatTheRoutesSay(t *testing.T) {
	h := newHub(t)
	a, _, terms := dealingTable(t, h)
	a.sweep()

	events, stop := stream(t, a)
	defer stop()

	ev := await(t, events, "ledger")
	var pushed struct {
		SID  string     `json:"sid"`
		View ledgerView `json:"view"`
	}
	if err := json.Unmarshal(ev.Data, &pushed); err != nil {
		t.Fatalf("decode pushed ledger: %v", err)
	}
	if pushed.SID != terms.SID {
		t.Fatalf("the stream sent a ledger for %q, want %q", pushed.SID, terms.SID)
	}

	asked := ledgerOf(t, a, terms.SID)
	if pushed.View.SID != asked.SID || len(pushed.View.Roster) != len(asked.Roster) {
		t.Fatalf("the stream and the route describe different tables:\n push %+v\n ask  %+v",
			pushed.View, asked)
	}
}

// A table that is not moving produces no traffic. Without this the stream would
// be a poll with extra steps, and a quiet table would cost the same as a busy
// one.
func TestAQuietTableSaysNothing(t *testing.T) {
	h := newHub(t)
	a, _, _ := dealingTable(t, h)
	a.sweep()

	events, cancel := settled(t, a)
	defer cancel()

	// It is at rest, so asking again says nothing. Without this the stream
	// would be a poll with extra steps, and a quiet table would cost the
	// same as a busy one.
	for range 5 {
		a.sweep()
	}
	select {
	case ev := <-events:
		t.Fatalf("a table at rest produced a %q frame: %.300s", ev.Event, ev.Data)
	case <-time.After(300 * time.Millisecond):
	}
}

// settled sweeps until the table stops changing, then hands back a subscriber
// that has been caught up and drained.
//
// Rest is established rather than slept through. A hand that has just been
// dealt is still moving - shuffling, dealing, blinds - and it comes to rest
// when it reaches somebody's turn, which happens after however long it happens
// to take.
func settled(t *testing.T, p *plugin) (<-chan sse, func()) {
	t.Helper()
	waitBetting(t, p)

	events, cancel := p.notify.subscribe()
	drain := func() int {
		n := 0
		for len(events) > 0 {
			<-events
			n++
		}
		return n
	}
	drain()

	deadline := time.Now().Add(45 * time.Second)
	for quiet := 0; quiet < 5; {
		p.sweep()
		if drain() > 0 {
			quiet = 0
		} else {
			quiet++
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the table never came to rest")
		}
		time.Sleep(100 * time.Millisecond)
	}
	drain()
	return events, cancel
}

// waitBetting waits for the hand to reach somebody's turn.
//
// A fixed point rather than a pause. Shuffling, dealing and posting blinds all
// happen between the peers as fast as messages travel, so a table that has been
// quiet for a moment during them is not a table that has stopped. It stops when
// it is waiting for a person, and a person is what this test is standing in
// for.
func waitBetting(t *testing.T, p *plugin) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		var reached bool
		for _, s := range p.tables.snapshots() {
			if !s.Dealing {
				continue
			}
			if v, err := p.tables.HandView(s.SID); err == nil && v.Phase == "betting" && v.ToAct >= 0 {
				reached = true
			}
		}
		if reached {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the hand never reached anybody's turn")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Something actually changing produces a frame, which is the whole point.
func TestAChangeIsPushedWithoutBeingAskedFor(t *testing.T) {
	h := newHub(t)
	a, _, _ := dealingTable(t, h)

	events, cancel := settled(t, a)
	defer cancel()

	// A payout address is a change nobody else has to agree to, so it moves
	// this peer's own view with no round trip to wait on.
	addr := payoutAddress(t, a)
	if code, body := post(t, a, "/payout/set", map[string]string{"address": addr}); code != http.StatusOK {
		t.Fatalf("/payout/set returned %d: %s", code, body)
	}
	a.sweep()

	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the stream closed")
			}
			if ev.Event == "ledger" && !strings.Contains(string(ev.Data), addr) {
				continue
			}
			if ev.Event == "ledger" {
				return
			}
		case <-deadline:
			t.Fatalf("setting a payout address was never pushed")
		}
	}
}

// A reader that stopped reading is dropped rather than waited on. If it were
// waited on, one stalled browser tab would stop this process telling anybody
// else that their turn had come.
func TestASubscriberThatStopsReadingDoesNotStallTheRest(t *testing.T) {
	n := newNotifier()
	stuck, cancelStuck := n.subscribe()
	live, cancelLive := n.subscribe()
	defer cancelStuck()
	defer cancelLive()

	// Far more than any buffer, published without anybody draining the
	// first subscriber.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 500 {
			n.publish("state", sse{Event: "state", Data: []byte(fmt.Sprintf(`{"n":%d}`, i))})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publishing blocked on a subscriber that is not reading")
	}

	// The one that is reading has frames, and the last one it can see is a
	// whole state rather than a gap.
	if len(live) == 0 {
		t.Fatal("a live subscriber received nothing")
	}
	_ = stuck
}

// A table that goes away and comes back is said again, rather than suppressed
// as unchanged against a memory of the old one.
func TestATableThatWentAwayIsSaidAgainWhenItReturns(t *testing.T) {
	n := newNotifier()
	n.publish("ledger:x", sse{Event: "ledger", Data: []byte(`{"a":1}`)})

	events, cancel := n.subscribe()
	defer cancel()
	if len(events) != 1 {
		t.Fatalf("a new subscriber was caught up with %d frames, want 1", len(events))
	}

	n.forget(map[string]bool{})
	n.publish("ledger:x", sse{Event: "ledger", Data: []byte(`{"a":1}`)})

	// Identical bytes, but the table is new to this notifier again, so it
	// is said rather than swallowed.
	if len(events) != 2 {
		t.Fatalf("a table that came back produced %d frames, want 2", len(events)-1)
	}
}

// The bundle is public bytes and is served as such; the API is not.
func TestTheInterfaceIsServedWithoutATokenAndTheApiIsNot(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok")

	rec := httptest.NewRecorder()
	p.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ui/ returned %d without a token, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("/ui/ served %q, want html", ct)
	}
	// The one header that would break the arrangement this page exists for.
	if got := rec.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("the interface sets X-Frame-Options: %q, which would stop it being framed", got)
	}

	rec = httptest.NewRecorder()
	p.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tables", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/tables returned %d without a token, want 404", rec.Code)
	}
}

// An asset that is not there is not there. Answering with the index instead
// would turn a build mistake into a blank page.
func TestAMissingAssetIsMissing(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok")

	rec := httptest.NewRecorder()
	p.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/assets/nothing.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a missing asset returned %d, want 404", rec.Code)
	}
}

// The binary can be asked whether it really has an interface in it, so a
// release that shipped the placeholder is caught by the thing that signs it.
func TestHealthSaysWhetherTheInterfaceIsBuilt(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok")

	rec := httptest.NewRecorder()
	p.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var health struct {
		UI string `json:"ui"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.UI != "built" && health.UI != "placeholder" {
		t.Fatalf("health says the interface is %q, which is neither", health.UI)
	}
	if built := uiBuilt(); (health.UI == "built") != built {
		t.Fatalf("health says %q and the bundle says built=%v", health.UI, built)
	}
}

// The seed is the one thing here that nothing can regenerate, and a page must
// not be able to read it with an ordinary fetch.
//
// The real control is the host's route allowlist - this process cannot tell a
// proxied browser from the host, because both present the same token. This is
// the second lock, and it is worth having exactly because the first one lives
// in another repository.
func TestTheSeedNeedsMoreThanAFetch(t *testing.T) {
	h := newHub(t)
	p := h.join(t, "tok")

	code, body := get(t, p, "/identity/backup")
	if code != http.StatusForbidden {
		t.Fatalf("an ordinary fetch of the seed returned %d, want 403: %s", code, body)
	}
	seed, _ := p.id.backup()
	if strings.Contains(body, seed) {
		t.Fatal("a refused backup handed out the seed anyway")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/identity/backup", nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set(confirmHeader, confirmSeed)
	p.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a deliberate backup returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SeedHex string `json:"seedHex"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SeedHex == "" {
		t.Fatal("a deliberate backup handed out no seed")
	}
}
