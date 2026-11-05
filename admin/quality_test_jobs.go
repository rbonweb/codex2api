package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Called before accepting HTTP requests. Shutdown cancels work before closing stores.
func (h *Handler) StartQualityTests(ctx context.Context) { h.qualityTestContext = ctx }
func (h *Handler) WaitQualityTests()                     { h.qualityTestWG.Wait() }

func (h *Handler) CreateQualityTestJob(c *gin.Context) {
	if h.db == nil || (h.qualityTestContext != nil && h.qualityTestContext.Err() != nil) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Quality test job service unavailable"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	var req qualityTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid test request"})
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.ReasoningEffort = strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if req.Model == "" || len(req.Model) > 200 || strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > qualityTestPromptLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Select a model; the test prompt cannot be empty and cannot exceed 16000 bytes"})
		return
	}
	timeout := time.Duration(req.TimeoutMinutes) * time.Minute
	if req.TimeoutMinutes != 0 && (timeout < database.QualityTestMinTimeout || timeout > database.QualityTestMaxTimeout) {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Timeout must be between %d and %d minutes", int(database.QualityTestMinTimeout.Minutes()), int(database.QualityTestMaxTimeout.Minutes()))})
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid account ID"})
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Account is not in the runtime pool"})
		return
	}
	if err := h.validateQualityTestForAccount(c.Request.Context(), account, req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	channel := "codex"
	account.Mu().RLock()
	name, plan := account.Email, account.PlanType
	if name == "" {
		name = fmt.Sprintf("ID %d", id)
	}
	account.Mu().RUnlock()
	row, err := h.db.GetAccountByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read account information"})
		return
	}
	if row != nil && row.Name != "" {
		name = row.Name
	}
	// Admission is short and detached: navigating away after submitting cannot leave
	// a committed record without its runner. Database uniqueness arbitrates all replicas.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	presetKind, presetRef, presetName := h.resolveQualityTestPreset(ctx, req)
	job, err := h.db.CreateQualityTestJob(ctx, database.QualityTestJob{AccountID: id, AccountName: name, PlanType: plan, Channel: channel, Model: req.Model, ReasoningEffort: req.ReasoningEffort, Prompt: req.Prompt, TimeoutMS: timeout.Milliseconds(), PresetKind: presetKind, PresetRef: presetRef, PresetName: presetName})
	if err != nil {
		if errors.Is(err, database.ErrQualityTestCapacity) || errors.Is(err, database.ErrQualityTestAccountBusy) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the test job"})
		return
	}
	if presetKind == "custom" {
		if err := h.db.IncrementQualityTestPromptUsage(ctx, req.PromptID); err != nil {
			log.Printf("[quality-test] job=%d preset=%d usage bookkeeping failed: %v", job.ID, req.PromptID, err)
		}
	}
	h.qualityTestWG.Add(1)
	if !h.startDBBackgroundTaskWithParent(h.qualityTestContext, func(parent context.Context) { defer h.qualityTestWG.Done(); h.runQualityTestJob(parent, *job, req) }) {
		h.qualityTestWG.Done()
		job.Status, job.Error = "interrupted", "The service is shutting down; start the test again"
		if err := h.db.FinishQualityTest(ctx, *job); err != nil {
			log.Printf("[quality-test] job=%d admission finalization failed: %v", job.ID, err)
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "The test job service is shutting down"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job": job})
}

func (h *Handler) ListQualityTests(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if size < 1 || size > 50 {
		size = 20
	}
	filter := database.QualityTestFilter{PlanType: strings.TrimSpace(c.Query("plan")), Model: strings.TrimSpace(c.Query("model"))}
	if effort, ok := c.GetQuery("effort"); ok {
		// "default" selects runs that used the model default (stored as "").
		filter.HasEffort = true
		if effort = strings.ToLower(strings.TrimSpace(effort)); effort != "default" {
			filter.ReasoningEffort = effort
		}
	}
	if id, err := strconv.ParseInt(c.Query("account_id"), 10, 64); err == nil && id > 0 {
		filter.AccountID = id
	}
	// preset=none | builtin:<key> | custom:<id>
	if preset, ok := c.GetQuery("preset"); ok {
		preset = strings.TrimSpace(preset)
		filter.HasPreset = true
		if kind, ref, found := strings.Cut(preset, ":"); found && (kind == "builtin" || kind == "custom") && ref != "" {
			filter.PresetKind, filter.PresetRef = kind, ref
		} else if preset != "none" {
			filter.HasPreset = false
		}
	}
	jobs, err := h.db.ListQualityTests(c.Request.Context(), page, size, filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read test records"})
		return
	}
	c.JSON(http.StatusOK, jobs)
}

func (h *Handler) GetQualityTest(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid test record ID"})
		return
	}
	job, err := h.db.GetQualityTestJob(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Test record not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read test records"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}

func (h *Handler) CancelQualityTest(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid test record ID"})
		return
	}
	if err := h.db.CancelQualityTest(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to stop the test"})
		return
	}
	h.GetQualityTest(c)
}

// The existing account-test pipeline writes SSE to this in-process event sink.
// No client socket, request credentials, or gin request context is retained by the task.
type qualityJobWriter struct {
	header http.Header
	status int
	emit   func(testEvent)
}

func (w *qualityJobWriter) Header() http.Header    { return w.header }
func (w *qualityJobWriter) WriteHeader(status int) { w.status = status }
func (w *qualityJobWriter) Flush()                 {}
func (w *qualityJobWriter) Write(body []byte) (int, error) {
	if w.status >= 400 {
		var payload struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			payload.Error = fmt.Sprintf("test request failed (HTTP %d)", w.status)
		}
		w.emit(testEvent{Type: "error", Error: payload.Error})
	} else {
		var event testEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(string(body), "data: "))), &event); err != nil {
			return 0, err
		}
		w.emit(event)
	}
	return len(body), nil
}

