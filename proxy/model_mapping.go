package proxy

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type modelMappingRule struct {
	From       string
	To         string
	Index      int
	Wildcard   bool
	LiteralLen int
	StarCount  int
}

func parseModelMappingRules(mappingJSON string) []modelMappingRule {
	mappingJSON = strings.TrimSpace(mappingJSON)
	if mappingJSON == "" || mappingJSON == "{}" {
		return nil
	}

	dec := json.NewDecoder(strings.NewReader(mappingJSON))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}

	rules := make([]modelMappingRule, 0)
	index := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil
		}
		var rawValue any
		if err := dec.Decode(&rawValue); err != nil {
			return nil
		}
		value, ok := rawValue.(string)
		if !ok {
			continue
		}

		from := strings.TrimSpace(key)
		to := strings.TrimSpace(value)
		if from == "" || to == "" {
			continue
		}
		starCount := strings.Count(from, "*")
		rules = append(rules, modelMappingRule{
			From:       from,
			To:         to,
			Index:      index,
			Wildcard:   starCount > 0,
			LiteralLen: len(strings.ReplaceAll(from, "*", "")),
			StarCount:  starCount,
		})
		index++
	}
	return rules
}

func resolveConfiguredModelMapping(model string, mappingJSON string, supportedModels []string) (string, bool) {
	return resolveModelMappingFromRules(model, parseModelMappingRules(mappingJSON), supportedModels)
}

func resolveModelMappingFromRules(model string, rules []modelMappingRule, supportedModels []string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}

	if len(rules) == 0 {
		return model, false
	}

	for _, rule := range rules {
		if !rule.Wildcard && strings.EqualFold(rule.From, model) {
			return canonicalizeCodexModel(rule.To, supportedModels), true
		}
	}

	var best *modelMappingRule
	for i := range rules {
		rule := &rules[i]
		if !rule.Wildcard || !wildcardModelPatternMatch(rule.From, model) {
			continue
		}
		if best == nil || isMoreSpecificModelMapping(rule, best) {
			best = rule
		}
	}
	if best != nil {
		return canonicalizeCodexModel(best.To, supportedModels), true
	}
	return model, false
}

func isMoreSpecificModelMapping(candidate, current *modelMappingRule) bool {
	if candidate.LiteralLen != current.LiteralLen {
		return candidate.LiteralLen > current.LiteralLen
	}
	if candidate.StarCount != current.StarCount {
		return candidate.StarCount < current.StarCount
	}
	return candidate.Index < current.Index
}

func wildcardModelPatternMatch(pattern string, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = strings.ToLower(strings.TrimSpace(model))
	if pattern == "" {
		return model == ""
	}
	if !strings.Contains(pattern, "*") {
		return pattern == model
	}

	parts := strings.Split(pattern, "*")
	position := 0
	if first := parts[0]; first != "" {
		if !strings.HasPrefix(model, first) {
			return false
		}
		position = len(first)
	}

	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		idx := strings.Index(model[position:], part)
		if idx < 0 {
			return false
		}
		position += idx + len(part)
	}

	if last := parts[len(parts)-1]; last != "" {
		return strings.HasSuffix(model, last)
	}
	return true
}

func applyReasoningEffortModelToBody(rawBody []byte, entry ReasoningEffortModel) ([]byte, error) {
	updatedBody, err := sjson.SetBytes(rawBody, "model", entry.Model)
	if err != nil {
		return rawBody, err
	}
	updatedBody, err = sjson.SetBytes(updatedBody, "reasoning_effort", entry.Effort)
	if err != nil {
		return rawBody, err
	}
	updatedBody, err = sjson.SetBytes(updatedBody, "reasoning.effort", entry.Effort)
	if err != nil {
		return rawBody, err
	}
	return updatedBody, nil
}

