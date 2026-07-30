package main

import (
	"encoding/json"
	"fmt"
	"log"

	"go.dedis.ch/kyber/v4"

	"github.com/vctt94/pokerbisonrelay/pkg/deck"
	"github.com/vctt94/pokerbisonrelay/pkg/driver"
	"github.com/vctt94/pokerbisonrelay/pkg/forfeit"
	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	gwire "github.com/vctt94/pokerbisonrelay/pkg/gaming/wire"
)

// Disputing a shuffle.
//
// A shuffle that arrives signed and fails verification wedges the hand: the
// refuser is still shuffling, the shuffler has moved on, each owes the other,
// both accusations get answered, and nothing breaks the tie - the first live
// table died exactly here. The dispute is the tie-breaker, and it is
// deterministic: the complaint carries everything a verdict needs, so every
// peer that reads it reaches the same answer with nobody's cooperation.
//
// Two questions, in order. First, is the claimed input real? The deck a round
// shuffles is fixed by everything accepted before it, so the claimed input is
// checked against the judge's own signed upstream - and for the complainer's
// own upstream that includes frames the complainer itself signed, so a false
// input is the complainer contradicting its own hand. Second, does the proof
// verify? The shuffle proof is deterministic public computation over the
// context, the joint key, both decks and the proof bytes - all of which the
// complaint carries. Verifies: the refusal was groundless, the complainer is
// named. Fails: the shuffler signed a frame whose proof is invalid, which
// honest software never emits, and the shuffler is named.
//
// Nothing secret ever moves. The named seat needs to do nothing and can
// refuse nothing: its punishment is its withheld bond release, and the hand -
// proven unable to finish - is void, so the table ends at the last signed
// boundary without unanimity. The proof is the agreement.

// The verdicts a complaint can end on. Final: a dispute that has been judged
// is not reopened.
const (
	// complaintFalse: the claimed input contradicts the judge's signed
	// upstream, or the disputed proof verifies after all. The complainer is
	// named.
	complaintFalse = "false complaint"
	// complaintCheat: the shuffler signed a frame whose proof does not
	// verify against the input both sides agree on. The shuffler is named.
	complaintCheat = "shuffler cheated"
)

// complaintCase is one dispute: the persisted evidence and its decoded parts.
type complaintCase struct {
	view *schema.ComplaintView

	pubs         []kyber.Point
	steps        []deck.Step
	input        deck.Deck
	refused      deck.Deck
	refusedProof []byte
}

// decodeComplaintCase rebuilds the working state from the stored view.
func decodeComplaintCase(view *schema.ComplaintView) (*complaintCase, error) {
	c := &complaintCase{view: view}
	for i, ph := range view.Pubs {
		p, err := readComplaintPoint(ph)
		if err != nil {
			return nil, fmt.Errorf("card key %d: %w", i, err)
		}
		c.pubs = append(c.pubs, p)
	}
	for i, sv := range view.Steps {
		d, prf, err := readComplaintStep(sv)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		c.steps = append(c.steps, deck.Step{Deck: d, Proof: prf})
	}
	in, refused, refusedProof, _, _, err := view.Complaint.Into()
	if err != nil {
		return nil, err
	}
	c.input, c.refused, c.refusedProof = in, refused, refusedProof
	return c, nil
}

