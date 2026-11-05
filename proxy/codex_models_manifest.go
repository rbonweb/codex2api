package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// CodexModelsManifestHandler serves the model manifest to Codex clients.
//
// When there are schedulable ChatGPT OAuth accounts, the official manifest is used and narrowed to the intersection of the current account pool's capabilities.
// The client cache uses the ETag the gateway produces, so an upstream 304 cannot bypass account scope or credential changes.
// When there are no ChatGPT accounts: rewrite the same /v1/models catalog as Cockpit into the format Codex expects,
// {"models":[{"slug":...}]}, so the client does not silently freeze on its local cache after a 503 (the picker is incomplete and model info cannot be fetched).
func (h *Handler) CodexModelsManifestHandler(c *gin.Context) {
	row := apiKeyRowFromContext(c)
	account := h.scopedCodexManifestAccount(row)
	if account == nil {
		if !h.serveScopedCodexManifest(c, row) {
			api.SendError(c, api.ErrServiceUnavailable)
		}
		return
	}
	restrictManifest := codexManifestNeedsFiltering(row, account)
	extraModels := h.extraDaybreakManifestModels(c.Request.Context(), row)
	// Capability intersections can change when account scope or credentials
	// change, even if the source descriptor is unchanged. Always fetch a body
	// and compare the client's validator against the final gateway descriptor.

	manifest, err := FetchCodexModelsManifest(
		c.Request.Context(),
		account,
		h.store.ResolveProxyForAccount(account),
		c.Query("client_version"),
		"",
		c.Request.Header,
	)
	if err != nil {
		if h.serveScopedCodexManifest(c, row) {
			return
		}
		api.SendErrorWithStatus(c,
			api.NewAPIError(api.ErrCodeUpstreamError, fmt.Sprintf("codex models manifest: %v", err), api.ErrorTypeUpstream),
			http.StatusBadGateway)
		return
	}

	if restrictManifest {
		if manifest.NotModified {
			api.SendErrorWithStatus(c,
				api.NewAPIError(api.ErrCodeUpstreamError, "codex models manifest returned an unusable 304", api.ErrorTypeUpstream),
				http.StatusBadGateway)
			return
		}
		body, _, filterErr := filterCodexManifest(manifest.Body, manifest.ETag, func(slug string) bool {
			return codexManifestModelAllowed(row, account, slug)
		})
		if filterErr != nil {
			api.SendErrorWithStatus(c,
				api.NewAPIError(api.ErrCodeUpstreamError, fmt.Sprintf("codex models manifest: %v", filterErr), api.ErrorTypeUpstream),
				http.StatusBadGateway)
			return
		}
		h.learnFetchedManifestAsync(manifest)
		h.writeMergedCodexManifest(c, body, "", extraModels)
		return
	}

	if manifest.NotModified {
		api.SendErrorWithStatus(c,
			api.NewAPIError(api.ErrCodeUpstreamError, "codex models manifest returned an unusable 304", api.ErrorTypeUpstream),
			http.StatusBadGateway)
		return
	}
	// Also learn new models in the manifest that the registry does not recognize into the registry (add only; never modify or delete),
	// so new models in the picker pass the request-side model validation right away, without waiting for a manual sync.
	h.learnFetchedManifestAsync(manifest)
	h.writeMergedCodexManifest(c, manifest.Body, manifest.ETag, extraModels)
}

func (h *Handler) serveScopedCodexManifest(c *gin.Context, row *database.APIKeyRow) bool {
	if h == nil || c == nil || row == nil {
		return false
	}
	models := h.scopedModels(c.Request.Context(), row)
	if len(models) == 0 {
		return false
	}
	body, err := buildScopedCodexManifest(models)
	if err != nil {
		log.Printf("build scoped Codex manifest: %v", err)
		return false
	}
	body = h.applyStoredModelCapabilities(c.Request.Context(), row, body)
	h.writeCodexManifest(c, body, "")
	return true
}

func (h *Handler) writeMergedCodexManifest(c *gin.Context, body []byte, upstreamETag string, extras []api.Model) {
	merged := body
	if len(extras) > 0 {
		next, err := mergeCodexManifestModels(body, extras)
		if err != nil {
			log.Printf("merge Daybreak models into Codex manifest: %v", err)
		} else {
			merged = next
		}
	}
	merged = h.applyUpstreamModelCapabilities(c.Request.Context(), apiKeyRowFromContext(c), merged)
	etag := upstreamETag
	if etag == "" || string(merged) != string(body) {
		etag = ""
	}
	h.writeCodexManifest(c, merged, etag)
}

