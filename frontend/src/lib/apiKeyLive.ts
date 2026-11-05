// Auto channels can still route Live requests to Codex accounts.
export function supportsChatGPTLive(channel: string): boolean {
  return channel === "auto" || channel === "codex";
}
