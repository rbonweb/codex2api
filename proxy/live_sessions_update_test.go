package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestLiveSessionsWSInstructionsUpdateIsAckedByUpstream(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{"instructions":"Be brief."}`)

	liveWSWrite(t, conn, `{"type":"session.update","event_id":"u1","session":{"instructions":"Answer in Hindi."}}`)
	sent := gjson.ParseBytes(liveWSTakeSent(t, srv.call))
	if sent.Get("type").String() != "session.context.append" || sent.Get("channel").String() != "commentary" {
		t.Fatalf("upstream event = %s", sent.Raw)
	}
	want := "Updated instructions for the rest of this call (these replace the earlier instructions): Answer in Hindi."
	if got := sent.Get("content.0.text").String(); got != want || sent.Get("content.0.type").String() != "input_text" {
		t.Fatalf("append text = %q", got)
	}

	srv.call.push(`{"type":"session.context.appended"}`)
	updated := gjson.ParseBytes(liveWSReadUntil(t, conn, "session.updated"))
	if updated.Get("session.instructions").String() != "Answer in Hindi." || updated.Get("client_event_id").String() != "u1" || updated.Get("session.id").String() != "sess_up1" {
		t.Fatalf("session.updated = %s", updated.Raw)
	}
	if srv.opens.Load() != 1 {
		t.Fatal("an instructions update opened a new call")
	}
}

func TestLiveSessionsWSVoiceChangeSwapsTheCall(t *testing.T) {
	queued := make(chan struct{}, 8)
	liveSwapQueuedForTest = func() { queued <- struct{}{} }
	t.Cleanup(func() { liveSwapQueuedForTest = nil })

	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	started := liveWSStart(t, srv, conn, `{"instructions":"Be brief.","audio":{"input":{"transcription":{"language":"gu"}}},"delegation":{"type":"client"}}`)
	id := gjson.GetBytes(started, "session.id").String()
	// The transcription language hint goes upstream right after session.started.
	hint := gjson.ParseBytes(liveWSTakeSent(t, srv.call))
	if hint.Get("type").String() != "session.context.append" || hint.Get("channel").String() != "commentary" || !strings.Contains(hint.Get("content.0.text").String(), "Gujarati") {
		t.Fatalf("language hint = %s", hint.Raw)
	}
	srv.call.push(`{"type":"session.context.appended"}`)

	srv.call.push(`{"type":"turn.done","turn":{"id":"t1","role":"user","transcript":" Hello there"}}`)
	srv.call.push(`{"type":"turn.done","turn":{"id":"t2","role":"assistant","transcript":" Hi, how can I help?"}}`)
	srv.call.push(`{"type":"delegation.created","offset_ms":10,"item":{"id":"item_1","type":"delegation","target":"client","content":[{"type":"input_text","text":"Open Safari"}]}}`)
	liveWSReadUntil(t, conn, "session.delegation.created")
	srv.call.push(`{"type":"session.usage.updated","usage":{"audio_duration_ms":5000}}`)
	if got := gjson.GetBytes(liveWSReadUntil(t, conn, "session.usage.updated"), "usage.seconds").Int(); got != 5 {
		t.Fatalf("usage before = %d", got)
	}

	call2 := newLiveWSFakeCall()
	srv.swapCalls <- call2
	liveWSWrite(t, conn, `{"type":"session.update","event_id":"v1","session":{"instructions":"Speak Hindi.","audio":{"output":{"voice":"cove"}}}}`)

	session2 := gjson.ParseBytes(<-srv.session)
	if session2.Get("audio.output.voice").String() != "cove" || session2.Get("instructions").String() != "Speak Hindi." {
		t.Fatalf("new call session = %s", session2.Raw)
	}
	items := session2.Get("initial_items").Array()
	if len(items) != 4 {
		t.Fatalf("initial_items = %s", session2.Get("initial_items").Raw)
	}
	if items[0].Get("role").String() != "developer" || items[0].Get("content.0.text").String() != "Speak Hindi." {
		t.Fatalf("item 0 = %s", items[0].Raw)
	}
	if items[1].Get("role").String() != "developer" || !strings.Contains(items[1].Get("content.0.text").String(), "Gujarati") {
		t.Fatalf("item 1 = %s", items[1].Raw)
	}
	if items[2].Get("role").String() != "user" || items[2].Get("content.0.text").String() != "Hello there" || items[2].Get("content.0.type").String() != "input_text" {
		t.Fatalf("item 2 = %s", items[2].Raw)
	}
	if items[3].Get("role").String() != "assistant" || items[3].Get("content.0.text").String() != "Hi, how can I help?" || items[3].Get("content.0.type").String() != "output_text" {
		t.Fatalf("item 3 = %s", items[3].Raw)
	}

	// Events sent while the new call opens are held back, not sent to the old call.
	liveWSWrite(t, conn, `{"type":"session.input_audio.append","audio":"`+liveWSPCM(960)+`"}`)
	liveWSWrite(t, conn, `{"type":"session.thinking.append","event_id":"a1","content":"note"}`)
	<-queued
	<-queued
	select {
	case event := <-srv.call.sent:
		t.Fatalf("the old call received %s during the swap", event)
	default:
	}
	select {
	case <-srv.call.closed:
		t.Fatal("the old call was closed before the new one started")
	default:
	}

	call2.push(`{"type":"session.started","session":{"id":"sess_up2"}}`)
	cancelled := gjson.ParseBytes(liveWSReadUntil(t, conn, "error"))
	if cancelled.Get("error.code").String() != "delegation_cancelled" || cancelled.Get("error.delegation_id").String() != "item_1" || !strings.Contains(cancelled.Get("error.message").String(), "item_1") {
		t.Fatalf("delegation error = %s", cancelled.Raw)
	}
	changed := gjson.ParseBytes(liveWSReadUntil(t, conn, "session.voice_changed"))
	if changed.Get("voice").String() != "cove" || changed.Get("previous_voice").String() != "marin" || !changed.Get("gap_ms").Exists() || changed.Get("gap_ms").Int() < 0 {
		t.Fatalf("voice_changed = %s", changed.Raw)
	}
	updated := gjson.ParseBytes(liveWSReadUntil(t, conn, "session.updated"))
	if updated.Get("session.audio.output.voice").String() != "cove" || updated.Get("session.instructions").String() != "Speak Hindi." || updated.Get("client_event_id").String() != "v1" || updated.Get("session.id").String() != id {
		t.Fatalf("session.updated = %s", updated.Raw)
	}
	select {
	case <-srv.call.closed:
	case <-liveWSAfter():
		t.Fatal("the old call was not closed")
	}

	// The new call gets the language hint first, then the held events are replayed.
	if hint2 := gjson.ParseBytes(liveWSTakeSent(t, call2)); !strings.Contains(hint2.Get("content.0.text").String(), "Gujarati") {
		t.Fatalf("language hint on the new call = %s", hint2.Raw)
	}
	call2.push(`{"type":"session.context.appended"}`)
	sent := gjson.ParseBytes(liveWSTakeSent(t, call2))
	if sent.Get("type").String() != "session.context.append" || sent.Get("content.0.text").String() != "note" {
		t.Fatalf("replayed append = %s", sent.Raw)
	}
	if got := liveWSCountFrames(t, call2, 2); got < 1 {
		t.Fatalf("replayed audio frames = %d", got)
	}
	call2.push(`{"type":"session.context.appended"}`)
	if ack := gjson.ParseBytes(liveWSReadUntil(t, conn, "session.thinking.appended")); ack.Get("client_event_id").String() != "a1" {
		t.Fatalf("ack = %s", ack.Raw)
	}

	// Usage adds both calls.
	call2.push(`{"type":"session.usage.updated","usage":{"audio_duration_ms":3000}}`)
	if got := gjson.GetBytes(liveWSReadUntil(t, conn, "session.usage.updated"), "usage.seconds").Int(); got != 8 {
		t.Fatalf("usage after = %d, want 8", got)
	}
	liveWSWrite(t, conn, `{"type":"session.close"}`)
	closed := gjson.ParseBytes(liveWSReadUntil(t, conn, "session.closed"))
	if closed.Get("reason").String() != "close_requested" || closed.Get("usage.seconds").Int() != 8 {
		t.Fatalf("closed = %s", closed.Raw)
	}
	select {
	case <-call2.closed:
	case <-liveWSAfter():
		t.Fatal("the new call was not closed")
	}
	if srv.opens.Load() != 2 {
		t.Fatalf("opens = %d", srv.opens.Load())
	}
}

func TestLiveSessionsWSVoiceChangeFailureKeepsTheOldCall(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)

	srv.swapErr.Store(newLiveGatewayError(503, "no_available_account", "server_error", "No account is available."))
	liveWSWrite(t, conn, `{"type":"session.update","event_id":"v1","session":{"audio":{"output":{"voice":"cove"}}}}`)
	liveWSWrite(t, conn, `{"type":"session.thinking.append","event_id":"a2","content":"still here"}`)
	failed := gjson.ParseBytes(liveWSReadUntil(t, conn, "error"))
	if failed.Get("error.code").String() != "voice_change_failed" || failed.Get("error.client_event_id").String() != "v1" || !strings.Contains(failed.Get("error.message").String(), "No account is available") {
		t.Fatalf("error = %s", failed.Raw)
	}
	sent := gjson.ParseBytes(liveWSTakeSent(t, srv.call))
	if sent.Get("content.0.text").String() != "still here" {
		t.Fatalf("append on the old call = %s", sent.Raw)
	}
	select {
	case <-srv.call.closed:
		t.Fatal("the old call was closed after a failed swap")
	default:
	}
	srv.call.push(`{"type":"output_transcript.added","item":{"text":"hello"},"start_ms":0,"end_ms":200}`)
	liveWSReadUntil(t, conn, "session.output_transcript.delta")
	srv.call.push(`{"type":"session.context.appended"}`)
	liveWSReadUntil(t, conn, "session.thinking.appended")
	liveWSWrite(t, conn, `{"type":"session.close"}`)
	if closed := gjson.ParseBytes(liveWSReadUntil(t, conn, "session.closed")); closed.Get("reason").String() != "close_requested" {
		t.Fatalf("closed = %s", closed.Raw)
	}
	if code := liveWSExpectClose(t, conn); code != websocket.CloseNormalClosure {
		t.Fatalf("close code = %d", code)
	}
}

func TestLiveSessionsWSOtherFieldsStayImmutable(t *testing.T) {
	srv := newLiveWSTestServer(t, true, true)
	conn := srv.dial(t)
	liveWSStart(t, srv, conn, `{}`)
	for param, session := range map[string]string{
		"session.model":        `{"model":"gpt-live-1"}`,
		"session.audio.format": `{"audio":{"format":{"type":"audio/pcm","rate":16000}}}`,
	} {
		liveWSWrite(t, conn, `{"type":"session.update","session":`+session+`}`)
		event := gjson.ParseBytes(liveWSReadUntil(t, conn, "error"))
		if event.Get("error.code").String() != "immutable_field_update" || event.Get("error.param").String() != param {
			t.Fatalf("%s: error = %s", param, event.Raw)
		}
	}
	if srv.opens.Load() != 1 {
		t.Fatal("an immutable update opened a call")
	}
}

func TestLiveSessionsAttachUpdate(t *testing.T) {
	fx := newLiveWebRTCFixture(t)
	id := fx.create(t, `{}`)
	client, _, err := fx.dialAttach(t, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	upstream := liveWebRTCSidebandConn(t, fx)
	if err := upstream.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.started","session":{"id":"rtc_web1"}}`)); err != nil {
		t.Fatal(err)
	}
	liveWebRTCReadClient(t, client)

	if err := client.WriteJSON(map[string]any{"type": "session.update", "event_id": "v1", "session": map[string]any{"audio": map[string]any{"output": map[string]any{"voice": "cove"}}}}); err != nil {
		t.Fatal(err)
	}
	denied := liveWebRTCReadClient(t, client)
	if denied.Get("error.code").String() != "immutable_field_update" || denied.Get("error.param").String() != "session.audio.output.voice" || !strings.Contains(denied.Get("error.message").String(), "WebSocket") || denied.Get("error.client_event_id").String() != "v1" {
		t.Fatalf("voice error = %s", denied.Raw)
	}

	if err := client.WriteJSON(map[string]any{"type": "session.update", "event_id": "i1", "session": map[string]any{"instructions": "Be formal."}}); err != nil {
		t.Fatal(err)
	}
	sent := gjson.Parse(liveWebRTCRead(t, fx.sbReceived))
	if sent.Get("type").String() != "session.context.append" || !strings.HasSuffix(sent.Get("content.0.text").String(), "Be formal.") {
		t.Fatalf("sideband got %s", sent.Raw)
	}
	if err := upstream.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.context.appended"}`)); err != nil {
		t.Fatal(err)
	}
	updated := liveWebRTCReadClient(t, client)
	if updated.Get("type").String() != "session.updated" || updated.Get("session.instructions").String() != "Be formal." || updated.Get("client_event_id").String() != "i1" {
		t.Fatalf("session.updated = %s", updated.Raw)
	}
}

