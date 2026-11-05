package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Helpers for choosing account groups during import.
//
// Group binding itself has existed for a long time (SetAccountGroups / batch-update). This part handles "bind right at import time":
// each import entry point passes in the group_ids the user selected, and every account created successfully is bound once.
//
// Two rules apply to every entry point:
//   - Validation runs **before** the account is inserted: the most common input error, a group that does not exist, is rejected while there are still no side effects,
//     so after insertion the only possible failure is a DB error; the account is kept and the response says the group was not bound.
//   - After the write, the runtime account pool must be synced: groups affect the concurrency limit, auto-pause thresholds and which API Keys can
//     schedule to the account; skipping the sync means the change only takes effect at the next full load.
//
// Groups of accounts that already exist (the duplicate and update paths) are never touched, so importing the same file again does not overwrite
// ownership that operators adjusted by hand in the admin panel; so each entry point only collects the IDs of accounts that were actually created.

// importGroupIDsField is the field name every entry point uses, and it also appears in error messages.
const importGroupIDsField = "group_ids"

// importGroupIDsContextKey carries the groups selected for this import on the gin context.
// File import splits by format into 4 parse functions that feed into importAccountsCommon; passing the groups through the request context
// is less noisy than adding a parameter to every function in the chain (consistent with how the proxy side passes request-level identity).
const importGroupIDsContextKey = "importGroupIDs"

// importGroupIDsFromContext returns the groups selected for this import; it returns nil (no binding) when there are none.
func importGroupIDsFromContext(c *gin.Context) []int64 {
	if c == nil {
		return nil
	}
	value, ok := c.Get(importGroupIDsContextKey)
	if !ok || value == nil {
		return nil
	}
	ids, _ := value.([]int64)
	return ids
}

// resolveImportGroupIDsJSON parses group_ids from the JSON request body and checks that the groups exist.
// A missing or empty array returns nil, meaning no group binding (not "clear the groups").
func (h *Handler) resolveImportGroupIDsJSON(ctx context.Context, raw json.RawMessage) ([]int64, error) {
	parsed, err := parseOptionalIntegerSliceField(raw, importGroupIDsField)
	if err != nil {
		return nil, err
	}
	return h.verifyImportGroupIDs(ctx, parsed.Values)
}

// resolveImportGroupIDsForm parses group_ids from the multipart form.
// Two formats are accepted: a JSON array ("[2,3]", which the frontend sends by default) and a comma-separated list ("2,3", handy for manual curl tests).
func (h *Handler) resolveImportGroupIDsForm(ctx context.Context, raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if strings.HasPrefix(raw, "[") {
		return h.resolveImportGroupIDsJSON(ctx, json.RawMessage(raw))
	}
	values := make([]int64, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value, err := strconv.ParseInt(part, 10, 64)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("%s must be an array of positive integers", importGroupIDsField)
		}
		values = append(values, value)
	}
	return h.verifyImportGroupIDs(ctx, values)
}

// verifyImportGroupIDs removes duplicates and then checks that the groups exist; an ID that does not exist is reported as an error straight away, and the caller returns 400.
func (h *Handler) verifyImportGroupIDs(ctx context.Context, groupIDs []int64) ([]int64, error) {
	unique := make([]int64, 0, len(groupIDs))
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 {
			return nil, fmt.Errorf("values in %s must be positive integers", importGroupIDsField)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, nil
	}
	missing, err := h.db.VerifyAccountGroupIDs(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("failed to verify account groups: %w", err)
	}
	if len(missing) > 0 {
		values := make([]string, 0, len(missing))
		for _, value := range missing {
			values = append(values, strconv.FormatInt(value, 10))
		}
		return nil, fmt.Errorf("%s contains group IDs that do not exist: %s", importGroupIDsField, strings.Join(values, ", "))
	}
	return unique, nil
}

// bindImportedAccountGroups binds groups to the accounts created in this import: it writes to the DB first (in one transaction), then syncs the runtime account pool.
// An error means the groups were not bound: the accounts are already stored, so the caller should tell the user rather than fail silently.
func (h *Handler) bindImportedAccountGroups(ctx context.Context, accountIDs []int64, groupIDs []int64) error {
	if len(accountIDs) == 0 || len(groupIDs) == 0 {
		return nil
	}
	// Group channel check (issue #487): the accounts in one import share a platform, so the first account decides the channel.
	if row, err := h.db.GetAccountByID(ctx, accountIDs[0]); err == nil {
		if err := h.validateGroupChannelForRows(ctx, []*database.AccountRow{row}, groupIDs); err != nil {
			log.Printf("import: group channel check failed (accounts=%v, groups=%v): %v", accountIDs, groupIDs, err)
			return err
		}
	}
	if err := h.db.BatchSetAccountGroups(ctx, accountIDs, groupIDs); err != nil {
		log.Printf("import: failed to bind account groups (accounts=%v, groups=%v): %v", accountIDs, groupIDs, err)
		return err
	}
	if h.store != nil {
		for _, id := range accountIDs {
			h.store.ApplyAccountGroups(id, groupIDs)
		}
	}
	return nil
}

// importedAccountIDs collects the IDs of accounts created in one import. Imports run concurrently, so collecting must take a lock.
type importedAccountIDs struct {
	mu  sync.Mutex
	ids []int64
}

func (c *importedAccountIDs) add(id int64) {
	if c == nil || id <= 0 {
		return
	}
	c.mu.Lock()
	c.ids = append(c.ids, id)
	c.mu.Unlock()
}

func (c *importedAccountIDs) snapshot() []int64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.ids...)
}
