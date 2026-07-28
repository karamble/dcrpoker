package main

import (
	"context"
	"encoding/json"
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
	Error    string `json:"error,omitempty"`
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
		if !p.Recorded && p.Error == "" {
			open = append(open, p)
		}
	}
	s.mu.Unlock()
	return open
}

// awaitSpend does everything that happens after the host has been asked.
//
// One function for both paths: the blocking handlers call it and wait, and a
// detached request calls it in a goroutine. Two copies of this would be two
// things to keep in step, and the thing they would drift about is money.
func (p *plugin) awaitSpend(ctx context.Context, req *pendingSpend, wait time.Duration) (pendingSpend, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	settled, err := p.bridge.AwaitSpend(ctx, req.ID)
	if err != nil {
		out, _ := p.spends.update(req.ID, func(s *pendingSpend) {
			s.State = "unanswered"
			s.Error = err.Error()
		})
		return out, err
	}
	if settled.State != transport.SpendApproved {
		err := fmt.Errorf("the host %s the payment: %s", settled.State, settled.Error)
		out, _ := p.spends.update(req.ID, func(s *pendingSpend) {
			s.State = string(settled.State)
			s.Error = settled.Error
		})
		return out, err
	}
	p.spends.update(req.ID, func(s *pendingSpend) {
		s.State = string(transport.SpendApproved)
		s.TxID = settled.TxID
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
		out, _ := p.spends.update(req.ID, func(s *pendingSpend) { s.Error = err.Error() })
		return out, err
	}
	out, _ := p.spends.update(req.ID, func(s *pendingSpend) {
		s.Outpoint = outpoint
		s.Recorded = true
	})
	p.notifyNow()
	return out, nil
}

// recordSpend files a completed payment where it belongs.
func (p *plugin) recordSpend(req *pendingSpend, outpoint string) error {
	switch req.Purpose {
	case purposeStake:
		out, err := p.recordOwnStake(req.SID, req.Seat, outpoint)
		if err != nil {
			return err
		}
		p.publish(p.ctx, out)
	case purposeTableBond:
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
	if !local.Recorded && local.Error == "" {
		if status, err := p.bridge.SpendStatus(r.Context(), id); err == nil {
			local.State = string(status.State)
			if status.TxID != "" {
				local.TxID = status.TxID
			}
		}
	}
	writeJSON(w, local)
}
