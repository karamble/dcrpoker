package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	dcrwire "github.com/decred/dcrd/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// The hub, as a bridge.
//
// It answers the same three questions the host answers - what does the chain
// say about this outpoint, will you relay this transaction, carry this frame -
// and it identifies a peer the way the real bridge does: from the certificate
// on the connection, never from anything in the message. A harness that let a
// peer name itself would pass while the thing it stands for was broken.
type hubService struct {
	gamingpb.UnimplementedBridgeServiceServer
	h *hub
}

// caller is which peer is on the other end of this call.
func caller(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "no connection")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", status.Error(codes.Unauthenticated, "no certificate")
	}
	return tlsInfo.State.PeerCertificates[0].Subject.CommonName, nil
}

func (s *hubService) Outpoint(_ context.Context, req *gamingpb.OutpointRequest) (*gamingpb.OutpointReply, error) {
	h := s.h
	key := req.GetTxid() + ":" + strconv.FormatUint(uint64(req.GetVout()), 10)

	h.mu.Lock()
	h.asked[key]++
	pkScript, gone := h.bonds[key], h.spent[key]
	if req.GetIncludeMempool() && h.pending[key] {
		gone = true
	}
	if req.GetIncludeMempool() && h.unmined[key] != "" {
		pkScript, gone = h.unmined[key], false
	}
	confs := h.confs
	deep, named := h.shallow[key]
	h.mu.Unlock()

	if confs == 0 {
		confs = int64(escrow.BondConfirmations)
	}
	if named {
		confs = deep
	}
	return &gamingpb.OutpointReply{
		// Spent is indistinguishable from never-existed here, and that is
		// what the real lookup says too: it answers about coin anybody can
		// still take, not about history.
		Found:         pkScript != "" && !gone,
		ValueAtoms:    testOutpointAtoms,
		PkScriptHex:   pkScript,
		Confirmations: confs,
	}, nil
}

func (s *hubService) Broadcast(_ context.Context, req *gamingpb.BroadcastRequest) (*gamingpb.BroadcastReply, error) {
	h := s.h
	raw, err := hex.DecodeString(req.GetRawTxHex())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "not hex")
	}
	tx := dcrwire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, status.Error(codes.InvalidArgument, "not a transaction: "+err.Error())
	}

	h.mu.Lock()
	for _, in := range tx.TxIn {
		if h.spent[in.PreviousOutPoint.String()] {
			// Already taken. A real node refuses this, and a test that
			// let it through would let two peers both pay the table out.
			h.mu.Unlock()
			return nil, status.Error(codes.FailedPrecondition, "already spent")
		}
	}
	for _, in := range tx.TxIn {
		h.spent[in.PreviousOutPoint.String()] = true
	}
	h.sent = append(h.sent, tx)
	h.mu.Unlock()

	return &gamingpb.BroadcastReply{Txid: tx.TxHash().String()}, nil
}

func (s *hubService) SendFrame(ctx context.Context, req *gamingpb.SendFrameRequest) (*gamingpb.SendFrameReply, error) {
	h := s.h
	from, err := caller(ctx)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	if kind, ok := frameKind(req.GetFrame()); ok && h.swallow[kind] > 0 {
		h.swallow[kind]--
		h.lost[kind]++
		h.mu.Unlock()
		return &gamingpb.SendFrameReply{}, nil
	}
	targets := make([]*plugin, 0, len(h.peers))
	if !h.muted[from] {
		for name, p := range h.peers {
			if name != from && !h.muted[name] {
				targets = append(targets, p)
			}
		}
	}
	h.mu.Unlock()

	// Deliver out of band. A member receiving a frame usually sends one of
	// its own, and doing that on this call's stack would make the fan-out
	// reentrant in a way the real bridge is not.
	for _, p := range targets {
		h.inflight.Add(1)
		go func(p *plugin) {
			defer h.inflight.Done()
			p.router.HandleGCMessage(req.GetGcid(), from, req.GetFrame(), time.Now())
		}(p)
	}
	return &gamingpb.SendFrameReply{}, nil
}

// Hello lets a peer connect. The name it is told is the one on its certificate,
// which is the whole of how the real bridge answers this too.
func (s *hubService) Hello(ctx context.Context, _ *gamingpb.HelloRequest) (*gamingpb.HelloReply, error) {
	name, err := caller(ctx)
	if err != nil {
		return nil, err
	}
	return &gamingpb.HelloReply{Game: name, Network: "simnet"}, nil
}

// Subscribe holds a stream open and sends nothing. Frames reach a peer in this
// harness by being handed to its router directly, which is what lets a test
// wait for the table to come to rest.
func (s *hubService) Subscribe(_ *gamingpb.SubscribeRequest, stream grpc.ServerStreamingServer[gamingpb.BridgeEvent]) error {
	if err := stream.Send(&gamingpb.BridgeEvent{
		Event: &gamingpb.BridgeEvent_Start{Start: &gamingpb.StreamStart{Epoch: "test"}},
	}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

// hubCert mints a throwaway pair whose common name is how the hub knows a peer.
func hubCert(t *testing.T, name string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("mint a certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
