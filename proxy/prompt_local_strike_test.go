package proxy

import (
	"testing"

	"github.com/codex2api/security/promptfilter"
)

// strikeEligibleForDecision decides "whether this verdict accrues to a person and can cause NewAPI to auto-ban the account".
// It is the single source of truth. Bans are irreversible, so this test pins each safety boundary one by one, and any loosening must first be stated here
// explicitly and reviewed.
func strikeCfg(localSevere, upstreamCYB bool) promptfilter.Config {
	cfg := promptfilter.DefaultConfig()
	cfg.Advanced.Enforcement.LocalSevereStrikeEnabled = localSevere
	cfg.Advanced.Enforcement.CYBStrikeEnabled = upstreamCYB
	return cfg
}

func TestStrikeEligibleForDecision(t *testing.T) {
	localSevere := promptfilter.Decision{
		Action: promptfilter.ActionBlock, ReasonCode: "targeted_operational_intrusion_request",
		StrikeEligible: true, Terminal: true,
	}
	upstreamCYB := promptfilter.Decision{
		Action: promptfilter.ActionBlock, ReasonCode: newAPIUpstreamCyberPolicyReasonCode,
		Terminal: true,
	}

	tests := []struct {
		name    string
		decide  func() promptfilter.Decision
		cfg     promptfilter.Config
		want    bool
		purpose string
	}{
		{
			name:    "local severe violation with switch on accrues strike",
			decide:  func() promptfilter.Decision { return localSevere },
			cfg:     strikeCfg(true, false),
			want:    true,
			purpose: "a local highest-confidence severe violation is the main path to an automatic ban",
		},
		{
			name:    "local severe violation with switch off never accrues",
			decide:  func() promptfilter.Decision { return localSevere },
			cfg:     strikeCfg(false, false),
			want:    false,
			purpose: "the operator can turn off local ban accrual",
		},
		{
			name: "local block that is not terminal never accrues",
			decide: func() promptfilter.Decision {
				d := localSevere
				d.Terminal = false
				return d
			},
			cfg:     strikeCfg(true, false),
			want:    false,
			purpose: "a non-terminal hit with insufficient confidence must not cause a ban",
		},
		{
			name: "local block not attributed to current user never accrues",
			decide: func() promptfilter.Decision {
				d := localSevere
				d.StrikeEligible = false // the guard pipeline only sets this for current-user+sensitive+terminal
				return d
			},
			cfg:     strikeCfg(true, false),
			want:    false,
			purpose: "tool output, history and other input not sent directly by the current user must not accrue to a person",
		},
		{
			name: "conversation lock repeat never accrues even if strike flag set",
			decide: func() promptfilter.Decision {
				return promptfilter.Decision{
					Action: promptfilter.ActionBlock, ReasonCode: promptConversationLockedReasonCode,
					StrikeEligible: true, Terminal: true, // defensive: even if set by mistake
				}
			},
			cfg:     strikeCfg(true, false),
			want:    false,
			purpose: "repeated blocks in a locked session must never accrue again, otherwise one violation would instantly fill the ban threshold",
		},
		{
			name:    "upstream CYB honours CYBStrikeEnabled on",
			decide:  func() promptfilter.Decision { d := upstreamCYB; d.StrikeEligible = true; return d },
			cfg:     strikeCfg(false, true),
			want:    true,
			purpose: "upstream CYB is the authoritative ban signal and is not affected by the local switch",
		},
		{
			name:    "upstream CYB honours CYBStrikeEnabled off",
			decide:  func() promptfilter.Decision { d := upstreamCYB; d.StrikeEligible = false; return d },
			cfg:     strikeCfg(true, false),
			want:    false,
			purpose: "when upstream CYB strike accrual is turned off, the local switch must not accrue on its behalf",
		},
		{
			name: "non-block decision never accrues",
			decide: func() promptfilter.Decision {
				d := localSevere
				d.Action = promptfilter.ActionWarn
				return d
			},
			cfg:     strikeCfg(true, true),
			want:    false,
			purpose: "only a block can accrue a strike; a warning or an allow must not",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strikeEligibleForDecision(tc.decide(), tc.cfg); got != tc.want {
				t.Fatalf("strikeEligibleForDecision = %t, want %t (%s)", got, tc.want, tc.purpose)
			}
		})
	}
}
