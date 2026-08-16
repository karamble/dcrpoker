package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vctt94/dcrpoker/pkg/gaming/schema"
	"github.com/vctt94/dcrpoker/pkg/gaming/transport"
)

const testToken = "tok"

func testPlugin(t *testing.T) *plugin {
	t.Helper()
	dir := t.TempDir()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	cert, key := hubCert(t, "poker")
	bc := transport.BridgeConfig{Addr: "127.0.0.1:1", ClientCert: cert, ClientKey: key, BridgeCert: cert}
	stampGameIdentity(&bc)
	p, err := newPlugin(context.Background(), bc, id, newStore(dir), testParams)
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}
	return p
}

// authed builds a request carrying the token the host issued, which every
// route but health requires.
func authed(method, path, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

// The portal reads this to decide whether the game is ready, which is what
// turns Ready true in the host's UI.
func TestHealthReportsTheGameAndVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	testPlugin(t).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("health returned %d", rec.Code)
	}
	var body struct {
		Game            string `json:"game"`
		ProtocolVersion int    `json:"protocolVersion"`
		OK              bool   `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Game != schema.Game || body.ProtocolVersion != schema.Version || !body.OK {
		t.Fatalf("health says %+v", body)
	}
}
func TestPluginRequiresBridgeAndToken(t *testing.T) {
	dir := t.TempDir()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	cert, key := hubCert(t, "poker")
	noAddr := transport.BridgeConfig{ClientCert: cert, ClientKey: key, BridgeCert: cert}
	stampGameIdentity(&noAddr)
	if _, err := newPlugin(context.Background(), noAddr, id, newStore(dir), testParams); err == nil {
		t.Fatal("a plugin with no bridge address should not start")
	}
	// A credential that does not load is the commonest way this goes wrong:
	// the operator copied one of the three blocks short. It has to be refused
	// here, where the message can say so, rather than at the first call.
	badKey := transport.BridgeConfig{Addr: "127.0.0.1:1", ClientCert: cert, ClientKey: []byte("not a key"), BridgeCert: cert}
	stampGameIdentity(&badKey)
	if _, err := newPlugin(context.Background(), badKey, id, newStore(dir), testParams); err == nil {
		t.Fatal("a plugin whose credential does not load should not start")
	}
}

// Until this process holds a table, nobody is authorized to talk to it - a
// stranger must not be able to fill the buffers of a game that is not playing.
func TestNoTableMeansNobodyIsAuthorized(t *testing.T) {
	p := testPlugin(t)
	p.router.HandleGCMessage("gc", "somebody",
		`--gaming[v=1,game=poker,gv=1,sid=ab,mid=cd,seq=1/2,exp=0]--QUJD`, time.Now())
	if p.router.Pending() != 0 {
		t.Fatalf("an unauthorized sender allocated %d partial messages", p.router.Pending())
	}
}
