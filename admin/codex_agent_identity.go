package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// isAgentIdentityCredentialRow reports whether a DB account row is an Agent Identity credential (used to tag list responses).
func isAgentIdentityCredentialRow(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(row.GetCredential("auth_mode")), auth.CodexAuthModeAgentIdentity) &&
		strings.TrimSpace(row.GetCredential("agent_runtime_id")) != "" &&
		strings.TrimSpace(row.GetCredential("agent_private_key")) != ""
}

// importAgentIdentityReq is the request body for importing an Agent Identity auth.json.
type importAgentIdentityReq struct {
	Name     string `json:"name"`
	AuthJSON string `json:"auth_json"`
	ProxyURL string `json:"proxy_url"`
}

// agentIdentityFields reads the fields from auth.json (the agent_identity object, or the root with auth_mode=agentIdentity)
// and is compatible with both snake_case and camelCase.
type agentIdentityFields struct {
	RuntimeID  string
	PrivateKey string
	TaskID     string
	AccountID  string
	UserID     string
	Email      string
	PlanType   string
	FedRAMP    bool
}

func agentIdentityString(node map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := node[key].(string); ok {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func agentIdentityBool(node map[string]any, keys ...string) bool {
	for _, key := range keys {
		if v, ok := node[key].(bool); ok {
			return v
		}
	}
	return false
}

// resolveAgentIdentityNode locates the node holding the Agent Identity fields within a JSON object:
//   - if there is an agent_identity / agentIdentity child object, use that child object;
//   - if the root has auth_mode=agentIdentity or directly carries agent_runtime_id, use the object itself.
//
// On a match it returns the node and true, otherwise false.
func resolveAgentIdentityNode(m map[string]any) (map[string]any, bool) {
	if m == nil {
		return nil, false
	}
	if sub, ok := m["agent_identity"].(map[string]any); ok {
		return sub, true
	}
	if subCamel, ok := m["agentIdentity"].(map[string]any); ok {
		return subCamel, true
	}
	mode := agentIdentityString(m, "auth_mode", "authMode")
	if strings.EqualFold(mode, auth.CodexAuthModeAgentIdentity) ||
		agentIdentityString(m, "agent_runtime_id", "agentRuntimeId") != "" {
		return m, true
	}
	return nil, false
}

// parseAgentIdentityAuthJSON parses an Agent Identity auth.json.
func parseAgentIdentityAuthJSON(raw string) (*agentIdentityFields, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("auth.json content is empty")
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil {
		return nil, fmt.Errorf("auth.json is not valid JSON: %w", err)
	}
	// Detect at the root level first; if that fails, unwrap the credentials wrapper (account JSON exported by sub2api / codex2api
	// wraps the Agent Identity fields in a credentials object).
	node, ok := resolveAgentIdentityNode(root)
	if !ok {
		if wrapped, wok := root["credentials"].(map[string]any); wok {
			node, ok = resolveAgentIdentityNode(wrapped)
		}
	}
	if !ok {
		return nil, fmt.Errorf("auth.json is not in Agent Identity format (missing agent_identity or auth_mode=agentIdentity)")
	}

	fields := &agentIdentityFields{
		RuntimeID:  agentIdentityString(node, "agent_runtime_id", "agentRuntimeId"),
		PrivateKey: agentIdentityString(node, "agent_private_key", "agentPrivateKey"),
		TaskID:     agentIdentityString(node, "task_id", "taskId"),
		AccountID:  agentIdentityString(node, "account_id", "accountId"),
		UserID:     agentIdentityString(node, "chatgpt_user_id", "chatgptUserId"),
		Email:      agentIdentityString(node, "email"),
		PlanType:   agentIdentityString(node, "plan_type", "planType"),
		FedRAMP:    agentIdentityBool(node, "chatgpt_account_is_fedramp", "chatgptAccountIsFedramp"),
	}
	if err := validateAgentIdentityFields(fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func validateAgentIdentityFields(fields *agentIdentityFields) error {
	if fields == nil {
		return fmt.Errorf("agent identity fields are empty")
	}
	runtimeID := strings.TrimSpace(fields.RuntimeID)
	privateKey := strings.TrimSpace(fields.PrivateKey)
	accountID := strings.TrimSpace(fields.AccountID)
	userID := strings.TrimSpace(fields.UserID)
	if runtimeID == "" || privateKey == "" || accountID == "" || userID == "" {
		return fmt.Errorf("agent identity is missing required fields (agent_runtime_id / agent_private_key / account_id / chatgpt_user_id)")
	}
	if !strings.HasPrefix(runtimeID, "agent-") {
		return fmt.Errorf("agent_runtime_id must start with agent-")
	}
	if err := auth.ValidateCodexAgentIdentityPrivateKey(privateKey); err != nil {
		return fmt.Errorf("agent identity private key is invalid: %w", err)
	}
	return nil
}

// ImportCodexAgentIdentity imports a Codex Agent Identity auth.json and creates the account.
// It does not store the OAuth access/refresh token; each upstream request is signed dynamically with the private key.
// POST /api/admin/accounts/codex/agent-identity
func (h *Handler) ImportCodexAgentIdentity(c *gin.Context) {
	var req importAgentIdentityReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if security.ContainsXSS(req.Name) || security.ContainsSQLInjection(req.Name) {
		writeError(c, http.StatusBadRequest, "Name contains illegal characters")
		return
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		writeError(c, http.StatusBadRequest, "Name cannot exceed 100 characters")
		return
	}
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid proxy URL")
		return
	}

	fields, err := parseAgentIdentityAuthJSON(req.AuthJSON)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	id, duplicate, err := h.createAgentIdentityAccountIfAbsent(ctx, fields, req.Name, req.ProxyURL, "agent_identity_import")
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if duplicate {
		writeError(c, http.StatusConflict, "This Agent Identity account already exists")
		return
	}

	security.SecurityAuditLog("CODEX_AGENT_IDENTITY_ADDED", fmt.Sprintf("account_id=%d email=%s ip=%s", id, fields.Email, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"message": "Agent Identity account imported successfully",
		"id":      id,
		"email":   fields.Email,
	})
}

func agentIdentityRuntimeKey(runtimeID string) string {
	return strings.ToLower(strings.TrimSpace(runtimeID))
}

func (h *Handler) existingAgentIdentityRuntimeIDs(ctx context.Context) (map[string]struct{}, error) {
	runtimeIDs := make(map[string]struct{})
	if h.store != nil {
		for _, account := range h.store.Accounts() {
			if account.IsCodexAgentIdentity() {
				runtimeIDs[agentIdentityRuntimeKey(account.AgentRuntimeID)] = struct{}{}
			}
		}
	}
	if h.db == nil {
		return runtimeIDs, nil
	}
	rows, err := h.db.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query existing Agent Identity accounts: %w", err)
	}
	for _, row := range rows {
		if isAgentIdentityCredentialRow(row) {
			runtimeIDs[agentIdentityRuntimeKey(row.GetCredential("agent_runtime_id"))] = struct{}{}
		}
	}
	return runtimeIDs, nil
}

func (h *Handler) beginAgentIdentityImport(ctx context.Context, allowDuplicate bool) (map[string]struct{}, func(), error) {
	h.agentIdentityImportMu.Lock()
	unlock := h.agentIdentityImportMu.Unlock
	if allowDuplicate {
		return nil, unlock, nil
	}
	runtimeIDs, err := h.existingAgentIdentityRuntimeIDs(ctx)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return runtimeIDs, unlock, nil
}

func (h *Handler) createAgentIdentityAccountChecked(ctx context.Context, fields *agentIdentityFields, name, proxyURL, source string, runtimeIDs map[string]struct{}, allowDuplicate bool) (int64, bool, error) {
	if err := validateAgentIdentityFields(fields); err != nil {
		return 0, false, err
	}
	runtimeKey := agentIdentityRuntimeKey(fields.RuntimeID)
	if !allowDuplicate {
		if _, exists := runtimeIDs[runtimeKey]; exists {
			return 0, true, nil
		}
	}
	id, err := h.createAgentIdentityAccount(ctx, fields, name, proxyURL, source)
	if err != nil {
		return 0, false, err
	}
	if !allowDuplicate {
		runtimeIDs[runtimeKey] = struct{}{}
	}
	return id, false, nil
}

func (h *Handler) createAgentIdentityAccountIfAbsent(ctx context.Context, fields *agentIdentityFields, name, proxyURL, source string) (int64, bool, error) {
	runtimeIDs, unlock, err := h.beginAgentIdentityImport(ctx, false)
	if err != nil {
		return 0, false, err
	}
	defer unlock()
	return h.createAgentIdentityAccountChecked(ctx, fields, name, proxyURL, source, runtimeIDs, false)
}

// createAgentIdentityAccount creates the account from the parsed fields, loads it into the pool, and returns the account ID.
func (h *Handler) createAgentIdentityAccount(ctx context.Context, fields *agentIdentityFields, name, proxyURL, source string) (int64, error) {
	if err := validateAgentIdentityFields(fields); err != nil {
		return 0, err
	}
	planType := fields.PlanType
	if planType == "" {
		planType = "free"
	}
	credentials := map[string]interface{}{
		"auth_mode":                  auth.CodexAuthModeAgentIdentity,
		"agent_runtime_id":           fields.RuntimeID,
		"agent_private_key":          fields.PrivateKey,
		"account_id":                 fields.AccountID,
		"chatgpt_user_id":            fields.UserID,
		"email":                      fields.Email,
		"plan_type":                  planType,
		"chatgpt_account_is_fedramp": fields.FedRAMP,
	}
	if fields.TaskID != "" {
		credentials["task_id"] = fields.TaskID
	}

	name = strings.TrimSpace(name)
	if name == "" {
		if fields.Email != "" {
			name = fields.Email
		} else {
			name = "agent-identity"
		}
	}

	id, err := h.db.InsertAccountWithCredentials(ctx, name, credentials, proxyURL)
	if err != nil {
		return 0, err
	}
	h.db.InsertAccountEventAsync(id, "added", source)
	if err := h.store.LoadAccountByID(ctx, id); err != nil {
		return 0, err
	}
	// After import, an asynchronous usage probe runs: Agent Identity uses the signed /responses probe, and fills the usage progress bars from the response headers.
	h.triggerImportedAccountUsageProbe(id, source)
	return id, nil
}

// importAgentIdentityTokens handles the Agent Identity entries recognized by the generic import (JSON file / folder scan):
// it validates the private key, de-duplicates by runtime_id (within the batch and against existing accounts; de-duplication is skipped when allowDuplicate is set), and creates and pools the accounts.
// It returns the counts of added, skipped duplicates and failures, plus the IDs of newly created accounts (so the importer can bind them to a group in one step).
func (h *Handler) importAgentIdentityTokens(ctx context.Context, tokens []importToken, settings importSettings) (success, duplicate, failed int, createdIDs []int64) {
	if len(tokens) == 0 {
		return 0, 0, 0, nil
	}
	allowDuplicate := settings.allowDuplicate
	runtimeIDs, unlock, err := h.beginAgentIdentityImport(ctx, allowDuplicate)
	if err != nil {
		return 0, 0, len(tokens), nil
	}
	defer unlock()
	for _, t := range tokens {
		fields := &agentIdentityFields{
			RuntimeID:  strings.TrimSpace(t.agentRuntimeID),
			PrivateKey: strings.TrimSpace(t.agentPrivateKey),
			TaskID:     strings.TrimSpace(t.agentTaskID),
			AccountID:  strings.TrimSpace(t.accountID),
			UserID:     strings.TrimSpace(t.chatgptUserID),
			Email:      strings.TrimSpace(t.email),
			PlanType:   strings.TrimSpace(t.planType),
			FedRAMP:    t.agentFedRAMP,
		}
		id, duplicated, err := h.createAgentIdentityAccountChecked(ctx, fields, t.name, settings.proxyForToken(t), "import_agent_identity", runtimeIDs, allowDuplicate)
		if err != nil {
			failed++
			continue
		}
		if duplicated {
			duplicate++
			continue
		}
		success++
		if id > 0 {
			createdIDs = append(createdIDs, id)
		}
	}
	return success, duplicate, failed, createdIDs
}

// batchImportAgentIdentityReq is the request body for batch-importing Agent Identity files.
// Files holds one raw JSON content per auth.json.
type batchImportAgentIdentityReq struct {
	Files    []string `json:"files"`
	ProxyURL string   `json:"proxy_url"`
}

type agentIdentityImportItem struct {
	Email string `json:"email,omitempty"`
	ID    int64  `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

const agentIdentityBatchMaxFiles = 200

// BatchImportCodexAgentIdentity batch-imports Agent Identity auth.json files.
// POST /api/admin/accounts/codex/agent-identity/import
func (h *Handler) BatchImportCodexAgentIdentity(c *gin.Context) {
	var req batchImportAgentIdentityReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid request format")
		return
	}
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "Invalid proxy URL")
		return
	}
	if len(req.Files) == 0 {
		writeError(c, http.StatusBadRequest, "No file content provided")
		return
	}
	if len(req.Files) > agentIdentityBatchMaxFiles {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("Up to %d files can be imported at once", agentIdentityBatchMaxFiles))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	runtimeIDs, unlock, err := h.beginAgentIdentityImport(ctx, false)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	defer unlock()

	items := make([]agentIdentityImportItem, 0, len(req.Files))
	imported := 0
	for _, content := range req.Files {
		item := agentIdentityImportItem{}
		fields, err := parseAgentIdentityAuthJSON(content)
		if err != nil {
			item.Error = err.Error()
			items = append(items, item)
			continue
		}
		item.Email = fields.Email
		id, duplicated, err := h.createAgentIdentityAccountChecked(ctx, fields, "", req.ProxyURL, "agent_identity_file_import", runtimeIDs, false)
		if duplicated {
			item.Error = "Account already exists, skipped"
			items = append(items, item)
			continue
		}
		if err != nil {
			item.Error = err.Error()
			items = append(items, item)
			continue
		}
		item.OK = true
		item.ID = id
		items = append(items, item)
		imported++
	}

	security.SecurityAuditLog("CODEX_AGENT_IDENTITY_FILE_IMPORTED", fmt.Sprintf("total=%d imported=%d ip=%s", len(req.Files), imported, c.ClientIP()))
	c.JSON(http.StatusOK, gin.H{
		"total":    len(req.Files),
		"imported": imported,
		"failed":   len(req.Files) - imported,
		"items":    items,
	})
}
