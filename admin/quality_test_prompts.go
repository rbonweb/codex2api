package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const qualityTestPromptNameLimit = 100

type qualityTestPromptPayload struct {
	Name   *string `json:"name"`
	Prompt *string `json:"prompt"`
}

// Presets keep the studio's validation: a non-empty prompt within the request
// limit. A blank name falls back to the prompt's opening characters.
func qualityTestPromptInput(req qualityTestPromptPayload, existing *database.QualityTestPrompt) (name, prompt string, err error) {
	if existing != nil {
		name, prompt = existing.Name, existing.Prompt
	}
	if req.Prompt != nil {
		prompt = *req.Prompt
	}
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if strings.TrimSpace(prompt) == "" || len(prompt) > qualityTestPromptLimit {
		return "", "", errors.New("the prompt cannot be empty and cannot exceed 16000 bytes")
	}
	if name == "" {
		name = strings.TrimSpace(prompt)
		if utf8.RuneCountInString(name) > 24 {
			name = string([]rune(name)[:24])
		}
	}
	if utf8.RuneCountInString(name) > qualityTestPromptNameLimit {
		return "", "", errors.New("the preset name cannot exceed 100 characters")
	}
	return name, prompt, nil
}

func (h *Handler) ListQualityTestPrompts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	prompts, err := h.db.ListQualityTestPrompts(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read prompt presets"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"prompts": prompts})
}

func (h *Handler) CreateQualityTestPrompt(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	var req qualityTestPromptPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid preset request"})
		return
	}
	name, prompt, err := qualityTestPromptInput(req, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	id, err := h.db.InsertQualityTestPrompt(ctx, name, prompt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the prompt preset"})
		return
	}
	item, err := h.db.GetQualityTestPrompt(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read prompt presets"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"prompt": item})
}

func (h *Handler) UpdateQualityTestPrompt(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid preset ID"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	var req qualityTestPromptPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid preset request"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	existing, err := h.db.GetQualityTestPrompt(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Prompt preset not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read prompt presets"})
		return
	}
	name, prompt, err := qualityTestPromptInput(req, existing)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.db.UpdateQualityTestPrompt(ctx, id, name, prompt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to save the prompt preset"})
		return
	}
	item, err := h.db.GetQualityTestPrompt(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read prompt presets"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"prompt": item})
}

func (h *Handler) DeleteQualityTestPrompt(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid preset ID"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.DeleteQualityTestPrompt(ctx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete the prompt preset"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Deleted"})
}