// verdictFor judges one dispute from its evidence and the judge's own
// upstream. Every peer holding the same signed history reaches the same
// answer, which is the property the whole dispute rests on.
func (c *complaintCase) verdictFor() (verdict string, named uint32, err error) {
	shuffler := c.view.Complaint.Shuffler
	if int(shuffler) >= len(c.pubs) {
		return "", 0, fmt.Errorf("the dispute names seat %d of %d", shuffler, len(c.pubs))
	}
	joint, err := deck.JointKey(c.pubs)
	if err != nil {
		return "", 0, err
	}
	prior := deck.Fresh(joint)
	if c.view.Round > 0 {
		if int(c.view.Round)-1 >= len(c.steps) {
			return "", 0, fmt.Errorf("the judge holds shuffles through round %d, not %d",
				len(c.steps), c.view.Round)
		}
		prior = c.steps[c.view.Round-1].Deck
	}
	if !deck.SameDeck(c.input, prior) {
		// The claimed input is not the deck the accepted upstream
		// produces. For the complainer's own judge that upstream includes
		// frames the complainer signed, so this is a complaint
		// contradicting its author's own hand.
		return complaintFalse, c.view.By, nil
	}
	ctx := deck.Context{
		Match:  c.view.Match,
		Hand:   c.view.Hand,
		Round:  c.view.Round,
		Prover: c.pubs[shuffler],
	}
	if err := deck.VerifyShuffle(ctx, joint, c.input, c.refused, c.refusedProof); err != nil {
		// A signed frame whose proof is invalid. Honest software never
		// emits one, so its signer answers for it.
		return complaintCheat, shuffler, nil
	}
	// The proof verifies for everyone who runs it, so the refusal was
	// groundless - a lie or a broken verifier, and the protocol cannot tell
	// those apart because it does not need to.
	return complaintFalse, c.view.By, nil
}

// openComplaintFrom publishes a dispute over the refusal the driver kept, and
// judges it on the spot - the verdict is deterministic, and this peer holds
// everything it needs.
func (tbl *table) openComplaintFrom(r *driver.ShuffleRefusal) []outgoing {
	mine, ok := tbl.form.OurSeat()
	if !ok || tbl.play == nil {
		return nil
	}
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil
	}
	hand := tbl.playingHand()
	if hand == 0 {
		return nil
	}
	if _, open := tbl.compl[hand]; open {
		return tbl.repeatComplaints()
	}
	if tbl.complJudged[hand] != "" {
		return nil
	}
	h := tbl.play.Hand()
	if h == nil {
		return nil
	}

	refusedDigest, err := driver.ShuffleFrameDigest(match, hand, r.Seat, r.Deck, r.Proof)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot digest the refused shuffle: %v", tbl.terms.SID, err)
		return nil
	}
	digest, err := driver.ShuffleComplaintDigest(match, hand, int(mine), uint32(r.Round), r.Input, refusedDigest)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot digest the complaint: %v", tbl.terms.SID, err)
		return nil
	}
	key, err := tbl.logKeyFor()
	if err != nil {
		return nil
	}
	// Position-signed: one complaint per hand, its content fixed by where
	// this seat stands. Shifting the story would sign twice here, and
	// equivocation publishes the key.
	sig, err := key.Sign(forfeit.DomainShuffleComplaint, hand, digest[:])
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot sign the complaint: %v", tbl.terms.SID, err)
		return nil
	}
	body, err := schema.ShuffleComplaintFrom(mine, uint32(r.Seat), hand, uint32(r.Round),
		r.Input, r.Deck, r.Proof, r.Sig, sig)
	if err != nil {
		log.Printf("pokerplugin: table %s: cannot render the complaint: %v", tbl.terms.SID, err)
		return nil
	}

	view := &schema.ComplaintView{Match: match, Hand: hand, Round: uint32(r.Round),
		By: mine, Complaint: body}
	for _, p := range h.Keys() {
		ph, err := complaintPointHex(p)
		if err != nil {
			return nil
		}
		view.Pubs = append(view.Pubs, ph)
	}
	for _, st := range h.Steps() {
		sv, err := complaintStepView(st)
		if err != nil {
			return nil
		}
		view.Steps = append(view.Steps, sv)
	}
	c, err := decodeComplaintCase(view)
	if err != nil {
		log.Printf("pokerplugin: table %s: the complaint does not decode back: %v", tbl.terms.SID, err)
		return nil
	}

	log.Printf("pokerplugin: table %s: disputing seat %d's shuffle for hand %d",
		tbl.terms.SID, r.Seat, hand)
	tbl.judgeComplaint(hand, c)
	return tbl.repeatComplaints()
}

