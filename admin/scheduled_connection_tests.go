package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const scheduledTestCredentialKey = "scheduled_connection_tests"
const scheduledTestTimezone = "Asia/Shanghai"
const scheduledTestJobPrefix = "scheduled_connection_test:"
const scheduledTestContextKey = "internal_scheduled_connection_test"

var scheduledTestTimePattern = regexp.MustCompile(`^(?:[01]\d|2[0-3]):[0-5]\d$`)

type accountScheduledTest struct {
	Enabled      bool     `json:"enabled"`
	Model        string   `json:"model"`
	Content      string   `json:"content"`
	WeekdayTimes []string `json:"weekday_times"`
	WeekendTimes []string `json:"weekend_times"`
}

type scheduledTestRecord struct {
	ScheduledAt   string `json:"scheduled_at"`
	AttemptedAt   string `json:"attempted_at,omitempty"`
	AccountID     int64  `json:"account_id"`
	Model         string `json:"model,omitempty"`
	Status        string `json:"status"`
	Success       bool   `json:"success"`
	Error         string `json:"error,omitempty"`
	NextReset5hAt string `json:"next_reset_5h_at,omitempty"`
}

func normalizeScheduledTestConfig(raw accountScheduledTest) (accountScheduledTest, error) {
	raw.Model = strings.TrimSpace(raw.Model)
	if len(raw.Model) > 256 {
		return raw, errors.New("the test model name is too long")
	}
	if strings.TrimSpace(raw.Content) == "" || len([]rune(raw.Content)) > auth.MaxTestContentRunes {
		return raw, errors.New("the test content cannot be empty and cannot exceed 8192 characters")
	}
	if raw.Enabled && raw.Model == "" {
		return raw, errors.New("select a test model")
	}
	normalize := func(values []string) ([]string, error) {
		result := make([]string, 0, len(values))
		seen := map[string]bool{}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if !scheduledTestTimePattern.MatchString(value) {
				return nil, errors.New("the test time format must be HH:mm")
			}
			if !seen[value] {
				result = append(result, value)
				seen[value] = true
			}
		}
		sort.Strings(result)
		return result, nil
	}
	var err error
	raw.WeekdayTimes, err = normalize(raw.WeekdayTimes)
	if err != nil {
		return raw, err
	}
	raw.WeekendTimes, err = normalize(raw.WeekendTimes)
	if err != nil {
		return raw, err
	}
	if raw.Enabled && len(raw.WeekdayTimes)+len(raw.WeekendTimes) == 0 {
		return raw, errors.New("add at least one test time point before enabling")
	}
	return raw, nil
}

func decodeScheduledTestConfig(raw string) (accountScheduledTest, error) {
	if strings.TrimSpace(raw) == "" {
		return accountScheduledTest{Content: "hi", WeekdayTimes: []string{}, WeekendTimes: []string{}}, nil
	}
	var config accountScheduledTest
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return config, errors.New("the scheduled activation config is invalid")
	}
	return normalizeScheduledTestConfig(config)
}

func scheduledTestDue(config accountScheduledTest, now time.Time) bool {
	if !config.Enabled {
		return false
	}
	loc, err := time.LoadLocation(scheduledTestTimezone)
	if err != nil {
		return false
	}
	now = now.In(loc)
	times := config.WeekdayTimes
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		times = config.WeekendTimes
	}
	for _, value := range times {
		if value == now.Format("15:04") {
			return true
		}
	}
	return false
}

func (h *Handler) scheduledTestAccount(c *gin.Context) (*database.AccountRow, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(400, gin.H{"error": "Invalid account ID"})
		return nil, false
	}
	row, err := h.db.GetAccountByID(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(404, gin.H{"error": "账号不存在"})
		return nil, false
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "Failed to read the account"})
		return nil, false
	}
	upstream := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	if upstream != "" && upstream != "codex" {
		c.JSON(400, gin.H{"error": "Scheduled activation only supports Codex accounts"})
		return nil, false
	}
	return row, true
}