func TestLiveSessionHistoryStaysInsideTheCaps(t *testing.T) {
	tr, _ := liveTestStarted(t, `{"instructions":"Be brief.","audio":{"input":{"transcription":{"language":"hi"}}}}`)
	for i := 0; i < 200; i++ {
		role := []string{"user", "assistant"}[i%2]
		tr.Upstream([]byte(fmt.Sprintf(`{"type":"turn.done","turn":{"id":"t%d","role":"%s","transcript":"turn number %d"}}`, i, role, i)))
	}
	cfg, _ := tr.SwapConfig(&liveVoiceChange{Voice: "cove", LiveVoice: "cove"})
	if len(cfg.InitialItems) != liveSessionMaxInputItems-2 {
		t.Fatalf("items = %d, want %d", len(cfg.InitialItems), liveSessionMaxInputItems-2)
	}
	if last := gjson.ParseBytes(cfg.InitialItems[len(cfg.InitialItems)-1]).Get("content.0.text").String(); last != "turn number 199" {
		t.Fatalf("newest item = %q", last)
	}
	if first := gjson.ParseBytes(cfg.InitialItems[0]).Get("content.0.text").String(); first != "turn number 74" {
		t.Fatalf("oldest item = %q", first)
	}

	big := strings.Repeat("word ", 1600) // 8,000 bytes, about 2,000 tokens
	for i := 0; i < 6; i++ {
		tr.Upstream([]byte(fmt.Sprintf(`{"type":"turn.done","turn":{"id":"b%d","role":"user","transcript":%q}}`, i, big)))
	}
	cfg, _ = tr.SwapConfig(&liveVoiceChange{Voice: "cove", LiveVoice: "cove"})
	total := 0
	for _, item := range cfg.InitialItems {
		total += len(gjson.ParseBytes(item).Get("content.0.text").String())
	}
	if total/4 > liveSessionMaxInputTokens || len(cfg.InitialItems) >= liveSessionMaxInputItems-2 {
		t.Fatalf("tokens = %d items = %d", total/4, len(cfg.InitialItems))
	}
}

// liveWSAfter is a channel that fires after the test timeout, for a select that waits on a closed signal.
func liveWSAfter() <-chan time.Time { return time.After(liveWSTestTimeout) }
