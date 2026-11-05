package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const liveWebRTCWait = 5 * time.Second

type liveWebRTCFixture struct {
	handler     *Handler
	server      *httptest.Server
	createBody  chan []byte
	callsHit    atomic.Int32
	sbConn      chan *websocket.Conn
	sbReceived  chan string
	finalized   chan struct{}
	allowLive   atomic.Bool
	apiKeyID    atomic.Int64
	upstreamErr atomic.Int32
}

func newLiveWebRTCFixture(t *testing.T) *liveWebRTCFixture {
	t.Helper()
	fx := &liveWebRTCFixture{
		createBody: make(chan []byte, 4), sbConn: make(chan *websocket.Conn, 1),
		sbReceived: make(chan string, 32), finalized: make(chan struct{}, 4),
	}
	fx.allowLive.Store(true)
	fx.apiKeyID.Store(31)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	sideband := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		fx.sbConn <- conn
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			fx.sbReceived <- string(payload)
		}
	}))
	t.Cleanup(sideband.Close)
	previousBase := liveSidebandUpstreamBaseURL
	t.Cleanup(func() { liveSidebandUpstreamBaseURL = previousBase })
	liveSidebandUpstreamBaseURL = "ws" + strings.TrimPrefix(sideband.URL, "http") + "/v1/live"

	previousLog := liveUsageLogForTest
	t.Cleanup(func() { liveUsageLogForTest = previousLog })
	liveUsageLogForTest = func(*database.UsageLogInput) { fx.finalized <- struct{}{} }

	handler, _ := gatewayLiveHandler(t, func(w http.ResponseWriter, r *http.Request) {
		fx.callsHit.Add(1)
		if status := int(fx.upstreamErr.Load()); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"bad session"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		fx.createBody <- body
		w.Header().Set("Location", "/v1/live/calls/rtc_web1")
		w.Header().Set("Content-Type", "application/sdp")
		_, _ = w.Write([]byte("v=0\r\nanswer\r\n"))
	}, 1)
	fx.handler = handler

	gin.SetMode(gin.TestMode)
	router := gin.New()
	authenticate := func(c *gin.Context) {
		id := fx.apiKeyID.Load()
		row := &database.APIKeyRow{ID: id}
		row.Limits.AllowLive = fx.allowLive.Load()
		c.Set(contextAPIKeyRow, row)
		c.Set(contextAPIKeyID, id)
		c.Next()
	}
	// The same layout as RegisterRoutes: the static sessions routes next to the :call_id wildcard.
	v1 := router.Group("/v1", authenticate)
	v1.POST("/live", handler.LiveCreate)
	v1.GET("/live/:call_id", handler.LiveSideband)
	v1.POST("/live/sessions", handler.LiveSessionsCreate)
	v1.GET("/live/sessions/:session_id/attach", handler.LiveSessionsAttach)
	fx.server = httptest.NewServer(router)
	t.Cleanup(fx.server.Close)
	return fx
}

func (fx *liveWebRTCFixture) post(t *testing.T, path, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(fx.server.URL+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (fx *liveWebRTCFixture) dialAttach(t *testing.T, id string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(fx.server.URL, "http")+"/v1/live/sessions/"+id+"/attach", nil)
}

func (fx *liveWebRTCFixture) create(t *testing.T, session string) string {
	t.Helper()
	status, raw := fx.post(t, "/v1/live/sessions", `{"session":`+session+`,"transport":{"type":"webrtc","sdp":"v=0\r\noffer\r\n"}}`)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", status, raw)
	}
	return gjson.GetBytes(raw, "session.id").String()
}

func liveWebRTCRead(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(liveWebRTCWait):
		t.Fatal("timed out waiting for a sideband message")
		return ""
	}
}

func liveWebRTCReadClient(t *testing.T, conn *websocket.Conn) gjson.Result {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(liveWebRTCWait))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read client: %v", err)
	}
	return gjson.ParseBytes(payload)
}

func liveWebRTCSidebandConn(t *testing.T, fx *liveWebRTCFixture) *websocket.Conn {
	t.Helper()
	select {
	case conn := <-fx.sbConn:
		return conn
	case <-time.After(liveWebRTCWait):
		t.Fatal("upstream sideband was not dialed")
		return nil
	}
}

