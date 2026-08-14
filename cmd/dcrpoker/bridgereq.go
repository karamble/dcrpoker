package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vctt94/dcrpoker/pkg/escrow"
	"github.com/vctt94/dcrpoker/pkg/gaming/gamingpb"
	"github.com/vctt94/dcrpoker/pkg/gaming/schema"
	"github.com/vctt94/dcrpoker/pkg/membership"
)

// What the operator asked for.
//
// These arrive on the one stream from the bridge and are the whole of what
// anybody else can make this process do. There is no route in: the console
// cannot reach this machine, does not know its address, and holds no credential
// for it - it can only put a request on a stream this game opened.
//
// Every one is answered, including the ones that failed. Somebody is watching a
// spinner, and an error they can read is better than a button that never comes
// back.

// serveBridgeRequests answers requests until the stream or the process ends.
//
// One at a time. They are things a person clicked, so they are rare, and doing
// them in order means a reclaim cannot race the state report that describes its
// outcome.
func (p *plugin) serveBridgeRequests(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-p.bridge.Requests():
			if !ok {
				return
			}
			p.answer(ctx, req)
		}
	}
}

// answer does one request and tells the bridge what happened.
func (p *plugin) answer(ctx context.Context, req *gamingpb.BridgeRequest) {
	reply := &gamingpb.RespondRequest{RequestId: req.GetRequestId(), Ok: true}

	if err := p.doRequest(ctx, req, reply); err != nil {
		brdgLog.Errorf("could not do %s: %v", describeRequest(req), err)
		reply.Ok, reply.Error, reply.Result = false, err.Error(), nil
	}

	// Answering is best effort, and deliberately not retried. The console
	// polls state for anything that matters; a reply that could not be
	// delivered is a spinner, where a retry loop against a bridge that has
	// gone away would be this process stuck.
	answerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.bridge.Respond(answerCtx, reply); err != nil {
		brdgLog.Errorf("could not answer %s: %v", req.GetRequestId(), err)
	}
}

// doRequest performs one request, filling in the answer's result.
//
// It writes into reply rather than returning the result, because the generated
// oneof wrapper is unexported and cannot be named from here.
func (p *plugin) doRequest(ctx context.Context, req *gamingpb.BridgeRequest, reply *gamingpb.RespondRequest) error {
	switch r := req.GetReq().(type) {
	case *gamingpb.BridgeRequest_AcceptInvite:
		sid, err := p.acceptInvite(ctx, r.AcceptInvite)
		if err != nil {
			return err
		}
		reply.Result = &gamingpb.RespondRequest_AcceptInvite{
			AcceptInvite: &gamingpb.AcceptInviteResult{Sid: sid},
		}
		return nil

	case *gamingpb.BridgeRequest_Reclaim:
		txid, err := p.doReclaim(r.Reclaim)
		if err != nil {
			return err
		}
		reply.Result = &gamingpb.RespondRequest_Reclaim{
			Reclaim: &gamingpb.ReclaimResult{Txid: txid},
		}
		return nil

	case *gamingpb.BridgeRequest_SetPayout:
		return p.setPayout(ctx, r.SetPayout.GetAddress())

	case *gamingpb.BridgeRequest_SetNames:
		p.setNames(r.SetNames.GetNames())
		return nil

	case *gamingpb.BridgeRequest_RefreshState:
		state := p.gameState(ctx)
		state.RequestId = req.GetRequestId()
		reply.Result = &gamingpb.RespondRequest_State{State: state}
		return nil

	default:
		// A request this build has no idea about. Saying so is better than
		// silence: the console offered a button this game is too old for,
		// and the person needs to know that rather than watch it hang.
		return fmt.Errorf("this game does not understand that request")
	}
}

func describeRequest(req *gamingpb.BridgeRequest) string {
	switch req.GetReq().(type) {
	case *gamingpb.BridgeRequest_AcceptInvite:
		return "accept an invitation"
	case *gamingpb.BridgeRequest_Reclaim:
		return "reclaim locked coin"
	case *gamingpb.BridgeRequest_SetPayout:
		return "set the payout address"
	case *gamingpb.BridgeRequest_SetNames:
		return "set player names"
	case *gamingpb.BridgeRequest_RefreshState:
		return "report state"
	}
	return "an unknown request"
}

