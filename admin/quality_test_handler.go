package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const qualityTestPromptLimit = 16000

// A separate document avoids inheriting the admin page's script-src policy (as
// srcdoc/blob documents do). Do not loosen the admin CSP or grant same-origin.
func serveQualityTestPreview(c *gin.Context) {
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; sandbox allow-scripts")
	c.Header("X-Frame-Options", "SAMEORIGIN")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!doctype html><html><head><meta charset="utf-8"></head><body><script>
addEventListener('message', function receive(event) {
  if (event.source !== parent || event.data?.type !== 'quality-test-preview' || typeof event.data.html !== 'string' || event.data.html.length > 1100000) return;
  removeEventListener('message', receive);
  document.open();
  document.write(event.data.html);
  document.close();
});
</script></body></html>`))
}

type qualityTestRequest struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	ReasoningEffort string `json:"reasoning_effort"`
	// PromptID references the custom preset the prompt came from; PresetKey names a
	// built-in one. PresetName is the client's display name, kept as a fallback only.
	PromptID   int64  `json:"prompt_id,omitempty"`
	PresetKey  string `json:"preset_key,omitempty"`
	PresetName string `json:"preset_name,omitempty"`
	// TimeoutMinutes is the run's hard limit; 0 selects the default.
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
}

var builtinPresetKey = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// resolveQualityTestPreset snapshots the preset a run came from. Custom presets
// take their name from the database; a vanished preset degrades to "hand-typed".
func (h *Handler) resolveQualityTestPreset(ctx context.Context, req qualityTestRequest) (kind, ref, name string) {
	if req.PromptID > 0 {
		preset, err := h.db.GetQualityTestPrompt(ctx, req.PromptID)
		if err != nil || preset.Prompt != req.Prompt {
			return "", "", ""
		}
		return "custom", strconv.FormatInt(preset.ID, 10), preset.Name
	}
	key := strings.ToLower(strings.TrimSpace(req.PresetKey))
	if key == "" || !builtinPresetKey.MatchString(key) {
		return "", "", ""
	}
	name = strings.TrimSpace(req.PresetName)
	if utf8.RuneCountInString(name) > qualityTestPromptNameLimit {
		name = string([]rune(name)[:qualityTestPromptNameLimit])
	}
	return "builtin", key, name
}

type qualityTestOptions struct {
	Models           []string `json:"models"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
}

// QualityTest runs one administrator-selected account, without scheduler fallback.
// It shares the connection test's transport, cancellation, diagnostics and quota handling.
func (h *Handler) QualityTest(c *gin.Context) {
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	h.testConnection(c, &req)
}

func (h *Handler) QualityTestOptions(c *gin.Context) {
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
	c.JSON(http.StatusOK, h.qualityTestOptionsForAccount(c.Request.Context(), account))
}

func (h *Handler) qualityTestOptionsForAccount(ctx context.Context, account *auth.Account) qualityTestOptions {
	options := qualityTestOptions{Models: []string{}, ReasoningEfforts: []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}}
	var candidates []string
	candidates = account.CodexModels()
	if len(candidates) == 0 {
		candidates = proxy.TextTestModelIDs(ctx, h.db)
	}
	for _, model := range candidates {
		model = strings.TrimSpace(model)
		// Effort aliases are proxy routing shortcuts, not upstream model IDs.
		if strings.EqualFold(model, "codex-auto-review") || !isTextConnectionModel(model) || strings.ContainsAny(model, "()") || slices.Contains(options.Models, model) {
			continue
		}
		options.Models = append(options.Models, model)
	}
	return options
}

func (h *Handler) validateQualityTestForAccount(ctx context.Context, account *auth.Account, req qualityTestRequest) error {
	options := h.qualityTestOptionsForAccount(ctx, account)
	if !slices.Contains(options.Models, req.Model) {
		return fmt.Errorf("this account does not support the test model: %s", req.Model)
	}
	if !slices.Contains(options.ReasoningEfforts, req.ReasoningEffort) {
		return fmt.Errorf("this channel does not support the selected reasoning effort")
	}
	return nil
}

func buildQualityTestPayload(account *auth.Account, model string, req qualityTestRequest) ([]byte, error) {
	const instructions = "Return a complete, self-contained HTML document for the user's request. Include all SVG, CSS and JavaScript inline. Do not use external resources. Return only HTML, without Markdown fences or explanations."
	body := map[string]any{"model": model, "stream": true}
	body["store"] = false
	body["instructions"] = instructions
	body["input"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": req.Prompt}}}}
	if req.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	return json.Marshal(body)
}