// acceptComplaint is somebody disputing a shuffle - most often one this peer
// sent. The verdict is reached here, from this peer's own signed history and
// the complaint alone.
func (tbl *table) acceptComplaint(body schema.ShuffleComplaint) []outgoing {
	input, refused, refusedProof, refusedSig, sig, err := body.Into()
	if err != nil {
		return nil
	}
	mine, ok := tbl.form.OurSeat()
	if !ok || body.Seat == body.Shuffler || body.Seat == mine {
		return nil
	}
	match, ok := tbl.form.MatchID()
	if !ok {
		return nil
	}
	if _, open := tbl.compl[body.Hand]; open {
		return nil
	}
	if tbl.complJudged[body.Hand] != "" {
		return nil
	}
	logSeats, ok := tbl.form.LogSeats()
	if !ok {
		return nil
	}
	byKey, ok := logSeats[body.Seat]
	if !ok {
		return nil
	}
	shufflerKey, ok := logSeats[body.Shuffler]
	if !ok {
		return nil
	}

	// The shuffler's signature over exactly what is disputed, and the
	// complainer's over exactly what it claims. A complaint about an
	// unsigned frame is a complaint about nothing.
	refusedDigest, err := driver.ShuffleFrameDigest(match, body.Hand, int(body.Shuffler), refused, refusedProof)
	if err != nil {
		return nil
	}
	if err := driver.VerifySeatSig(shufflerKey, refusedDigest, refusedSig, int(body.Shuffler)); err != nil {
		log.Printf("pokerplugin: table %s: a dispute over a shuffle seat %d never signed: %v",
			tbl.terms.SID, body.Shuffler, err)
		return nil
	}
	digest, err := driver.ShuffleComplaintDigest(match, body.Hand, int(body.Seat), body.Round, input, refusedDigest)
	if err != nil {
		return nil
	}
	if err := driver.VerifySeatSig(byKey, digest, sig, int(body.Seat)); err != nil {
		log.Printf("pokerplugin: table %s: a complaint seat %d did not sign: %v",
			tbl.terms.SID, body.Seat, err)
		return nil
	}

	// This peer's own account of the hand, which is what the verdict judges
	// against. A peer that cannot reconstruct the upstream judges nothing
	// and holds nothing up - a finding no other peer can reach must not be
	// able to hold everybody's money.
	pubs, steps, ok := tbl.disputedUpstream(body.Hand)
	if !ok {
		tbl.note(eventBlocked, fmt.Sprintf(
			"hand %d's shuffle is disputed and this peer cannot reconstruct the deck it is about", body.Hand),
			"", seatp(int(body.Seat)))
		return nil
	}
	view := &schema.ComplaintView{Match: match, Hand: body.Hand, Round: body.Round,
		By: body.Seat, Complaint: body}
	for _, p := range pubs {
		ph, err := complaintPointHex(p)
		if err != nil {
			return nil
		}
		view.Pubs = append(view.Pubs, ph)
	}
	for i := 0; i < int(body.Round) && i < len(steps); i++ {
		sv, err := complaintStepView(steps[i])
		if err != nil {
			return nil
		}
		view.Steps = append(view.Steps, sv)
	}
	c, err := decodeComplaintCase(view)
	if err != nil {
		return nil
	}
	tbl.judgeComplaint(body.Hand, c)
	return nil
}

// disputedUpstream is this peer's own account of a disputed hand: the card
// keys and the shuffles it accepted.
func (tbl *table) disputedUpstream(hand uint64) ([]kyber.Point, []deck.Step, bool) {
	if tbl.play != nil && tbl.playingHand() == hand {
		if h := tbl.play.Hand(); h != nil {
			return h.Keys(), h.Steps(), true
		}
	}
	return nil, nil, false
}

