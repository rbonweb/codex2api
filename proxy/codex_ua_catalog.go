package proxy

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// ==================== Codex client shape catalog ====================
//
// The User-Agent shape of a real Codex client is
//   {originator}/{cli version} ({os} {os version}; {arch}) {terminal} ({app name}; {app version})
// The trailing marker differs by shape: codex-tui / codex_exec reuse the prefix and CLI version; the ChatGPT desktop app is fixed
// "Codex Desktop" plus the desktop build number; the VS Code extension writes the host IDE name (VS Code / Cursor / Windsurf)
// plus the extension build number. Historical versions in the catalog are only used as the default profile when not yet synced.
//
// The weights below come from downstream UA statistics on about 65,000 de-duplicated requests from 2026-09-07~08 (rounded to whole percentages),
// used for: preset defaults, the pairing candidates on the admin page, and the account pool persona distribution.

// CodexClientKind is a Codex client shape that can be simulated.
type CodexClientKind string

const (
	CodexClientKindTUI     CodexClientKind = "codex-tui"
	CodexClientKindDesktop CodexClientKind = "codex-desktop"
	CodexClientKindVSCode  CodexClientKind = "codex-vscode"
	CodexClientKindExec    CodexClientKind = "codex-exec"
	CodexClientKindCustom  CodexClientKind = "custom"
)

const (
	CodexUserAgentModeSingle = "single"
	CodexUserAgentModePool   = "pool"
)

type codexUAWeighted struct {
	Value  string
	Weight int
}

type codexUAPlatform struct {
	OSName    string
	OSVersion string
	Arch      string
	Weight    int
}

// codexUAVersionPair is one real observed (CLI version, trailing marker version) pair; Weight is the observed share.
type codexUAVersionPair struct {
	CLIVersion string
	AppVersion string
	Weight     int
}

type codexUAKindSpec struct {
	Kind          CodexClientKind
	ClientName    string // UA prefix, also used as the Originator
	AppFollowsCLI bool   // trailing marker = (client_name; cli_version), with no separate build number
	// Default values: used when a field is left empty in single-profile mode. The TUI keeps the historical constant, so the outbound bytes of existing deployments do not change.
	DefaultPlatform codexUAPlatform
	DefaultTerminal string
	AppNames        []codexUAWeighted // candidates for the trailing marker name; the first is the default
	Terminals       []codexUAWeighted
	// ReferenceTerminals is only used for admin page presets and pairing validation; it does not take part in account pool sampling, so it does not change existing account profiles.
	ReferenceTerminals []string
	Platforms          []codexUAPlatform
	// ReferencePlatforms is the same as ReferenceTerminals: only for presets and pairing validation, not part of account pool sampling.
	ReferencePlatforms []codexUAPlatform
	VersionPairs       []codexUAVersionPair // profile defaults when not synced; empty when AppFollowsCLI is set
}

var codexUAKindOrder = []CodexClientKind{
	CodexClientKindTUI,
	CodexClientKindDesktop,
	CodexClientKindVSCode,
	CodexClientKindExec,
}

// codexUAReferenceTerminals are tokens that do not appear in the traffic statistics but that real terminals produce, derived per Codex
// terminal-detection rules: when TERM_PROGRAM is set, use "TERM_PROGRAM/TERM_PROGRAM_VERSION",
// then the dedicated environment variables such as Konsole / VTE, and finally fall back to TERM. Versions are the latest stable release of each terminal as of 2026-09.
var codexUAReferenceTerminals = []string{
	"iTerm.app/3.7.3",
	"Apple_Terminal/470.2",
	"ghostty/1.3.1",
	"WarpTerminal/v0.2026.09.16.08.27.stable_02",
	"WezTerm/20240203-110809-5046fc22",
	"WezTerm/20260819-012343-33891b4a",
	"vscode/1.139.1",
	"vscode/3.19.19", // Cursor:TERM_PROGRAM=vscode, the version is Cursor's own version
	"zed/1.21.0",
	"kitty",
	"Alacritty",
	"Konsole/260801",
	"gnome-terminal",
	"VTE/8401",
	"WindowsTerminal",
	"mintty/3.8.3",
	"xterm-256color",
	"tmux-256color",
	"screen-256color",
}

// codexUAReferencePlatforms are platforms released after the traffic statistics and not yet observed.
var codexUAReferencePlatforms = []codexUAPlatform{
	{OSName: "Mac OS", OSVersion: "27.0.0", Arch: "arm64"},
}

