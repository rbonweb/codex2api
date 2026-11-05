package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const promptFilterAuditContextMaxRunes = 2000

type promptFilterLogsResponse struct {
	Logs     []*database.PromptFilterLog `json:"logs"`
	Total    int                         `json:"total"`
	Page     int                         `json:"page"`
	PageSize int                         `json:"page_size"`
}

type promptPolicyIncidentsResponse struct {
	Incidents []*database.PromptPolicyIncident `json:"incidents"`
	Total     int                              `json:"total"`
	Page      int                              `json:"page"`
	PageSize  int                              `json:"page_size"`
}

type promptPolicyIncidentDetailResponse struct {
	Incident  *database.PromptPolicyIncident        `json:"incident"`
	Matches   json.RawMessage                       `json:"matches"`
	Candidate *database.PromptRuleCandidate         `json:"candidate,omitempty"`
	Evidence  *database.PromptRuleCandidateEvidence `json:"evidence,omitempty"`
	// RiskSubjects are the profile subjects linked to this CY (person / conversation / key / IP / upstream account),
	// so attribution can show them directly and jump to the matching person profile.
	RiskSubjects []database.PromptRiskIncidentSubject `json:"risk_subjects"`
}

type promptPolicyAuditQueueHealth struct {
	Enqueued      uint64 `json:"enqueued"`
	Completed     uint64 `json:"completed"`
	DroppedHigh   uint64 `json:"dropped_high"`
	DroppedLow    uint64 `json:"dropped_low"`
	Failed        uint64 `json:"failed"`
	PendingHigh   int    `json:"pending_high"`
	PendingLow    int    `json:"pending_low"`
	RetainedBytes int64  `json:"retained_bytes"`
}

type promptPolicyAuditHealthResponse struct {
	OK                      bool                             `json:"ok"`
	Status                  string                           `json:"status"`
	StorageReady            bool                             `json:"storage_ready"`
	PromptFilterEnabled     bool                             `json:"prompt_filter_enabled"`
	ReviewEnabled           bool                             `json:"review_enabled"`
	ReviewFailClosed        bool                             `json:"review_fail_closed"`
	ReviewPool              promptfilter.ReviewKeyPoolHealth `json:"review_pool"`
	ConversationLockEnabled bool                             `json:"conversation_lock_enabled"`
	IncidentCount           int                              `json:"incident_count"`
	LatestIncidentID        string                           `json:"latest_incident_id,omitempty"`
	LatestIncidentAt        *time.Time                       `json:"latest_incident_at,omitempty"`
	Queue                   promptPolicyAuditQueueHealth     `json:"queue"`
}

type promptFilterTestRequest struct {
	Text     string `json:"text"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
}

type promptFilterTestResponse struct {
	Verdict  promptfilter.Verdict     `json:"verdict"`
	Decision promptfilter.Decision    `json:"decision"`
	Protocol promptfilter.Protocol    `json:"protocol"`
	Provider promptfilter.ModelFamily `json:"provider"`
	Endpoint string                   `json:"endpoint"`
	Model    string                   `json:"model"`
}

type promptReviewTestRequest struct {
	Text                 string             `json:"text"`
	APIKey               string             `json:"api_key"`
	BaseURL              string             `json:"base_url"`
	Model                string             `json:"model"`
	RequestMode          string             `json:"request_mode"`
	SystemPrompt         string             `json:"system_prompt"`
	UserPromptTemplate   string             `json:"user_prompt_template"`
	PayloadTemplate      string             `json:"payload_template"`
	ConfidenceThreshold  float64            `json:"confidence_threshold"`
	ModerationThresholds map[string]float64 `json:"moderation_thresholds"`
	TimeoutSeconds       int                `json:"timeout_seconds"`
	MaxConcurrent        int                `json:"max_concurrent"`
	MaxTextLength        int                `json:"max_text_length"`
	TestAllKeys          bool               `json:"test_all_keys"`
}

type promptReviewKeyTestResult struct {
	KeyIndex             int                `json:"key_index"`
	KeyID                string             `json:"key_id,omitempty"`
	KeyMasked            string             `json:"key_masked,omitempty"`
	OK                   bool               `json:"ok"`
	Endpoint             string             `json:"endpoint,omitempty"`
	Model                string             `json:"model,omitempty"`
	Flagged              bool               `json:"flagged"`
	Confidence           float64            `json:"confidence"`
	Reason               string             `json:"reason,omitempty"`
	HighestCategory      string             `json:"highest_category,omitempty"`
	DecisionCategory     string             `json:"decision_category,omitempty"`
	DecisionScore        float64            `json:"decision_score"`
	DecisionThreshold    float64            `json:"decision_threshold"`
	CategoryScores       map[string]float64 `json:"category_scores,omitempty"`
	ModerationThresholds map[string]float64 `json:"moderation_thresholds,omitempty"`
	LatencyMS            int64              `json:"latency_ms"`
	Error                string             `json:"error,omitempty"`
}

type promptReviewAPIKeyDescriptor struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	Masked string `json:"masked"`
}

type promptReviewAPIKeysResponse struct {
	Items []promptReviewAPIKeyDescriptor `json:"items"`
	Count int                            `json:"count"`
}

type promptReviewProfileResponse struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	BaseURL       string    `json:"base_url"`
	Model         string    `json:"model"`
	RequestMode   string    `json:"request_mode"`
	TimeoutSecond int       `json:"timeout_seconds"`
	KeyCount      int       `json:"key_count"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type promptReviewProfilesResponse struct {
	Profiles []promptReviewProfileResponse `json:"profiles"`
}

type promptReviewProfileRequest struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	RequestMode   string `json:"request_mode"`
	APIKey        string `json:"api_key"`
	AdapterJSON   string `json:"adapter_json"`
	TimeoutSecond int    `json:"timeout_seconds"`
}

