package schema

import (
	"encoding/hex"
	"fmt"

	"github.com/vctt94/pokerbisonrelay/pkg/gamelog"
)

// Checkpoint is a seat's signature over the stacks at the end of a hand.
//
// Every seat sends one at every hand boundary, and a peer keeps the newest set
// it holds signed by everybody. That set is what a table settles on if it stops:
// the hand under way was never agreed - one seat is mid-decision and the others
// have not seen it - so there is nothing to settle it by, and it is simply void.
//
// Which is also why nobody has to work out who walked off. The settlement is the
// same transaction whoever broadcasts it, so there is no accusation to make and
// nothing for a liar to gain by making one.
type Checkpoint struct {
	Hand   uint64  `json:"hand"`
	Stacks []int64 `json:"stacks"`
	Seat   uint32  `json:"seat"`
	Signer string  `json:"signer"` // hex compressed log pubkey
	Sig    string  `json:"sig"`
}

// CheckpointFrom renders a checkpoint for the wire.
func CheckpointFrom(c *gamelog.Checkpoint) Checkpoint {
	if c == nil {
		return Checkpoint{}
	}
	return Checkpoint{
		Hand:   c.Hand,
		Stacks: append([]int64(nil), c.Stacks...),
		Seat:   c.Seat,
		Signer: hex.EncodeToString(c.Signer),
		Sig:    hex.EncodeToString(c.Sig),
	}
}

// Into reads a checkpoint back.
//
// The signature is not checked here. That is gamelog's job and it needs the
// roster to do it - a checkpoint that verifies against a key nobody at the table
// holds is worth exactly nothing, and checking it here without the roster would
// look like a check while being none.
func (c Checkpoint) Into() (*gamelog.Checkpoint, error) {
	signer, err := hex.DecodeString(c.Signer)
	if err != nil {
		return nil, fmt.Errorf("checkpoint signer: %w", err)
	}
	sig, err := hex.DecodeString(c.Sig)
	if err != nil {
		return nil, fmt.Errorf("checkpoint signature: %w", err)
	}
	if len(c.Stacks) == 0 {
		return nil, fmt.Errorf("a checkpoint carries no stacks")
	}
	return &gamelog.Checkpoint{
		Hand:   c.Hand,
		Stacks: append([]int64(nil), c.Stacks...),
		Seat:   c.Seat,
		Signer: signer,
		Sig:    sig,
	}, nil
}

// Claim proposes taking an absent player's bond, for the other seats to
// co-sign.
//
// It carries no evidence and asserts nothing, which is deliberate. Whether the
// named seat has really gone is not decided by this message or by anyone
// reading it - the claim is delayed on chain and the accused answers by spending
// the same output, so what settles it is whether they are still there. A peer
// that received this and disagreed simply does not sign, and a peer that never
// received it loses nothing.
type Claim struct {
	// Seat is the one being claimed against.
	Seat uint32 `json:"seat"`
	// Bond is the outpoint holding that seat's table bond, and the script
	// it is locked behind. The script travels because it is what says which
	// roster can take the coin, and a peer must check it names them before
	// putting a signature to anything.
	BondOutpoint string `json:"bondOutpoint"`
	BondScript   string `json:"bondScript"`
	// Tx is the unsigned claim transaction, hex-encoded, so every co-signer
	// puts their name to the same bytes rather than to a description of
	// them.
	Tx string `json:"tx"`
	// Sig is this peer's signature over its input, if it has signed.
	Signer string `json:"signer,omitempty"` // hex compressed session pubkey
	Sig    string `json:"sig,omitempty"`
	// AfterSeq is the log position the claimed seat last acted at. It is
	// for humans reading a record afterwards; nothing acts on it, because a
	// sequence number one peer holds is not evidence about another.
	AfterSeq uint64 `json:"afterSeq,omitempty"`
}

// Validate reports whether a claim could be acted on at all.
//
// Shape only. Whether the bond really names this table, and whether the
// transaction really pays where it should, are checked against the script and
// the roster by whoever is deciding to sign - not here, where the roster is not
// known.
func (c Claim) Validate() error {
	if c.BondOutpoint == "" {
		return fmt.Errorf("a claim names no bond deposit")
	}
	if _, err := hex.DecodeString(c.BondScript); err != nil || c.BondScript == "" {
		return fmt.Errorf("a claim carries no bond script")
	}
	if _, err := hex.DecodeString(c.Tx); err != nil || c.Tx == "" {
		return fmt.Errorf("a claim carries no transaction to sign")
	}
	if (c.Signer == "") != (c.Sig == "") {
		return fmt.Errorf("a claim carries a signature without a signer, or the reverse")
	}
	return nil
}
