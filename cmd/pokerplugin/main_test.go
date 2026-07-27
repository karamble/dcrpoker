package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pokerui/golib"
)

const testToken = "tok"

func testPlugin(t *testing.T) *plugin {
	t.Helper()
	dir := t.TempDir()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	p, err := newPlugin(context.Background(), "http://host/gaming", testToken, id, newStore(dir), testParams)
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

// The command surface is golib's, reached over HTTP. A command that needs no
// client proves the wiring without standing a table up.
func TestCmdReachesTheCommandSurface(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"handle":  0,
		"type":    golib.CTHello,
		"payload": json.RawMessage(`"world"`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := httptest.NewRecorder()
	testPlugin(t).routes().ServeHTTP(rec, authed(http.MethodPost, "/cmd", string(payload)))

	if rec.Code != http.StatusOK {
		t.Fatalf("cmd returned %d: %s", rec.Code, rec.Body.String())
	}
	var greeting string
	if err := json.Unmarshal(rec.Body.Bytes(), &greeting); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if greeting != "hello world" {
		t.Fatalf("got %q", greeting)
	}
}

// A failing command is a 4xx with the reason, not a 200 the caller has to
// inspect for an error field.
func TestFailingCommandIsAnError(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"handle":  99,
		"type":    golib.CTGetEscrowStatus,
		"payload": json.RawMessage(`{"escrow_id":"x"}`),
	})
	rec := httptest.NewRecorder()
	testPlugin(t).routes().ServeHTTP(rec, authed(http.MethodPost, "/cmd", string(payload)))

	if rec.Code == http.StatusOK {
		t.Fatalf("a command against an unknown client should not succeed: %s", rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Fatalf("the failure should carry a reason: %s", rec.Body.String())
	}
}

func TestCmdRequiresPost(t *testing.T) {
	rec := httptest.NewRecorder()
	testPlugin(t).routes().ServeHTTP(rec, authed(http.MethodGet, "/cmd", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /cmd returned %d", rec.Code)
	}
}

// A plugin with no way to reach the table, or no identity to reach it as,
// should refuse to start rather than run unable to play.
func TestPluginRequiresBridgeAndToken(t *testing.T) {
	dir := t.TempDir()
	id, err := loadIdentity(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if _, err := newPlugin(context.Background(), "", "tok", id, newStore(dir), testParams); err == nil {
		t.Fatal("a plugin with no bridge should not start")
	}
	if _, err := newPlugin(context.Background(), "http://host/gaming", "", id, newStore(dir), testParams); err == nil {
		t.Fatal("a plugin with no token should not start")
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