func toPromptReviewProfileResponse(profile database.PromptReviewProfile) promptReviewProfileResponse {
	return promptReviewProfileResponse{
		ID: profile.ID, Name: profile.Name, BaseURL: profile.BaseURL, Model: profile.Model,
		RequestMode: profile.RequestMode, TimeoutSecond: profile.TimeoutSecond,
		KeyCount: len((promptfilter.ReviewConfig{APIKey: profile.APIKeys}).APIKeyList()),
		Active:   profile.Active, CreatedAt: profile.CreatedAt, UpdatedAt: profile.UpdatedAt,
	}
}

func promptReviewProfileAdapterJSON(raw string, fallback promptfilter.ReviewAdapterConfig) (string, promptfilter.ReviewAdapterConfig, error) {
	adapter := fallback
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &adapter); err != nil {
			return "", adapter, errors.New("invalid adapter_json")
		}
	}
	adapter = promptfilter.NormalizeReviewAdapterConfig(adapter)
	encoded, err := json.Marshal(adapter)
	if err != nil {
		return "", adapter, err
	}
	return string(encoded), adapter, nil
}

func (h *Handler) ListPromptReviewProfiles(c *gin.Context) {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	profiles, err := h.db.ListPromptReviewProfiles(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	items := make([]promptReviewProfileResponse, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, toPromptReviewProfileResponse(profile))
	}
	c.JSON(http.StatusOK, promptReviewProfilesResponse{Profiles: items})
}

func (h *Handler) SavePromptReviewProfile(c *gin.Context) {
	if h == nil || h.db == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	var req promptReviewProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 120 {
		writeError(c, http.StatusBadRequest, "Profile name must not be empty and must not exceed 120 characters")
		return
	}
	current := h.store.GetPromptFilterConfig().Review
	profileID := strings.TrimSpace(req.ID)
	var existing *database.PromptReviewProfile
	if profileID != "" {
		var err error
		existing, err = h.db.GetPromptReviewProfile(c.Request.Context(), profileID)
		if err != nil {
			writeInternalError(c, err)
			return
		}
	}
	if profileID == "" {
		profileID = uuid.NewString()
	}
	apiKeys := strings.TrimSpace(req.APIKey)
	if apiKeys == "" {
		if existing != nil {
			apiKeys = existing.APIKeys
		} else {
			// Leaving it empty keeps the currently active key, so users do not have to
			// paste the existing DeepSeek key again just to save a new profile.
			apiKeys = current.APIKey
		}
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" && existing != nil {
		baseURL = existing.BaseURL
	}
	model := strings.TrimSpace(req.Model)
	if model == "" && existing != nil {
		model = existing.Model
	}
	requestMode := strings.TrimSpace(req.RequestMode)
	if requestMode == "" && existing != nil {
		requestMode = existing.RequestMode
	}
	if req.TimeoutSecond <= 0 && existing != nil {
		req.TimeoutSecond = existing.TimeoutSecond
	}
	if req.TimeoutSecond <= 0 {
		req.TimeoutSecond = current.TimeoutSeconds
	}
	adapterRaw := req.AdapterJSON
	if strings.TrimSpace(adapterRaw) == "" && existing != nil {
		adapterRaw = existing.AdapterJSON
	}
	if strings.TrimSpace(adapterRaw) == "" {
		adapterRaw, _, _ = promptReviewProfileAdapterJSON("", current.Adapter)
	}
	canonicalAdapter, adapter, err := promptReviewProfileAdapterJSON(adapterRaw, current.Adapter)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	reviewCfg := current
	reviewCfg.APIKey, reviewCfg.BaseURL, reviewCfg.Model = apiKeys, baseURL, model
	reviewCfg.TimeoutSeconds, reviewCfg.Adapter = req.TimeoutSecond, adapter
	reviewCfg = promptfilter.NormalizeReviewConfig(reviewCfg)
	if err := promptfilter.ValidateReviewConfig(reviewCfg); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid Prompt review configuration: "+err.Error())
		return
	}
	profile := database.PromptReviewProfile{ID: profileID, Name: req.Name, BaseURL: reviewCfg.BaseURL, Model: reviewCfg.Model,
		RequestMode: reviewCfg.Adapter.RequestMode, AdapterJSON: canonicalAdapter, APIKeys: reviewCfg.APIKey,
		TimeoutSecond: reviewCfg.TimeoutSeconds, Active: existing != nil && existing.Active}
	if err := h.db.UpsertPromptReviewProfile(c.Request.Context(), profile); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, toPromptReviewProfileResponse(profile))
}