// acceptInvite joins a table somebody accepted in the console.
//
// Accepting is a person's decision and it is taken where the invitation
// arrived, which is the operator's interface - this game never sees the chat it
// came in on.
func (p *plugin) acceptInvite(ctx context.Context, req *gamingpb.AcceptInvite) (string, error) {
	inv, err := schema.ParseInvite(req.GetInvite())
	if err != nil {
		return "", fmt.Errorf("that invitation does not parse: %w", err)
	}
	gcID := strings.ToLower(strings.TrimSpace(req.GetGcid()))
	if !gcIDRe.MatchString(gcID) {
		return "", fmt.Errorf("gcid must be 64 hex characters")
	}

	out, err := p.tables.join(inv, gcID, p.id)
	if err != nil {
		return "", err
	}
	p.publish(ctx, out)
	return inv.SID, nil
}

// doReclaim takes back coin that is this player's own.
//
// The destination comes from the bridge, derived from the account this game is
// bound to. It is not this game's to choose: a game that named where its
// recovered money went could name itself.
//
// Deliberately not bounded by the request's deadline. A reclaim that has been
// signed and broadcast has moved real coin, and abandoning it half way because
// a console stopped waiting would leave the money somewhere nobody is watching.
func (p *plugin) doReclaim(req *gamingpb.Reclaim) (string, error) {
	dest := strings.TrimSpace(req.GetDestAddr())
	if dest == "" {
		return "", fmt.Errorf("the bridge named no destination to send it to")
	}
	ctx := p.ctx

	switch req.GetKind() {
	case gamingpb.Reclaim_BOND:
		outpoint := p.id.bondDeposit()
		if outpoint == "" {
			return "", fmt.Errorf("this player holds no bond")
		}
		script, err := p.id.bondScript()
		if err != nil {
			return "", err
		}
		key, err := p.id.bondKey()
		if err != nil {
			return "", err
		}
		txid, err := p.reclaim(ctx, outpoint, script, key,
			escrow.MinBondBlocks, escrow.BondSigScript, dest, req.GetFeeAtoms())
		if err != nil {
			return "", err
		}
		// The coin is already moving, so a bookkeeping failure here is
		// worth saying and not worth failing over.
		if err := p.id.setBondDeposit(""); err != nil {
			brdgLog.Errorf("swept the bond as %s but could not forget it: %v", txid, err)
		}
		return txid, nil

	case gamingpb.Reclaim_STAKE:
		sid := strings.ToLower(strings.TrimSpace(req.GetSid()))
		seat, dep, terms, stake, err := p.tables.ourDeposit(sid)
		if err != nil {
			return "", err
		}
		// A named outpoint is coin other than the seat's current stake -
		// a second payment into the same address, which the seat's record
		// does not know about.
		named := strings.TrimSpace(req.GetOutpoint())
		if named != "" {
			if err := p.paysOurDeposit(ctx, named, dep.PkScriptHex); err != nil {
				return "", err
			}
			stake = named
		}
		if stake == "" {
			return "", fmt.Errorf("nothing was ever paid into seat %d of %s", seat, sid)
		}
		redeem, err := hex.DecodeString(dep.RedeemScriptHex)
		if err != nil {
			return "", err
		}
		key, err := p.id.sessionKey(sid)
		if err != nil {
			return "", err
		}
		txid, err := p.reclaim(ctx, stake, redeem, key,
			terms.CSVBlocks, escrow.RefundSigScript, dest, req.GetFeeAtoms())
		if err != nil {
			return "", err
		}
		if named == "" {
			p.tables.forgetStake(sid, seat)
		}
		return txid, nil

	case gamingpb.Reclaim_TABLE_BOND:
		sid := strings.ToLower(strings.TrimSpace(req.GetSid()))
		seat, bond, outpoint, err := p.tables.ourTableBond(sid)
		if err != nil {
			return "", err
		}
		script, err := hex.DecodeString(bond.ScriptHex)
		if err != nil {
			return "", err
		}
		// The session key, not the bond key: a table bond is derived per
		// table, and the backstop branch is the seat's own.
		key, err := p.id.sessionKey(sid)
		if err != nil {
			return "", err
		}
		txid, err := p.reclaim(ctx, outpoint, script, key,
			membership.TableBondBlocks, escrow.BackstopSigScript, dest, req.GetFeeAtoms())
		if err != nil {
			return "", err
		}
		p.tables.forgetTableBond(sid, seat)
		return txid, nil
	}
	return "", fmt.Errorf("this game does not know how to reclaim that")
}

