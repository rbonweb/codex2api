package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"
)

// ==================== Dynamic User-Agent generation ====================
//
// Real Codex CLI/TUI UA format (source: codex-rs/login/src/auth/default_client.rs):
//   {originator}/{version} ({OS} {OS_version}; {arch}) {terminal} ({originator}; {version})
//
// Example:
//   codex-tui/0.153.3 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.153.3)
//   codex-tui/0.144.0-alpha.10 (Mac OS 13.7.8; arm64) xterm-256color (codex-tui; 0.144.0-alpha.10)
//   Codex Desktop/0.153.3 (Windows 10.0.26100; x86_64) unknown (Codex Desktop; 26.901.41123)
//
// originator may contain spaces (the ChatGPT desktop app is exactly "Codex Desktop"), and a real client's
// UA prefix and Originator header are always the same identifier.

// ClientProfile represents the full identity of one simulated client
type ClientProfile struct {
	UserAgent string // full User-Agent string
	Version   string // codex CLI version (must match the version in the UA)
}

const (
	latestCodexClientName          = "codex-tui"
	latestCodexCLIVersion          = "0.160.0"
	latestCodexCLIUserAgentPrefix  = "codex-tui/" + latestCodexCLIVersion
	defaultCodexUserAgentOSName    = "Mac OS"
	defaultCodexUserAgentOSVersion = "15.5.0"
	defaultCodexUserAgentArch      = "arm64"
	defaultCodexUserAgentTerminal  = "xterm-256color"
	defaultCodexCLIUserAgent       = latestCodexCLIUserAgentPrefix + " (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; " + latestCodexCLIVersion + ")"
)