func (h *Handler) ActivatePromptReviewProfile(c *gin.Context) {
	if h == nil || h.db == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	profile, err := h.db.GetPromptReviewProfile(c.Request.Context(), strings.TrimSpace(c.Param("profile_id")))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if profile == nil {
		writeError(c, http.StatusNotFound, "Review configuration profile not found")
		return
	}
	var adapter promptfilter.ReviewAdapterConfig
	if err := json.Unmarshal([]byte(profile.AdapterJSON), &adapter); err != nil {
		writeError(c, http.StatusConflict, "Adapter configuration in the review configuration profile is invalid")
		return
	}
	adapter = promptfilter.NormalizeReviewAdapterConfig(adapter)
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	settings, err := h.db.GetSystemSettings(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if settings == nil {
		writeError(c, http.StatusNotFound, "System settings not found")
		return
	}
	patchRaw, _ := json.Marshal(map[string]any{"review_adapter": adapter})
	document, err := promptfilter.MergeAdvancedConfigDocument(settings.PromptFilterAdvancedConfig, string(patchRaw))
	if err != nil {
		writeError(c, http.StatusConflict, "Review configuration profile cannot be applied: "+err.Error())
		return
	}
	settings.PromptFilterReviewAPIKey = profile.APIKeys
	settings.PromptFilterReviewBaseURL = profile.BaseURL
	settings.PromptFilterReviewModel = profile.Model
	settings.PromptFilterReviewTimeoutSeconds = profile.TimeoutSecond
	settings.PromptFilterAdvancedConfig = document.Raw
	settings.PreservePromptFilterReviewAPIKey = false
	if err := h.db.UpdateSystemSettings(c.Request.Context(), settings); err != nil {
		writeInternalError(c, err)
		return
	}
	runtimeCfg := h.store.GetPromptFilterConfig()
	runtimeCfg.Review.APIKey, runtimeCfg.Review.BaseURL, runtimeCfg.Review.Model = profile.APIKeys, profile.BaseURL, profile.Model
	runtimeCfg.Review.TimeoutSeconds, runtimeCfg.Review.Adapter = profile.TimeoutSecond, adapter
	if err := h.store.SetPromptFilterConfigWithAdvancedRaw(runtimeCfg, document.Raw); err != nil {
		writeInternalError(c, err)
		return
	}
	if err := h.db.SetPromptReviewProfileActive(c.Request.Context(), profile.ID); err != nil {
		writeInternalError(c, err)
		return
	}
	profile.Active = true
	c.JSON(http.StatusOK, toPromptReviewProfileResponse(*profile))
}

func (h *Handler) DeletePromptReviewProfile(c *gin.Context) {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	if err := h.db.DeletePromptReviewProfile(c.Request.Context(), strings.TrimSpace(c.Param("profile_id"))); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func promptReviewAPIKeyID(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// maskPromptReviewAPIKey slices by rune (byte slicing would cut multi-byte characters), and only shows a plaintext fragment
// when the prefix and suffix cannot overlap and most characters are still masked (≥12 characters).
func maskPromptReviewAPIKey(key string) string {
	runes := []rune(strings.TrimSpace(key))
	if len(runes) < 12 {
		return "••••"
	}
	return string(runes[:3]) + "••••" + string(runes[len(runes)-4:])
}

func promptReviewAPIKeyDescriptors(keys []string) []promptReviewAPIKeyDescriptor {
	items := make([]promptReviewAPIKeyDescriptor, 0, len(keys))
	for index, key := range keys {
		items = append(items, promptReviewAPIKeyDescriptor{
			ID: promptReviewAPIKeyID(key), Index: index + 1, Masked: maskPromptReviewAPIKey(key),
		})
	}
	return items
}

type promptReviewTestResponse struct {
	OK                   bool                        `json:"ok"`
	Endpoint             string                      `json:"endpoint"`
	Model                string                      `json:"model"`
	Flagged              bool                        `json:"flagged"`
	Confidence           float64                     `json:"confidence"`
	ConfidenceThreshold  float64                     `json:"confidence_threshold"`
	Reason               string                      `json:"reason,omitempty"`
	HighestCategory      string                      `json:"highest_category,omitempty"`
	DecisionCategory     string                      `json:"decision_category,omitempty"`
	DecisionScore        float64                     `json:"decision_score"`
	DecisionThreshold    float64                     `json:"decision_threshold"`
	CategoryScores       map[string]float64          `json:"category_scores,omitempty"`
	ModerationThresholds map[string]float64          `json:"moderation_thresholds,omitempty"`
	LatencyMS            int64                       `json:"latency_ms"`
	KeyCount             int                         `json:"key_count,omitempty"`
	Results              []promptReviewKeyTestResult `json:"results,omitempty"`
}

type promptFilterRulePatternTestRequest struct {
	Pattern string `json:"pattern"`
	Text    string `json:"text"`
}

type promptFilterRulePatternTestResponse struct {
	Matched bool   `json:"matched"`
	Error   string `json:"error,omitempty"`
}

type promptFilterRuleItem struct {
	Name     string `json:"name"`
	Pattern  string `json:"pattern"`
	Weight   int    `json:"weight"`
	Category string `json:"category,omitempty"`
	Strict   bool   `json:"strict,omitempty"`
	Enabled  bool   `json:"enabled"`
	Builtin  bool   `json:"builtin"`
}

type promptFilterRulesResponse struct {
	BuiltinPatterns  []promptFilterRuleItem       `json:"builtin_patterns"`
	CustomPatterns   []promptfilter.PatternConfig `json:"custom_patterns"`
	DisabledPatterns []string                     `json:"disabled_patterns"`
}

func (h *Handler) inspectImageStudioPromptFilter(c *gin.Context, text string, model string, keyID int64, keyName string, keyMasked string) bool {
	return h.inspectImagePromptFilter(c, text, model, keyID, keyName, keyMasked, "/api/admin/images/jobs", nil, false)
}

func (h *Handler) inspectImagePromptFilter(c *gin.Context, text string, model string, keyID int64, keyName string, keyMasked string, endpoint string, writeBlock func(*gin.Context), redactPreview bool) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	verdict := promptfilter.InspectText(text, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg)
		verdict = promptfilter.ApplyReviewMode(verdict, cfg.Mode)
	}
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
		return false
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	textPreview := promptfilter.RedactedPreview(verdict.TextPreview, 500)
	if redactPreview {
		textPreview = "[redacted]"
	}
	h.recordPromptFilterLog(c, &database.PromptFilterLogInput{
		Source:          "local_filter",
		Endpoint:        endpoint,
		Model:           model,
		Action:          verdict.Action,
		Mode:            verdict.Mode,
		Score:           verdict.Score,
		Threshold:       verdict.Threshold,
		MatchedPatterns: promptfilter.MatchesJSON(verdict.Matched),
		TextPreview:     textPreview,
		MatchContext:    promptfilter.RedactedPreview(verdict.MatchContext, promptFilterAuditContextMaxRunes),
		APIKeyID:        keyID,
		APIKeyName:      keyName,
		APIKeyMasked:    keyMasked,
		ClientIP:        c.ClientIP(),
		ReviewModel:     verdict.ReviewModel,
		ReviewFlagged:   verdict.ReviewFlagged,
		ReviewError:     verdict.ReviewError,
	})
	if writeBlock != nil {
		writeBlock(c)
	} else {
		writeError(c, http.StatusBadRequest, "Blocked by Prompt inspection rule")
	}
	return true
}

func (h *Handler) recordPromptFilterLog(c *gin.Context, input *database.PromptFilterLogInput) {
	if h == nil || h.db == nil || input == nil {
		return
	}
	priority := database.PromptFilterLogPriorityLow
	if input.Action == promptfilter.ActionWarn || input.Action == promptfilter.ActionBlock {
		priority = database.PromptFilterLogPriorityHigh
	}
	_ = h.db.EnqueuePromptFilterLog(input, priority)
}

func (h *Handler) ListPromptFilterLogs(c *gin.Context) {
	page := positiveQueryInt(c, "page", 1)
	pageSize := positiveQueryInt(c, "page_size", positiveQueryInt(c, "limit", 100))
	apiKeyID := int64(0)
	if raw := strings.TrimSpace(c.Query("api_key_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			apiKeyID = parsed
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	logs, total, err := h.db.ListPromptFilterLogsPage(ctx, database.PromptFilterLogQuery{
		Page:                page,
		PageSize:            pageSize,
		Source:              c.Query("source"),
		Action:              c.Query("action"),
		Endpoint:            c.Query("endpoint"),
		Model:               c.Query("model"),
		APIKeyID:            apiKeyID,
		Query:               c.Query("q"),
		ReviewState:         c.Query("reviewed"),
		ReviewResult:        c.Query("review_result"),
		ExcludeIntelligence: true,
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if logs == nil {
		logs = []*database.PromptFilterLog{}
	}
	c.JSON(http.StatusOK, promptFilterLogsResponse{Logs: logs, Total: total, Page: page, PageSize: pageSize})
}

func (h *Handler) ClearPromptFilterLogs(c *gin.Context) {
	// Manual clearing now deletes in background batches: hundreds of thousands of log rows cannot be cleared within one DELETE and a 10s request timeout,
	// so it now returns immediately and cleans up in the background at 5000 rows per batch; logs linked to a CY are always skipped.
	filter := database.PromptLogPurgeFilter{}
	message := "Prompt inspection log cleanup has started in the background; CY-linked logs, risk profiles and upstream CY events are kept"
	source := strings.ToLower(strings.TrimSpace(c.Query("source")))
	if source != "" {
		if source != "local_filter" {
			writeError(c, http.StatusBadRequest, "source only supports local_filter")
			return
		}
		filter.Source = source
		message = "Local filter and async audit log cleanup has started in the background; CY-linked logs and risk profiles are kept"
	} else {
		switch strings.ToLower(strings.TrimSpace(c.Query("reviewed"))) {
		case "":
		case "true", "reviewed":
			reviewed := true
			filter.Reviewed = &reviewed
			message = "External model review history cleanup has started in the background; CY-linked logs and risk profiles are kept"
		case "false", "not_reviewed":
			reviewed := false
			filter.Reviewed = &reviewed
			message = "Local filter and async audit log cleanup has started in the background; CY-linked logs and risk profiles are kept"
		default:
			writeError(c, http.StatusBadRequest, "reviewed must be true or false")
			return
		}
	}
	cutoff := time.Now().Add(time.Second)
	if !h.startPromptLogPurge(func(ctx context.Context) {
		started := time.Now()
		result, err := h.db.PurgePromptFilterLogs(ctx, cutoff, filter, promptLogPurgeBatchSize, promptLogPurgeBatchPause)
		if err != nil {
			log.Printf("[prompt-retention] manual log clear failed: %v (deleted %d rows)", err, result.Logs)
			return
		}
		log.Printf("[prompt-retention] manual log clear completed: deleted %d rows, batches=%d, %s", result.Logs, result.Batches, time.Since(started).Round(time.Millisecond))
	}) {
		writeError(c, http.StatusConflict, "A cleanup task is already running; try again later")
		return
	}
	writeMessage(c, http.StatusOK, message)
}

func (h *Handler) ClearPromptPolicyIncidents(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.ClearPromptPolicyIncidents(ctx); err != nil {
		writeInternalError(c, err)
		return
	}
	writeMessage(c, http.StatusOK, "Upstream CY events cleared; risk profiles are kept")
}

func (h *Handler) DeletePromptPolicyIncident(c *gin.Context) {
	incidentID := strings.TrimSpace(c.Param("incident_id"))
	if incidentID == "" {
		writeError(c, http.StatusBadRequest, "incident_id must not be empty")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.DeletePromptPolicyIncident(ctx, incidentID); errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "CY event not found or already deleted")
		return
	} else if err != nil {
		writeInternalError(c, err)
		return
	}
	writeMessage(c, http.StatusOK, "CY event deleted; risk profiles and learning evidence are kept")
}

func (h *Handler) GetPromptPolicyAuditHealth(c *gin.Context) {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "CY audit storage unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	incidents, total, err := h.db.ListPromptPolicyIncidentsPage(ctx, database.PromptPolicyIncidentQuery{Page: 1, PageSize: 1})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	stats := h.db.PromptFilterAuditStats()
	cfg := promptfilter.DefaultConfig()
	if h.store != nil {
		cfg = h.store.GetPromptFilterConfig()
	}
	response := promptPolicyAuditHealthResponse{
		OK: true, Status: "healthy", StorageReady: true,
		PromptFilterEnabled: cfg.Enabled, ReviewEnabled: cfg.Review.Enabled,
		ReviewFailClosed: cfg.Review.FailClosed, ReviewPool: promptfilter.ReviewKeyPoolStatus(cfg.Review),
		ConversationLockEnabled: cfg.Advanced.Enforcement.ConversationLockEnabled,
		IncidentCount:           total,
		Queue: promptPolicyAuditQueueHealth{
			Enqueued: stats.Enqueued, Completed: stats.Completed, DroppedHigh: stats.DroppedHigh,
			DroppedLow: stats.DroppedLow, Failed: stats.Failed, PendingHigh: stats.PendingHigh,
			PendingLow: stats.PendingLow, RetainedBytes: stats.RetainedBytes,
		},
	}
	// A feature that was switched off on purpose is not a fault (the panel already shows its own switch state); only real errors degrade it:
	// the audit queue drops high-priority items or fails writes, or review is enabled but the key pool is empty or fully cooling down.
	reviewPoolFault := response.ReviewEnabled && (response.ReviewPool.Configured == 0 || response.ReviewPool.Available == 0)
	if reviewPoolFault || stats.DroppedHigh > 0 || stats.Failed > 0 {
		response.OK = false
		response.Status = "degraded"
	}
	if len(incidents) > 0 && incidents[0] != nil {
		response.LatestIncidentID = incidents[0].IncidentID
		latest := incidents[0].CreatedAt
		response.LatestIncidentAt = &latest
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) ListPromptPolicyIncidents(c *gin.Context) {
	page := positiveQueryInt(c, "page", 1)
	pageSize := positiveQueryInt(c, "page_size", positiveQueryInt(c, "limit", 20))
	apiKeyID := int64(0)
	if raw := strings.TrimSpace(c.Query("api_key_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			apiKeyID = parsed
		}
	}
	accountID := int64(0)
	if raw := strings.TrimSpace(c.Query("account_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			accountID = parsed
		}
	}
	var localMiss *bool
	if raw := strings.TrimSpace(c.Query("local_miss")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			localMiss = &parsed
		}
	}
	evaluationState := strings.TrimSpace(c.Query("evaluation_state"))
	if evaluationState == "" {
		evaluationState = strings.TrimSpace(c.Query("status"))
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	incidents, total, err := h.db.ListPromptPolicyIncidentsPage(ctx, database.PromptPolicyIncidentQuery{
		Page: page, PageSize: pageSize, Endpoint: c.Query("endpoint"), Model: c.Query("model"), APIKeyID: apiKeyID, AccountID: accountID,
		EvaluationState: evaluationState, Outcome: c.Query("outcome"), LocalComparison: c.Query("local_comparison"), LocalMiss: localMiss, Query: c.Query("q"),
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if incidents == nil {
		incidents = []*database.PromptPolicyIncident{}
	}
	for _, incident := range incidents {
		h.enrichPromptPolicyIncidentRouting(incident)
	}
	c.JSON(http.StatusOK, promptPolicyIncidentsResponse{Incidents: incidents, Total: total, Page: page, PageSize: pageSize})
}

func (h *Handler) GetPromptPolicyIncident(c *gin.Context) {
	incidentID := strings.TrimSpace(c.Param("incident_id"))
	if incidentID == "" {
		writeError(c, http.StatusBadRequest, "Missing incident_id")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	incident, err := h.db.GetPromptPolicyIncident(ctx, incidentID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "CY event not found")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.enrichPromptPolicyIncidentRouting(incident)
	matches := json.RawMessage(incident.LocalMatchedPatterns)
	if !json.Valid(matches) {
		matches = json.RawMessage("[]")
	}
	response := promptPolicyIncidentDetailResponse{Incident: incident, Matches: matches, RiskSubjects: []database.PromptRiskIncidentSubject{}}
	if subjects, subjectsErr := h.db.ListPromptRiskSubjectsForIncident(ctx, incidentID); subjectsErr == nil {
		response.RiskSubjects = subjects
	}
	if incident.CandidateID > 0 {
		if candidate, candidateErr := h.db.GetPromptRuleCandidate(ctx, incident.CandidateID); candidateErr == nil {
			response.Candidate = candidate
		}
		if incident.CandidateEvidenceID > 0 {
			if item, evidenceErr := h.db.GetPromptRuleCandidateEvidence(ctx, incident.CandidateEvidenceID); evidenceErr == nil && item.CandidateID == incident.CandidateID {
				response.Evidence = item
			}
		}
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) enrichPromptPolicyIncidentRouting(incident *database.PromptPolicyIncident) {
	if incident == nil {
		return
	}
	hasEventSnapshot := strings.TrimSpace(incident.AccountName) != "" ||
		strings.TrimSpace(incident.AccountPlatform) != "" ||
		len(incident.AccountGroupIDs) > 0 || len(incident.AccountGroupNames) > 0 ||
		len(incident.APIKeyAllowedGroupIDs) > 0 || len(incident.APIKeyAllowedGroupNames) > 0
	if hasEventSnapshot {
		incident.RoutingSnapshotState = "event_snapshot"
		return
	}
	if h == nil || h.store == nil {
		incident.RoutingSnapshotState = "unavailable"
		return
	}

	inferred := false
	if incident.AccountID > 0 {
		if account := h.store.FindByID(incident.AccountID); account != nil {
			account.Mu().RLock()
			incident.AccountName = strings.TrimSpace(account.Email)
			incident.AccountPlatform = strings.TrimSpace(account.UpstreamType)
			account.Mu().RUnlock()
			if incident.AccountPlatform == "" {
				incident.AccountPlatform = database.UpstreamChannelCodex
			}
			incident.AccountGroupIDs = account.GroupIDSnapshot()
			incident.AccountGroupNames = h.store.ResolveGroupNames(incident.AccountGroupIDs)
			inferred = true
		}
	}
	if incident.APIKeyID > 0 {
		incident.APIKeyAllowedGroupIDs = h.store.GetAPIKeyAllowedGroups(incident.APIKeyID)
		incident.APIKeyAllowedGroupNames = h.store.ResolveGroupNames(incident.APIKeyAllowedGroupIDs)
		if len(incident.APIKeyAllowedGroupIDs) > 0 {
			inferred = true
		}
	}
	if inferred {
		incident.RoutingSnapshotState = "current_inferred"
		return
	}
	incident.RoutingSnapshotState = "unavailable"
}

// MatchPromptFilterLog finds the prompt filter log entry closest to a request by time / endpoint / APIKey,
// used to view the full request that triggered a cyber_policy error when clicking it in "usage statistics".
// GET /api/prompt-filter/logs/match?at=<RFC3339>&endpoint=&api_key_id=&source=
func (h *Handler) MatchPromptFilterLog(c *gin.Context) {
	atRaw := strings.TrimSpace(c.Query("at"))
	if atRaw == "" {
		writeError(c, http.StatusBadRequest, "Missing at parameter")
		return
	}
	at, err := time.Parse(time.RFC3339, atRaw)
	if err != nil {
		writeError(c, http.StatusBadRequest, "Invalid at parameter format (RFC3339 required)")
		return
	}
	source := strings.TrimSpace(c.Query("source"))
	if source == "" {
		source = "upstream_cyber_policy"
	}
	apiKeyID := int64(0)
	if raw := strings.TrimSpace(c.Query("api_key_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			apiKeyID = parsed
		}
	}
	windowSeconds := positiveQueryInt(c, "window", 15)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	log, err := h.db.FindNearestPromptFilterLog(ctx, at, source, strings.TrimSpace(c.Query("endpoint")), apiKeyID, windowSeconds)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"found": log != nil, "log": log, "legacy_inferred": log != nil})
}

func (h *Handler) TestPromptFilter(c *gin.Context) {
	var req promptFilterTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeError(c, http.StatusBadRequest, "text must not be empty")
		return
	}
	if len([]rune(req.Text)) > 20000 {
		writeError(c, http.StatusBadRequest, "text must not exceed 20000 characters")
		return
	}
	cfg := h.store.GetPromptFilterConfig()
	evaluator := h.imageProxy
	if evaluator == nil {
		evaluator = proxy.NewHandler(nil, nil, nil, nil)
	}
	result := evaluator.EvaluatePromptGuardTextForTest(c, cfg, req.Text, req.Endpoint, req.Model)
	c.JSON(http.StatusOK, promptFilterTestResponse{
		Verdict:  result.Verdict,
		Decision: result.Decision,
		Protocol: result.Protocol,
		Provider: result.Provider,
		Endpoint: result.Endpoint,
		Model:    result.Model,
	})
}

type promptReviewModelsRequest struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// ListPromptReviewModels fetches the OpenAI-compatible model list from the review provider, for the frontend to pick a review model.
// base_url/api_key may carry draft form values that override the saved config, so the list can be fetched before saving.
func (h *Handler) ListPromptReviewModels(c *gin.Context) {
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	var req promptReviewModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	reviewCfg := h.store.GetPromptFilterConfig().Review
	if key := strings.TrimSpace(req.APIKey); key != "" {
		reviewCfg.APIKey = key
	}
	if baseURL := strings.TrimSpace(req.BaseURL); baseURL != "" {
		reviewCfg.BaseURL = baseURL
	}
	if req.TimeoutSeconds > 0 {
		reviewCfg.TimeoutSeconds = req.TimeoutSeconds
	}
	if len(promptfilter.NormalizeReviewConfig(reviewCfg).APIKeyList()) == 0 {
		writeError(c, http.StatusBadRequest, "Configure the review API key first")
		return
	}
	models, endpoint, err := promptfilter.DefaultReviewClient.ListReviewModels(c.Request.Context(), reviewCfg)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Failed to fetch model list: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"endpoint": endpoint, "models": models})
}

func (h *Handler) TestPromptReviewConnection(c *gin.Context) {
	var req promptReviewTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeError(c, http.StatusBadRequest, "text must not be empty")
		return
	}
	if len([]rune(req.Text)) > 20000 {
		writeError(c, http.StatusBadRequest, "text must not exceed 20000 characters")
		return
	}
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	reviewCfg := h.store.GetPromptFilterConfig().Review
	reviewCfg.Enabled = true
	if key := strings.TrimSpace(req.APIKey); key != "" {
		reviewCfg.APIKey = key
	}
	if baseURL := strings.TrimSpace(req.BaseURL); baseURL != "" {
		reviewCfg.BaseURL = baseURL
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		reviewCfg.Model = model
	}
	if req.TimeoutSeconds > 0 {
		reviewCfg.TimeoutSeconds = req.TimeoutSeconds
	}
	reviewCfg.Adapter = promptfilter.ReviewAdapterConfig{
		RequestMode:          req.RequestMode,
		SystemPrompt:         req.SystemPrompt,
		UserPromptTemplate:   req.UserPromptTemplate,
		PayloadTemplate:      req.PayloadTemplate,
		ConfidenceThreshold:  req.ConfidenceThreshold,
		ModerationThresholds: req.ModerationThresholds,
		MaxConcurrent:        req.MaxConcurrent,
		MaxTextLength:        req.MaxTextLength,
	}
	reviewCfg = promptfilter.NormalizeReviewConfig(reviewCfg)
	if err := promptfilter.ValidateReviewConfig(reviewCfg); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid Prompt review configuration: "+err.Error())
		return
	}
	keys := reviewCfg.APIKeyList()
	if req.TestAllKeys && len(keys) > 1 {
		type indexedResult struct {
			index   int
			outcome promptfilter.ReviewOutcome
			latency int64
			err     error
		}
		resultCh := make(chan indexedResult, len(keys))
		started := time.Now()
		limit := reviewCfg.Adapter.MaxConcurrent
		if limit <= 0 || limit > len(keys) {
			limit = len(keys)
		}
		slots := make(chan struct{}, limit)
		for index, key := range keys {
			go func(index int, key string) {
				slots <- struct{}{}
				defer func() { <-slots }()
				keyCfg := reviewCfg
				keyCfg.APIKey = key
				keyStarted := time.Now()
				outcome, testErr := promptfilter.DefaultReviewClient.ReviewTextDetailed(c.Request.Context(), req.Text, keyCfg)
				resultCh <- indexedResult{index: index, outcome: outcome, latency: time.Since(keyStarted).Milliseconds(), err: testErr}
			}(index, key)
		}
		results := make([]promptReviewKeyTestResult, len(keys))
		descriptors := promptReviewAPIKeyDescriptors(keys)
		allOK := true
		var first promptfilter.ReviewOutcome
		for range keys {
			item := <-resultCh
			if item.index == 0 {
				first = item.outcome
			}
			result := promptReviewKeyTestResult{
				KeyIndex: item.index + 1, KeyID: descriptors[item.index].ID, KeyMasked: descriptors[item.index].Masked,
				OK: item.err == nil, Flagged: item.outcome.Flagged,
				Endpoint: item.outcome.Endpoint, Model: item.outcome.Model, Confidence: item.outcome.Confidence,
				Reason: item.outcome.Reason, HighestCategory: item.outcome.HighestCategory,
				DecisionCategory: item.outcome.DecisionCategory, DecisionScore: item.outcome.DecisionScore,
				DecisionThreshold: item.outcome.DecisionThreshold,
				CategoryScores:    item.outcome.CategoryScores, ModerationThresholds: item.outcome.ModerationThresholds,
				LatencyMS: item.latency,
			}
			if item.err != nil {
				allOK = false
				result.Error = item.err.Error()
			}
			results[item.index] = result
		}
		c.JSON(http.StatusOK, promptReviewTestResponse{
			OK: allOK, Endpoint: first.Endpoint, Model: reviewCfg.Model, Flagged: first.Flagged,
			Confidence: first.Confidence, ConfidenceThreshold: reviewCfg.Adapter.ConfidenceThreshold,
			Reason: first.Reason, HighestCategory: first.HighestCategory,
			DecisionCategory: first.DecisionCategory, DecisionScore: first.DecisionScore,
			DecisionThreshold: first.DecisionThreshold,
			CategoryScores:    first.CategoryScores, ModerationThresholds: first.ModerationThresholds,
			LatencyMS: time.Since(started).Milliseconds(), KeyCount: len(keys), Results: results,
		})
		return
	}
	started := time.Now()
	outcome, err := promptfilter.DefaultReviewClient.ReviewTextDetailed(c.Request.Context(), req.Text, reviewCfg)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Prompt review connection test failed: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, promptReviewTestResponse{
		OK:                   true,
		Endpoint:             outcome.Endpoint,
		Model:                outcome.Model,
		Flagged:              outcome.Flagged,
		Confidence:           outcome.Confidence,
		ConfidenceThreshold:  reviewCfg.Adapter.ConfidenceThreshold,
		Reason:               outcome.Reason,
		HighestCategory:      outcome.HighestCategory,
		DecisionCategory:     outcome.DecisionCategory,
		DecisionScore:        outcome.DecisionScore,
		DecisionThreshold:    outcome.DecisionThreshold,
		CategoryScores:       outcome.CategoryScores,
		ModerationThresholds: outcome.ModerationThresholds,
		LatencyMS:            time.Since(started).Milliseconds(),
		KeyCount:             len(keys),
	})
}

func (h *Handler) ListPromptReviewAPIKeys(c *gin.Context) {
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	items := promptReviewAPIKeyDescriptors(h.store.GetPromptFilterConfig().Review.APIKeyList())
	c.JSON(http.StatusOK, promptReviewAPIKeysResponse{Items: items, Count: len(items)})
}

func (h *Handler) DeletePromptReviewAPIKey(c *gin.Context) {
	if h == nil || h.store == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt review configuration unavailable")
		return
	}
	keyID := strings.ToLower(strings.TrimSpace(c.Param("key_id")))
	if len(keyID) != sha256.Size*2 {
		writeError(c, http.StatusBadRequest, "Invalid review key ID")
		return
	}
	if _, err := hex.DecodeString(keyID); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid review key ID")
		return
	}

	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	settings, err := h.db.GetSystemSettings(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if settings == nil {
		writeError(c, http.StatusNotFound, "Review key not found")
		return
	}
	// storedRaw keeps the raw database value without trimming: the CAS compares the raw value exactly, so if an existing value carries
	// a newline or tab, trimming would never match and it would always return 409.
	storedRaw := settings.PromptFilterReviewAPIKey
	keys := (promptfilter.ReviewConfig{APIKey: storedRaw}).APIKeyList()
	remaining := make([]string, 0, len(keys))
	found := false
	deletedMasked := ""
	for _, key := range keys {
		if promptReviewAPIKeyID(key) == keyID {
			found = true
			deletedMasked = maskPromptReviewAPIKey(key)
			continue
		}
		remaining = append(remaining, key)
	}
	if !found {
		writeError(c, http.StatusNotFound, "Review key not found or already deleted")
		return
	}
	if settings.PromptFilterReviewEnabled && len(remaining) == 0 {
		writeError(c, http.StatusConflict, "The last review key cannot be deleted while model review is enabled; turn off model review or add a replacement key first")
		return
	}
	replacement := strings.Join(remaining, "\n")
	runtimeCfg := h.store.GetPromptFilterConfig()
	runtimeCfg.Review.APIKey = replacement
	runtimeCfg = promptfilter.NormalizeConfig(runtimeCfg)
	if err := promptfilter.ValidateReviewConfig(runtimeCfg.Review); err != nil {
		writeError(c, http.StatusConflict, "Review configuration is invalid after deletion: "+err.Error())
		return
	}
	swapped, err := h.db.CompareAndSwapPromptFilterReviewAPIKeys(c.Request.Context(), storedRaw, replacement)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if !swapped {
		writeError(c, http.StatusConflict, "The review key list was changed by another operation; refresh and try again")
		return
	}
	if err := h.store.SetPromptFilterConfigWithAdvancedRaw(runtimeCfg, h.store.GetPromptFilterAdvancedConfig()); err != nil {
		writeError(c, http.StatusInternalServerError, "Review key saved, but the runtime config update failed")
		return
	}
	log.Printf("review key deleted: %s (from %s), %d remaining", deletedMasked, c.ClientIP(), len(remaining))
	items := promptReviewAPIKeyDescriptors(remaining)
	c.JSON(http.StatusOK, promptReviewAPIKeysResponse{Items: items, Count: len(items)})
}