func (h *Handler) applyBaseModelMappingToBody(rawBody []byte, supportedModels []string) ([]byte, string, string, bool) {
	originalModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	effectiveModel := originalModel
	if originalModel == "" || !gjson.ValidBytes(rawBody) || h == nil || h.store == nil {
		return rawBody, originalModel, effectiveModel, false
	}

	updatedBody := rawBody
	modelForMapping := originalModel
	mappingApplied := false
	if entry, ok := resolveReasoningEffortModelAlias(originalModel, h.store.GetReasoningEffortModels(), supportedModels); ok {
		var err error
		updatedBody, err = applyReasoningEffortModelToBody(updatedBody, entry)
		if err != nil {
			return rawBody, originalModel, effectiveModel, false
		}
		modelForMapping = entry.Model
		effectiveModel = entry.Model
		mappingApplied = !strings.EqualFold(originalModel, entry.Model)
	}

	mappedModel, ok := resolveConfiguredModelMapping(modelForMapping, h.store.GetCodexModelMapping(), supportedModels)
	if ok && mappedModel != "" && !strings.EqualFold(mappedModel, modelForMapping) {
		var err error
		updatedBody, err = sjson.SetBytes(updatedBody, "model", mappedModel)
		if err != nil {
			return rawBody, originalModel, effectiveModel, mappingApplied
		}
		effectiveModel = mappedModel
		mappingApplied = true
	}
	return updatedBody, originalModel, effectiveModel, mappingApplied
}

func (h *Handler) resolveConfiguredRequestModel(model string, supportedModels []string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" || h == nil || h.store == nil {
		return model, false
	}
	resolved := false
	if entry, ok := resolveReasoningEffortModelAlias(model, h.store.GetReasoningEffortModels(), supportedModels); ok {
		model = entry.Model
		resolved = true
	}
	mappedModel, ok := resolveConfiguredModelMapping(model, h.store.GetCodexModelMapping(), supportedModels)
	if !ok || mappedModel == "" || mappedModel == model {
		return model, resolved
	}
	return mappedModel, true
}

func usageEffectiveModelForMapping(originalModel string, effectiveModel string, mapped bool) string {
	if !mapped {
		return ""
	}
	originalModel = strings.TrimSpace(originalModel)
	effectiveModel = strings.TrimSpace(effectiveModel)
	if originalModel == "" || effectiveModel == "" || strings.EqualFold(originalModel, effectiveModel) {
		return ""
	}
	return effectiveModel
}

// compactOpenAIModelSuffix is the model-name suffix that aggregation gateways such as newapi append to /responses/compact requests.
// For example, when newapi routes gpt-5.4 to the compact endpoint it sends gpt-5.4-openai-compact,
// but the real Codex upstream only recognizes the base model name gpt-5.4.
const compactOpenAIModelSuffix = "-openai-compact"

// stripCompactModelSuffix removes the -openai-compact suffix from a compact model name and returns the stripped
// model name and whether a rewrite happened. It is used by explicit /v1/responses/compact and by /v1/responses with
// compaction_trigger: it lets newapi-side channels still be named gpt-5.4-openai-compact,
// while codex2api validates and forwards upstream as gpt-5.4 internally.
func stripCompactModelSuffix(model string) (string, bool) {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return model, false
	}
	if !strings.HasSuffix(strings.ToLower(trimmed), compactOpenAIModelSuffix) {
		return model, false
	}
	stripped := strings.TrimSpace(trimmed[:len(trimmed)-len(compactOpenAIModelSuffix)])
	if stripped == "" {
		return model, false
	}
	return stripped, true
}

// stripCompactModelSuffixFromBody removes the -openai-compact suffix if the request body's model carries it,
// returning the rewritten body, the stripped model name, and whether it was rewritten. Invalid JSON or a missing model field returns the input unchanged.
func stripCompactModelSuffixFromBody(rawBody []byte) ([]byte, string, bool) {
	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	stripped, ok := stripCompactModelSuffix(model)
	if !ok {
		return rawBody, model, false
	}
	updated, err := sjson.SetBytes(rawBody, "model", stripped)
	if err != nil {
		return rawBody, model, false
	}
	return updated, stripped, true
}