func (h *Handler) GetScheduledConnectionTests(c *gin.Context) {
	row, ok := h.scheduledTestAccount(c)
	if !ok {
		return
	}
	config, err := decodeScheduledTestConfig(row.GetCredential(scheduledTestCredentialKey))
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	jobs, err := h.db.ListOneShotMaintenanceJobs(c.Request.Context(), row.ID, scheduledTestJobPrefix, 10)
	if err != nil {
		c.JSON(500, gin.H{"error": "Failed to read execution records"})
		return
	}
	records := make([]scheduledTestRecord, 0, len(jobs))
	for _, job := range jobs {
		record := scheduledTestRecord{AccountID: row.ID, ScheduledAt: job.DueAt.Format(time.RFC3339), Status: "unknown"}
		if job.LastError != "" {
			_ = json.Unmarshal([]byte(job.LastError), &record)
		}
		records = append(records, record)
	}
	c.JSON(200, gin.H{"config": config, "timezone": scheduledTestTimezone, "records": records})
}

func (h *Handler) UpdateScheduledConnectionTests(c *gin.Context) {
	row, ok := h.scheduledTestAccount(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var config accountScheduledTest
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(400, gin.H{"error": "Invalid test plan JSON"})
		return
	}
	config, err := normalizeScheduledTestConfig(config)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if config.Enabled && config.Model != "" {
		account := h.store.FindByID(row.ID)
		if account == nil {
			c.JSON(409, gin.H{"error": "The account is not loaded, so the test model cannot be validated"})
			return
		}
		model, modelErr := h.connectionTestModelForAccount(c.Request.Context(), account, config.Model)
		if modelErr != nil {
			c.JSON(400, gin.H{"error": modelErr.Error()})
			return
		}
		config.Model = model
	}
	encoded, _ := json.Marshal(config)
	if err := h.db.UpdateCredentials(c.Request.Context(), row.ID, map[string]interface{}{scheduledTestCredentialKey: string(encoded)}); err != nil {
		c.JSON(500, gin.H{"error": "Failed to save the scheduled activation"})
		return
	}
	c.JSON(200, config)
}

func (h *Handler) StartScheduledConnectionTests(ctx context.Context) {
	if h == nil || h.store == nil || h.db == nil || ctx == nil {
		return
	}
	h.scheduledTestStartOnce.Do(func() {
		h.scheduledTestWG.Add(1)
		go func() {
			defer h.scheduledTestWG.Done()
			for {
				timer := time.NewTimer(time.Until(time.Now().Truncate(time.Minute).Add(time.Minute)))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				h.dispatchScheduledConnectionTests(ctx, time.Now())
			}
		}()
	})
}

// The minute loop remains alive while dispatch adds workers to the wait group.
// Slow batches must not delay claiming the next minute's scheduled slots.
func (h *Handler) dispatchScheduledConnectionTests(ctx context.Context, now time.Time) {
	h.scheduledTestWG.Add(1)
	go func() {
		defer h.scheduledTestWG.Done()
		h.runScheduledConnectionTests(ctx, now)
	}()
}

func (h *Handler) scheduledConnectionTestGate() chan struct{} {
	h.scheduledTestGateOnce.Do(func() {
		h.scheduledTestGate = make(chan struct{}, 4)
	})
	return h.scheduledTestGate
}

func (h *Handler) WaitScheduledConnectionTests() {
	if h != nil {
		h.scheduledTestWG.Wait()
	}
}