// judgeComplaint reaches the verdict, names its seat, records everything,
// voids the hand and files the dispute for repeating.
func (tbl *table) judgeComplaint(hand uint64, c *complaintCase) {
	verdict, named, err := c.verdictFor()
	if err != nil {
		log.Printf("pokerplugin: table %s: hand %d's dispute could not be judged here: %v",
			tbl.terms.SID, hand, err)
		return
	}
	c.view.Verdict = verdict

	if tbl.compl == nil {
		tbl.compl = map[uint64]*complaintCase{}
	}
	tbl.compl[hand] = c
	if tbl.complJudged == nil {
		tbl.complJudged = map[uint64]string{}
	}
	tbl.complJudged[hand] = verdict
	tbl.saveComplaint(hand, c)

	if tbl.cheats == nil {
		tbl.cheats = map[uint32]bool{}
	}
	tbl.cheats[named] = true
	log.Printf("pokerplugin: table %s: hand %d's dispute: %s; seat %d is named",
		tbl.terms.SID, hand, verdict, named)
	tbl.note(eventCheat, fmt.Sprintf("hand %d's dispute: %s", hand, verdict), "", seatp(int(named)))

	// The wedge is proven, so the hand is void and the table ends at the
	// last signed boundary - no unanimity, the proof is the agreement.
	if tbl.play != nil {
		tbl.play.VoidWedgedHand()
	}
	_ = tbl.save()
}

func (tbl *table) saveComplaint(hand uint64, c *complaintCase) {
	if tbl.st == nil || c.view == nil {
		return
	}
	blob, err := json.Marshal(c.view)
	if err != nil {
		return
	}
	if err := tbl.st.saveComplaint(tbl.terms.SID, hand, blob); err != nil {
		log.Printf("pokerplugin: table %s: cannot write down hand %d's dispute: %v",
			tbl.terms.SID, hand, err)
		tbl.note(eventBlocked, fmt.Sprintf(
			"hand %d's dispute could not be written down, and cannot be shown after a restart", hand),
			"", nil)
	}
}

// repeatComplaints says this seat's own disputes again, byte-identical, until
// the table itself is gone.
//
// The verdict is local and instant, but the frame is what lets the other side
// reach it too - and the settlement the void enables needs both sides to have
// voided. Repeated like a challenge is, because this channel loses messages
// and what is being repeated is an accusation with money behind it.
func (tbl *table) repeatComplaints() []outgoing {
	mine, ok := tbl.form.OurSeat()
	if !ok {
		return nil
	}
	var out []outgoing
	for _, c := range tbl.compl {
		if c.view.By != mine {
			continue
		}
		out = append(out, tbl.frame(schema.KindShuffleComplaint, c.view.Complaint, gwire.ClassDispute))
	}
	return out
}

// Small helpers over the schema encodings, kept here so the view stays the
// single at-rest form.

func complaintPointHex(p kyber.Point) (string, error) {
	if p == nil {
		return "", fmt.Errorf("no point")
	}
	b, err := p.MarshalBinary()
	if err != nil {
		return "", err
	}
	return schema.B64(b), nil
}

func readComplaintPoint(s string) (kyber.Point, error) {
	b, err := schema.UnB64(s, "point")
	if err != nil {
		return nil, err
	}
	p := deck.Suite().Point()
	if err := p.UnmarshalBinary(b); err != nil {
		return nil, err
	}
	return p, nil
}

func complaintStepView(st deck.Step) (schema.StepView, error) {
	db, err := schema.DeckBytes(st.Deck)
	if err != nil {
		return schema.StepView{}, err
	}
	return schema.StepView{Deck: schema.B64(db), Proof: schema.B64(st.Proof)}, nil
}

func readComplaintStep(sv schema.StepView) (deck.Deck, []byte, error) {
	raw, err := schema.UnB64(sv.Deck, "deck")
	if err != nil {
		return nil, nil, err
	}
	d, err := schema.ReadDeck(raw)
	if err != nil {
		return nil, nil, err
	}
	prf, err := schema.UnB64(sv.Proof, "proof")
	if err != nil {
		return nil, nil, err
	}
	return d, prf, nil
}