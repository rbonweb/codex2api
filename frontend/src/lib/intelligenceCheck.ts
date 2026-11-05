export type IntelligenceStatus =
  "queued" | "running" | "normal" | "degraded" | "error" | "interrupted";
export type IntelligenceAction = "none" | "pause" | "switch_group";
export interface IntelligenceQuestion {
  id: string;
  name: string;
  prompt: string;
  expected_answer: string;
  match_mode: "exact" | "contains" | "regex";
  max_duration_ms: number;
  enabled: boolean;
}
export interface IntelligenceSettings {
  adaptive_enabled: boolean;
  pause_initial_seconds: number;
  pause_max_seconds: number;
  recovery_initial_seconds: number;
  recovery_min_seconds: number;
  revision: number;
  enabled: boolean;
  gpt_model: string;
  claude_model: string;
  reasoning_effort: string;
  judge_mode: "answer" | "latency" | "both";
  interval_seconds: number;
  retry_seconds: number;
  timeout_seconds: number;
  concurrency: number;
  retention: number;
  new_accounts_enabled: boolean;
  action: IntelligenceAction;
  degrade_threshold: number;
  recover_threshold: number;
  target_group_id: number;
  questions: IntelligenceQuestion[];
}
export interface IntelligenceTargetConflict {
  code: "intelligence_check_target_conflict";
  target_group_id: number;
  target_group_channel: string;
  conflict_count: number;
  conflicts: { account_id: number; account_name: string; channel: string }[];
}

export function intelligenceTargetConflict(error: unknown): IntelligenceTargetConflict | null {
  if (!error || typeof error !== "object" || !("details" in error)) return null;
  const details = error.details as Partial<IntelligenceTargetConflict> | null;
  if (!details || details.code !== "intelligence_check_target_conflict" ||
    !Number.isSafeInteger(details.target_group_id) ||
    typeof details.target_group_channel !== "string" ||
    !Number.isSafeInteger(details.conflict_count) || (details.conflict_count ?? 0) <= 0 ||
    !Array.isArray(details.conflicts) || details.conflicts.length === 0 || details.conflicts.length > 200 ||
    details.conflicts.some(a => !a || !Number.isSafeInteger(a.account_id) || a.account_id <= 0 || typeof a.account_name !== "string" || typeof a.channel !== "string")) return null;
  return details as IntelligenceTargetConflict;
}

export interface IntelligenceRecord {
  id: number;
  status: IntelligenceStatus;
  question_name?: string;
  duration_ms?: number;
  created_at?: string;
  completed_at?: string | null;
}
export interface IntelligenceRun extends IntelligenceRecord {
  account_id: number;
  account_name: string;
  channel: string;
  source: "manual" | "automatic";
  config_revision: number;
  answer_passed?: boolean;
  latency_passed?: boolean;
  model: string;
  response_model?: string;
  reasoning_effort: string;
  judge_mode: IntelligenceSettings["judge_mode"];
  question: IntelligenceQuestion;
  output: string;
  error: string;
  input_tokens: number;
  output_tokens: number;
  reasoning_tokens: number;
  started_at?: string | null;
  action: string;
  action_message?: string;
  effective_context?: Record<string, unknown>;
}
export interface IntelligenceAccount {
  account_id: number;
  account_name: string;
  channel: string;
  enabled: boolean;
  latest_status: IntelligenceStatus | "";
  config_error?: string;
  failed_streak: number;
  passed_streak: number;
  action: IntelligenceAction;
  next_due_at?: string | null;
}
export interface IntelligenceBar {
  probe_phase?: string;
  next_interval_seconds?: number;
  next_due_at?: string | null;
  passed_streak?: number;
  active_run_id?: number;
  active_status?: "queued" | "running";
  enabled: boolean;
  action: IntelligenceAction;
  records: IntelligenceRecord[];
}
export interface IntelligencePage<T> {
  items: T[];
  total: number;
  page: number;
  page_size: number;
}
export const intelligenceLabels: Record<string, string> = {
  queued: "Queued",
  running: "Checking",
  normal: "Normal",
  degraded: "Failed",
  error: "upstream error",
  interrupted: "Interrupted",
  resume: "Resume scheduling",
  restore_groups: "Restore groups",
  none: "No action",
  pause: "Pause scheduling",
  switch_group: "Switch group",
};
export function intelligenceColor(status: string): string | undefined {
  return (
    { normal: "#22c55e", degraded: "#ef4444", error: "#991b1b" } as Record<
      string,
      string
    >
  )[status];
}
export function intelligenceWindow<
  T extends { id: number; status: string; created_at?: string },
>(records: T[]) {
  const recent = records
    .filter((r) => !["queued", "running"].includes(r.status))
    .slice()
    .sort(
      (a, b) =>
        (a.created_at ?? "").localeCompare(b.created_at ?? "") || a.id - b.id,
    )
    .slice(-20);
  const normal = recent.filter((r) => r.status === "normal").length;
  const degraded = recent.filter((r) => r.status === "degraded").length;
  return {
    records: recent,
    rate: normal + degraded ? (normal / (normal + degraded)) * 100 : null,
    errors: recent.filter((r) => r.status === "error").length,
  };
}
export function intelligenceQuery(
  page = 1,
  filters: Record<string, string | number | undefined> = {},
) {
  const params = new URLSearchParams({ page: String(page), page_size: "20" });
  for (const [key, value] of Object.entries(filters))
    if (value !== undefined && value !== "") params.set(key, String(value));
  return params.toString();
}
export function latestRequestGuard() {
  let revision = 0;
  return {
    next: () => ++revision,
    isCurrent: (value: number) => value === revision,
  };
}

export function intelligenceScheduleLabel(bar: IntelligenceBar): string {
  const labels: Record<string, string> = {
    normal: 'Normal check', backoff: 'Backing off', recovering: 'Confirming recovery',
    retrying: 'Retrying after error', credential_blocked: 'Waiting for credential fix',
  };
  const label = labels[bar.probe_phase || ''];
  if (!label) return '';
  if (bar.probe_phase === 'credential_blocked') return label;
  const progress = bar.probe_phase === 'recovering' ? ` · ${bar.passed_streak ?? 0} consecutive passes` : '';
  const due = bar.enabled && bar.next_due_at ? ` · next run ${new Date(bar.next_due_at).toLocaleString()}` : '';
  return label + progress + due;
}
