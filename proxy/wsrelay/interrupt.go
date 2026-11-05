package wsrelay

import (
	"fmt"
	"io"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// interrupt writes directly to the currently committed lease, and cannot reuse the response.create lease commit entry point.
func (r *WsResponse) interrupt(payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.streamCompleted || r.connBroken || r.conn == nil || r.pendingReq == nil {
		return proxy.ErrResponsesWSInterruptFinished
	}
	r.interruptResponseID = gjson.GetBytes(payload, "response_id").String()
	if err := r.conn.writeInterrupt(payload, r.pendingReq.RequestID); err != nil {
		r.interruptResponseID = ""
		return err
	}
	r.interruptTimer = time.AfterFunc(proxy.ResponsesWSInterruptAckTimeout, r.expireInterrupt)
	return nil
}

func (r *WsResponse) expireInterrupt() {
	r.mu.Lock()
	if r.closed || r.interruptResponseID == "" {
		r.mu.Unlock()
		return
	}
	r.connBroken = true
	r.mu.Unlock()
	if r.manager != nil {
		r.manager.DiscardConnection(r.conn)
	} else {
		_ = r.conn.Close()
	}
}

func (wc *WsConnection) writeInterrupt(payload []byte, requestID string) error {
	wc.writeMu.Lock()
	defer wc.writeMu.Unlock()
	if !wc.IsConnected() || wc.conn == nil {
		return proxy.ErrResponsesWSInterruptFinished
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	if state.activeLease != requestID || state.leasePhase != readLeaseCommitted || state.readerStopped {
		state.mu.Unlock()
		return proxy.ErrResponsesWSInterruptFinished
	}
	state.interruptResponseID = gjson.GetBytes(payload, "response_id").String()
	state.mu.Unlock()
	if err := wc.conn.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil {
		wc.finishReadPumpForLease(err, requestID)
		return err
	}
	defer wc.conn.SetWriteDeadline(time.Time{})
	if err := wc.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		wc.finishReadPumpForLease(err, requestID)
		return err
	}
	return nil
}

func isInterruptCompletion(payload []byte) bool {
	switch gjson.GetBytes(payload, "type").String() {
	case "response.completed", "response.incomplete", "response.done":
		return true
	}
	return false
}

// Keep the original event order; when the terminal state arrives first, keep reading to confirm, to avoid polluting the next round's lease.
func (r *WsResponse) handleInterruptMessage(payload []byte, callback func([]byte) bool) (bool, error) {
	eventType := gjson.GetBytes(payload, "type").String()
	r.mu.Lock()
	if isInterruptCompletion(payload) && r.interruptResponseID != "" {
		duplicate := r.interruptTerminalSeen
		r.interruptTerminalID = gjson.GetBytes(payload, "response.id").String()
		r.interruptTerminalSeen = true
		r.mu.Unlock()
		if !duplicate && !callback(payload) {
			r.markConnBroken()
			return true, io.EOF
		}
		return true, nil
	}
	if eventType != "response.interrupt.accepted" && eventType != "response.interrupt.failed" {
		r.mu.Unlock()
		return false, nil
	}
	if r.interruptResponseID == "" || gjson.GetBytes(payload, "response_id").String() != r.interruptResponseID {
		r.mu.Unlock()
		r.markConnBroken()
		return true, fmt.Errorf("unexpected response interruption acknowledgement")
	}
	r.mu.Unlock()
	return true, r.finishInterruptAcknowledgement(payload, callback)
}

func (r *WsResponse) finishInterruptAcknowledgement(payload []byte, callback func([]byte) bool) error {
	r.mu.Lock()
	terminalID, terminalSeen := r.interruptTerminalID, r.interruptTerminalSeen
	r.interruptResponseID, r.interruptTerminalID, r.interruptTerminalSeen = "", "", false
	if r.interruptTimer != nil {
		r.interruptTimer.Stop()
	}
	r.mu.Unlock()
	if terminalSeen {
		r.bindCompletedResponse(terminalID)
		r.markStreamCompleted()
	}
	if !callback(payload) {
		if !terminalSeen {
			r.markConnBroken()
		}
		return io.EOF
	}
	if terminalSeen {
		return io.EOF
	}
	return nil
}