func (h *Handler) TestPromptFilterRulePattern(c *gin.Context) {
	var req promptFilterRulePatternTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request body")
		return
	}
	trimmedPattern := strings.TrimSpace(req.Pattern)
	if trimmedPattern == "" {
		writeError(c, http.StatusBadRequest, "pattern must not be empty")
		return
	}
	if req.Text == "" {
		writeError(c, http.StatusBadRequest, "text must not be empty")
		return
	}
	if len([]rune(req.Pattern)) > 5000 {
		writeError(c, http.StatusBadRequest, "pattern must not exceed 5000 characters")
		return
	}
	if len([]rune(req.Text)) > 20000 {
		writeError(c, http.StatusBadRequest, "text must not exceed 20000 characters")
		return
	}
	re, err := regexp.Compile(req.Pattern)
	if err != nil {
		c.JSON(http.StatusOK, promptFilterRulePatternTestResponse{Matched: false, Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, promptFilterRulePatternTestResponse{Matched: re.MatchString(req.Text)})
}

func (h *Handler) GetPromptFilterRules(c *gin.Context) {
	cfg := h.store.GetPromptFilterConfig()
	disabled := map[string]bool{}
	for _, name := range cfg.DisabledPatterns {
		disabled[strings.ToLower(strings.TrimSpace(name))] = true
	}
	builtin := promptfilter.BuiltinPatternConfigs()
	items := make([]promptFilterRuleItem, 0, len(builtin))
	for _, pattern := range builtin {
		items = append(items, promptFilterRuleItem{
			Name:     pattern.Name,
			Pattern:  pattern.Pattern,
			Weight:   pattern.Weight,
			Category: pattern.Category,
			Strict:   pattern.Strict,
			Enabled:  !disabled[strings.ToLower(strings.TrimSpace(pattern.Name))],
			Builtin:  true,
		})
	}
	c.JSON(http.StatusOK, promptFilterRulesResponse{
		BuiltinPatterns:  items,
		CustomPatterns:   cfg.CustomPatterns,
		DisabledPatterns: cfg.DisabledPatterns,
	})
}

func positiveQueryInt(c *gin.Context, key string, fallback int) int {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func shouldReviewPromptFilterVerdict(verdict promptfilter.Verdict, cfg promptfilter.Config) bool {
	return cfg.Enabled && promptfilter.ShouldReviewVerdict(verdict, cfg.Review)
}

func reviewPromptFilterVerdict(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	if strings.TrimSpace(text) == "" {
		return verdict
	}
	outcome, err := promptfilter.DefaultReviewClient.ReviewTextDetailed(ctx, text, cfg.Review)
	return promptfilter.ApplyReviewOutcome(verdict, outcome, err, cfg.Review)
}