func (h *Handler) runQualityTestJob(parent context.Context, job database.QualityTestJob, req qualityTestRequest) {
	ctx, cancel := context.WithDeadline(parent, job.DeadlineAt)
	defer cancel()
	var mu sync.Mutex
	complete, dirty := false, false
	// interrupted tracks whether the latest error event was a transport/stream break.
	interrupted := false
	secrets := codexTestSecrets(h.store.FindByID(job.AccountID))
	emit := func(event testEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch event.Type {
		case "content":
			if len(job.Output)+len(event.Text) > 1<<20 {
				job.Error = "Output exceeded the 1 MiB limit; generation stopped"
				cancel()
				return
			}
			job.Output += event.Text
			if job.FirstContentMS == nil && event.Text != "" {
				value := time.Since(job.CreatedAt).Milliseconds()
				job.FirstContentMS = &value
			}
		case "error":
			job.Error = sanitizeCodexTestText(event.Error, secrets)
			interrupted = event.Interrupted
		case "test_complete":
			complete = event.Success
		}
		if d := event.CodexDiagnostics; d != nil {
			job.ResponseModel = d.ResponseModel
			// Diagnostics time from the attempt start; after a retry keep the job-relative value.
			if d.FirstContentMS != nil && job.Retries == 0 {
				job.FirstContentMS = d.FirstContentMS
			}
			if d.Usage != nil {
				job.InputTokens = d.Usage.InputTokens
				job.OutputTokens = d.Usage.OutputTokens
				job.ReasoningTokens = d.Usage.ReasoningTokens
			}
		}
		dirty = true
	}
	done, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				status, err := h.db.QualityTestStatus(ctx, job.ID)
				if err != nil {
					mu.Lock()
					job.Error = "Failed to read the test job status"
					mu.Unlock()
					cancel()
					return
				}
				if status != "running" {
					cancel()
					return
				}
				mu.Lock()
				snapshot, changed := job, dirty
				dirty = false
				mu.Unlock()
				if changed {
					if err := h.db.SaveQualityTestProgress(ctx, snapshot); err != nil {
						mu.Lock()
						job.Error = "Failed to save test progress; the job has stopped"
						mu.Unlock()
						cancel()
						return
					}
				}
			}
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			mu.Lock()
			job.Error = "The test job ended abnormally"
			mu.Unlock()
		}
		close(done)
		<-watched
		job.DurationMS = time.Since(job.CreatedAt).Milliseconds()
		switch {
		case parent.Err() != nil:
			job.Status = "interrupted"
			job.Error = "The service shut down; the test was interrupted"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			job.Status = "error"
			job.Error = fmt.Sprintf("The test exceeded %d minutes and was stopped", int(job.DeadlineAt.Sub(job.CreatedAt).Round(time.Minute).Minutes()))
		case job.Error != "":
			job.Status = "error"
			// A break caused by our own cancellation is not an upstream interruption.
			job.Interrupted = interrupted && ctx.Err() == nil
		case ctx.Err() != nil:
			job.Status = "stopped"
		case complete:
			job.Status = "completed"
		default:
			job.Status = "error"
			job.Error = "The stream was interrupted; no successful completion event was received"
		}
		finalCtx, finalCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer finalCancel()
		if err := h.db.FinishQualityTest(finalCtx, job); err != nil {
			log.Printf("[quality-test] job=%d finalization failed: %v", job.ID, err)
		}
	}()
	router := gin.New()
	router.POST("/accounts/:id/test", func(c *gin.Context) { h.testConnection(c, &req) })
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("/accounts/%d/test", job.AccountID), nil)
		if err != nil {
			mu.Lock()
			job.Error = "Failed to create the test request"
			mu.Unlock()
			return
		}
		router.ServeHTTP(&qualityJobWriter{header: make(http.Header), emit: emit}, request)
		if !h.retryQualityTestAttempt(ctx, &mu, &job, &interrupted, &dirty, complete) {
			return
		}
	}
}

// qualityTestMaxRetries bounds automatic re-attempts. Only a break before any
// output is retried: once output exists a retry would regenerate (and bill) it all.
const qualityTestMaxRetries = 1

var qualityTestRetryDelay = 2 * time.Second

func (h *Handler) retryQualityTestAttempt(ctx context.Context, mu *sync.Mutex, job *database.QualityTestJob, interrupted, dirty *bool, complete bool) bool {
	mu.Lock()
	retry := !complete && *interrupted && job.Output == "" && job.Error != "" && job.Retries < qualityTestMaxRetries && ctx.Err() == nil
	if retry {
		log.Printf("[quality-test] job=%d attempt %d interrupted before output, retrying: %s", job.ID, job.Retries+1, job.Error)
		job.Retries++
		job.Error, *interrupted, *dirty = "", false, true
	}
	mu.Unlock()
	if !retry {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(qualityTestRetryDelay):
		return true
	}
}
