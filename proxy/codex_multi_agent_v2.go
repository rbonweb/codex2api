package proxy

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Compatibility layer for Codex multi-agent v2.
//
// Background: the v2 collaboration tools in Codex CLI/Desktop mark the message parameter of spawn_agent /
// send_message / followup_task as encrypted. The official OpenAI
// upstream, on receiving that marker, generates ciphertext, and the client stores it as the
// encrypted_content of agent_message; third-party sub-models cannot read it. Codex only goes down the
// DirectPlaintextMessage branch when function_call has an explicit empty array
// encrypted_function_args and the namespace is collaboration.
//
// Before the parent request is sent to the Codex upstream, the compatibility layer does two things:
//  1. Removes the encrypted marker from the message schema of the three collaboration tools;
//  2. Temporarily renames the collaboration namespace to collaboration-optimize, to avoid the upstream
//     reserved-schema character-by-character validation (issue #342).
//
// collaboration-optimize in upstream responses is restored to collaboration, so that Codex handles them as
// the plaintext path. When it cannot tell safely, it keeps the native path and does not guess at the ciphertext content.
//
// The core transformation follows the similar implementation in CLIProxyAPI (MIT):
// https://github.com/router-for-me/CLIProxyAPI/blob/911f49c/internal/client/codex/optimize-multi-agent-v2/optimize_multi_agent_v2.go
const (
	codexMultiAgentV2CompatEnv = "CODEX_MULTI_AGENT_V2_COMPAT"

	codexCollaborationNamespace          = "collaboration"
	codexNamespaceDotPrefix              = codexCollaborationNamespace + "."
	codexNamespaceDoublePrefix           = codexCollaborationNamespace + "__"
	codexOptimizedCollaborationNamespace = "collaboration-optimize"
	codexOptimizedNamespaceDotPrefix     = codexOptimizedCollaborationNamespace + "."
	codexOptimizedNamespaceDoublePrefix  = codexOptimizedCollaborationNamespace + "__"

	// The official client's collaboration tools have only one namespace layer. Limit the depth and the number of rewrites,
	// so that abnormal requests cannot use deep nesting or many same-named tools to inflate scanning and copying costs.
	codexMultiAgentV2MaxToolDepth = 8
	codexMultiAgentV2MaxToolPaths = 16
)

var codexCollaborationMessageTools = map[string]struct{}{
	"spawn_agent":   {},
	"send_message":  {},
	"followup_task": {},
}

var (
	codexCollaborationNamespaceBytes = []byte(codexCollaborationNamespace)
	codexOptimizedNamespaceBytes     = []byte(codexOptimizedCollaborationNamespace)
	codexFunctionCallBytes           = []byte("function_call")
)

// codexMultiAgentV2State records whether this request actually performed the namespace rewrite.
// Only a rewritten request needs restoring on the response side, so ordinary requests are not processed twice.
// Account, model and diagnostic deduplication uses a mutex, because the attempt goroutine may be abandoned early,
// while the response callback still reads the state.
type codexMultiAgentV2State struct {
	mu        sync.Mutex
	optimized bool
	accountID int64
	model     string
	diagnosed map[string]struct{}
}

func (s *codexMultiAgentV2State) markOptimized(accountID int64, model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.optimized = true
	s.accountID = accountID
	s.model = model
}

func (s *codexMultiAgentV2State) isOptimizedFor(accountID int64) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.optimized && s.accountID == accountID
}

// isOptimized means this request did perform the namespace rewrite. Response completion checks this again inside the function,
// so a future response path that forgets the outer account gate does not add the marker by mistake.
func (s *codexMultiAgentV2State) isOptimized() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.optimized
}

func (s *codexMultiAgentV2State) diagnosticContext() (int64, string) {
	if s == nil {
		return 0, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountID, s.model
}

// markDiagnosed returns true when the same call_id has already been recorded, and the caller should skip logging.
func (s *codexMultiAgentV2State) markDiagnosed(key string) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.diagnosed == nil {
		s.diagnosed = make(map[string]struct{})
	}
	if _, logged := s.diagnosed[key]; logged {
		return true
	}
	s.diagnosed[key] = struct{}{}
	return false
}