// Default account pool mix (rounded from real traffic share): the desktop app is over half, VS Code is next, then the TUI.
var defaultCodexUAPoolMix = map[CodexClientKind]int{
	CodexClientKindDesktop: 50,
	CodexClientKindVSCode:  30,
	CodexClientKindTUI:     20,
}

var codexUACatalog = map[CodexClientKind]*codexUAKindSpec{
	CodexClientKindTUI: {
		Kind:            CodexClientKindTUI,
		ClientName:      latestCodexClientName,
		AppFollowsCLI:   true,
		DefaultPlatform: codexUAPlatform{OSName: defaultCodexUserAgentOSName, OSVersion: defaultCodexUserAgentOSVersion, Arch: defaultCodexUserAgentArch},
		DefaultTerminal: defaultCodexUserAgentTerminal,
		AppNames:        []codexUAWeighted{{latestCodexClientName, 100}},
		Terminals: []codexUAWeighted{
			{"unknown", 30}, {"WindowsTerminal", 21}, {"kitty", 7}, {"xterm-256color", 7},
			{"vscode/1.135.0", 3}, {"vscode/1.126.0", 3}, {"Apple_Terminal/470.2", 3}, {"Apple_Terminal/455.1", 2},
			{"gnome-terminal", 3}, {"iTerm.app/3.6.11", 2}, {"vscode/1.101.2", 2}, {"xterm", 2}, {"ghostty/1.3.1", 1},
		},
		ReferenceTerminals: codexUAReferenceTerminals,
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.26200", "x86_64", 47}, {"NixOS", "26.5.0", "x86_64", 8}, {"Ubuntu", "22.4.0", "x86_64", 7},
			{"Windows", "10.0.19045", "x86_64", 6}, {"Ubuntu", "24.4.0", "x86_64", 4}, {"Ubuntu", "20.4.0", "aarch64", 3},
			{"Mac OS", "14.6.1", "x86_64", 3}, {"Mac OS", "26.6.2", "arm64", 2}, {"Windows", "10.0.22631", "x86_64", 2},
			{"CentOS", "7.0.0", "x86_64", 1}, {"Windows", "10.0.26100", "x86_64", 1}, {"Mac OS", "15.7.7", "x86_64", 1},
			{"Mac OS", "15.7.3", "arm64", 1}, {"Mac OS", "15.5.0", "arm64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
	},
	CodexClientKindDesktop: {
		Kind:            CodexClientKindDesktop,
		ClientName:      "Codex Desktop",
		DefaultPlatform: codexUAPlatform{OSName: "Windows", OSVersion: "10.0.26200", Arch: "x86_64"},
		DefaultTerminal: "unknown",
		AppNames:        []codexUAWeighted{{"Codex Desktop", 100}},
		Terminals:       []codexUAWeighted{{"unknown", 999}, {"dumb", 1}},
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.26200", "x86_64", 47}, {"Mac OS", "26.5.2", "arm64", 8}, {"Windows", "10.0.19045", "x86_64", 7},
			{"Windows", "10.0.22631", "x86_64", 5}, {"Mac OS", "14.4.1", "arm64", 5}, {"Mac OS", "26.5.1", "arm64", 4},
			{"Mac OS", "26.6.2", "arm64", 3}, {"Windows", "10.0.26100", "x86_64", 3}, {"Mac OS", "26.5.0", "arm64", 2},
			{"Windows", "10.0.22621", "x86_64", 2}, {"Mac OS", "26.2.0", "arm64", 1}, {"Mac OS", "26.3.0", "arm64", 1},
			{"Windows", "10.0.26220", "x86_64", 1}, {"Mac OS", "26.4.1", "arm64", 1}, {"Mac OS", "15.7.3", "arm64", 1},
			{"Mac OS", "26.4.0", "arm64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
		// Only stable release pairings are included; alpha builds are not in the presets. The 0.160.0 pairing is taken from the official appcast,
		// so that even unsynced deployments can get past the gpt-6.1-sol upstream version gate (measured 0.159.0).
		VersionPairs: []codexUAVersionPair{
			{"0.160.0", "26.930.51102", 1},
			{"0.153.4", "26.901.51231", 63}, {"0.153.4", "26.901.41600", 7}, {"0.153.3", "26.901.41123", 2},
			{"0.153.1", "26.901.31953", 1}, {"0.153.0", "26.901.22334", 2}, {"0.152.1", "26.831.21537", 1},
			{"0.152.0", "26.831.20005", 3}, {"0.150.1", "26.901.31953", 1}, {"0.149.1", "26.901.51231", 1},
		},
	},
	CodexClientKindVSCode: {
		Kind:            CodexClientKindVSCode,
		ClientName:      "codex_vscode",
		DefaultPlatform: codexUAPlatform{OSName: "Ubuntu", OSVersion: "22.4.0", Arch: "x86_64"},
		DefaultTerminal: "unknown",
		AppNames:        []codexUAWeighted{{"VS Code", 96}, {"Cursor", 3}, {"Windsurf", 1}, {"Positron", 1}},
		Terminals:       []codexUAWeighted{{"unknown", 70}, {"xterm-256color", 29}, {"WindowsTerminal", 1}, {"gnome-terminal", 1}},
		Platforms: []codexUAPlatform{
			{"Ubuntu", "22.4.0", "x86_64", 36}, {"Windows", "10.0.26200", "x86_64", 20}, {"Windows", "10.0.19045", "x86_64", 11},
			{"Ubuntu", "24.4.0", "x86_64", 9}, {"Mac OS", "26.6.2", "arm64", 3}, {"Mac OS", "26.4.0", "arm64", 3},
			{"CentOS", "8.6.2205", "x86_64", 3}, {"Ubuntu", "20.4.0", "x86_64", 2}, {"Windows", "10.0.22631", "x86_64", 2},
			{"Windows", "10.0.26100", "x86_64", 2}, {"Mac OS", "15.7.9", "arm64", 2}, {"CentOS", "7.0.0", "x86_64", 2},
			{"Windows", "10.0.22631", "aarch64", 1}, {"Mac OS", "26.5.2", "arm64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
		// The extension's main line is still 0.153.0, out of sync with the latest CLI 0.153.4. The 0.160.0 pairing is taken from the official
		// marketplace, serving the same purpose as the desktop app.
		VersionPairs: []codexUAVersionPair{
			{"0.160.0", "26.930.41038", 1},
			{"0.153.4", "26.901.22334", 1}, {"0.153.0", "26.901.22334", 96}, {"0.147.0", "26.519.32039", 1},
			{"0.144.5", "26.707.91948", 1}, {"0.142.5", "26.623.81905", 1},
		},
	},
	CodexClientKindExec: {
		Kind:               CodexClientKindExec,
		ClientName:         "codex_exec",
		AppFollowsCLI:      true,
		DefaultPlatform:    codexUAPlatform{OSName: "Windows", OSVersion: "10.0.19045", Arch: "x86_64"},
		DefaultTerminal:    "unknown",
		AppNames:           []codexUAWeighted{{"codex_exec", 100}},
		Terminals:          []codexUAWeighted{{"unknown", 56}, {"dumb", 43}, {"kitty", 1}},
		ReferenceTerminals: codexUAReferenceTerminals,
		Platforms: []codexUAPlatform{
			{"Windows", "10.0.19045", "x86_64", 52}, {"Ubuntu", "24.4.0", "x86_64", 40}, {"Ubuntu", "22.4.0", "x86_64", 3},
			{"Windows", "10.0.26100", "x86_64", 2}, {"Windows", "10.0.26200", "x86_64", 1}, {"Mac OS", "26.6.2", "arm64", 1},
			{"NixOS", "26.5.0", "x86_64", 1},
		},
		ReferencePlatforms: codexUAReferencePlatforms,
	},
}

func codexUAKindSpecFor(kind CodexClientKind) (*codexUAKindSpec, bool) {
	spec, ok := codexUACatalog[kind]
	return spec, ok
}

// normalizeCodexClientKind normalizes client_kind in the config; an empty string means "not specified, infer from client_name".
func normalizeCodexClientKind(value string) (CodexClientKind, bool) {
	kind := CodexClientKind(strings.ToLower(strings.TrimSpace(value)))
	switch kind {
	case "":
		return "", true
	case CodexClientKindTUI, CodexClientKindDesktop, CodexClientKindVSCode, CodexClientKindExec, CodexClientKindCustom:
		return kind, true
	default:
		return "", false
	}
}

// inferCodexClientKind infers the shape from the client name: old configs without a kind, and a manually entered "Codex Desktop",
// both automatically map to the matching preset; unrecognized names are treated as custom.
func inferCodexClientKind(clientName string) CodexClientKind {
	name := strings.ToLower(strings.TrimSpace(clientName))
	if name == "" {
		return CodexClientKindTUI
	}
	for _, kind := range codexUAKindOrder {
		if spec := codexUACatalog[kind]; strings.EqualFold(spec.ClientName, name) {
			return kind
		}
	}
	return CodexClientKindCustom
}

func effectiveCodexClientKind(cfg CodexUserAgentConfig) CodexClientKind {
	if kind, ok := normalizeCodexClientKind(cfg.ClientKind); ok && kind != "" {
		return kind
	}
	return inferCodexClientKind(cfg.ClientName)
}

func codexUAHash(seed string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("codex2api:ua-catalog:v1:" + seed))
	return h.Sum32()
}

func pickCodexUAWeighted(items []codexUAWeighted, seed string) string {
	total := 0
	for _, it := range items {
		if it.Weight > 0 {
			total += it.Weight
		}
	}
	if total <= 0 || len(items) == 0 {
		return ""
	}
	r := int(codexUAHash(seed) % uint32(total))
	for _, it := range items {
		if it.Weight <= 0 {
			continue
		}
		if r < it.Weight {
			return it.Value
		}
		r -= it.Weight
	}
	return items[len(items)-1].Value
}

func pickCodexUAPlatform(items []codexUAPlatform, seed string) codexUAPlatform {
	total := 0
	for _, it := range items {
		if it.Weight > 0 {
			total += it.Weight
		}
	}
	if total <= 0 || len(items) == 0 {
		return codexUAPlatform{}
	}
	r := int(codexUAHash(seed) % uint32(total))
	for _, it := range items {
		if it.Weight <= 0 {
			continue
		}
		if r < it.Weight {
			return it
		}
		r -= it.Weight
	}
	return items[len(items)-1]
}

func codexVersionAtLeast(version, floor string) bool {
	floor = normalizeCodexClientVersionText(floor)
	if floor == "" {
		return true
	}
	cmp, ok := compareCodexClientVersions(version, floor)
	return ok && cmp >= 0
}

// resolveCodexVersionPair provides the default-platform pairing choice for legacy callers.
func resolveCodexVersionPair(spec *codexUAKindSpec, cliVersion, appVersion, versionFloor string) (string, string) {
	choice := codexVersionSelection{CLIOverride: cliVersion, AppOverride: appVersion, VersionFloor: versionFloor}
	if spec != nil {
		choice.OSName, choice.Arch = spec.DefaultPlatform.OSName, spec.DefaultPlatform.Arch
	}
	cli, app, _ := resolveCodexCurrentVersions(spec, choice)
	return cli, app
}

// codexPoolMix returns the effective account pool mix; when not configured, the default mix is used.
func codexPoolMix(cfg CodexUserAgentConfig) []codexUAWeighted {
	mix := cfg.PoolMix
	if len(mix) == 0 {
		mix = map[string]int{}
		for kind, w := range defaultCodexUAPoolMix {
			mix[string(kind)] = w
		}
	}
	keys := make([]string, 0, len(mix))
	for k := range mix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]codexUAWeighted, 0, len(keys))
	for _, k := range keys {
		if mix[k] > 0 {
			out = append(out, codexUAWeighted{Value: k, Weight: mix[k]})
		}
	}
	return out
}

// codexPoolPersona deterministically draws a full persona from the catalog for each account: the same account always gets the same persona,
// while different accounts are spread out according to the mix and the real distribution.
func codexPoolPersona(cfg CodexUserAgentConfig, accountID int64, versionFloor string) (userAgent, version string, ok bool, err error) {
	mix := codexPoolMix(cfg)
	if len(mix) == 0 {
		return "", "", false, nil
	}
	seed := fmt.Sprintf("%d", accountID)
	kind := CodexClientKind(pickCodexUAWeighted(mix, "kind:"+seed))
	spec, found := codexUAKindSpecFor(kind)
	if !found {
		return "", "", false, nil
	}
	platform := pickCodexUAPlatform(spec.Platforms, "platform:"+seed)
	terminal := pickCodexUAWeighted(spec.Terminals, "terminal:"+seed)
	appName := spec.ClientName
	if !spec.AppFollowsCLI {
		appName = pickCodexUAWeighted(spec.AppNames, "app:"+seed)
	}
	cliVersion, appVersion, err := resolveCodexCurrentVersions(spec, codexVersionSelection{OSName: platform.OSName, Arch: platform.Arch, CLIOverride: cfg.ClientVersion, AppOverride: cfg.AppVersion, VersionFloor: versionFloor})
	if err != nil {
		return "", "", false, err
	}
	return formatCodexUserAgentWithApp(spec.ClientName, cliVersion, platform.OSName, platform.OSVersion, platform.Arch, terminal, appName, appVersion), cliVersion, true, nil
}
