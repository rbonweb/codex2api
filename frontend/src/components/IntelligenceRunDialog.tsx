import { useEffect, useState } from "react";
import { api } from "../api";
import {
  intelligenceLabels,
  type IntelligenceRun,
} from "../lib/intelligenceCheck";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "./ui/dialog";
import { Button } from "./ui/button";
import { getErrorMessage } from "../utils/error";

export default function IntelligenceRunDialog({
  id,
  onClose,
}: {
  id: number | null;
  onClose: () => void;
}) {
  const [run, setRun] = useState<IntelligenceRun | null>(null);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    setRun(null);
    setError("");
    if (!id) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof window.setTimeout> | undefined;
    const load = async () => {
      try {
        const { run } = await api.getIntelligenceRun(id, controller.signal);
        if (controller.signal.aborted) return;
        setRun(run);
        setError("");
        if (run.status === "queued" || run.status === "running")
          timer = window.setTimeout(() => {
            void load();
          }, 4000);
      } catch (e) {
        if (!controller.signal.aborted) setError(getErrorMessage(e));
      }
    };
    void load();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [id, retry]);
  return (
    <Dialog
      open={!!id}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>Intelligence check details #{id}</DialogTitle>
          <DialogDescription>
            Saves this run's questions, verdict configuration, and final visible answer.
          </DialogDescription>
        </DialogHeader>
        {error && (
          <div role="alert" className="text-destructive">
            {error}
            <Button variant="outline" onClick={() => setRetry((n) => n + 1)}>
              Retry
            </Button>
          </div>
        )}
        {!run && !error && <p className="text-muted-foreground">Loading...</p>}
        {run && (
          <div className="space-y-4 text-sm">
            <dl className="grid grid-cols-2 gap-3 sm:grid-cols-3">
              {[
                ["Account", `${run.account_name} (#${run.account_id})`],
                ["Result", intelligenceLabels[run.status]],
                ["Request model", run.model],
                ["Response model", run.response_model || "—"],
                ["Reasoning effort", run.reasoning_effort || "Default"],
                [
                  "Verdict method",
                  { answer: "Answer only", latency: "Latency only", both: "Answer and latency" }[
                    run.judge_mode
                  ],
                ],
                [
                  "Latency / threshold",
                  `${run.duration_ms ?? 0} / ${run.question.max_duration_ms} ms`,
                ],
                ["Config revision", String(run.config_revision)],
                ["Source", run.source === "manual" ? "Manual" : "Automatic"],
                [
                  "Input / output / reasoning tokens",
                  `${run.input_tokens} / ${run.output_tokens} / ${run.reasoning_tokens}`,
                ],
                [
                  "Start time",
                  run.started_at
                    ? new Date(run.started_at).toLocaleString()
                    : "Not started yet",
                ],
                ["Action", intelligenceLabels[run.action] || run.action || "None"],
              ].map(([label, value]) => (
                <div key={label}>
                  <dt className="text-xs text-muted-foreground">{label}</dt>
                  <dd className="mt-1 break-words">{value}</dd>
                </div>
              ))}
            </dl>
            <div>
              <h3 className="mb-1 font-medium">{run.question.name}</h3>
              <pre className="whitespace-pre-wrap break-words rounded-lg border bg-muted/30 p-3 font-sans">
                {run.question.prompt}
              </pre>
            </div>
            <p>
              Expected answer: <code>{run.question.expected_answer}</code> · Match method:
              {run.question.match_mode}
            </p>
            <div>
              <h3 className="mb-1 font-medium">Final answer</h3>
              <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-lg border bg-muted/30 p-3 font-sans">
                {run.output || "No answer yet"}
              </pre>
            </div>
            {run.action_message && (
              <p className="text-muted-foreground">{run.action_message}</p>
            )}
            {run.error && (
              <p
                role="alert"
                className="whitespace-pre-wrap break-words text-destructive"
              >
                {run.error}
              </p>
            )}
            {run.effective_context && (
              <details>
                <summary className="cursor-pointer text-muted-foreground">
                  Effective outbound configuration
                </summary>
                <pre className="mt-2 whitespace-pre-wrap break-words text-xs">
                  {JSON.stringify(run.effective_context, null, 2)}
                </pre>
              </details>
            )}
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