func (h *Handler) runScheduledConnectionTests(ctx context.Context, now time.Time) {
	loc, err := time.LoadLocation(scheduledTestTimezone)
	if err != nil {
		log.Printf("[scheduled-test] time zone is unavailable")
		return
	}
	now = now.In(loc)
	plans, err := h.db.ListScheduledConnectionTestConfigs(ctx)
	if err != nil {
		log.Printf("[scheduled-test] failed to read the plan: %v", err)
		return
	}
	// Bound concurrency across overlapping minute batches, not just within one.
	gate := h.scheduledConnectionTestGate()
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, plan := range plans {
		config, decodeErr := decodeScheduledTestConfig(plan.Config)
		if decodeErr != nil {
			log.Printf("[scheduled-test] account %d config is invalid", plan.AccountID)
			continue
		}
		if !scheduledTestDue(config, now) {
			continue
		}
		key := scheduledTestJobPrefix + now.Format("2006-01-02T15:04")
		claimed, claimErr := h.db.TryClaimMaintenanceJob(ctx, plan.AccountID, key, now)
		if claimErr != nil {
			log.Printf("[scheduled-test] account %d failed to declare its slot", plan.AccountID)
			continue
		}
		if !claimed {
			continue
		}
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			h.finishScheduledConnectionTestSlot(key, scheduledTestRecord{
				AccountID: plan.AccountID, ScheduledAt: now.Format(time.RFC3339),
				Model: config.Model, Status: "failed", Error: "The test was canceled before it ran",
			})
			return
		}
		wg.Add(1)
		go func(id int64, config accountScheduledTest) {
			defer wg.Done()
			defer func() { <-gate }()
			h.runScheduledConnectionTestSlot(ctx, id, key, now, config)
		}(plan.AccountID, config)
	}
}

func (h *Handler) runScheduledConnectionTestSlot(parent context.Context, id int64, key string, now time.Time, config accountScheduledTest) {
	result := scheduledTestRecord{AccountID: id, ScheduledAt: now.Format(time.RFC3339), AttemptedAt: time.Now().In(now.Location()).Format(time.RFC3339), Model: config.Model, Status: "failed"}
	// Persist the outcome even if shutdown cancels the single upstream attempt.
	defer func() {
		if recover() != nil {
			result.Error = "The test ended abnormally"
			result.Success = false
			result.Status = "failed"
		}
		if account := h.store.FindByID(id); account != nil {
			if reset := account.GetReset5hAt(); !reset.IsZero() {
				result.NextReset5hAt = reset.Format(time.RFC3339)
			}
		}
		h.finishScheduledConnectionTestSlot(key, result)
	}()
	// Re-read before execution so deletion/disable of an account or plan cancels queued work.
	row, err := h.db.GetAccountByID(parent, id)
	if err != nil || !row.Enabled {
		result.Status = "skipped"
		result.Error = "The account is disabled or deleted"
		return
	}
	current, err := decodeScheduledTestConfig(row.GetCredential(scheduledTestCredentialKey))
	if err != nil || !scheduledTestDue(current, now) {
		result.Status = "skipped"
		result.Error = "The plan was changed or disabled"
		return
	}
	config = current
	result.Model = config.Model
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	complete := false
	router := gin.New()
	router.GET("/accounts/:id/test", func(c *gin.Context) { c.Set(scheduledTestContextKey, true); h.TestConnection(c) })
	params := url.Values{"model": {config.Model}, "prompt": {config.Content}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/accounts/%d/test?%s", id, params.Encode()), nil)
	if err != nil {
		result.Error = "Failed to create the test request"
		return
	}
	// Reuse exactly one connection-test execution; consume SSE events in process.
	// No loop, HTTP self-call, pool selector, or quality-job retry is involved.
	router.ServeHTTP(&qualityJobWriter{header: make(http.Header), emit: func(event testEvent) {
		switch event.Type {
		case "error":
			result.Error = event.Error
		case "test_complete":
			complete = event.Success
		}
	}}, request)
	if ctx.Err() != nil {
		result.Error = "The test was canceled or timed out"
	}
	if result.Error == "" && !complete {
		result.Error = "No successful test event was received"
	}
	result.Success = result.Error == "" && complete
	if result.Success {
		result.Status = "success"
	}
	if account := h.store.FindByID(id); account != nil {
		result.Error = sanitizeCodexTestText(result.Error, codexTestSecrets(account))
	}
}

func (h *Handler) finishScheduledConnectionTestSlot(key string, result scheduledTestRecord) {
	encoded, _ := json.Marshal(result)
	finalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.db.FinishMaintenanceJob(finalCtx, result.AccountID, key, string(encoded)); err != nil {
		log.Printf("[scheduled-test] account %d failed to write the record", result.AccountID)
	}
	log.Printf("[scheduled-test] %s", encoded)
}