func (h *Handler) extraDaybreakManifestModels(ctx context.Context, row *database.APIKeyRow) []api.Model {
	if h == nil || row == nil {
		return nil
	}
	records := h.scopedModelRecords(ctx, row)
	extras := make([]api.Model, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		_, daybreak := auth.ParseDaybreakAlias(record.id)
		if daybreak == "" {
			continue
		}
		extras = append(extras, api.Model{ID: record.id, Object: "model", OwnedBy: scopedModelOwner(record)})
	}
	sort.Slice(extras, func(i, j int) bool {
		return strings.ToLower(extras[i].ID) < strings.ToLower(extras[j].ID)
	})
	return extras
}

func (h *Handler) writeCodexManifest(c *gin.Context, body []byte, etag string) {
	if etag == "" {
		etag = scopedCodexManifestETag(body)
	}
	c.Header("ETag", etag)
	if etagHeaderMatches(c.GetHeader("If-None-Match"), etag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json", body)
}

type codexReasoningLevel = api.ModelReasoningLevel

type scopedCodexManifestItem struct {
	ServiceTiers               []map[string]string   `json:"service_tiers,omitempty"`
	Slug                       string                `json:"slug"`
	DisplayName                string                `json:"display_name"`
	Hidden                     bool                  `json:"hidden"`
	Availability               string                `json:"availability"`
	SupportedInAPI             bool                  `json:"supported_in_api"`
	PreferWebsockets           bool                  `json:"prefer_websockets"`
	UseResponsesLite           bool                  `json:"use_responses_lite"`
	InputModalities            []string              `json:"input_modalities,omitempty"`
	SupportedReasoningLevels   []codexReasoningLevel `json:"supported_reasoning_levels"`
	DefaultReasoningLevel      string                `json:"default_reasoning_level"`
	Description                string                `json:"description"`
	ShellType                  string                `json:"shell_type"`
	Visibility                 string                `json:"visibility"`
	Priority                   int                   `json:"priority"`
	BaseInstructions           string                `json:"base_instructions"`
	SupportsReasoningSummaries bool                  `json:"supports_reasoning_summaries"`
	SupportVerbosity           bool                  `json:"support_verbosity"`
	SupportsParallelToolCalls  bool                  `json:"supports_parallel_tool_calls"`
	ExperimentalSupportedTools []string              `json:"experimental_supported_tools"`
	TruncationPolicy           map[string]any        `json:"truncation_policy"`
	ContextWindow              int                   `json:"context_window,omitempty"`
}

func buildScopedCodexManifest(models []api.Model) ([]byte, error) {
	items := make([]scopedCodexManifestItem, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		slug := strings.TrimSpace(model.ID)
		if slug == "" {
			continue
		}
		key := strings.ToLower(slug)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		item := scopedCodexManifestItem{
			Slug:                     slug,
			DisplayName:              slug,
			Availability:             "available",
			SupportedInAPI:           true,
			PreferWebsockets:         false,
			UseResponsesLite:         false,
			InputModalities:          []string{"text"},
			SupportedReasoningLevels: []codexReasoningLevel{},
			DefaultReasoningLevel:    "none",
			Description:              "Gateway model: " + slug,
			ShellType:                "shell_command", Visibility: "list",
			BaseInstructions:           "You are a helpful coding assistant.",
			ExperimentalSupportedTools: []string{},
			TruncationPolicy:           map[string]any{"mode": "tokens", "limit": 10000},
		}
		if strings.Contains(key, "image") {
			item.InputModalities = []string{"text", "image"}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Slug) < strings.ToLower(items[j].Slug)
	})
	return json.Marshal(struct {
		Models []scopedCodexManifestItem `json:"models"`
	}{Models: items})
}

