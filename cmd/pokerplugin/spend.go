package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/transport"
)

// Asking for money without waiting on the answer.
//
// Every payment here is approved by a person, in the host's interface, and a
// person takes as long as they take: five minutes for a stake, up to thirty for
// a bond. The existing handlers hold the request open for exactly that long,
// which is right when the caller is a script and impossible when it is a page.
//
// So the same work runs either way and the only difference is who waits. With
// {"detach": true} the handler returns as soon as the host has been asked, and
// the rest happens in a goroutine that does precisely what the blocking path
// would have done. Without it, nothing changes at all.
//
// What is asked for is recorded on disk before the answer arrives. That is the
// part worth being careful about: a payment approved after this process
// restarted used to be a payment with nothing pointing at it, recoverable only
// through /table/deposit/set by somebody who knew to look. Tolerable when a
// person is watching a curl; not when a panel is showing "pending".

// spendPurpose says what a payment was for, so a caller can be told.
type spendPurpose string

const (
	purposeStake     spendPurpose = "table stake"
	purposeTableBond spendPurpose = "table bond"
	purposeBond      spendPurpose = "registration bond"
)

// pendingSpend is one payment this process asked for.
type pendingSpend struct {
	ID      string       `json:"id"`
	Purpose spendPurpose `json:"purpose"`
	SID     string       `json:"sid,omitempty"`
	Seat    uint32       `json:"seat"`
	// PkScript is the script the payment must land in, which is how the
	// output is found afterwards. Kept so a resumed spend can still find it
	// without re-deriving anything.
	PkScript string `json:"pkScript"`
	AtAtoms  int64  `json:"atAtoms"`

	State    string `json:"state"`
	TxID     string `json:"txid,omitempty"`
	Outpoint string `json:"outpoint,omitempty"`
	Recorded bool   `json:"recorded"`
	// Error is the host's own refusal, and Settled says the host answered
	// at all. Kept apart from Unreachable, which only says this process
	// could not ask: a payment nobody could ask about may still have been
	// made, and treating the two alike loses money.
	Error       string `json:"error,omitempty"`
	Settled     bool   `json:"settled,omitempty"`
	Unreachable string `json:"unreachable,omitempty"`
}

// open reports whether this payment is still worth asking about. A refusal the
// host actually gave is done with; anything else is not.
func (p *pendingSpend) open() bool {
	return !p.Recorded && !p.Settled
}

// spends is every payment asked for and not yet accounted for.
//
// Its own lock, at the bottom of no hierarchy: it is never held while the table
// registry's is, and the registry's is never held while this one is. The
// recording step releases this before touching a table, exactly as the blocking
// handlers already do.
type spends struct {
	mu    sync.Mutex
	m     map[string]*pendingSpend
	store *store
}

func newSpends(st *store) *spends {
	return &spends{m: map[string]*pendingSpend{}, store: st}
}

func (s *spends) put(p *pendingSpend) {
	s.mu.Lock()
	s.m[p.ID] = p
	s.mu.Unlock()
	s.save(p)
}

func (s *spends) get(id string) (pendingSpend, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok {
		return pendingSpend{}, false
	}
	return *p, true
}

// update applies a change and writes it down. Returns a copy, because the
// caller must not hold a pointer into this map.
func (s *spends) update(id string, change func(*pendingSpend)) (pendingSpend, bool) {
	s.mu.Lock()
	p, ok := s.m[id]
	if !ok {
		s.mu.Unlock()
		return pendingSpend{}, false
	}
	change(p)
	out := *p
	s.mu.Unlock()

	s.save(&out)
	return out, true
}

func (s *spends) save(p *pendingSpend) {
	if s.store == nil {
		return
	}
	if err := s.store.saveSpend(p); err != nil {
		log.Printf("pokerplugin: could not write down a payment: %v", err)
	}
}

// openFor finds a payment already asked for and not yet accounted for.
//
// The already-paid checks at each call site only see money that has arrived. A
// request whose answer was lost is money that may well have been paid and has
// nothing pointing at it yet, and asking again for that is how a stake gets
// paid twice.
func (s *spends) openFor(purpose spendPurpose, sid string, seat uint32) (pendingSpend, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.m {
		if p.Purpose == purpose && p.SID == sid && p.Seat == seat && p.open() {
			return *p, true
		}
	}
	return pendingSpend{}, false
}