// compactMappingCandidate is one attempted name when mapping the model of a compact request. synthetic marks
// a name that was built internally as a <model>-openai-compact alias (the client never actually sent it):
// such an alias may only match rules that explicitly end in -openai-compact, so generic rules such as "gpt-*" or "*"
// are not hijacked by a synthetic alias and do not override a rule that the base name should have matched.
type compactMappingCandidate struct {
	model     string
	synthetic bool
}

// compactMappingCandidates returns endpoint-qualified aliases before their
// base names so compact-specific rules override general model rules.
func compactMappingCandidates(models ...string) []compactMappingCandidate {
	candidates := make([]compactMappingCandidate, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)
	add := func(model string, synthetic bool) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, compactMappingCandidate{model: model, synthetic: synthetic})
	}

	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if baseModel, stripped := stripCompactModelSuffix(model); stripped {
			add(model, false)
			add(baseModel, false)
			continue
		}
		add(model+compactOpenAIModelSuffix, true)
		add(model, false)
	}
	return candidates
}

// compactAliasScopedRules filters for rules that explicitly target the -openai-compact alias (From ends with that
// suffix, including wildcards such as "*-openai-compact"). Synthetic aliases match only against these rules.
func compactAliasScopedRules(rules []modelMappingRule) []modelMappingRule {
	scoped := make([]modelMappingRule, 0, len(rules))
	for _, rule := range rules {
		if strings.HasSuffix(strings.ToLower(rule.From), compactOpenAIModelSuffix) {
			scoped = append(scoped, rule)
		}
	}
	return scoped
}

// normalizeCompactMappingTarget strips the -openai-compact suffix from a mapping target. Whether the compact
// semantics finally go to the explicit /v1/responses/compact, or to /v1/responses with compaction_trigger to keep SSE,
// that suffix is only an inbound alias convention. Forwarding it verbatim makes the upstream look up the literal model name and fail
// (xxx-openai-compact in relay model lists is usually just a routing marker).
func normalizeCompactMappingTarget(model string) string {
	if base, stripped := stripCompactModelSuffix(model); stripped {
		return base
	}
	return model
}

// applyConfiguredCompactModelMappingToBody resolves the endpoint-qualified
// compact alias before the base model. This lets the same rule work whether a
// client sends "model" or "model-openai-compact", while preserving the full
// requested name for per-account routing and usage logs.
func (h *Handler) applyConfiguredCompactModelMappingToBody(rawBody []byte, supportedModels []string) ([]byte, string, string, bool) {
	originalModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if originalModel == "" || !gjson.ValidBytes(rawBody) || h == nil || h.store == nil {
		return rawBody, originalModel, originalModel, false
	}

	if candidates := compactMappingCandidates(originalModel); len(candidates) > 0 {
		compactAlias := candidates[0]
		rules := parseModelMappingRules(h.store.GetCodexModelMapping())
		if compactAlias.synthetic {
			rules = compactAliasScopedRules(rules)
		}
		if mappedModel, ok := resolveModelMappingFromRules(compactAlias.model, rules, supportedModels); ok && mappedModel != "" && !strings.EqualFold(mappedModel, compactAlias.model) {
			mappedModel = normalizeCompactMappingTarget(mappedModel)
			effectiveModel := mappedModel
			updatedBody := rawBody
			var err error
			if entry, ok := resolveReasoningEffortModelAlias(mappedModel, h.store.GetReasoningEffortModels(), supportedModels); ok {
				updatedBody, err = applyReasoningEffortModelToBody(updatedBody, entry)
				effectiveModel = entry.Model
			} else {
				updatedBody, err = sjson.SetBytes(updatedBody, "model", mappedModel)
			}
			if err != nil {
				return rawBody, originalModel, originalModel, false
			}
			return updatedBody, originalModel, effectiveModel, true
		}
	}

	baseBody, baseModel, stripped := stripCompactModelSuffixFromBody(rawBody)
	if !stripped {
		baseBody = rawBody
		baseModel = originalModel
	}
	mappedBody, _, effectiveModel, mappingApplied := h.applyConfiguredModelMappingToBody(baseBody, supportedModels)
	if effectiveModel == "" {
		effectiveModel = baseModel
	}
	return mappedBody, originalModel, effectiveModel, mappingApplied || stripped
}
