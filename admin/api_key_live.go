package admin

import "github.com/codex2api/database"

// On create, distinguish "not configured" from "explicitly disabled"; edit still uses the existing full limits semantics.
type createAPIKeyLimits struct {
	database.APIKeyLimits
	AllowLive *bool `json:"allow_live"`
}

func (in *createAPIKeyLimits) withDefaults() database.APIKeyLimits {
	limits := database.APIKeyLimits{}
	if in != nil {
		limits = in.APIKeyLimits
	}
	channel := limits.ResolveUpstreamChannel()
	limits.AllowLive = channel == database.UpstreamChannelAuto || channel == database.UpstreamChannelCodex
	if in != nil && in.AllowLive != nil {
		limits.AllowLive = *in.AllowLive
	}
	return limits
}
