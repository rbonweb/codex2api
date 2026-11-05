import { useEffect, useRef, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import {
  ArrowDown,
  ArrowUp,
  Plus,
  Save,
  Trash2,
} from "lucide-react";
import { api } from "../api";
import PageHeader from "../components/PageHeader";
import AccountIntelligenceHistoryDialog from "../components/AccountIntelligenceHistoryDialog";
import IntelligenceRunDialog from "../components/IntelligenceRunDialog";
import { Button } from "../components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "../components/ui/card";
import { Input } from "../components/ui/input";
import { Switch } from "../components/ui/switch";
import { Select } from "../components/ui/select";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import { refreshIntelligenceBars } from "../hooks/useIntelligenceBars";
import {
  intelligenceTargetConflict,
  type IntelligenceTargetConflict,
  type IntelligenceQuestion,
  type IntelligenceSettings,
} from "../lib/intelligenceCheck";

const fieldClass =
  "w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring";
const field = (label: string, child: ReactNode, hint?: string) => (
  <label className="flex flex-col gap-2 text-sm">
    <span className="font-medium">{label}</span>
    {child}
    {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
  </label>
);
const options = (values: Record<string, string>) =>
  Object.entries(values).map(([value, label]) => ({ value, label }));

export default function IntelligenceCheck() {
  const [params, setParams] = useSearchParams();
  const accountID = Number(params.get("account_id")) || undefined;
  const runID = Number(params.get("run")) || null;
  const [settings, setSettings] = useState<IntelligenceSettings | null>(null);
  const [saved, setSaved] = useState<IntelligenceSettings | null>(null);
  const [groups, setGroups] = useState<
    { id: number; name: string; channel?: string }[]
  >([]);
  const [targetConflict, setTargetConflict] = useState<IntelligenceTargetConflict | null>(null);
  const conflictPanel = useRef<HTMLDivElement>(null);
  const [configError, setConfigError] = useState("");
  const [busy, setBusy] = useState(false);
  const [editor, setEditor] = useState<string | null>(null);
  const { showToast } = useToast();
  const dirty =
    !!settings && JSON.stringify(settings) !== JSON.stringify(saved);

  useEffect(() => {
    if (targetConflict) conflictPanel.current?.scrollIntoView({ block: "center" });
  }, [targetConflict]);

  useEffect(() => {
    const controller = new AbortController();
    void Promise.all([
      api.getIntelligenceSettings(controller.signal),
      api.listAccountGroups(),
    ])
      .then(([config, result]) => {
        if (controller.signal.aborted) return;
        setSettings(config);
        setSaved(config);
        setGroups(result.groups ?? []);
        setConfigError("");
      })
      .catch((e) => {
        if (!controller.signal.aborted) setConfigError(getErrorMessage(e));
      });
    return () => controller.abort();
  }, []);
  useEffect(() => {
    if (!dirty) return;
    const handler = (event: BeforeUnloadEvent) => {
      event.preventDefault();
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);

  const change = <K extends keyof IntelligenceSettings>(
    key: K,
    value: IntelligenceSettings[K],
  ) => {
    if (key === "action" || key === "target_group_id") setTargetConflict(null);
    setSettings((s) => (s ? { ...s, [key]: value } : s));
  };
  const mutate = async (fn: () => Promise<unknown>, message: string) => {
    setBusy(true);
    try {
      await fn();
      showToast(message);
    } catch (e) {
      const conflict = intelligenceTargetConflict(e);
      if (conflict) setTargetConflict(conflict);
      showToast(
        conflict
          ? `There are ${conflict.conflict_count} accounts whose check channel is incompatible; handle the list below and save again`
          : getErrorMessage(e),
        "error",
      );
    } finally {
      setBusy(false);
    }
  };
  const save = () =>
    mutate(async () => {
      if (!settings) return;
      const next = await api.saveIntelligenceSettings(settings);
      setSettings(next);
      setSaved(next);
      setTargetConflict(null);
    }, "Check settings and question bank saved");
  const disableConflicts = () =>
    mutate(async () => {
      if (!targetConflict) return;
      await api.setIntelligenceAccounts(
        targetConflict.conflicts.map(a => a.account_id), false,
      );
      refreshIntelligenceBars();
      setTargetConflict(null);
    }, "Auto-check turned off for the listed accounts; save the settings again");
  const question = settings?.questions.find((q) => q.id === editor);
  const editQuestion = (patch: Partial<IntelligenceQuestion>) => {
    if (settings)
      change(
        "questions",
        settings.questions.map((q) =>
          q.id === editor ? { ...q, ...patch } : q,
        ),
      );
  };
  const reorder = (index: number, direction: number) => {
    if (!settings) return;
    const questions = [...settings.questions];
    [questions[index], questions[index + direction]] = [
      questions[index + direction],
      questions[index],
    ];
    change("questions", questions);
  };
  const numeric = (
    key:
      | "pause_initial_seconds"
      | "pause_max_seconds"
      | "recovery_initial_seconds"
      | "recovery_min_seconds"
      | "interval_seconds"
      | "retry_seconds"
      | "timeout_seconds"
      | "concurrency"
      | "retention"
      | "degrade_threshold"
      | "recover_threshold",
    label: string,
    min: number,
    max: number,
  ) =>
    field(
      label,
      <Input
        type="number"
        min={min}
        max={max}
        value={settings?.[key] ?? ""}
        onChange={(e) => change(key, Number(e.target.value))}
      />,
    );
  return (
    <>
      <PageHeader
        title="Intelligence check"
        description="Checks fixed accounts question by question, records answers and durations, and handles consecutive results by the saved policy."
        actions={
          <div className="flex flex-wrap items-center gap-2">
            {dirty && (
              <span className="text-xs text-muted-foreground">
                Unsaved changes
              </span>
            )}
            <Button
              onClick={() => void save()}
              disabled={!settings || busy || !dirty}
            >
              <Save className="size-4" />
              Save configuration
            </Button>
          </div>
        }
      />
      {configError && (
        <p role="alert" className="mb-4 text-sm text-destructive">
          Failed to load configuration: {configError}. Refresh the page and try again.
        </p>
      )}
      {settings && <div className="grid items-start gap-6 xl:grid-cols-2">
        <section aria-labelledby="intelligence-settings-title" className="min-w-0 space-y-4">
          <h2 id="intelligence-settings-title" className="text-lg font-semibold">Check settings</h2>
          <p className="text-xs text-muted-foreground">
            Checks use the account's bound proxy; without one, they connect directly. Client identity, fingerprint and time zone follow the account or global configuration.
          </p>
          <Card>
            <CardHeader className="pb-0">
              <CardTitle>Models and verdicts</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-5 sm:grid-cols-2">
              {field(
                "Auto-check master switch",
                <Switch
                  aria-label="Auto-check master switch"
                  checked={settings.enabled}
                  onCheckedChange={(v) => change("enabled", v)}
                />,
                "Takes effect after saving; manual checks are still available when turned off.",
              )}
              {field(
                "GPT / Codex model",
                <Input
                  value={settings.gpt_model}
                  onChange={(e) => change("gpt_model", e.target.value)}
                />,
              )}
              {field(
                "GPT reasoning effort",
                <Select
                  value={settings.reasoning_effort}
                  onValueChange={(v) => change("reasoning_effort", v)}
                  options={options({
                    "": "Model default",
                    none: "none",
                    minimal: "minimal",
                    low: "low",
                    medium: "medium",
                    high: "high",
                    xhigh: "xhigh",
                    max: "max",
                    ultra: "ultra",
                  })}
                />,
              )}
              {field(
                "Verdict method",
                <Select
                  value={settings.judge_mode}
                  onValueChange={(v) =>
                    change(
                      "judge_mode",
                      v as IntelligenceSettings["judge_mode"],
                    )
                  }
                  options={options({
                    answer: "Answer only",
                    latency: "Latency only",
                    both: "Answer and latency both pass",
                  })}
                />,
                "Only complete successful responses are judged, and latency must be strictly below the question's threshold.",
              )}
              {field(
                "Check new accounts by default",
                <Switch
                  checked={settings.new_accounts_enabled}
                  onCheckedChange={(v) => change("new_accounts_enabled", v)}
                />,
                "Codex only. When switching groups, only new accounts on the same channel are included by default. Existing accounts are not affected by this option.",
              )}
            </CardContent>
          </Card>
          <Card>
            <CardHeader className="pb-0">
              <CardTitle>Frequency and resources</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-5 sm:grid-cols-2">
              {numeric("interval_seconds", "Normal interval (seconds)", 1, 86400)}
              {numeric("retry_seconds", "Error retry interval (seconds)", 1, 86400)}
              {numeric("timeout_seconds", "Request timeout (seconds)", 1, 3600)}
              {numeric("concurrency", "Global check concurrency", 1, 100)}
              {numeric("retention", "Records kept per account", 1, 10000)}
              <div className="sm:col-span-2">
                {field("Adaptive checks", <Switch aria-label="Adaptive checks" checked={settings.adaptive_enabled ?? false} onCheckedChange={v => change("adaptive_enabled", v)} />,
                  "After a pause, a failed check doubles the interval; a passing check speeds up confirmation. After scheduling resumes, normal checks gradually reduce the historical backoff. When off, the fixed interval is used.")}
              </div>
              {settings.adaptive_enabled && <>
                {numeric("pause_initial_seconds", "Initial pause interval (seconds)", 1, 2592000)}
                {numeric("pause_max_seconds", "Maximum pause interval (seconds)", 1, 2592000)}
                {numeric("recovery_initial_seconds", "Initial recovery check interval (seconds)", 1, 2592000)}
                {numeric("recovery_min_seconds", "Minimum recovery check interval (seconds)", 1, 2592000)}
                <p className="text-xs text-muted-foreground sm:col-span-2">The recovery check interval halves on each attempt (when the minimum interval is at least one minute, it is rounded down to whole minutes) until the consecutive pass count below is reached. The actual wait has up to 10% early jitter. The longer the maximum interval, the later a recovery may be detected.</p>
              </>}

            </CardContent>
          </Card>
          <Card>
            <CardHeader className="pb-0">
              <CardTitle>Automatic handling</CardTitle>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="grid gap-5 sm:grid-cols-2">
                {field(
                  "When the failure threshold is reached",
                  <Select
                    value={settings.action}
                    onValueChange={(v) =>
                      change("action", v as IntelligenceSettings["action"])
                    }
                    options={options({
                      none: "No automatic handling",
                      pause: "Pause business scheduling",
                      switch_group: "Switch group",
                    })}
                  />,
                )}
                {numeric("degrade_threshold", "Consecutive failures", 1, 1000)}
                {numeric("recover_threshold", "Consecutive recovery passes", 1, 1000)}
                {settings.action === "switch_group" &&
                  field(
                    "Target group",
                    <Select
                      value={String(settings.target_group_id || "")}
                      onValueChange={(v) =>
                        change("target_group_id", Number(v))
                      }
                      placeholder="Select group"
                      options={groups
                        .filter(g => !g.channel || g.channel === "codex")
                        .map(g => ({
                          value: String(g.id),
                          label: `${g.name} (Codex)`,
                        }))}
                    />,
                  )}
              </div>
              {targetConflict && (
                <div
                  ref={conflictPanel}
                  role="alert"
                  className="space-y-3 rounded-md border border-destructive/40 p-3 text-sm"
                >
                  <p className="font-medium">
                    There are {targetConflict.conflict_count}{" "}
                    accounts whose check channel is incompatible with the target group. Settings are not saved yet.
                  </p>
                  <p className="text-muted-foreground">
                    Target group:
                    {groups.find(
                      (g) => g.id === targetConflict.target_group_id,
                    )?.name || `#${targetConflict.target_group_id}`}
                    ({targetConflict.target_group_channel}
                    ). You can turn off auto-check for the accounts below and save again, or adjust the handling policy.
                  </p>
                  <ul className="max-h-60 space-y-2 overflow-y-auto">
                    {targetConflict.conflicts.map((account) => (
                      <li
                        key={account.account_id}
                        className="flex flex-wrap items-center justify-between gap-2"
                      >
                        <span className="min-w-0 break-all">
                          #{account.account_id} ·{" "}
                          {account.account_name || "Unnamed account"} ·{" "}
                          {account.channel}
                        </span>
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={busy}
                          onClick={() => {
                            const next = new URLSearchParams(params);
                            next.delete("run");
                            next.set(
                              "account_id",
                              String(account.account_id),
                            );
                            setParams(next);
                          }}
                        >
                          View account checks
                        </Button>
                      </li>
                    ))}
                  </ul>
                  {targetConflict.conflict_count >
                    targetConflict.conflicts.length && (
                    <p className="text-muted-foreground">
                      Showing the first {targetConflict.conflicts.length}{" "}
                      accounts; save again after handling them to see the remaining conflicts.
                    </p>
                  )}
                  <Button
                    variant="outline"
                    disabled={busy}
                    onClick={() => void disableConflicts()}
                  >
                    Turn off auto-check for the above {targetConflict.conflicts.length}{" "}
                    accounts
                  </Button>
                  <p className="text-xs text-muted-foreground">
                    This only turns off auto-check; the accounts' business scheduling and existing handling recovery state stay unchanged.
                  </p>
                </div>
              )}
              <p className="text-xs leading-relaxed text-muted-foreground">
                Upstream errors are not counted as intelligence degradation; with adaptive checks on, they interrupt recovery confirmation. Manual changes to scheduling or group take priority and reset the consecutive count. Turning off the pause policy does not automatically release existing pauses; after turning off the switch-group policy, accounts already switched can still be restored to their original group by the recovery threshold. Checks keep running during a pause.
              </p>
            </CardContent>
          </Card>
        </section>
        <section aria-labelledby="intelligence-questions-title" className="min-w-0 space-y-4">
          <h2 id="intelligence-questions-title" className="text-lg font-semibold">Check question bank</h2>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-sm text-muted-foreground">
              Each account rotates through the enabled questions in order, one question at a time; edits are saved together.
            </p>
            <Button
              variant="outline"
              onClick={() => {
                const id = crypto.randomUUID();
                change("questions", [
                  ...settings.questions,
                  {
                    id,
                    name: "New question",
                    prompt: "",
                    expected_answer: "",
                    match_mode: "exact",
                    max_duration_ms: 20000,
                    enabled: false,
                  },
                ]);
                setEditor(id);
              }}
            >
              <Plus className="size-4" />
              Add question
            </Button>
          </div>
          {settings.questions.map((q, index) => (
            <Card key={q.id}>
              <CardContent className="flex flex-wrap items-center gap-3 py-4">
                <Switch
                  aria-label={`Enable ${q.name}`}
                  checked={q.enabled}
                  onCheckedChange={(enabled) =>
                    change(
                      "questions",
                      settings.questions.map((item) =>
                        item.id === q.id ? { ...item, enabled } : item,
                      ),
                    )
                  }
                />
                <div className="min-w-0 flex-1">
                  <div className="font-medium">{q.name}</div>
                  <div className="mt-1 text-xs text-muted-foreground">
                    {q.match_mode} · {q.max_duration_ms} ms · Expected{" "}
                    {q.expected_answer.slice(0, 60)}
                  </div>
                </div>
                <div className="flex gap-1">
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`Move ${q.name} up`}
                    disabled={index === 0}
                    onClick={() => reorder(index, -1)}
                  >
                    <ArrowUp className="size-4" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`Move ${q.name} down`}
                    disabled={index === settings.questions.length - 1}
                    onClick={() => reorder(index, 1)}
                  >
                    <ArrowDown className="size-4" />
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setEditor(editor === q.id ? null : q.id)}
                  >
                    Edit
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`Delete ${q.name}`}
                    onClick={() => {
                      change(
                        "questions",
                        settings.questions.filter((item) => item.id !== q.id),
                      );
                      if (editor === q.id) setEditor(null);
                    }}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </div>
                {question?.id === q.id && (
                  <div className="grid w-full gap-4 border-t pt-4 sm:grid-cols-2">
                    {field(
                      "Question name",
                      <Input
                        value={question.name}
                        onChange={(e) => editQuestion({ name: e.target.value })}
                      />,
                    )}
                    {field(
                      "Match mode",
                      <Select
                        value={question.match_mode}
                        onValueChange={(v) =>
                          editQuestion({
                            match_mode: v as IntelligenceQuestion["match_mode"],
                          })
                        }
                        options={options({
                          exact: "Exact answer (full match)",
                          contains: "Contains match",
                          regex: "Regex match (RE2)",
                        })}
                      />,
                    )}
                    <div className="sm:col-span-2">
                      {field(
                        "Question text",
                        <textarea
                          rows={7}
                          className={fieldClass}
                          value={question.prompt}
                          onChange={(e) =>
                            editQuestion({ prompt: e.target.value })
                          }
                        />,
                      )}
                    </div>
                    {field(
                      "Expected answer / regex",
                      <Input
                        value={question.expected_answer}
                        onChange={(e) =>
                          editQuestion({ expected_answer: e.target.value })
                        }
                      />,
                    )}
                    {field(
                      "Latency threshold (ms)",
                      <Input
                        type="number"
                        min={1}
                        value={question.max_duration_ms}
                        onChange={(e) =>
                          editQuestion({
                            max_duration_ms: Number(e.target.value),
                          })
                        }
                      />,
                    )}
                  </div>
                )}
              </CardContent>
            </Card>
          ))}
          {!settings.questions.length && (
            <p className="py-12 text-center text-muted-foreground">
              No questions yet. Before turning on auto-check, add and enable at least one question.
            </p>
          )}
        </section>
      </div>}
      <AccountIntelligenceHistoryDialog accountID={runID ? null : accountID ?? null} onClose={() => { const next = new URLSearchParams(params); next.delete("account_id"); setParams(next); }} />
      <IntelligenceRunDialog
        id={runID}
        onClose={() => {
          const next = new URLSearchParams(params);
          next.delete("run");
          setParams(next);
        }}
      />
    </>
  );
}