type CodexUserAgentConfig struct {
	RawUserAgent  string `json:"raw_user_agent,omitempty"`
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	OSName        string `json:"os_name,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Terminal      string `json:"terminal,omitempty"`
	// ClientKind is the client kind preset (see codex_ua_catalog.go); when empty, it is inferred from client_name.
	ClientKind string `json:"client_kind,omitempty"`
	// AppName / AppVersion are the trailing marker "(app name; app version)"; when left empty, they are derived from the kind automatically,
	// desktop and VS Code extension build numbers are paired from the catalog by CLI version.
	AppName    string `json:"app_name,omitempty"`
	AppVersion string `json:"app_version,omitempty"`
	// When Mode is "pool", single-profile fields are ignored, and each account deterministically draws a catalog profile according to the PoolMix ratio.
	Mode    string         `json:"mode,omitempty"`
	PoolMix map[string]int `json:"pool_mix,omitempty"`
}

var codexOfficialClientUserAgentPrefixes = []string{
	"codex-tui/",
	"codex_cli_rs/",
	"codex_vscode/",
	"codex_app/",
	"codex_chatgpt_desktop/",
	"codex_atlas/",
	"codex_exec/",
	"codex_sdk_ts/",
	"codex ",
	"opencode/",
}

var codexStrictOfficialClientUserAgentPrefixes = []string{
	"codex-tui/",
	"codex_cli_rs/",
	"codex_vscode/",
	"codex_app/",
	"codex_chatgpt_desktop/",
	"codex_atlas/",
	"codex_exec/",
	"codex_sdk_ts/",
}

const codexStrictSpacedUserAgentPrefix = "codex "

// Third-party CLIs that ChatGPT-backend Codex accepts as first-party originators.
// opencode advertises itself via Originator: "opencode" and reaching upstream with
// that identity is required for features like reasoning_effort=xhigh to take effect.
var codexOfficialClientOriginatorPrefixes = []string{
	"codex_",
	"codex-tui",
	"codex ",
	"opencode",
}

var codexStrictOfficialClientOriginators = []string{
	"codex-tui",
	"codex_cli_rs",
	"codex_vscode",
	"codex_app",
	"codex_chatgpt_desktop",
	"codex_atlas",
	"codex_exec",
	"codex_sdk_ts",
}

func IsCodexOfficialClientByHeaders(userAgent, originator string) bool {
	return matchCodexClientHeaderPrefixes(userAgent, codexOfficialClientUserAgentPrefixes) ||
		matchCodexClientHeaderPrefixes(originator, codexOfficialClientOriginatorPrefixes)
}

func IsCodexStrictOfficialClientByHeaders(userAgent, originator string) bool {
	return matchCodexClientHeaderPrefixStrict(userAgent, codexStrictOfficialClientUserAgentPrefixes) ||
		matchCodexSpacedUserAgentStrict(userAgent) ||
		matchCodexClientHeaderExact(originator, codexStrictOfficialClientOriginators)
}

func LatestCodexCLIVersionForHeaders() string {
	return effectiveLatestCodexCLIVersion()
}

// effectiveLatestCodexCLIVersion returns the currently effective "latest Codex CLI version":
// it takes the larger of the built-in constant and the remotely synced version (CodexSyncedCLIVersion), and is never below the built-in constant,
// so an expired or invalid remote value never causes a downgrade.
func effectiveLatestCodexCLIVersion() string {
	synced := normalizeCodexClientVersionText(CurrentRuntimeSettings().CodexSyncedCLIVersion)
	if synced == "" {
		return latestCodexCLIVersion
	}
	if cmp, ok := compareCodexClientVersions(synced, latestCodexCLIVersion); ok && cmp > 0 {
		return synced
	}
	return latestCodexCLIVersion
}

func MinimalCodexCLIUserAgentForHeaders() string {
	return replaceCodexUserAgentVersion(defaultCodexCLIUserAgent, effectiveLatestCodexCLIVersion())
}

func DefaultCodexUserAgentConfigJSON() string {
	return "{}"
}

func NormalizeCodexUserAgentConfigJSON(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultCodexUserAgentConfigJSON(), nil
	}
	var cfg CodexUserAgentConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", errors.New("codex_user_agent_config must be a JSON object")
	}
	cfg = normalizeCodexUserAgentConfig(cfg)
	if err := validateCodexUserAgentConfig(cfg); err != nil {
		return "", err
	}
	if isEmptyCodexUserAgentConfig(cfg) {
		return DefaultCodexUserAgentConfigJSON(), nil
	}
	buf, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func normalizeCodexUserAgentConfig(cfg CodexUserAgentConfig) CodexUserAgentConfig {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == CodexUserAgentModeSingle {
		mode = ""
	}
	var poolMix map[string]int
	for k, v := range cfg.PoolMix {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if poolMix == nil {
			poolMix = map[string]int{}
		}
		poolMix[k] = v
	}
	return CodexUserAgentConfig{
		RawUserAgent:  strings.TrimSpace(cfg.RawUserAgent),
		ClientName:    normalizeCodexUserAgentClientName(cfg.ClientName),
		ClientVersion: normalizeCodexClientVersionText(cfg.ClientVersion),
		OSName:        strings.TrimSpace(cfg.OSName),
		OSVersion:     strings.TrimSpace(cfg.OSVersion),
		Arch:          strings.TrimSpace(cfg.Arch),
		Terminal:      strings.TrimSpace(cfg.Terminal),
		ClientKind:    strings.ToLower(strings.TrimSpace(cfg.ClientKind)),
		AppName:       normalizeCodexUserAgentClientName(cfg.AppName),
		AppVersion:    strings.TrimSpace(cfg.AppVersion),
		Mode:          mode,
		PoolMix:       poolMix,
	}
}

func validateCodexUserAgentConfig(cfg CodexUserAgentConfig) error {
	if cfg.RawUserAgent != "" {
		if !validHTTPHeaderValue(cfg.RawUserAgent) {
			return errors.New("codex raw User-Agent contains invalid HTTP header characters")
		}
	}
	if cfg.ClientVersion != "" && !validCodexClientVersionString(cfg.ClientVersion) {
		return errors.New("codex User-Agent client_version must be a semantic version like 0.153.3 or 0.144.0-alpha.10")
	}
	if cfg.ClientName != "" && !validCodexUserAgentClientName(cfg.ClientName) {
		return errors.New("codex User-Agent client_name contains invalid characters")
	}
	if cfg.AppName != "" && !validCodexUserAgentClientName(cfg.AppName) {
		return errors.New("codex User-Agent app_name contains invalid characters")
	}
	if _, ok := normalizeCodexClientKind(cfg.ClientKind); !ok {
		return errors.New("codex User-Agent client_kind must be one of codex-tui, codex-desktop, codex-vscode, codex-exec, custom")
	}
	if cfg.Mode != "" && cfg.Mode != CodexUserAgentModePool {
		return errors.New("codex User-Agent mode must be single or pool")
	}
	for kind, weight := range cfg.PoolMix {
		if k, ok := normalizeCodexClientKind(kind); !ok || k == "" || k == CodexClientKindCustom {
			return fmt.Errorf("codex User-Agent pool_mix has unknown client kind %q", kind)
		}
		if weight < 0 {
			return fmt.Errorf("codex User-Agent pool_mix weight for %s must not be negative", kind)
		}
	}
	if cfg.Mode == CodexUserAgentModePool && len(codexPoolMix(cfg)) == 0 {
		return errors.New("codex User-Agent pool_mix needs at least one positive weight")
	}
	tokenFields := map[string]string{
		"client_version": cfg.ClientVersion,
		"app_version":    cfg.AppVersion,
		"arch":           cfg.Arch,
		"terminal":       cfg.Terminal,
	}
	for name, value := range tokenFields {
		if value == "" {
			continue
		}
		if !validCodexUserAgentToken(value) {
			return fmt.Errorf("codex User-Agent %s contains invalid characters", name)
		}
	}
	for name, value := range map[string]string{"os_name": cfg.OSName, "os_version": cfg.OSVersion} {
		if value == "" {
			continue
		}
		if !validCodexUserAgentPlatformPart(value) {
			return fmt.Errorf("codex User-Agent %s contains invalid characters", name)
		}
	}
	return nil
}

func validHTTPHeaderValue(value string) bool {
	for _, r := range value {
		if r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validCodexUserAgentToken(value string) bool {
	if !validHTTPHeaderValue(value) {
		return false
	}
	return !strings.ContainsAny(value, " \t();")
}

func validCodexUserAgentPlatformPart(value string) bool {
	if !validHTTPHeaderValue(value) {
		return false
	}
	return !strings.ContainsAny(value, "();")
}

// validCodexUserAgentClientName validates the client name in the UA prefix. Unlike arch/terminal,
// it allows spaces: the ChatGPT desktop app's originator is exactly "Codex Desktop" (issue #653).
// It still rejects parentheses, semicolons, and control characters that would break the "{name}/{version} (...)" shape.
func validCodexUserAgentClientName(value string) bool {
	if !validHTTPHeaderValue(value) {
		return false
	}
	return !strings.ContainsAny(value, "\t();")
}

// normalizeCodexUserAgentClientName trims leading and trailing whitespace and collapses internal runs of whitespace into a single space,
// so it never builds a double space like "Codex  Desktop/..." that no real client would produce.
func normalizeCodexUserAgentClientName(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// CodexOriginatorForGeneratedUserAgent returns the Originator header that matches the User-Agent the gateway generates itself
// Originator header. A real Codex client's UA prefix and Originator are always the same identifier (source:
// codex-rs default_client.rs). When simulating a client such as "Codex Desktop", the two must match,
// otherwise upstream sees a self-contradictory fingerprint: the UA says desktop, while Originator says codex-tui.
// When the prefix is not a known official identifier (for example a custom route name), it falls back to the default Originator.
func CodexOriginatorForGeneratedUserAgent(userAgent string) string {
	name := codexUserAgentClientName(userAgent)
	if name != "" && IsCodexOfficialClientByHeaders("", name) {
		return name
	}
	return Originator
}

// codexUserAgentClientName takes the client name before the first "/" in the UA; without a "/" it is treated as unrecognized.
func codexUserAgentClientName(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	idx := strings.IndexByte(userAgent, '/')
	if idx <= 0 {
		return ""
	}
	return strings.TrimSpace(userAgent[:idx])
}

func isEmptyCodexUserAgentConfig(cfg CodexUserAgentConfig) bool {
	return cfg.RawUserAgent == "" &&
		cfg.ClientName == "" &&
		cfg.ClientVersion == "" &&
		cfg.OSName == "" &&
		cfg.OSVersion == "" &&
		cfg.Arch == "" &&
		cfg.Terminal == "" &&
		cfg.ClientKind == "" &&
		cfg.AppName == "" &&
		cfg.AppVersion == "" &&
		cfg.Mode == "" &&
		len(cfg.PoolMix) == 0
}

func codexUserAgentConfigFromJSON(raw string) CodexUserAgentConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return CodexUserAgentConfig{}
	}
	var cfg CodexUserAgentConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return CodexUserAgentConfig{}
	}
	cfg = normalizeCodexUserAgentConfig(cfg)
	if err := validateCodexUserAgentConfig(cfg); err != nil {
		return CodexUserAgentConfig{}
	}
	return cfg
}

func validCodexClientVersionString(value string) bool {
	_, _, ok := parseCodexClientVersionDetails(latestCodexClientName + "/" + normalizeCodexClientVersionText(value))
	return ok
}

// codexUserAgentFromConfigChecked builds the outbound identity from the global UA config. accountID takes part only in account pool mode
// in the profile draw (the same account always gets the same profile).
func codexUserAgentFromConfigChecked(raw string, accountID int64, versionFloor string) (string, string, bool, error) {
	cfg := codexUserAgentConfigFromJSON(raw)
	if isEmptyCodexUserAgentConfig(cfg) {
		return "", "", false, nil
	}
	if cfg.RawUserAgent != "" {
		return cfg.RawUserAgent, codexVersionFromUserAgent(cfg.RawUserAgent, cfg.ClientVersion), true, nil
	}
	if cfg.Mode == CodexUserAgentModePool {
		return codexPoolPersona(cfg, accountID, versionFloor)
	}
	return buildCodexStructuredUserAgent(cfg, versionFloor)
}

// buildCodexStructuredUserAgent fills blank fields from the kind preset and assembles the UA: client name, trailing marker name,
// platform and terminal take the kind's defaults; the CLI version and build number are chosen as a complete pair by resolveCodexCurrentVersions.
// When no kind is given and the client name is not recognized, it is treated as custom, and the trailing marker reuses the client name and CLI version.
func buildCodexStructuredUserAgent(cfg CodexUserAgentConfig, versionFloor string) (userAgent, version string, ok bool, err error) {
	kind := effectiveCodexClientKind(cfg)
	spec, hasSpec := codexUAKindSpecFor(kind)
	clientName := strings.TrimSpace(cfg.ClientName)
	platform := codexUAPlatform{OSName: defaultCodexUserAgentOSName, OSVersion: defaultCodexUserAgentOSVersion, Arch: defaultCodexUserAgentArch}
	terminal := defaultCodexUserAgentTerminal
	appName := ""
	if hasSpec {
		clientName = firstNonEmptyString(clientName, spec.ClientName)
		platform = spec.DefaultPlatform
		terminal = spec.DefaultTerminal
		if spec.AppFollowsCLI {
			appName = clientName
		} else if len(spec.AppNames) > 0 {
			appName = spec.AppNames[0].Value
		}
	} else {
		clientName = firstNonEmptyString(clientName, latestCodexClientName)
		appName = clientName
	}
	osName := firstNonEmptyString(cfg.OSName, platform.OSName)
	cliVersion, appVersion, err := resolveCodexCurrentVersions(spec, codexVersionSelection{
		OSName: osName, Arch: firstNonEmptyString(cfg.Arch, platform.Arch), CLIOverride: cfg.ClientVersion, AppOverride: cfg.AppVersion,
		VersionFloor: versionFloor,
	})
	if err != nil {
		return "", "", false, err
	}
	appName = firstNonEmptyString(cfg.AppName, appName)
	osVersion := firstNonEmptyString(cfg.OSVersion, platform.OSVersion)
	arch := firstNonEmptyString(cfg.Arch, platform.Arch)
	terminal = firstNonEmptyString(cfg.Terminal, terminal)
	return formatCodexUserAgentWithApp(clientName, cliVersion, osName, osVersion, arch, terminal, appName, appVersion), cliVersion, true, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func formatCodexUserAgent(clientName, clientVersion, osName, osVersion, arch, terminal string) string {
	clientName = firstNonEmptyString(clientName, latestCodexClientName)
	clientVersion = firstNonEmptyString(clientVersion, latestCodexCLIVersion)
	osName = firstNonEmptyString(osName, defaultCodexUserAgentOSName)
	osVersion = firstNonEmptyString(osVersion, defaultCodexUserAgentOSVersion)
	arch = firstNonEmptyString(arch, defaultCodexUserAgentArch)
	terminal = firstNonEmptyString(terminal, defaultCodexUserAgentTerminal)
	return formatCodexUserAgentWithApp(clientName, clientVersion, osName, osVersion, arch, terminal, clientName, clientVersion)
}

// formatCodexUserAgentWithApp assembles the full shape; the trailing marker may differ from the prefix (desktop app / VS Code extension).
func formatCodexUserAgentWithApp(clientName, clientVersion, osName, osVersion, arch, terminal, appName, appVersion string) string {
	appName = firstNonEmptyString(appName, clientName)
	appVersion = firstNonEmptyString(appVersion, clientVersion)
	platform := strings.TrimSpace(osName + " " + osVersion)
	return fmt.Sprintf("%s/%s (%s; %s) %s (%s; %s)", clientName, clientVersion, platform, arch, terminal, appName, appVersion)
}

func normalizeCodexClientVersionText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if value[0] == 'v' || value[0] == 'V' {
		return value[1:]
	}
	return value
}

func compareCodexClientVersions(current, floor string) (int, bool) {
	currentVersion, currentPrerelease, ok := codexClientVersionParts(current)
	if !ok {
		return 0, false
	}
	floorVersion, floorPrerelease, ok := codexClientVersionParts(floor)
	if !ok {
		return 0, false
	}
	if cmp := currentVersion.Compare(floorVersion); cmp != 0 {
		return cmp, true
	}
	return compareSemverPrerelease(currentPrerelease, floorPrerelease), true
}

func codexClientVersionParts(value string) (cliVersion, string, bool) {
	version, rawVersion, ok := parseCodexClientVersionDetails(latestCodexClientName + "/" + normalizeCodexClientVersionText(value))
	if !ok {
		return cliVersion{}, "", false
	}
	if idx := strings.IndexByte(rawVersion, '-'); idx >= 0 {
		return version, rawVersion[idx+1:], true
	}
	return version, "", true
}

func compareSemverPrerelease(current, floor string) int {
	if current == "" && floor == "" {
		return 0
	}
	if current == "" {
		return 1
	}
	if floor == "" {
		return -1
	}

	currentParts := strings.Split(current, ".")
	floorParts := strings.Split(floor, ".")
	for i := 0; i < len(currentParts) && i < len(floorParts); i++ {
		currentPart := currentParts[i]
		floorPart := floorParts[i]
		currentNumeric := isNumericSemverIdentifier(currentPart)
		floorNumeric := isNumericSemverIdentifier(floorPart)
		switch {
		case currentNumeric && floorNumeric:
			if cmp := compareNumericSemverIdentifier(currentPart, floorPart); cmp != 0 {
				return cmp
			}
		case currentNumeric:
			return -1
		case floorNumeric:
			return 1
		case currentPart != floorPart:
			if currentPart > floorPart {
				return 1
			}
			return -1
		}
	}
	switch {
	case len(currentParts) > len(floorParts):
		return 1
	case len(currentParts) < len(floorParts):
		return -1
	default:
		return 0
	}
}

func isNumericSemverIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func compareNumericSemverIdentifier(current, floor string) int {
	current = strings.TrimLeft(current, "0")
	floor = strings.TrimLeft(floor, "0")
	if current == "" {
		current = "0"
	}
	if floor == "" {
		floor = "0"
	}
	switch {
	case len(current) > len(floor):
		return 1
	case len(current) < len(floor):
		return -1
	case current > floor:
		return 1
	case current < floor:
		return -1
	default:
		return 0
	}
}

func replaceCodexUserAgentVersion(userAgent, version string) string {
	_, rawVersion, ok := parseCodexClientVersionDetails(userAgent)
	if !ok || rawVersion == "" || rawVersion == version {
		return userAgent
	}
	return strings.ReplaceAll(userAgent, rawVersion, version)
}

func codexProfileUserAgent(osName, osVersion, arch, terminal string) string {
	return formatCodexUserAgent(latestCodexClientName, latestCodexCLIVersion, osName, osVersion, arch, terminal)
}

func matchCodexClientHeaderPrefixes(value string, prefixes []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(value, prefix) || strings.Contains(value, prefix) {
			return true
		}
	}
	return false
}

func matchCodexClientHeaderPrefixStrict(value string, prefixes []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.ToLower(strings.TrimSpace(prefix))
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func matchCodexSpacedUserAgentStrict(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, codexStrictSpacedUserAgentPrefix) {
		return false
	}
	remainder := strings.TrimSpace(strings.TrimPrefix(value, codexStrictSpacedUserAgentPrefix))
	return remainder != ""
}

func matchCodexClientHeaderExact(value string, allowed []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	for _, candidate := range allowed {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate != "" && value == candidate {
			return true
		}
	}
	return false
}

// Predefined pool of real client profiles
// Distributed by common developer environments: macOS (primary) > Linux > Windows
var clientProfiles = []ClientProfile{
	// ---- macOS arm64 (most common: Apple Silicon developers) ----
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.4.1", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.3.0", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "Apple_Terminal/464"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.2.0", "arm64", "iTerm.app/3.5.10"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "vscode/1.100.0"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.4.0", "arm64", "tmux/3.5a"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "14.7.4", "arm64", "xterm-256color"), latestCodexCLIVersion},
	// ---- macOS x86_64 (a few Intel Macs) ----
	{codexProfileUserAgent("Mac OS", "15.4.0", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "14.7.0", "x86_64", "iTerm.app/3.5.8"), latestCodexCLIVersion},
	// ---- Linux (servers and developer workstations) ----
	{codexProfileUserAgent("Linux", "Unknown", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Ubuntu", "24.04", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Ubuntu", "24.10", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Arch Linux", "Rolling", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Fedora Linux", "41", "x86_64", "vscode/1.100.0"), latestCodexCLIVersion},
	// ---- Windows ----
	{codexProfileUserAgent("Windows", "10.0.26120", "x86_64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Windows", "10.0.22631", "x86_64", "WindowsTerminal"), latestCodexCLIVersion},
	// ---- Fallback terminal profiles (kept at the latest Codex CLI version) ----
	{codexProfileUserAgent("Mac OS", "15.5.0", "arm64", "xterm-256color"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.3.0", "arm64", "Ghostty/1.1.0"), latestCodexCLIVersion},
	{codexProfileUserAgent("Mac OS", "15.4.0", "arm64", "vscode/1.98.0"), latestCodexCLIVersion},
	{codexProfileUserAgent("Ubuntu", "24.04", "x86_64", "xterm-256color"), latestCodexCLIVersion},
}

// ProfileForAccount deterministically selects one ClientProfile based on the account ID
// The same account always returns the same profile; different accounts very likely return different profiles
func ProfileForAccount(accountID int64) ClientProfile {
	if len(clientProfiles) == 0 {
		return ClientProfile{
			UserAgent: defaultCodexCLIUserAgent,
			Version:   latestCodexCLIVersion,
		}
	}

	// Uses an FNV hash to map accountID to the profile pool, keeping the distribution even
	h := fnv.New32a()
	fmt.Fprintf(h, "codex2api:ua-profile:%d", accountID)
	idx := int(h.Sum32()) % len(clientProfiles)
	if idx < 0 {
		idx = -idx
	}

	return clientProfiles[idx]
}
