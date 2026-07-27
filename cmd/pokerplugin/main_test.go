package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vctt94/pokerbisonrelay/pkg/gaming/schema"
	"github.com/vctt94/pokerbisonrelay/pokerui/golib"
)

func testPlugin(t *testing.T) *plugin {
	t.Helper()
	p, err := newPlugin("http://host/gaming", "tok")
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}
	return p
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
	req := httptest.NewRequest(http.MethodPost, "/cmd", strings.NewReader(string(payload)))
	testPlugin(t).routes().ServeHTTP(rec, req)

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
	testPlugin(t).routes().ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/cmd", strings.NewReader(string(payload))))

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
	testPlugin(t).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cmd", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /cmd returned %d", rec.Code)
	}
}

// A plugin with no way to reach the table, or no identity to reach it as,
// should refuse to start rather than run unable to play.
func TestPluginRequiresBridgeAndToken(t *testing.T) {
	if _, err := newPlugin("", "tok"); err == nil {
		t.Fatal("a plugin with no bridge should not start")
	}
	if _, err := newPlugin("http://host/gaming", ""); err == nil {
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