type codexMultiAgentV2StateKey struct{}

func withCodexMultiAgentV2State(ctx context.Context, state *codexMultiAgentV2State) context.Context {
	if state == nil {
		return ctx
	}
	return context.WithValue(ctx, codexMultiAgentV2StateKey{}, state)
}

func codexMultiAgentV2StateFromContext(ctx context.Context) *codexMultiAgentV2State {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(codexMultiAgentV2StateKey{}).(*codexMultiAgentV2State)
	return state
}

// codexMultiAgentV2CompatEnabled is off by default; deployers enable it explicitly through an environment variable.
func codexMultiAgentV2CompatEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(codexMultiAgentV2CompatEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// isCodexMultiAgentV2Client only matches the official Codex client identity, so ordinary
// Responses/Chat requests or third-party clients are not pulled into the rewrite.
func isCodexMultiAgentV2Client(userAgent string) bool {
	userAgent = strings.TrimSpace(userAgent)
	return strings.HasPrefix(userAgent, "Codex Desktop/") ||
		strings.HasPrefix(userAgent, "codex-tui/") ||
		userAgent == "codex_cli_rs" ||
		strings.HasPrefix(userAgent, "codex_cli_rs/") ||
		strings.HasPrefix(userAgent, "codex_exec/")
}

type codexFlatToolRename struct {
	suffix string
	prefix string
}

type codexMultiAgentV2ToolScan struct {
	messagePaths     []string
	flatNames        map[string]codexFlatToolRename // tool path -> original name suffix and renamed prefix
	namespacePaths   []string
	namespaceRecords map[string]struct{}
	conflict         bool
	limitExceeded    bool
}

// prepareCodexMultiAgentV2Request is a pure function: it returns the rewritten request body and whether it performed the
// namespace rename. In any uncertain or conflicting case it returns the original body, so the upstream keeps its native behavior.
func prepareCodexMultiAgentV2Request(body []byte) ([]byte, bool, error) {
	if !bytes.Contains(body, codexCollaborationNamespaceBytes) {
		return body, false, nil
	}
	if !gjson.ValidBytes(body) {
		return body, false, fmt.Errorf("invalid JSON request body")
	}

	scan := codexMultiAgentV2ToolScan{}
	roots := gjson.GetManyBytes(body, "tools", "input")
	if tools := roots[0]; tools.IsArray() {
		scanCodexMultiAgentV2Tools(tools, "tools", "", "", true, 0, &scan)
	}
	if input := roots[1]; input.IsArray() {
		for index, item := range input.Array() {
			if !item.IsObject() || strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			scanCodexMultiAgentV2Tools(item.Get("tools"), fmt.Sprintf("input.%d.tools", index), "", "", true, 0, &scan)
		}
	}

	if scan.conflict {
		return body, false, fmt.Errorf("request defines reserved %q namespace", codexOptimizedCollaborationNamespace)
	}
	if scan.limitExceeded {
		return body, false, fmt.Errorf("collaboration tool scan exceeds nesting or count limit")
	}
	// Only handles collaboration tools with message.encrypted: when there is no encryption marker to remove,
	// keep the native path, so the schema and namespace are not changed for nothing.
	if len(scan.messagePaths) == 0 || (len(scan.namespacePaths) == 0 && len(scan.flatNames) == 0) {
		return body, false, nil
	}

	updated := body
	for _, path := range scan.messagePaths {
		var err error
		updated, err = sjson.DeleteBytes(updated, path+".parameters.properties.message.encrypted")
		if err != nil {
			return body, false, err
		}
	}
	for _, path := range scan.namespacePaths {
		var err error
		updated, err = sjson.SetBytes(updated, path+".name", codexOptimizedCollaborationNamespace)
		if err != nil {
			return body, false, err
		}
	}
	for path, rename := range scan.flatNames {
		var err error
		updated, err = sjson.SetBytes(updated, path+".name", rename.prefix+rename.suffix)
		if err != nil {
			return body, false, err
		}
	}
	updated, err := rewriteCodexMultiAgentV2ToolChoice(updated, &scan)
	if err != nil {
		return body, false, err
	}
	return updated, true, nil
}

// rewriteCodexMultiAgentV2ToolChoice also rewrites the tool_choice that points at a collaboration tool,
// so that after a tool definition is renamed, the choice still pointing at the old name does not cause an upstream 400. It only rewrites when this request actually changed
// the namespace or the corresponding flat tool.
func rewriteCodexMultiAgentV2ToolChoice(body []byte, scan *codexMultiAgentV2ToolScan) ([]byte, error) {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() {
		return body, nil
	}
	switch strings.TrimSpace(choice.Get("type").String()) {
	case "function", "custom":
		return rewriteCodexMultiAgentV2ToolChoiceItem(body, "tool_choice", choice, scan)
	case "allowed_tools":
		tools := choice.Get("tools")
		if !tools.IsArray() {
			return body, nil
		}
		updated := body
		for index, item := range tools.Array() {
			var err error
			updated, err = rewriteCodexMultiAgentV2ToolChoiceItem(
				updated, fmt.Sprintf("tool_choice.tools.%d", index), item, scan,
			)
			if err != nil {
				return body, err
			}
		}
		return updated, nil
	default:
		return body, nil
	}
}

func rewriteCodexMultiAgentV2ToolChoiceItem(body []byte, path string, item gjson.Result, scan *codexMultiAgentV2ToolScan) ([]byte, error) {
	if !item.IsObject() {
		return body, nil
	}
	namespace := strings.TrimSpace(item.Get("namespace").String())
	name := strings.TrimSpace(item.Get("name").String())
	updated := body
	if scan != nil && len(scan.namespacePaths) > 0 && namespace == codexCollaborationNamespace {
		var err error
		updated, err = sjson.SetBytes(updated, path+".namespace", codexOptimizedCollaborationNamespace)
		if err != nil {
			return body, err
		}
	}
	var suffix, renamePrefix string
	switch {
	case strings.HasPrefix(name, codexNamespaceDotPrefix):
		suffix = strings.TrimPrefix(name, codexNamespaceDotPrefix)
		renamePrefix = codexOptimizedNamespaceDotPrefix
	case strings.HasPrefix(name, codexNamespaceDoublePrefix):
		suffix = strings.TrimPrefix(name, codexNamespaceDoublePrefix)
		renamePrefix = codexOptimizedNamespaceDoublePrefix
	}
	if suffix != "" && scan != nil && scan.flatToolRenamed(suffix) {
		var err error
		updated, err = sjson.SetBytes(updated, path+".name", renamePrefix+suffix)
		if err != nil {
			return body, err
		}
	}
	return updated, nil
}

func (s *codexMultiAgentV2ToolScan) flatToolRenamed(name string) bool {
	if s == nil {
		return false
	}
	for _, rename := range s.flatNames {
		if rename.suffix == name {
			return true
		}
	}
	return false
}

// scanCodexMultiAgentV2Tools only recognizes the two official forms of the collaboration tool family:
// the three functions under the top-level collaboration namespace, and the top-level flat
// collaboration.<tool>. Other namespaces or bare tool names are not rewritten, even with the same name.
//
// It only records when parameters.properties.message.encrypted is present, so it does not do a pointless rename on a
// schema that is already plaintext.
func scanCodexMultiAgentV2Tools(tools gjson.Result, path, namespace, namespacePath string, root bool, depth int, scan *codexMultiAgentV2ToolScan) {
	if !tools.IsArray() {
		return
	}
	if depth > codexMultiAgentV2MaxToolDepth {
		scan.limitExceeded = true
		return
	}
	for index, tool := range tools.Array() {
		if !tool.IsObject() {
			continue
		}
		toolType := strings.TrimSpace(tool.Get("type").String())
		name := strings.TrimSpace(tool.Get("name").String())

		if isCodexOptimizedNamespaceName(name) {
			scan.conflict = true
			continue
		}

		switch toolType {
		case "function":
			if namespace == codexCollaborationNamespace {
				if _, ok := codexCollaborationMessageTools[name]; !ok {
					continue
				}
				if !tool.Get("parameters.properties.message.encrypted").Exists() {
					continue
				}
				if len(scan.messagePaths) >= codexMultiAgentV2MaxToolPaths {
					scan.limitExceeded = true
					return
				}
				scan.messagePaths = append(scan.messagePaths, fmt.Sprintf("%s.%d", path, index))
				if namespacePath != "" {
					if _, recorded := scan.namespaceRecords[namespacePath]; !recorded {
						if scan.namespaceRecords == nil {
							scan.namespaceRecords = make(map[string]struct{})
						}
						scan.namespaceRecords[namespacePath] = struct{}{}
						scan.namespacePaths = append(scan.namespacePaths, namespacePath)
					}
				}
				continue
			}
			if root {
				var suffix, renamePrefix string
				switch {
				case strings.HasPrefix(name, codexNamespaceDotPrefix):
					suffix = strings.TrimPrefix(name, codexNamespaceDotPrefix)
					renamePrefix = codexOptimizedNamespaceDotPrefix
				case strings.HasPrefix(name, codexNamespaceDoublePrefix):
					suffix = strings.TrimPrefix(name, codexNamespaceDoublePrefix)
					renamePrefix = codexOptimizedNamespaceDoublePrefix
				}
				if suffix == "" {
					continue
				}
				if _, ok := codexCollaborationMessageTools[suffix]; !ok {
					continue
				}
				if !tool.Get("parameters.properties.message.encrypted").Exists() {
					continue
				}
				if len(scan.messagePaths) >= codexMultiAgentV2MaxToolPaths {
					scan.limitExceeded = true
					return
				}
				toolPath := fmt.Sprintf("%s.%d", path, index)
				scan.messagePaths = append(scan.messagePaths, toolPath)
				if scan.flatNames == nil {
					scan.flatNames = make(map[string]codexFlatToolRename)
				}
				scan.flatNames[toolPath] = codexFlatToolRename{suffix: suffix, prefix: renamePrefix}
			}
		case "namespace":
			childNamespace := ""
			if root && name == codexCollaborationNamespace {
				childNamespace = codexCollaborationNamespace
			}
			toolPath := fmt.Sprintf("%s.%d", path, index)
			scanCodexMultiAgentV2Tools(tool.Get("tools"), toolPath+".tools", childNamespace, toolPath, false, depth+1, scan)
		}
	}
}

func isCodexOptimizedNamespaceName(name string) bool {
	return name == codexOptimizedCollaborationNamespace ||
		strings.HasPrefix(name, codexOptimizedNamespaceDotPrefix) ||
		strings.HasPrefix(name, codexOptimizedNamespaceDoublePrefix)
}

// restoreCodexMultiAgentV2Response restores the temporary namespace in the upstream response to
// collaboration. It only rewrites tool call items and namespace declarations; arguments/input/output
// and other opaque fields stay as they are.
//
// This rewrites targeted known response containers, rather than decoding the whole package into any and then marshaling it:
// response.completed echoes the full tool array, and re-encoding the whole thing would increase latency and garbage.
func restoreCodexMultiAgentV2Response(payload []byte, state *codexMultiAgentV2State) []byte {
	if len(payload) == 0 {
		return payload
	}
	if !bytes.Contains(payload, codexOptimizedNamespaceBytes) {
		// The upstream may normalize the namespace back to collaboration; when this request did rewrite it,
		// still check whether function_call is missing encrypted_function_args.
		if state == nil || !state.isOptimized() || !bytes.Contains(payload, codexFunctionCallBytes) {
			return payload
		}
	}
	if !gjson.ValidBytes(payload) {
		return payload
	}

	paths := [...]string{"item", "output", "response.output", "tools", "response.tools", "tool_choice", "response.tool_choice"}
	results := gjson.GetManyBytes(payload, paths[:]...)
	updated := payload
	for index, result := range results {
		var err error
		updated, err = restoreCodexMultiAgentV2Container(updated, paths[index], result, state)
		if err != nil {
			return payload
		}
	}
	return updated
}

func restoreCodexMultiAgentV2Container(updated []byte, path string, container gjson.Result, state *codexMultiAgentV2State) ([]byte, error) {
	switch {
	case container.IsObject():
		return restoreCodexMultiAgentV2Item(updated, path, container, state)
	case container.IsArray():
		var err error
		for index, item := range container.Array() {
			updated, err = restoreCodexMultiAgentV2Item(updated, fmt.Sprintf("%s.%d", path, index), item, state)
			if err != nil {
				return updated, err
			}
		}
	}
	return updated, nil
}

func restoreCodexMultiAgentV2Item(updated []byte, path string, item gjson.Result, state *codexMultiAgentV2State) ([]byte, error) {
	if !item.IsObject() {
		return updated, nil
	}
	itemType := strings.TrimSpace(item.Get("type").String())
	isToolCall := itemType == "function_call" || itemType == "custom_tool_call"
	if isToolCall {
		namespace := strings.TrimSpace(item.Get("namespace").String())
		name := strings.TrimSpace(item.Get("name").String())
		restoredName := name
		restoreNamespace := namespace == codexOptimizedCollaborationNamespace
		switch {
		case strings.HasPrefix(name, codexOptimizedNamespaceDotPrefix):
			restoredName = strings.TrimPrefix(name, codexOptimizedNamespaceDotPrefix)
			restoreNamespace = true
		case strings.HasPrefix(name, codexOptimizedNamespaceDoublePrefix):
			restoredName = strings.TrimPrefix(name, codexOptimizedNamespaceDoublePrefix)
			restoreNamespace = true
		}
		if restoreNamespace {
			var err error
			updated, err = sjson.SetBytes(updated, path+".namespace", codexCollaborationNamespace)
			if err != nil {
				return updated, err
			}
		}
		// The upstream may return collaboration directly instead of the temporary namespace; as long as this request
		// did rewrite it and it is one of the three collaboration tools, check it and fill in the plaintext marker as needed.
		if itemType == "function_call" && (restoreNamespace || namespace == codexCollaborationNamespace) {
			var err error
			var injected bool
			updated, injected, err = ensureCodexPlaintextFunctionArgs(updated, path, restoredName, item, state)
			if err != nil {
				return updated, err
			}
			if !injected {
				diagnoseCodexMultiAgentV2ToolCall(restoredName, item, state)
			}
		}
		if restoredName != name {
			var err error
			updated, err = sjson.SetBytes(updated, path+".name", restoredName)
			if err != nil {
				return updated, err
			}
		}
		return updated, nil
	}
	if itemType == "namespace" && strings.TrimSpace(item.Get("name").String()) == codexOptimizedCollaborationNamespace {
		var err error
		updated, err = sjson.SetBytes(updated, path+".name", codexCollaborationNamespace)
		if err != nil {
			return updated, err
		}
		return updated, nil
	}
	if itemType == "allowed_tools" {
		tools := item.Get("tools")
		if tools.IsArray() {
			for index, child := range tools.Array() {
				var err error
				updated, err = restoreCodexMultiAgentV2Item(
					updated, fmt.Sprintf("%s.tools.%d", path, index), child, state,
				)
				if err != nil {
					return updated, err
				}
			}
		}
		return updated, nil
	}
	// Some responses echo the tool definitions or tool_choice; flat tools were changed on the request side to
	// collaboration-optimize.<tool>, so the original prefix is restored before returning to the client.
	if itemType == "function" || itemType == "custom" {
		if namespace := strings.TrimSpace(item.Get("namespace").String()); namespace == codexOptimizedCollaborationNamespace {
			var err error
			updated, err = sjson.SetBytes(updated, path+".namespace", codexCollaborationNamespace)
			if err != nil {
				return updated, err
			}
		}
		name := strings.TrimSpace(item.Get("name").String())
		var restoredName string
		switch {
		case strings.HasPrefix(name, codexOptimizedNamespaceDotPrefix):
			restoredName = codexNamespaceDotPrefix + strings.TrimPrefix(name, codexOptimizedNamespaceDotPrefix)
		case strings.HasPrefix(name, codexOptimizedNamespaceDoublePrefix):
			restoredName = codexCollaborationNamespace + "__" + strings.TrimPrefix(name, codexOptimizedNamespaceDoublePrefix)
		}
		if restoredName != "" && restoredName != name {
			var err error
			updated, err = sjson.SetBytes(updated, path+".name", restoredName)
			if err != nil {
				return updated, err
			}
		}
	}
	return updated, nil
}

// ensureCodexPlaintextFunctionArgs explicitly fills in
// the encrypted_function_args the upstream omitted, as []. The client treats collaboration tool arguments as plaintext only when encrypted_function_args == Some([]),
// and when the field is missing or null it still treats them as ciphertext, so the plaintext message generated by the parent model would be
// re-wrapped by the client into the sub-session's encrypted_content.
//
// Before filling in, it checks that arguments is valid JSON, message is a string, and it does not look like a known ciphertext prefix.
// If the upstream still returns ciphertext, it is left as is, and the judgment is left to diagnostic logs and upper-level errors.
func ensureCodexPlaintextFunctionArgs(updated []byte, path, name string, item gjson.Result, state *codexMultiAgentV2State) ([]byte, bool, error) {
	if state == nil || !state.isOptimized() {
		return updated, false, nil
	}
	if _, ok := codexCollaborationMessageTools[name]; !ok {
		return updated, false, nil
	}
	marker := item.Get("encrypted_function_args")
	if marker.Exists() && marker.Type != gjson.Null {
		return updated, false, nil
	}
	argumentsResult := item.Get("arguments")
	if argumentsResult.Type != gjson.String {
		return updated, false, nil
	}
	arguments := strings.TrimSpace(argumentsResult.String())
	if arguments == "" || !gjson.Valid(arguments) {
		return updated, false, nil
	}
	message := gjson.Get(arguments, "message")
	if !message.Exists() || message.Type != gjson.String {
		return updated, false, nil
	}
	messageText := strings.TrimSpace(message.String())
	if messageText == "" || looksLikeCodexCiphertext(messageText) {
		return updated, false, nil
	}
	updated, err := sjson.SetBytes(updated, path+".encrypted_function_args", []string{})
	if err != nil {
		return updated, false, err
	}
	accountID, model := state.diagnosticContext()
	if callID := strings.TrimSpace(item.Get("call_id").String()); callID == "" || !state.markDiagnosed(callID+"\x00"+name+"\x00plaintext") {
		log.Printf("[multi-agent-v2] filled in encrypted_function_args: [] for upstream %s call (account=%d model=%s)",
			name, accountID, model)
	}
	return updated, true, nil
}

// looksLikeCodexCiphertext is a fail-closed heuristic applied before response completion, not a complete
// ciphertext detector. Known Fernet prefixes, the JWE compact format and the enc_ prefix are rejected outright; a long
// single token made only of base64url characters is also treated as ciphertext. On a match it keeps the value as is, so the client
// keeps handling it as an encrypted message, so ciphertext is not disguised as a plaintext task.
func looksLikeCodexCiphertext(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, codexEncryptedContentPrefix) ||
		strings.HasPrefix(value, "eyJ") ||
		strings.HasPrefix(value, "enc_") {
		return true
	}
	if len(value) < 64 || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '=':
		default:
			return false
		}
	}
	return true
}

