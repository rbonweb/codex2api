import { useEffect, useRef, useState } from "react";
import { Check, ChevronDown, RefreshCw } from "lucide-react";
import { DropdownMenu } from "radix-ui";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { autoRefreshCountdownStep } from "@/lib/autoRefreshCountdown";
import { cn } from "@/lib/utils";

// Split refresh button: manual refresh on the left, a dropdown on the right to pick the auto-refresh interval (0 = off).
// The timer and the "refreshing" state both stay inside the component, so the host page does not re-render extra on every round.
// Auto-refresh only schedules the next round after the previous one completes, and is skipped while the page is in the background.
export function AutoRefreshButton({
  onRefresh,
  onAutoRefresh,
  intervals,
  loadSeconds,
  saveSeconds,
  label,
}: {
  onRefresh: () => void;
  onAutoRefresh: () => Promise<unknown>;
  // Optional intervals (seconds); should include 0 as "off".
  intervals: readonly number[];
  loadSeconds?: () => number;
  saveSeconds?: (seconds: number) => void;
  label?: string;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [seconds, setSeconds] = useState(() => {
    const initial = loadSeconds?.() ?? 0;
    return intervals.includes(initial) ? initial : 0;
  });
  const [running, setRunning] = useState(false);
  // Seconds remaining until the next auto-refresh, counting down each second; only re-renders this button.
  const [remaining, setRemaining] = useState(seconds);
  const onAutoRefreshRef = useRef(onAutoRefresh);
  useEffect(() => {
    onAutoRefreshRef.current = onAutoRefresh;
  }, [onAutoRefresh]);
  // After a manual refresh, the countdown restarts from the full interval; if an auto-refresh is in progress, it is rescheduled when that finishes.
  const restartCountdownRef = useRef<(() => void) | null>(null);

  useEffect(() => {
    if (seconds <= 0) return undefined;
    let stopped = false;
    let refreshing = false;
    let timer: number | undefined;
    let deadline = 0;
    const wait = () => {
      const step = autoRefreshCountdownStep(deadline, Date.now());
      setRemaining(step.remaining);
      timer = window.setTimeout(tick, step.delay);
    };
    const restart = () => {
      if (timer !== undefined) window.clearTimeout(timer);
      deadline = Date.now() + seconds * 1000;
      wait();
    };
    const tick = async () => {
      if (stopped) return;
      if (Date.now() < deadline) {
        wait();
        return;
      }
      // Do not refresh while the page is in the background; start the next countdown directly.
      if (document.hidden) {
        restart();
        return;
      }
      refreshing = true;
      setRunning(true);
      try {
        await onAutoRefreshRef.current();
      } catch {
        // An auto-refresh failure does not bother the user; the next round continues.
      } finally {
        refreshing = false;
        if (!stopped) setRunning(false);
      }
      if (!stopped) restart();
    };
    restartCountdownRef.current = () => {
      if (!stopped && !refreshing) restart();
    };
    restart();
    return () => {
      stopped = true;
      restartCountdownRef.current = null;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [seconds]);

  const resolvedLabel = label ?? t("common.refresh");
  const autoLabel =
    seconds > 0
      ? running
        ? t("common.autoRefreshRunning")
        : t("common.autoRefreshCountdown", { seconds: remaining })
      : t("common.autoRefreshOff");

  return (
    <div className="inline-flex shrink-0 items-center">
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => {
          onRefresh();
          restartCountdownRef.current?.();
        }}
        className="rounded-r-none"
        title={seconds > 0 ? `${resolvedLabel} · ${autoLabel}` : resolvedLabel}
      >
        <RefreshCw className={cn("size-3.5", running && "animate-spin")} />
        <span className="max-[380px]:hidden">{resolvedLabel}</span>
        {seconds > 0 ? (
          // Fixed minimum width, so the button does not jump when the seconds go from two digits to one.
          <span
            aria-hidden="true"
            className="min-w-[2.25rem] rounded bg-primary/10 px-1 text-center text-[11px] font-semibold tabular-nums text-primary"
          >
            {running ? "···" : `${remaining}s`}
          </span>
        ) : null}
      </Button>
      <DropdownMenu.Root open={open} onOpenChange={setOpen} modal={false}>
        <DropdownMenu.Trigger asChild>
          <Button
            type="button"
            variant="outline"
            size="sm"
            aria-label={t("common.autoRefresh")}
            title={t("common.autoRefresh")}
            className={cn(
              "-ml-px rounded-l-none px-1.5",
              open && "border-primary/25 bg-accent text-accent-foreground",
            )}
          >
            <ChevronDown
              className={cn(
                "size-3.5 text-muted-foreground transition-transform",
                open && "rotate-180",
              )}
            />
          </Button>
        </DropdownMenu.Trigger>
        <DropdownMenu.Portal>
          <DropdownMenu.Content
            data-slot="action-menu-popover"
            align="end"
            sideOffset={8}
            collisionPadding={12}
            className="action-menu-surface z-[200] w-44 rounded-xl border border-border/80 bg-popover p-1.5 text-popover-foreground shadow-[0_12px_40px_-12px_hsl(222_30%_12%/0.3)] outline-none"
          >
            <DropdownMenu.Label className="px-2.5 pb-1 pt-1.5 text-[10px] font-semibold tracking-wide text-muted-foreground">
              {t("common.autoRefresh")}
            </DropdownMenu.Label>
            <DropdownMenu.RadioGroup
              value={String(seconds)}
              onValueChange={(value) => {
                const next = Number(value);
                if (!intervals.includes(next)) return;
                setSeconds(next);
                saveSeconds?.(next);
              }}
            >
              {intervals.map((value) => (
                <DropdownMenu.RadioItem
                  key={value}
                  value={String(value)}
                  className="flex min-h-8 cursor-default select-none items-center gap-2 rounded-lg px-2.5 py-1.5 text-[13px] text-foreground outline-none transition-colors data-[highlighted]:bg-accent"
                >
                  <span className="flex size-4 shrink-0 items-center justify-center">
                    <DropdownMenu.ItemIndicator>
                      <Check className="size-3.5 text-primary" />
                    </DropdownMenu.ItemIndicator>
                  </span>
                  <span className="min-w-0 flex-1">
                    {value > 0
                      ? t("common.autoRefreshEvery", { seconds: value })
                      : t("common.autoRefreshOff")}
                  </span>
                </DropdownMenu.RadioItem>
              ))}
            </DropdownMenu.RadioGroup>
          </DropdownMenu.Content>
        </DropdownMenu.Portal>
      </DropdownMenu.Root>
    </div>
  );
}
