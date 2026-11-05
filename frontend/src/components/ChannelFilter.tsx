import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import ChannelLogo from "./ChannelLogo";
import { cn } from "@/lib/utils";
import { useVisibleChannels } from "../visibleChannels";

// 仪表盘/用量页共用的上游渠道过滤（全部/Codex）。
// 选择持久化到 localStorage，两页共享同一份状态键。
// 取值只会是 "" 或 "codex"；其余字面量暂留在类型里，等页面不再比较它们后收窄。
export type UsageChannel = "" | "codex" | "grok" | "antigravity" | "claude";

const USAGE_CHANNEL_KEY = "codex2api:usage:channel";

export function useUsageChannel(): [UsageChannel, (next: UsageChannel) => void] {
  const [channel, setChannel] = useState<UsageChannel>(() => {
    try {
      const raw = window.localStorage.getItem(USAGE_CHANNEL_KEY);
      if (raw === "codex") return raw;
    } catch {
      // ignore
    }
    return "";
  });
  useEffect(() => {
    try {
      window.localStorage.setItem(USAGE_CHANNEL_KEY, channel);
    } catch {
      // ignore
    }
  }, [channel]);
  return [channel, setChannel];
}

export default function ChannelFilter({
  value,
  onChange,
  className,
}: {
  value: UsageChannel;
  onChange: (next: UsageChannel) => void;
  className?: string;
}) {
  const { t } = useTranslation();
  const { isChannelVisible } = useVisibleChannels();
  const allOptions: Array<{
    key: UsageChannel;
    label: string;
    logo?: "codex";
  }> = [
    { key: "", label: t("usage.channelAll") },
    { key: "codex", label: "Codex", logo: "codex" },
  ];
  const options = allOptions.filter((o) => !o.logo || isChannelVisible(o.logo));
  const count = options.length;
  // 当前选中的渠道被设置页隐藏后回到「全部」，否则过滤条件停在一个看不见的选项上。
  const hiddenSelection = value !== "" && !options.some((o) => o.key === value);
  // 只剩 Codex 一个渠道时过滤器没有意义：不渲染，并清掉残留的过滤值。
  const singleChannel = options.filter((o) => o.logo).length < 2;
  useEffect(() => {
    if (hiddenSelection || (singleChannel && value !== "")) onChange("");
  }, [hiddenSelection, singleChannel, value, onChange]);
  if (singleChannel) return null;
  const activeIndex = Math.max(
    0,
    options.findIndex((o) => o.key === value),
  );
  return (
    <div
      className={cn(
        "relative grid items-center rounded-lg border border-border bg-muted/40 p-0.5",
        className,
      )}
      style={{ gridTemplateColumns: `repeat(${count}, minmax(0, 1fr))` }}
    >
      {/* 滑块指示器：按可见渠道数等分，translateX 过渡到选中项 */}
      <span
        aria-hidden
        className="absolute inset-y-0.5 left-0.5 rounded-md bg-background shadow-sm transition-transform duration-300 ease-out"
        style={{ width: `calc((100% - 4px) / ${count})`, transform: `translateX(${activeIndex * 100}%)` }}
      />
      {options.map(({ key, label, logo }) => (
        <button
          key={key || "all"}
          type="button"
          onClick={() => onChange(key)}
          aria-pressed={value === key}
          title={label}
          className={cn(
            "relative z-10 inline-flex items-center justify-center gap-1.5 rounded-md px-3.5 py-1.5 text-sm font-semibold transition-colors duration-200",
            value === key
              ? "text-foreground"
              : "text-muted-foreground opacity-75 grayscale hover:opacity-100 hover:grayscale-0 hover:text-foreground",
          )}
        >
          {logo ? <ChannelLogo channel={logo} size={16} /> : null}
          {label}
        </button>
      ))}
    </div>
  );
}