// diagnoseCodexMultiAgentV2ToolCall checks, while restoring the response, whether the upstream really returned the
// plaintext marker. When encrypted_function_args: [] is missing, the client still handles the call as an encrypted message,
// which is the key diagnosable signal that the compatibility layer did not take effect. It only records the account, model, tool name and ciphertext shape,
// not the task body.
func diagnoseCodexMultiAgentV2ToolCall(name string, item gjson.Result, state *codexMultiAgentV2State) {
	if _, ok := codexCollaborationMessageTools[name]; !ok {
		return
	}
	arguments := item.Get("arguments").String()
	if strings.TrimSpace(arguments) == "" {
		// output_item.added may not have arguments yet; judge again at done.
		return
	}
	if marker := item.Get("encrypted_function_args"); marker.IsArray() && len(marker.Array()) == 0 {
		return
	}
	accountID, model := state.diagnosticContext()
	if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
		if state.markDiagnosed(callID + "\x00" + name) {
			return
		}
	}
	message := gjson.Get(arguments, "message").String()
	log.Printf("[multi-agent-v2] upstream %s call did not return encrypted_function_args: [], Codex will still treat it as an encrypted message (account=%d model=%s ciphertext_like=%t)",
		name, accountID, model, looksLikeCodexCiphertext(message))
}
