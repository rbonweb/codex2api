package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var errLoading = errors.New("LOADING Redis is loading the dataset in memory")

func TestIsRedisLoadingError(t *testing.T) {
	if !isRedisLoadingError(errLoading) {
		t.Fatal("a LOADING reply should be recognized as a loading error")
	}
	if isRedisLoadingError(errors.New("NOAUTH Authentication required")) {
		t.Fatal("an auth error should not be recognized as a loading error")
	}
	if isRedisLoadingError(errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")) {
		t.Fatal("connection refused should not be recognized as a loading error")
	}
	if isRedisLoadingError(nil) {
		t.Fatal("nil should not be recognized as a loading error")
	}
}

func TestWaitForRedisLoadingSucceedsAfterRetries(t *testing.T) {
	calls := 0
	ping := func(context.Context) error {
		calls++
		if calls <= 3 {
			return errLoading
		}
		return nil
	}
	var slept time.Duration
	sleep := func(d time.Duration) { slept += d }

	if err := waitForRedisLoading(context.Background(), ping, time.Minute, sleep); err != nil {
		t.Fatalf("should succeed after loading completes: %v", err)
	}
	if calls != 4 {
		t.Fatalf("expected 4 pings (3 LOADING + 1 success), got %d", calls)
	}
	if slept == 0 {
		t.Fatal("there should be a wait between retries")
	}
}

func TestWaitForRedisLoadingFailsFastOnOtherErrors(t *testing.T) {
	calls := 0
	authErr := errors.New("WRONGPASS invalid username-password pair")
	ping := func(context.Context) error {
		calls++
		return authErr
	}
	err := waitForRedisLoading(context.Background(), ping, time.Minute, func(time.Duration) {})
	if !errors.Is(err, authErr) {
		t.Fatalf("a non-LOADING error should be returned as is: %v", err)
	}
	if calls != 1 {
		t.Fatalf("a non-LOADING error should not be retried, but pinged %d times", calls)
	}
}

func TestWaitForRedisLoadingZeroWaitFailsFast(t *testing.T) {
	calls := 0
	ping := func(context.Context) error {
		calls++
		return errLoading
	}
	err := waitForRedisLoading(context.Background(), ping, 0, func(time.Duration) {})
	if !errors.Is(err, errLoading) {
		t.Fatalf("with wait=0 the LOADING error should be returned immediately: %v", err)
	}
	if calls != 1 {
		t.Fatalf("with wait=0 there should be no retry, but pinged %d times", calls)
	}
}

func TestWaitForRedisLoadingTimesOut(t *testing.T) {
	ping := func(context.Context) error { return errLoading }
	err := waitForRedisLoading(context.Background(), ping, 10*time.Second, func(time.Duration) {})
	if err == nil {
		t.Fatal("persistent LOADING should return an error after the timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("the timeout error should include the timeout description: %v", err)
	}
	if !errors.Is(err, errLoading) {
		t.Fatalf("the timeout error should wrap the original LOADING error: %v", err)
	}
}

func TestRedisLoadingWaitEnvOverride(t *testing.T) {
	t.Setenv("CODEX_REDIS_LOADING_WAIT_SECONDS", "")
	if got := redisLoadingWait(); got != defaultRedisLoadingWait {
		t.Fatalf("default should be %s, got %s", defaultRedisLoadingWait, got)
	}
	t.Setenv("CODEX_REDIS_LOADING_WAIT_SECONDS", "30")
	if got := redisLoadingWait(); got != 30*time.Second {
		t.Fatalf("expected 30s, got %s", got)
	}
	t.Setenv("CODEX_REDIS_LOADING_WAIT_SECONDS", "0")
	if got := redisLoadingWait(); got != 0 {
		t.Fatalf("0 should disable the wait, got %s", got)
	}
	t.Setenv("CODEX_REDIS_LOADING_WAIT_SECONDS", "abc")
	if got := redisLoadingWait(); got != defaultRedisLoadingWait {
		t.Fatalf("an invalid value should fall back to the default, got %s", got)
	}
}
