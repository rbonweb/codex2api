import { cn } from "@/lib/utils";
import type { UpstreamChannel } from "@/types";

/**
 * Channel brand icon (Codex).
 *
 * Uses `@lobehub/icons-static-svg`, aligned with the Codex.Avatar color scheme of `@lobehub/icons`,
 * but does not pull in antd / @lobehub/ui peer deps.
 *
 * - Codex: `codex-color.svg` (white rounded background + purple-blue gradient mark, i.e. Codex.Avatar)
 */
const ICON_URLS = import.meta.glob(
  ["../../node_modules/@lobehub/icons-static-svg/icons/codex-color.svg"],
  { eager: true, query: "?url", import: "default" },
) as Record<string, string>;

const URL_BY_FILE = (() => {
  const map = new Map<string, string>();
  for (const [path, url] of Object.entries(ICON_URLS)) {
    const file = path.split("/").pop();
    if (file) map.set(file.replace(/\.svg$/, ""), url);
  }
  return map;
})();

export default function ChannelLogo({
  channel,
  size = 14,
  className,
  title,
}: {
  channel: UpstreamChannel;
  size?: number;
  className?: string;
  title?: string;
}) {
  if (channel !== "codex") return null;
  const src = URL_BY_FILE.get("codex-color");
  if (!src) return null;
  return (
    <img
      src={src}
      alt={title ?? "Codex"}
      title={title}
      width={size}
      height={size}
      draggable={false}
      className={cn("inline-block shrink-0 select-none rounded-[20%]", className)}
      style={{ width: size, height: size }}
    />
  );
}