func TestLiveSessionsWebRTCCreateTranslatesSession(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	status, raw := fx.post(t, "/v1/live/sessions",
		`{"session":{"model":"gpt-live-1","instructions":"be brief","audio":{"output":{"voice":"marin"}},"delegation":{"type":"client","acknowledgement":"none"}},"transport":{"type":"webrtc","sdp":"v=0\r\noffer\r\n"}}`)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	id := gjson.GetBytes(raw, "session.id").String()
	if id != "rtc_web1" || gjson.GetBytes(raw, "transport.type").String() != "webrtc" || gjson.GetBytes(raw, "transport.sdp").String() != "v=0\r\nanswer\r\n" {
		t.Fatalf("response=%s", raw)
	}
	upstream := <-fx.createBody
	if gjson.GetBytes(upstream, "sdp").String() != "v=0\r\noffer\r\n" {
		t.Fatalf("upstream sdp = %q", gjson.GetBytes(upstream, "sdp").String())
	}
	session := gjson.GetBytes(upstream, "session")
	if session.Get("model").String() != "gpt-live-1-codex" || session.Get("instructions").String() != "be brief" ||
		session.Get("audio.output.voice").String() == "" || session.Get("delegation.type").String() != "client" ||
		!session.Get("delegation.ack_filler").Exists() || session.Get("delegation.ack_filler").Bool() {
		t.Fatalf("upstream session = %s", session.Raw)
	}
	record, _, err := fx.handler.liveCalls().get(context.Background(), id)
	if err != nil || record == nil || record.Controller == liveControllerProxy || record.Controller == liveControllerClosed {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	fx.handler.liveCalls().finalize(record)
}

func TestLiveSessionsWebRTCCreateRejectsBadBodies(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	cases := []struct {
		name, body, code, param string
	}{
		{"unknown top-level", `{"session":{},"transport":{"type":"webrtc","sdp":"x"},"extra":1}`, "unknown_parameter", "extra"},
		{"unknown transport key", `{"session":{},"transport":{"type":"webrtc","sdp":"x","ice":1}}`, "unknown_parameter", "transport.ice"},
		{"unknown session key", `{"session":{"bogus":1},"transport":{"type":"webrtc","sdp":"x"}}`, "unknown_parameter", "session.bogus"},
		{"websocket transport", `{"session":{},"transport":{"type":"websocket"}}`, "unsupported_value", "transport.type"},
		{"missing sdp", `{"session":{},"transport":{"type":"webrtc"}}`, "missing_required_parameter", "transport.sdp"},
		{"missing transport", `{"session":{}}`, "missing_required_parameter", "transport"},
		{"audio format", `{"session":{"audio":{"format":{"type":"audio/pcm","rate":24000}}},"transport":{"type":"webrtc","sdp":"x"}}`, "unsupported_value", "session.audio.format"},
	}
	for _, tc := range cases {
		status, raw := fx.post(t, "/v1/live/sessions", tc.body)
		if status != http.StatusBadRequest || gjson.GetBytes(raw, "error.code").String() != tc.code ||
			gjson.GetBytes(raw, "error.param").String() != tc.param || gjson.GetBytes(raw, "error.type").String() != "invalid_request_error" ||
			!gjson.GetBytes(raw, "error.message").Exists() {
			t.Errorf("%s: status=%d body=%s", tc.name, status, raw)
		}
	}
	if fx.callsHit.Load() != 0 {
		t.Fatalf("upstream was called %d times for invalid bodies", fx.callsHit.Load())
	}
}

func TestLiveSessionsWebRTCCreateRequiresAllowLive(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	fx.allowLive.Store(false)
	status, raw := fx.post(t, "/v1/live/sessions", `{"session":{},"transport":{"type":"webrtc","sdp":"x"}}`)
	if status != http.StatusForbidden || !gjson.GetBytes(raw, "error.code").Exists() {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if fx.callsHit.Load() != 0 {
		t.Fatal("upstream must not be called")
	}
}

func TestLiveSessionsWebRTCCreateUpstreamRejection(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	fx.upstreamErr.Store(http.StatusBadRequest)
	status, raw := fx.post(t, "/v1/live/sessions", `{"session":{},"transport":{"type":"webrtc","sdp":"x"}}`)
	if status != http.StatusBadRequest || gjson.GetBytes(raw, "error.message").String() != "bad session" || gjson.GetBytes(raw, "error.type").String() == "" {
		t.Fatalf("status=%d body=%s", status, raw)
	}
}

func TestLiveSessionsAttachTranslatesEvents(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	id := fx.create(t, `{"instructions":"x","delegation":{"type":"client","acknowledgement":"none"}}`)

	client, _, err := fx.dialAttach(t, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	upstream := liveWebRTCSidebandConn(t, fx)

	// A client event before upstream session.started is held back.
	if err := client.WriteJSON(map[string]any{"type": "session.thinking.append", "event_id": "ev1", "delegation_id": "item_1", "content": "looking"}); err != nil {
		t.Fatal(err)
	}
	// Audio is not valid on the sideband.
	if err := client.WriteJSON(map[string]any{"type": "session.input_audio.append", "event_id": "ev_audio", "audio": "AAAA"}); err != nil {
		t.Fatal(err)
	}
	errEvent := liveWebRTCReadClient(t, client)
	if errEvent.Get("type").String() != "error" || errEvent.Get("error.code").String() != "unsupported_value" ||
		errEvent.Get("error.param").String() != "type" || errEvent.Get("error.client_event_id").String() != "ev_audio" {
		t.Fatalf("audio error = %s", errEvent.Raw)
	}
	select {
	case msg := <-fx.sbReceived:
		t.Fatalf("sideband received %q before session.started", msg)
	default:
	}

	if err := upstream.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.started","session":{"id":"rtc_web1"}}`)); err != nil {
		t.Fatal(err)
	}
	started := liveWebRTCReadClient(t, client)
	if started.Get("type").String() != "session.started" || started.Get("session.delegation.acknowledgement").String() != "none" {
		t.Fatalf("started = %s", started.Raw)
	}
	flushed := gjson.Parse(liveWebRTCRead(t, fx.sbReceived))
	if flushed.Get("type").String() != "delegation.context.append" || flushed.Get("channel").String() != "commentary" ||
		flushed.Get("delegation_item_id").String() != "item_1" || flushed.Get("content.0.text").String() != "looking" {
		t.Fatalf("flushed = %s", flushed.Raw)
	}

	// delegation.created becomes the public extension event.
	created := `{"type":"delegation.created","offset_ms":4000,"item":{"id":"item_1","type":"delegation","target":"client","content":[{"type":"input_text","text":"Open Safari"}]}}`
	if err := upstream.WriteMessage(websocket.TextMessage, []byte(created)); err != nil {
		t.Fatal(err)
	}
	delegation := liveWebRTCReadClient(t, client)
	if delegation.Get("type").String() != "session.delegation.created" || delegation.Get("delegation.id").String() != "item_1" ||
		delegation.Get("delegation.content").String() != "Open Safari" {
		t.Fatalf("delegation = %s", delegation.Raw)
	}

	// The ack carries the client event id.
	if err := upstream.WriteMessage(websocket.TextMessage, []byte(`{"type":"delegation.context.appended","delegation_item_id":"item_1"}`)); err != nil {
		t.Fatal(err)
	}
	ack := liveWebRTCReadClient(t, client)
	if ack.Get("type").String() != "session.thinking.appended" || ack.Get("client_event_id").String() != "ev1" {
		t.Fatalf("ack = %s", ack.Raw)
	}

	// Output audio is dropped: the next event the client sees is the close.
	if err := upstream.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.output_audio.delta","delta":"AAAA"}`)); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteJSON(map[string]any{"type": "session.close"}); err != nil {
		t.Fatal(err)
	}
	closed := liveWebRTCReadClient(t, client)
	if closed.Get("type").String() != "session.closed" || closed.Get("reason").String() != "close_requested" {
		t.Fatalf("closed = %s", closed.Raw)
	}
	if got := gjson.Parse(liveWebRTCRead(t, fx.sbReceived)); got.Get("type").String() != "session.close" {
		t.Fatalf("internal close = %s", got.Raw)
	}
	select {
	case <-fx.finalized:
	case <-time.After(liveWebRTCWait):
		t.Fatal("call was not finalized")
	}
	record, _, _ := fx.handler.liveCalls().get(context.Background(), id)
	if record != nil && record.Controller != liveControllerClosed {
		t.Fatalf("record still %q", record.Controller)
	}
}

func TestLiveSessionsAttachRemoteHangup(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	id := fx.create(t, `{}`)
	client, _, err := fx.dialAttach(t, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	upstream := liveWebRTCSidebandConn(t, fx)
	_ = upstream.Close()
	closed := liveWebRTCReadClient(t, client)
	if closed.Get("type").String() != "session.closed" || closed.Get("reason").String() != "remote_hangup" {
		t.Fatalf("closed = %s", closed.Raw)
	}
	select {
	case <-fx.finalized:
	case <-time.After(liveWebRTCWait):
		t.Fatal("call was not finalized")
	}
}

func TestLiveSessionsAttachRejectsOtherAPIKeyAndConflict(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	id := fx.create(t, `{}`)

	fx.apiKeyID.Store(99)
	_, resp, err := fx.dialAttach(t, id)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other key: err=%v resp=%v", err, resp)
	}
	fx.apiKeyID.Store(31)

	_, resp, err = fx.dialAttach(t, "rtc_missing")
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing: err=%v resp=%v", err, resp)
	}

	first, _, err := fx.dialAttach(t, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	_ = liveWebRTCSidebandConn(t, fx)
	_, resp, err = fx.dialAttach(t, id)
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("second attach: err=%v resp=%v", err, resp)
	}
}

func TestLiveSessionsWebRTCStoreIsBounded(t *testing.T) {
	store := &liveSessionConfigStore{entries: map[string]liveSessionConfigEntry{}}
	cfg, _ := parseLiveSessionObject(gjson.Parse(`{}`), "")
	for i := 0; i < liveSessionsConfigCap+10; i++ {
		store.put("id"+liveItoa(i), cfg, time.Now().Add(time.Hour))
	}
	if len(store.entries) > liveSessionsConfigCap {
		t.Fatalf("entries=%d", len(store.entries))
	}
	store.put("old", cfg, time.Now().Add(-time.Second))
	if store.get("old") != nil {
		t.Fatal("expired entry returned")
	}
	if _, err := json.Marshal(cfg.PublicSession("x")); err != nil {
		t.Fatal(err)
	}
}
