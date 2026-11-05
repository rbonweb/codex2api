package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// sendCodexTelemetryJob sends one batch of telemetry using the account's network configuration.
//
// Telemetry, like /responses, carries the account identity and goes to chatgpt.com. When Resin is enabled it must likewise go through the reverse proxy,
// otherwise all accounts would share this machine's egress IP for direct connections, which is inconsistent with the egress of that account's /responses traffic
// (the invariant of issue #372). The metrics endpoint carries no Bearer token, but the real client sends from the same egress,
// so this stays consistent here.
func sendCodexTelemetryJob(job codexTelemetryJob) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexTelemetryTimeout)
	defer cancel()
	finalURL, resinClient, viaResin := resinMaintenanceTarget(job.client.account, job.url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, finalURL, bytes.NewReader(job.body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if job.metrics {
		req.Header.Set("User-Agent", "OTel-OTLP-Exporter-Rust/0.31.0")
		req.Header.Set("statsig-api-key", codexStatsigAPIKey())
	} else {
		applyCodexAnalyticsHeaders(req.Header, job.client)
	}
	client := codexTelemetryHTTPClient(getPooledClient(job.client.account, job.client.proxyURL))
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(job.client.account))
		client = codexTelemetryHTTPClient(resinClient)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &codexTelemetryHTTPError{status: resp.StatusCode}
	}
	return nil
}

// codexTelemetryHTTPClient clones the shared pooled client and forbids following redirects.
//
// Analytics requests carry the account's Bearer access token; Go's default policy only strips
// Authorization on cross-host redirects, and an https→http downgrade on the same host is not stripped. The pooled client is reused per account,
// and cannot be modified in place, so here we make a shallow copy and override CheckRedirect, to avoid forwarding the credential
// to the redirect target. This matches the existing approach in grok_media.go / claude_api_key.go.
func codexTelemetryHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		return &http.Client{CheckRedirect: codexTelemetryRejectRedirect}
	}
	clone := *base
	clone.CheckRedirect = codexTelemetryRejectRedirect
	return &clone
}

// codexTelemetryRejectRedirect refuses to follow any redirect.
func codexTelemetryRejectRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

type codexTelemetryHTTPError struct{ status int }

// Error returns the HTTP status description of the telemetry upstream.
func (e *codexTelemetryHTTPError) Error() string { return http.StatusText(e.status) }

// applyCodexAnalyticsHeaders adds the client identity headers required by the Codex analytics endpoint.
//
// The real client's send_track_events_request explicitly adds only the auth header and Content-Type, but its
// create_client() injects originator and User-Agent into all requests through default_headers()
// (codex-rs/login/src/auth/default_client.rs), so these two headers are sent here as well.
func applyCodexAnalyticsHeaders(headers http.Header, client codexTelemetryClient) {
	headers.Set("Authorization", "Bearer "+client.accessToken)
	headers.Set("Chatgpt-Account-Id", client.accountID)
	headers.Set("User-Agent", client.userAgent)
	headers.Set("Originator", client.originator)
}