func mergeCodexManifestModels(body []byte, extras []api.Model) ([]byte, error) {
	if len(extras) == 0 {
		return body, nil
	}
	extraBody, err := buildScopedCodexManifest(extras)
	if err != nil {
		return nil, err
	}
	var extraRoot struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(extraBody, &extraRoot); err != nil {
		return nil, err
	}
	if len(extraRoot.Models) == 0 {
		return body, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	rawModels, ok := root["models"]
	if !ok {
		return nil, fmt.Errorf("unsupported codex manifest schema: models is missing")
	}
	var models []json.RawMessage
	if err := json.Unmarshal(rawModels, &models); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(models)+len(extraRoot.Models))
	for _, raw := range models {
		var item struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if slug := strings.ToLower(strings.TrimSpace(item.Slug)); slug != "" {
			seen[slug] = struct{}{}
		}
	}
	for _, raw := range extraRoot.Models {
		var item struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		slug := strings.ToLower(strings.TrimSpace(item.Slug))
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		models = append(models, raw)
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	root["models"] = encoded
	return json.Marshal(root)
}

func scopedCodexManifestETag(body []byte) string {
	sum := sha256.Sum256(append([]byte("codex2api-scoped-manifest-v1\x00"), body...))
	return `"codex2api-` + hex.EncodeToString(sum[:]) + `"`
}

// manifestLearnKnown caches the model slugs (lower-case) already confirmed in the registry, so that on every
// picker refresh the client does not run a DB set difference against the registry. The TTL lets rows that an admin deleted by hand be learned again.
var manifestLearnKnown = struct {
	sync.Mutex
	slugs   map[string]struct{}
	expires time.Time
}{}

const manifestLearnCacheTTL = 10 * time.Minute

// learnManifestModelsAsync checks whether the manifest has slugs that are not in the cache, and learns them in the background if so.
// A learning failure is only logged and never affects the manifest passthrough itself.
func (h *Handler) learnManifestModelsAsync(manifestBody []byte, accounts ...*auth.Account) {
	for _, account := range accounts {
		h.learnModelCapabilitiesAsync(account, manifestBody)
	}
	h.learnManifestModelRegistryAsync(manifestBody)
}

// learnFetchedManifestAsync carries the credential generation captured before
// the upstream request into capability persistence. The response body must
// never be re-associated with a generation read after a token rotation.
func (h *Handler) learnFetchedManifestAsync(manifest *CodexModelsManifest) {
	if manifest == nil {
		return
	}
	if manifest.capabilityAccount != nil {
		h.learnModelCapabilitiesAsyncAtGeneration(
			manifest.capabilityAccount,
			manifest.capabilitySnapshot.CredentialGeneration,
			manifest.Body,
		)
	}
	h.learnManifestModelRegistryAsync(manifest.Body)
}

func (h *Handler) learnManifestModelRegistryAsync(manifestBody []byte) {
	if len(manifestBody) == 0 {
		return
	}
	// Learning lite capability does not depend on the DB: every manifest passthrough refreshes the true value of use_responses_lite,
	// so the combination that always fails, "non-lite model + lite signal", is stripped before sending.
	RecordResponsesLiteSupportFromManifest(manifestBody)
	if h == nil || h.db == nil {
		return
	}
	slugs := ExtractManifestModelSlugs(manifestBody)
	if len(slugs) == 0 {
		return
	}

	now := time.Now()
	manifestLearnKnown.Lock()
	if manifestLearnKnown.slugs == nil || now.After(manifestLearnKnown.expires) {
		manifestLearnKnown.slugs = make(map[string]struct{})
		manifestLearnKnown.expires = now.Add(manifestLearnCacheTTL)
	}
	fresh := false
	for _, slug := range slugs {
		if _, ok := manifestLearnKnown.slugs[strings.ToLower(slug)]; !ok {
			fresh = true
			break
		}
	}
	manifestLearnKnown.Unlock()
	if !fresh {
		return
	}

	body := append([]byte(nil), manifestBody...)
	h.db.RunBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		added, err := LearnModelsFromManifest(ctx, h.db, body, time.Now().UTC())
		if err != nil {
			log.Printf("model manifest learning failed (passthrough not affected): %v", err)
			return
		}
		// After learning succeeds (or everything is already known), all slugs in this round's manifest are recorded in the cache.
		manifestLearnKnown.Lock()
		for _, slug := range ExtractManifestModelSlugs(body) {
			manifestLearnKnown.slugs[strings.ToLower(slug)] = struct{}{}
		}
		manifestLearnKnown.Unlock()
		if len(added) > 0 {
			log.Printf("learned %d new models from the upstream model manifest into the registry: %s", len(added), strings.Join(added, ", "))
		}
	})
}