// setPayout records where this player's winnings go.
//
// This process holds no wallet, so it has to be told. Telling the table is part
// of it: the address goes into the settlement every seat signs, so a seat that
// changed it without saying would be asking the others to sign something they
// had not seen.
func (p *plugin) setPayout(ctx context.Context, address string) error {
	if err := p.id.setPayout(address, p.params); err != nil {
		return err
	}
	p.publish(ctx, p.tables.announcePayouts(p.id.payoutAddress()))
	return nil
}

// Names for chairs.
//
// A seat is a session key, and a session key is deliberately nobody: it is
// minted for one table and thrown away. But the people at the table arrived
// through a conversation, and a person looking at a chair wants to see who is
// sitting in it, not a number.
//
// The mapping comes in two halves that meet here. This process learns which
// Bison Relay identity spoke for each seat - the sender of the seat's own
// funding and bond announcements, the two messages only their owner ever sends.
// The host, which is the only party that can ask Bison Relay anything, says
// what those identities are called. Neither half is worth anything alone, and
// the joined result is worth exactly one thing: a label.
//
// Labels, not identity. Nothing that moves money reads a name, a seat that
// cannot be resolved is shown by its number, and a name proves nothing about
// who holds the key. Names arrive on the bridge's stream rather than from this
// game's own interface, because what things are called is the operator's
// knowledge to give: they are the one with the address book.
//
// Merged rather than replaced, and an empty name removes one, so the console
// can correct a single entry without restating the rest.
func (p *plugin) setNames(names map[string]string) {
	p.tables.mu.Lock()
	defer p.tables.mu.Unlock()
	for uid, name := range names {
		if name == "" {
			delete(p.tables.names, uid)
			continue
		}
		p.tables.names[uid] = name
	}
}

// gameState is what this game tells the console it is doing.
//
// Built from the same calls the interface's own routes use, so what an operator
// sees and what a player sees cannot drift apart - they are one answer rendered
// twice.
//
// Absolute facts only. A maturity height stays true while this game is not
// running; a count of blocks remaining is true for one block and then quietly
// wrong, and the console derives that from the bridge's own tip.
func (p *plugin) gameState(ctx context.Context) *gamingpb.GameState {
	state := &gamingpb.GameState{
		ReportedAt:             time.Now().Unix(),
		PayoutAddress:          p.id.payoutAddress(),
		SeedBackupAcknowledged: p.store.seedBackedUp(),
	}

	for _, s := range p.tables.snapshots() {
		state.Tables = append(state.Tables, &gamingpb.Table{
			Sid:        s.SID,
			Gcid:       s.GCID,
			State:      s.State,
			Seats:      s.Seats,
			BuyinAtoms: int64(s.BuyInAtoms),
			Until:      s.Until,
			Over:       s.Over,
		})
	}

	// The bond, described the way the interface describes it, then narrowed.
	// Reading the chain can fail without making the rest untrue, so a failure
	// becomes chain_err and the bond is still reported.
	script, err := p.id.bondScript()
	if err != nil {
		state.ChainErr = err.Error()
		return state
	}
	addr, _, err := escrow.BondAddress(script, p.params)
	if err != nil {
		state.ChainErr = err.Error()
		return state
	}
	outpoint := p.id.bondDeposit()
	bond := &gamingpb.Bond{
		Address:    addr.String(),
		ScriptHex:  hex.EncodeToString(script),
		Outpoint:   outpoint,
		MinAtoms:   int64(escrow.MinBondAtoms),
		HasDeposit: outpoint != "",
	}
	if outpoint != "" {
		facts := map[string]any{}
		p.describeBond(ctx, outpoint, facts)
		bond.Atoms, _ = facts["atoms"].(int64)
		bond.MaturesAt, _ = facts["maturesAt"].(int64)
		bond.Spent, _ = facts["spent"].(bool)
		if why, ok := facts["chainErr"].(string); ok {
			state.ChainErr = why
		}
	}
	state.Bond = bond
	return state
}

// handleSeedAcknowledge records that the person has written their seed down.
//
// It is the person's own claim, made from their own interface, and it is the
// only thing about the seed that ever leaves this machine - as a boolean on the
// state report, so the console can stop telling them to take a backup without
// ever being told what the backup is.
func (p *plugin) handleSeedAcknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	if err := p.store.markSeedBackedUp(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"acknowledged": true})
}
