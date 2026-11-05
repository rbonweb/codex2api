package imagestore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigJSON is the JSON shape persisted in system_settings.image_storage_config.
//
// omitempty keeps the serialized default LocalBackend configuration concise (only {"backend":"local"}).
type ConfigJSON struct {
	Backend        string `json:"backend,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	Region         string `json:"region,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	AccessKey      string `json:"access_key,omitempty"`
	SecretKey      string `json:"secret_key,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	ForcePathStyle bool   `json:"force_path_style,omitempty"`
}

// ParseConfigJSON parses system_settings.image_storage_config into a Config.
// An empty string or "{}" is treated as the local default.
func ParseConfigJSON(raw string) (Config, error) {
	raw = strings.TrimSpace(raw)
	cfg := Config{Backend: BackendLocal}
	if raw == "" || raw == "{}" {
		return cfg, nil
	}
	var v ConfigJSON
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return cfg, fmt.Errorf("failed to parse image storage config: %w", err)
	}
	cfg.Backend = v.Backend
	cfg.Endpoint = v.Endpoint
	cfg.Region = v.Region
	cfg.Bucket = v.Bucket
	cfg.AccessKey = v.AccessKey
	cfg.SecretKey = v.SecretKey
	cfg.Prefix = v.Prefix
	cfg.ForcePathStyle = v.ForcePathStyle
	return cfg.Normalize(), nil
}

// EncodeConfigJSON serializes in reverse, for transfer to handlers/frontend (excludes LocalDir).
func EncodeConfigJSON(cfg Config) (string, error) {
	cfg = cfg.Normalize()
	v := ConfigJSON{
		Backend:        cfg.Backend,
		Endpoint:       cfg.Endpoint,
		Region:         cfg.Region,
		Bucket:         cfg.Bucket,
		AccessKey:      cfg.AccessKey,
		SecretKey:      cfg.SecretKey,
		Prefix:         strings.TrimSuffix(cfg.Prefix, "/"), // the trailing slash is removed when persisting and added back when loading
		ForcePathStyle: cfg.ForcePathStyle,
	}
	if v.Backend == BackendLocal {
		// The local backend persists only the backend field, to avoid accidentally storing meaningless fields such as an empty endpoint
		v = ConfigJSON{Backend: BackendLocal}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ApplyFromJSON parses the JSON and calls Configure; it is the convenience entry point for main.go and handlers.
//
// localDir is the directory used by LocalBackend (passed in by the caller). Even when switching to S3,
// a LocalBackend is also constructed as a fallback, so historical local images stay readable.
func ApplyFromJSON(raw, localDir string) (Config, error) {
	cfg, err := ParseConfigJSON(raw)
	if err != nil {
		// On a parse failure, fall back to local, so startup does not block the service
		fallback := Config{Backend: BackendLocal, LocalDir: localDir}
		_ = Configure(fallback)
		return fallback, err
	}
	cfg.LocalDir = localDir
	if err := Configure(cfg); err != nil {
		// On a configuration failure, also fall back to local
		fallback := Config{Backend: BackendLocal, LocalDir: localDir}
		_ = Configure(fallback)
		return fallback, err
	}
	return cfg, nil
}
