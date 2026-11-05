package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// ==================== Pass-through instructions observation ====================
//
// The gateway has no default system prompt of its own: the instructions of /v1/responses are passed through by the client.
// Here, before rule rewriting, the most recently observed raw instructions samples are recorded for display in the admin panel,
// so that when an admin configures append/override rules they can see the exact prompt text the client actually sent.

const (
	observedInstructionsMaxSamples = 8
	observedInstructionsMaxLength  = 32 * 1024
)

// ObservedInstructionsSample is one observed sample.
type ObservedInstructionsSample struct {
	Model        string    `json:"model"`
	Originator   string    `json:"originator"`
	Instructions string    `json:"instructions"`
	Length       int       `json:"length"`
	Truncated    bool      `json:"truncated"`
	ObservedAt   time.Time `json:"observed_at"`
}

var (
	observedInstructionsMu      sync.Mutex
	observedInstructionsSamples []ObservedInstructionsSample
	observedInstructionsIndex   = map[string]int{} // dedup key → samples index
)

// RecordObservedInstructions records the raw instructions in the request body (called before rewriting).
// Deduplicates by (model, originator, content hash), and keeps only the most recent few entries.
func RecordObservedInstructions(body []byte, headers http.Header) {
	instructions := gjson.GetBytes(body, "instructions").String()
	if instructions == "" {
		return
	}
	model := gjson.GetBytes(body, "model").String()
	originator := ""
	if headers != nil {
		originator = headers.Get("Originator")
		if originator == "" {
			originator = headers.Get("User-Agent")
		}
	}
	sum := sha256.Sum256([]byte(instructions))
	key := model + "|" + originator + "|" + hex.EncodeToString(sum[:8])

	sample := ObservedInstructionsSample{
		Model:        model,
		Originator:   originator,
		Instructions: instructions,
		Length:       len(instructions),
		ObservedAt:   time.Now(),
	}
	if len(sample.Instructions) > observedInstructionsMaxLength {
		sample.Instructions = sample.Instructions[:observedInstructionsMaxLength]
		sample.Truncated = true
	}

	observedInstructionsMu.Lock()
	defer observedInstructionsMu.Unlock()
	if idx, ok := observedInstructionsIndex[key]; ok {
		observedInstructionsSamples[idx].ObservedAt = sample.ObservedAt
		return
	}
	if len(observedInstructionsSamples) >= observedInstructionsMaxSamples {
		// Evict the oldest sample
		oldest := 0
		for i := 1; i < len(observedInstructionsSamples); i++ {
			if observedInstructionsSamples[i].ObservedAt.Before(observedInstructionsSamples[oldest].ObservedAt) {
				oldest = i
			}
		}
		for k, idx := range observedInstructionsIndex {
			if idx == oldest {
				delete(observedInstructionsIndex, k)
			} else if idx > oldest {
				observedInstructionsIndex[k] = idx - 1
			}
		}
		observedInstructionsSamples = append(observedInstructionsSamples[:oldest], observedInstructionsSamples[oldest+1:]...)
	}
	observedInstructionsSamples = append(observedInstructionsSamples, sample)
	observedInstructionsIndex[key] = len(observedInstructionsSamples) - 1
}

// ObservedInstructions returns the most recent observed samples (newest → oldest).
func ObservedInstructions() []ObservedInstructionsSample {
	observedInstructionsMu.Lock()
	defer observedInstructionsMu.Unlock()
	out := make([]ObservedInstructionsSample, len(observedInstructionsSamples))
	copy(out, observedInstructionsSamples)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
