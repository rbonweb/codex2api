const RESPONSE_CACHE_CONFIG_GENERATION = "response_cache_config_generation";
const OUTPUT_GUARD_CONFIG_GENERATION = "output_guard_config_generation";
const CODEX_IMAGES_DEFAULT_MAIN_MODEL = "codex_images_default_main_model";
// Read-only summary the backend computes from the current Resin config; writing it back has no meaning.
const CODEX_EGRESS = "codex_egress";
const CODEX_CLIENT_VERSIONS = "codex_client_versions";

export function buildWritableSettingsPayload<
  T extends object,
>(settings: T): Omit<T, typeof RESPONSE_CACHE_CONFIG_GENERATION | typeof OUTPUT_GUARD_CONFIG_GENERATION | typeof CODEX_IMAGES_DEFAULT_MAIN_MODEL | typeof CODEX_EGRESS | typeof CODEX_CLIENT_VERSIONS> {
  const payload = { ...settings };
  Reflect.deleteProperty(payload, RESPONSE_CACHE_CONFIG_GENERATION);
  Reflect.deleteProperty(payload, OUTPUT_GUARD_CONFIG_GENERATION);
  Reflect.deleteProperty(payload, CODEX_IMAGES_DEFAULT_MAIN_MODEL);
  Reflect.deleteProperty(payload, CODEX_EGRESS);
  Reflect.deleteProperty(payload, CODEX_CLIENT_VERSIONS);
  return payload;
}