// writeOpenSpend answers somebody asking to pay for what is already being paid.
func writeOpenSpend(w http.ResponseWriter, open pendingSpend) {
	w.WriteHeader(http.StatusConflict)
	writeJSON(w, map[string]any{
		"spendId": open.ID,
		"state":   open.State,
		"pending": true,
		"error": fmt.Sprintf("a %s was already asked for as %s and has not been "+
			"answered yet, so waiting on it is right and asking again would pay twice",
			open.Purpose, open.ID),
	})
}

// resume picks up payments that were still in flight when this process stopped.
func (s *spends) resume() []*pendingSpend {
	if s.store == nil {
		return nil
	}
	saved, err := s.store.loadSpends()
	if err != nil {
		log.Printf("pokerplugin: could not read back pending payments: %v", err)
		return nil
	}
	var open []*pendingSpend
	s.mu.Lock()
	for _, p := range saved {
		s.m[p.ID] = p
		if p.open() {
			open = append(open, p)
		}
	}
	s.mu.Unlock()
	return open
}

// awaitAnswer waits for the host's answer, and keeps asking if the host is
// simply not there.
//
// The host restarting mid-payment is ordinary - it is a container next to this
// one - and it survives its own restart, so the answer is still there when it
// comes back. Giving up on the first refused connection is what turned a
// restart into a payment made twice.
func (p *plugin) awaitAnswer(ctx context.Context, req *pendingSpend) (transport.Spend, error) {
	var last error
	for {
		settled, err := p.bridge.AwaitSpend(ctx, req.ID)
		if err == nil {
			return settled, nil
		}
		last = err
		if ctx.Err() != nil {
			return transport.Spend{}, last
		}
		log.Printf("pokerplugin: cannot ask about the %s asked for as %s, trying again: %v",
			req.Purpose, req.ID, err)
		select {
		case <-ctx.Done():
			return transport.Spend{}, last
		case <-time.After(spendRetryEvery):
		}
	}
}

// spendRetryEvery is how long to leave the host alone between attempts. Long
// enough not to spin against a container that is starting, short enough that a
// person watching a panel sees it resolve.
const spendRetryEvery = 5 * time.Second

// awaitSpend does everything that happens after the host has been asked.
//
// One function for both paths: the blocking handlers call it and wait, and a
// detached request calls it in a goroutine. Two copies of this would be two
// things to keep in step, and the thing they would drift about is money.
func (p *plugin) awaitSpend(ctx context.Context, req *pendingSpend, wait time.Duration) (pendingSpend, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	settled, err := p.awaitAnswer(ctx, req)
	if err != nil {
		// Not being able to ask is not an answer. The host may have paid
		// while this could not reach it - a restart of the host is
		// exactly when that happens - so the payment stays open and is
		// asked about again rather than being written off.
		//
		// Recording it as refused here once cost a real 0.01 DCR: the
		// stake was paid, this stopped watching, the interface still
		// said it was owed, and it was paid a second time.
		out, _ := p.spends.update(req.ID, func(s *pendingSpend) {
			s.State = "unknown"
			s.Unreachable = err.Error()
		})
		return out, err
	}
	if settled.State != transport.SpendApproved {
		// The host answered, and the answer was no. That is terminal.
		err := fmt.Errorf("the host %s the payment: %s", settled.State, settled.Error)
		out, _ := p.spends.update(req.ID, func(s *pendingSpend) {
			s.State = string(settled.State)
			s.Error = settled.Error
			s.Settled = true
		})
		return out, err
	}
	p.spends.update(req.ID, func(s *pendingSpend) {
		s.State = string(transport.SpendApproved)
		s.TxID = settled.TxID
		s.Settled = true
		s.Unreachable = ""
	})

	find := p.findDepositOutput
	if req.Purpose == purposeBond {
		find = p.findBondOutput
	}
	outpoint, err := find(ctx, settled.TxID, req.PkScript)
	if err != nil {
		// The money moved and this could not say where. The /set routes
		// are the way back, and the payment is on disk so somebody can be
		// told which one to fix.
		out, _ := p.spends.update(req.ID, func(s *pendingSpend) { s.Error = err.Error() })
		return out, err
	}

	if err := p.recordSpend(req, outpoint); err != nil {
		// Surplus coin is settled: asking again would not change the
		// answer. It is written down with where it landed, because that
		// outpoint is the only way anything reaches it later.
		out, _ := p.spends.update(req.ID, func(s *pendingSpend) {
			s.Error = err.Error()
			if errors.Is(err, errSurplus) {
				s.Outpoint = outpoint
				s.Settled = true
			}
		})
		if errors.Is(err, errSurplus) {
			log.Printf("pokerplugin: the %s asked for as %s is spare coin at %s; "+
				"reclaim it by naming that outpoint", req.Purpose, req.ID, outpoint)
		}
		return out, err
	}
	out, _ := p.spends.update(req.ID, func(s *pendingSpend) {
		s.Outpoint = outpoint
		s.Recorded = true
	})
	p.notifyNow()
	return out, nil
}

