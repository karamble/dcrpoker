package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Saying when something changed.
//
// Everything else here answers a question. This is the one thing that speaks
// first, and it exists because a table moves for reasons nobody asked about: a
// block arrives, somebody else acts, a claim lands. Without it a caller has to
// poll fast enough to catch a turn and slow enough not to be wasteful, and
// there is no such rate.
//
// Two decisions shape the whole file.
//
// **Server-sent events, not a websocket.** The page that reads this runs in a
// sandboxed frame with an opaque origin, so it sends Origin: null - and
// gorilla's default CheckOrigin refuses exactly that. Allowing it would mean
// replacing the only check there is with one that returns true, in order to buy
// a channel back from the browser that nothing here wants: every input already
// arrives as a POST that the same bearer token guards. SSE is a GET, so it is
// covered by that guard as it stands, and there is no origin check to disable
// because there never was one.
//
// **State, not deltas, and computed by polling this process rather than by
// instrumenting it.** Ten-odd places change a table, all of them under the
// registry lock, and one of them already broadcasts to the chain while holding
// it. Fanning out from each would compound that, and would be ten places to
// forget one. Recomputing cannot go stale, and sending whole state means a
// subscriber that missed a frame is not behind - which is what lets the fan-out
// drop instead of blocking.

// notifyEvery is how often this process is asked whether anything changed.
//
// A turn should feel immediate and nothing here is expensive: the answer is
// usually that nothing changed, and then nothing is sent at all.
const notifyEvery = 500 * time.Millisecond

// notifyPing is how often a comment goes out on an idle stream, so a proxy
// between here and the reader does not reap a connection that is merely quiet.
const notifyPing = 15 * time.Second

// notifyBuffer is how many frames are held for a subscriber that is not
// reading. Small on purpose: every frame carries whole state, so the one after
// a drop says everything the dropped one would have.
const notifyBuffer = 4

// notifier tracks what was last said, and to whom.
type notifier struct {
	mu   sync.Mutex
	subs map[int]chan sse
	next int
	// last is the most recent payload for every key, which is both what a
	// change is measured against and what a new subscriber is caught up
	// with.
	last map[string]sse
	// order keeps the keys in a stable sequence, so a new subscriber is
	// caught up in the same order every time rather than in map order.
	order []string

	// wake asks for a sweep before the next tick. One slot, because a sweep
	// says whole state: two requests and one produce the same answer.
	wake chan struct{}
}

// sse is one thing to say.
type sse struct {
	Event string
	Data  []byte
}

func newNotifier() *notifier {
	return &notifier{
		subs: map[int]chan sse{},
		last: map[string]sse{},
		wake: make(chan struct{}, 1),
	}
}

// subscribe opens a stream, caught up to everything currently known.
//
// The catch-up happens under the same lock that publishes, so a subscriber
// cannot see a state and then miss the change that came after it.
func (n *notifier) subscribe() (<-chan sse, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Deep enough to hold the whole catch-up as well as a few frames after
	// it, so a subscriber that has not started reading yet does not lose the
	// state it was just handed.
	ch := make(chan sse, notifyBuffer+len(n.last))

	for _, key := range n.order {
		if ev, ok := n.last[key]; ok {
			ch <- ev
		}
	}
	id := n.next
	n.next++
	n.subs[id] = ch

	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if c, ok := n.subs[id]; ok {
			delete(n.subs, id)
			close(c)
		}
	}
}

// publish says something if it is not what was said last.
func (n *notifier) publish(key string, ev sse) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if prev, ok := n.last[key]; ok && prev.Event == ev.Event && bytes.Equal(prev.Data, ev.Data) {
		return
	}
	if _, seen := n.last[key]; !seen {
		n.order = append(n.order, key)
	}
	n.last[key] = ev

	for _, ch := range n.subs {
		select {
		case ch <- ev:
		default:
			// A reader that stopped reading is dropped rather than
			// waited on. The next frame carries whole state, so it
			// is not behind - and blocking here would stall the
			// goroutine that builds that state for everybody.
		}
	}
}

// forget drops keys that no longer exist, so a table that comes back is said
// again rather than suppressed as unchanged.
func (n *notifier) forget(live map[string]bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	kept := n.order[:0]
	for _, key := range n.order {
		if live[key] {
			kept = append(kept, key)
			continue
		}
		delete(n.last, key)
	}
	n.order = kept
}

// watchTables says what changed, for as long as this process runs.
//
// One goroutine, so sweeps cannot overlap. Two running at once could publish in
// either order and leave the record of what was last said holding the older of
// them - after which the next real change would be measured against the wrong
// thing and might be suppressed as unchanged.
func (p *plugin) watchTables(ctx context.Context) {
	ticker := time.NewTicker(notifyEvery)
	defer ticker.Stop()

	for {
		p.sweep()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.notify.wake:
		}
	}
}

// sweep recomputes everything a caller can ask for and publishes the parts that
// moved.
//
// Deliberately built from the same calls the routes use, so a subscriber and a
// caller polling cannot be told apart by what they receive. That is what lets
// the reader have one code path for both.
func (p *plugin) sweep() {
	snaps := p.tables.snapshots()

	live := map[string]bool{"state": true}
	if data, err := json.Marshal(map[string]any{"tables": snaps}); err == nil {
		p.notify.publish("state", sse{Event: "state", Data: data})
	}

	// A stable order, because the tables come out of a map and a caller
	// catching up should not see them shuffle.
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].SID < snaps[j].SID })

	for _, s := range snaps {
		key := "ledger:" + s.SID
		live[key] = true
		if view, err := p.tables.Ledger(s.SID); err == nil {
			if data, err := json.Marshal(map[string]any{"sid": s.SID, "view": view}); err == nil {
				p.notify.publish(key, sse{Event: "ledger", Data: data})
			}
		}

		if !s.Dealing {
			continue
		}
		key = "hand:" + s.SID
		live[key] = true
		if view, err := p.tables.HandView(s.SID); err == nil {
			if data, err := json.Marshal(map[string]any{"sid": s.SID, "view": view}); err == nil {
				p.notify.publish(key, sse{Event: "hand", Data: data})
			}
		}
	}
	p.notify.forget(live)
}

// handleEvents streams what changed.
func (p *plugin) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Say so rather than accepting a connection that will never
		// deliver anything. A caller told no can fall back to asking;
		// one left holding an open stream cannot tell the difference
		// between that and a quiet table.
		writeErr(w, http.StatusNotImplemented,
			fmt.Errorf("this server cannot stream"))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// For any proxy in the path that would otherwise buffer this into
	// silence, which looks exactly like a table where nothing is happening.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, cancel := p.notify.subscribe()
	defer cancel()

	ping := time.NewTicker(notifyPing)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-p.ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, ev.Data); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// notifyNow asks for a sweep sooner than the next tick.
//
// Used where an answer is already in hand and half a second would be a visible
// pause - a decision taken through /table/act, most of all, where the caller is
// a person who just pressed something.
//
// It asks rather than sweeps. The request coalesces into a single slot, so a
// burst of them produces one extra sweep and not one each, and the sweeping
// still happens in the one goroutine that does it.
func (p *plugin) notifyNow() {
	if p.notify == nil {
		return
	}
	select {
	case p.notify.wake <- struct{}{}:
	default:
		// One is already pending, and a sweep reports whole state, so a
		// second would say exactly what the first is about to.
	}
}
