package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// TestErrorMissingAPIKey tests the missing API key error
func TestErrorMissingAPIKey(t *testing.T) {
	err := ErrMissingAPIKey()

	if err.Code != ErrorCodeMissingAPIKey {
		t.Errorf("expected code %s, got %s", ErrorCodeMissingAPIKey, err.Code)
	}
	if err.Type != ErrorTypeAuthentication {
		t.Errorf("expected type %s, got %s", ErrorTypeAuthentication, err.Type)
	}
	if err.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, err.HTTPStatus)
	}
	if err.Retryable {
		t.Error("missing API key error should not be retryable")
	}
}

// TestErrorRateLimited tests the rate-limit error
func TestErrorRateLimited(t *testing.T) {
	err := ErrRateLimited("custom rate limit message")

	if err.Code != ErrorCodeRateLimited {
		t.Errorf("expected code %s, got %s", ErrorCodeRateLimited, err.Code)
	}
	if err.Message != "custom rate limit message" {
		t.Errorf("expected message %s, got %s", "custom rate limit message", err.Message)
	}
	if err.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("expected status %d, got %d", http.StatusTooManyRequests, err.HTTPStatus)
	}
	if !err.Retryable {
		t.Error("rate limited error should be retryable")
	}
}

// TestErrorAccountPoolUsageLimit tests the account pool usage limit error
func TestErrorAccountPoolUsageLimit(t *testing.T) {
	err := ErrAccountPoolUsageLimit("quota exhausted", "plus", 0, 60)

	if err.Code != ErrorCodeAccountPoolUsageLimit {
		t.Errorf("expected code %s, got %s", ErrorCodeAccountPoolUsageLimit, err.Code)
	}
	// Should include the plan type and reset information
	if err.Message == "" {
		t.Error("message should not be empty")
	}
	if err.HTTPStatus != http.StatusServiceUnavailable {
		t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, err.HTTPStatus)
	}
	if !err.Retryable {
		t.Error("account pool usage limit error should be retryable")
	}
}

// TestErrorUpstream tests the upstream error
func TestErrorUpstream(t *testing.T) {
	cause := errors.New("connection refused")
	err := ErrUpstream(http.StatusBadGateway, "upstream failed", cause)

	if err.Code != ErrorCodeUpstreamError {
		t.Errorf("expected code %s, got %s", ErrorCodeUpstreamError, err.Code)
	}
	if err.HTTPStatus != http.StatusBadGateway {
		t.Errorf("expected status %d, got %d", http.StatusBadGateway, err.HTTPStatus)
	}
	if err.Cause != cause {
		t.Error("cause should match")
	}
	if err.Retryable {
		t.Error("502 upstream error should not be legacy retryable")
	}

	if timeout := ErrUpstream(http.StatusGatewayTimeout, "upstream timeout", cause); timeout.Retryable {
		t.Error("504 upstream error should not be legacy retryable")
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		if upstream := ErrUpstream(status, "legacy retryable", cause); !upstream.Retryable {
			t.Errorf("status %d upstream error should remain legacy retryable", status)
		}
	}
	if notFound := ErrUpstream(http.StatusNotFound, "not found", cause); notFound.Retryable {
		t.Error("404 upstream error should not be retryable without a narrow classification")
	}
}

func TestUpstreamErrorBodyIsValidJSONForInvalidUTF8(t *testing.T) {
	err := &Error{
		Code:    "upstream_error",
		Message: string([]byte{'b', 0xff, 'd'}),
		Type:    ErrorTypeUpstreamError,
	}
	body := err.UpstreamErrorBody()
	if !json.Valid(body) {
		t.Fatalf("UpstreamErrorBody returned invalid JSON: %q", body)
	}
}

// TestErrorUnwrap tests the error chain Unwrap
func TestErrorUnwrap(t *testing.T) {
	cause := errors.New("underlying error")
	err := ErrUpstream(http.StatusInternalServerError, "server error", cause)

	// Using errors.Unwrap
	unwrapped := errors.Unwrap(err)
	if unwrapped != cause {
		t.Error("Unwrap should return the cause")
	}

	// Using errors.Is
	if !errors.Is(err, cause) {
		t.Error("errors.Is should match the cause")
	}
}

// TestIsRetryableError tests the IsRetryableError function
func TestIsRetryableError(t *testing.T) {
	// Retryable error
	retryable := ErrRateLimited("rate limited")
	if !IsRetryableError(retryable) {
		t.Error("rate limited error should be retryable")
	}

	// Non-retryable error
	notRetryable := ErrMissingAPIKey()
	if IsRetryableError(notRetryable) {
		t.Error("missing API key error should not be retryable")
	}

	// nil error
	if IsRetryableError(nil) {
		t.Error("nil error should not be retryable")
	}

	// wrapped error
	wrapped := errors.Join(retryable, errors.New("additional context"))
	if !IsRetryableError(wrapped) {
		t.Error("wrapped retryable error should be retryable")
	}
}

// TestStatusCodeFromError tests the StatusCodeFromError function
func TestStatusCodeFromError(t *testing.T) {
	// Error with a status code
	err := ErrRateLimited("rate limited")
	status := StatusCodeFromError(err)
	if status != http.StatusTooManyRequests {
		t.Errorf("expected status %d, got %d", http.StatusTooManyRequests, status)
	}

	// nil error
	status = StatusCodeFromError(nil)
	if status != http.StatusOK {
		t.Errorf("expected status %d for nil error, got %d", http.StatusOK, status)
	}

	// wrapped error
	wrapped := errors.Join(err, errors.New("wrapped"))
	status = StatusCodeFromError(wrapped)
	if status != http.StatusTooManyRequests {
		t.Errorf("expected status %d for wrapped error, got %d", http.StatusTooManyRequests, status)
	}
}

// TestErrorErrorMethod tests the string output of the Error method
func TestErrorErrorMethod(t *testing.T) {
	// Error without a cause
	err := ErrMissingAPIKey()
	errStr := err.Error()
	expected := "missing_api_key: Missing Authorization header"
	if errStr != expected {
		t.Errorf("expected %s, got %s", expected, errStr)
	}

	// Error with a cause
	cause := errors.New("connection timeout")
	err = ErrUpstream(http.StatusGatewayTimeout, "timeout", cause)
	errStr = err.Error()
	expected = "upstream_error: timeout (caused by: connection timeout)"
	if errStr != expected {
		t.Errorf("expected %s, got %s", expected, errStr)
	}
}