// errSurplus says a payment arrived somewhere that is already paid for.
//
// Not a failure and not something to retry: the coin is real and is this
// player's, it is simply not the seat's stake. Filing it would displace the
// outpoint the other seats have already checked and announce a stake that is
// not the one being played with, which is worse than leaving it where it is.
var errSurplus = errors.New("this seat is already paid for, so this payment is spare coin at the same script; " +
	"take it back by naming its outpoint")

// recordSpend files a completed payment where it belongs.
func (p *plugin) recordSpend(req *pendingSpend, outpoint string) error {
	switch req.Purpose {
	case purposeStake:
		if _, _, _, already, err := p.tables.ourDeposit(req.SID); err == nil &&
			already != "" && !strings.EqualFold(already, outpoint) {
			return errSurplus
		}
		out, err := p.recordOwnStake(req.SID, req.Seat, outpoint)
		if err != nil {
			return err
		}
		p.publish(p.ctx, out)
	case purposeTableBond:
		if _, _, already, err := p.tables.ourBond(req.SID); err == nil &&
			already != "" && !strings.EqualFold(already, outpoint) {
			return errSurplus
		}
		out, err := p.recordOwnBond(req.SID, req.Seat, outpoint)
		if err != nil {
			return err
		}
		p.publish(p.ctx, out)
	case purposeBond:
		if err := p.id.setBondDeposit(outpoint); err != nil {
			return err
		}
	default:
		return fmt.Errorf("a payment for %q is not something this knows how to file", req.Purpose)
	}
	return nil
}

// detach runs a payment to completion out of band.
func (p *plugin) detach(req *pendingSpend, wait time.Duration) {
	go func() {
		if _, err := p.awaitSpend(context.Background(), req, wait); err != nil {
			log.Printf("pokerplugin: the %s asked for as %s did not complete: %v",
				req.Purpose, req.ID, err)
		}
	}()
}

// resumeSpends picks up where a restart left off.
func (p *plugin) resumeSpends() {
	for _, req := range p.spends.resume() {
		wait := fundWait
		if req.Purpose == purposeBond {
			wait = bondWait
		}
		log.Printf("pokerplugin: still waiting on the %s asked for as %s", req.Purpose, req.ID)
		p.detach(req, wait)
	}
}

// wantsDetach reads the flag without consuming the body the caller still needs.
func wantsDetach(raw json.RawMessage) bool {
	var probe struct {
		Detach bool `json:"detach"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.Detach
}

// handleSpend reports what became of a payment.
//
// The host's own view merged with what this process did with the result,
// because "approved" and "recorded against a seat" are different facts and the
// gap between them is exactly where a payment gets lost.
func (p *plugin) handleSpend(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("which payment"))
		return
	}
	local, known := p.spends.get(id)
	if !known {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no payment was asked for as %s", id))
		return
	}

	// The host is asked too, so a payment answered while this process was
	// not watching is still reported correctly.
	if local.open() {
		if status, err := p.bridge.SpendStatus(r.Context(), id); err == nil {
			local.State = string(status.State)
			if status.TxID != "" {
				local.TxID = status.TxID
			}
		}
	}
	writeJSON(w, local)
}