// CodexModelsManifestURL is the Codex model manifest endpoint on the ChatGPT backend.
// Codex CLI / Codex App sends GET {base_url}/models?client_version=... to the provider
// (custom provider mode) or GET /backend-api/codex/models (chatgpt_base_url mode)
// to refresh the model picker and expects the manifest format ({"models":[{slug,...}]}) rather than an OpenAI-compatible list;
// when parsing fails, the client silently falls back to its local cache, the model picker freezes there, and new models never appear.
const CodexModelsManifestURL = "https://chatgpt.com/backend-api/codex/models"

// codexModelsManifestURLForTest lets tests replace the default URL. Production code must not assign to it.
var codexModelsManifestURLForTest = ""

// CodexModelsManifest holds the raw upstream manifest and cache metadata, so the handler can pass them through to the client unchanged.
type CodexModelsManifest struct {
	Body        []byte
	ETag        string
	NotModified bool

	capabilityAccount  *auth.Account
	capabilitySnapshot database.ModelCapabilitySnapshot
}

// FetchCodexModelsManifest fetches the Codex model manifest live from the ChatGPT backend using the account's credentials.
//
// The response body is passed through unchanged, and the manifest is neither parsed nor maintained locally: the manifest schema evolves with the Codex client version,
// so passing it through means the gateway does not need to follow schema changes, and what it returns is always the account's real model permissions
// (as opposed to the built-in registry's "theoretical list").
//
// downstream is the original headers of the downstream passthrough request (background fetches pass nil), used in
// outbound identity resolution, with the same rules as conversation requests.
func FetchCodexModelsManifest(ctx context.Context, account *auth.Account, proxyURL, clientVersion, ifNoneMatch string, downstream http.Header) (*CodexModelsManifest, error) {
	endpoint := CodexModelsManifestURL
	if codexModelsManifestURLForTest != "" {
		endpoint = codexModelsManifestURLForTest
	}
	return fetchCodexModelsManifestWithURL(ctx, account, proxyURL, endpoint, clientVersion, ifNoneMatch, downstream)
}

func fetchCodexModelsManifestWithURL(ctx context.Context, account *auth.Account, proxyURL, endpoint, clientVersion, ifNoneMatch string, downstream http.Header) (*CodexModelsManifest, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	account.Mu().RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	generation := account.CredentialGeneration
	account.Mu().RUnlock()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}

	// The manifest filter version stays consistent with the identity version of this conversation.
	identity, err := ResolveCodexMaintenanceIdentity(account, downstream)
	if err != nil {
		return nil, err
	}
	clientVersion = firstNonEmptyString(identity.Version, clientVersion)
	requestURL := endpoint + "?client_version=" + url.QueryEscape(clientVersion)

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build codex models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("Originator", identity.Originator)
	req.Header.Set("Version", clientVersion)
	if ifNoneMatch = strings.TrimSpace(ifNoneMatch); ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	// Consistent with the wham query: when a custom header overrides the workspace ID, the manifest is queried in the overridden workspace.
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	// Reuse the same transport as the gateway (supports the uTLS Chrome fingerprint), consistent with /responses and wham.
	// Pooled rather than created each time: the Codex client pulls the manifest periodically, and a one-off uTLS transport
	// would keep leaking connections and goroutines until the process exits (issue #446).
	client := getCodexMaintenanceClient(account, proxyURL)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex models request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return &CodexModelsManifest{ETag: resp.Header.Get("ETag"), NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("codex models upstream status %d: %s", resp.StatusCode, message)
	}

	body, err := ReadModelsListBody(resp.Body, CurrentRuntimeSettings().ModelsListReadMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("read codex models response: %w", err)
	}
	observedAt := time.Now().UnixNano()
	return &CodexModelsManifest{
		Body: body, ETag: resp.Header.Get("ETag"),
		capabilityAccount: account,
		capabilitySnapshot: database.ModelCapabilitySnapshot{
			AccountID: account.ID(), CredentialGeneration: generation, ObservedAt: observedAt,
		},
	}, nil
}
