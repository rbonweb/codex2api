package proxy

import "testing"

// selectNativeProjector 必须按真实上游协议选择投影器，而不是按下游入口路径猜测。
func TestNativeHTTP_SelectNativeProjectorDispatchesByUpstreamProtocol(t *testing.T) {
	cases := []struct {
		name            string
		protocol        string
		codexResponses  bool
		wantProjector   any
		wantDescription string
	}{
		{name: "codex_responses", protocol: "chat", codexResponses: true, wantProjector: &ResponsesProjector{}, wantDescription: "ResponsesProjector"},
		{name: "chat_protocol", protocol: "chat", wantProjector: &ChatProjector{}, wantDescription: "ChatProjector"},
		{name: "chat_completions_protocol", protocol: "chat_completions", wantProjector: &ChatProjector{}, wantDescription: "ChatProjector"},
		{name: "responses_protocol", protocol: "responses", wantProjector: &ResponsesProjector{}, wantDescription: "ResponsesProjector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectNativeProjector(tc.protocol, tc.codexResponses)
			switch tc.wantProjector.(type) {
			case *ResponsesProjector:
				if _, ok := got.(*ResponsesProjector); !ok {
					t.Fatalf("projector = %T, want %s", got, tc.wantDescription)
				}
			case *ChatProjector:
				if _, ok := got.(*ChatProjector); !ok {
					t.Fatalf("projector = %T, want %s", got, tc.wantDescription)
				}
			}
		})
	}
}
