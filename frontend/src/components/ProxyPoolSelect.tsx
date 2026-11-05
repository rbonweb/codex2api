import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";
import { ChevronDown, MapPin, Check } from "lucide-react";

import type { ProxyRow } from "../api";
import { cn } from "@/lib/utils";

interface ProxyPoolSelectProps {
  proxies: ProxyRow[];
  onSelect: (url: string) => void;
  /** Currently selected proxy URL (controlled display); when it matches a proxy URL, that item is highlighted and echoed in the trigger. */
  value?: string;
  disabled?: boolean;
  className?: string;
}

const DROPDOWN_GAP = 6;
const DROPDOWN_MAX_HEIGHT = 256;
const VIEWPORT_PADDING = 8;

interface DropdownPosition {
  top: number;
  left: number;
  width: number;
  maxHeight: number;
  openUp: boolean;
}

// Radix Dialog's remove-scroll intercepts native wheel events on the portal menu, so scrollTop is driven manually.
function applyManualScroll(el: HTMLElement, deltaY: number): boolean {
  if (el.scrollHeight <= el.clientHeight + 1 || deltaY === 0) return false;
  const maxTop = el.scrollHeight - el.clientHeight;
  const next = Math.min(maxTop, Math.max(0, el.scrollTop + deltaY));
  if (next === el.scrollTop) return false;
  el.scrollTop = next;
  return true;
}

// ProxyPoolSelect is the dropdown in the account form for "pick one proxy from the proxy pool".
// Custom rendering: each item shows a badge with 📍location + idle (green) / bound N (amber); idle items are sorted to the top,
// after selection the trigger echoes the chosen proxy (label/url + location), so the user knows exactly which one was picked.
// The list is portaled to body with fixed positioning: this field often sits at the bottom of a dialog, and with absolute positioning
// it would be clipped by the dialog body's overflow and sit under the footer buttons; when there is not enough space it flips upward automatically.
export function ProxyPoolSelect({
  proxies,
  onSelect,
  value,
  disabled = false,
  className,
}: ProxyPoolSelectProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState<DropdownPosition | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);

  const computePosition = useCallback(() => {
    const anchor = rootRef.current;
    if (!anchor) return;
    const rect = anchor.getBoundingClientRect();
    const viewportHeight = window.innerHeight;
    const viewportWidth = window.innerWidth;
    const spaceBelow = viewportHeight - rect.bottom - DROPDOWN_GAP - VIEWPORT_PADDING;
    const spaceAbove = rect.top - DROPDOWN_GAP - VIEWPORT_PADDING;
    const preferUp = spaceBelow < Math.min(DROPDOWN_MAX_HEIGHT, 160) && spaceAbove > spaceBelow;
    const maxHeight = Math.max(120, Math.min(DROPDOWN_MAX_HEIGHT, preferUp ? spaceAbove : spaceBelow));
    const width = Math.min(Math.max(rect.width, 240), viewportWidth - VIEWPORT_PADDING * 2);
    const maxLeft = viewportWidth - width - VIEWPORT_PADDING;
    const left = Math.min(Math.max(VIEWPORT_PADDING, rect.left), Math.max(VIEWPORT_PADDING, maxLeft));
    setPosition({
      top: preferUp ? rect.top - DROPDOWN_GAP : rect.bottom + DROPDOWN_GAP,
      left,
      width,
      maxHeight,
      openUp: preferUp,
    });
  }, []);

  useLayoutEffect(() => {
    if (!open) {
      setPosition(null);
      return;
    }
    computePosition();
  }, [open, computePosition, proxies.length]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      const target = e.target as Node | null;
      if (!target) return;
      if (rootRef.current?.contains(target) || dropdownRef.current?.contains(target)) return;
      setOpen(false);
    };
    // Radix Dialog handles Escape in the capture phase; here we only close the dropdown,
    // and "pressing Esc should not also close the dialog" is enforced by DialogContent's onEscapeKeyDown, based on
    // whether data-select-dropdown is present.
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    const onReposition = () => computePosition();
    const onWheel = (e: WheelEvent) => {
      const list = dropdownRef.current;
      const target = e.target as Node | null;
      if (!list || !target || !list.contains(target)) return;
      if (applyManualScroll(list, e.deltaY)) e.preventDefault();
    };
    document.addEventListener("pointerdown", onDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("resize", onReposition);
    window.addEventListener("scroll", onReposition, true);
    document.addEventListener("wheel", onWheel, { passive: false, capture: true });
    return () => {
      document.removeEventListener("pointerdown", onDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("resize", onReposition);
      window.removeEventListener("scroll", onReposition, true);
      document.removeEventListener("wheel", onWheel, true);
    };
  }, [open, computePosition]);

  // Idle (bound_count=0) first; the rest in ascending bound count, so the least loaded come first.
  const sorted = useMemo(
    () => [...proxies].sort((a, b) => (a.bound_count ?? 0) - (b.bound_count ?? 0)),
    [proxies],
  );
  const selected = useMemo(
    () => (value ? proxies.find((p) => p.url === value.trim()) : undefined),
    [proxies, value],
  );

  if (proxies.length === 0) return null;

  const IdleBadge = () => (
    <span className="inline-flex shrink-0 items-center rounded-full bg-emerald-500/12 px-1.5 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400">
      {t("proxies.idle")}
    </span>
  );
  const BoundBadge = ({ count }: { count: number }) => (
    <span className="inline-flex shrink-0 items-center rounded-full bg-amber-500/12 px-1.5 py-0.5 text-[10px] font-medium text-amber-600 dark:text-amber-400">
      {t("proxies.boundCount", { count })}
    </span>
  );
  const LocationTag = ({ loc }: { loc: string }) => (
    <span className="inline-flex shrink-0 items-center gap-0.5 text-[10px] text-muted-foreground">
      <MapPin className="size-2.5" />
      {loc}
    </span>
  );

  const selectedLoc = selected?.test_location?.trim();

  const dropdown =
    open && position
      ? createPortal(
          <div
            ref={dropdownRef}
            data-select-dropdown="true"
            role="listbox"
            className="pointer-events-auto fixed z-[1000] overflow-y-auto overscroll-contain rounded-lg border border-border bg-popover p-1 shadow-lg [scrollbar-width:thin]"
            style={
              position.openUp
                ? {
                    left: position.left,
                    width: position.width,
                    bottom: window.innerHeight - position.top,
                    maxHeight: position.maxHeight,
                  }
                : {
                    left: position.left,
                    width: position.width,
                    top: position.top,
                    maxHeight: position.maxHeight,
                  }
            }
          >
            {sorted.map((proxy) => {
              const count = proxy.bound_count ?? 0;
              const loc = proxy.test_location?.trim();
              const name = proxy.label?.trim() || proxy.url;
              const active = selected?.url === proxy.url;
              return (
                <button
                  key={proxy.url}
                  type="button"
                  role="option"
                  aria-selected={active}
                  onClick={() => {
                    onSelect(proxy.url);
                    setOpen(false);
                  }}
                  className={cn(
                    "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left transition-colors hover:bg-muted",
                    active && "bg-primary/8",
                  )}
                >
                  <span className="flex w-4 shrink-0 justify-center">
                    {active ? <Check className="size-3.5 text-primary" /> : null}
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-1.5">
                      <span className="truncate text-[13px] font-medium text-foreground">{name}</span>
                      {count === 0 ? <IdleBadge /> : <BoundBadge count={count} />}
                    </span>
                    <span className="flex items-center gap-2 text-[11px] text-muted-foreground">
                      {loc ? <LocationTag loc={loc} /> : null}
                      <span className="truncate font-mono opacity-80">{proxy.url}</span>
                    </span>
                  </span>
                </button>
              );
            })}
          </div>,
          document.body,
        )
      : null;

  return (
    <div ref={rootRef} className={cn("relative", className)}>
      <button
        type="button"
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-haspopup="listbox"
        className={cn(
          "flex h-9 w-full items-center justify-between gap-2 rounded-md border border-input bg-background px-2.5 text-left text-sm outline-none transition-colors focus-visible:border-ring disabled:opacity-50",
          open && "border-ring",
        )}
      >
        <span className="flex min-w-0 items-center gap-1.5">
          {selected ? (
            <>
              <span className="truncate text-foreground">{selected.label?.trim() || selected.url}</span>
              {selectedLoc ? <LocationTag loc={selectedLoc} /> : null}
              {(selected.bound_count ?? 0) === 0 ? <IdleBadge /> : <BoundBadge count={selected.bound_count ?? 0} />}
            </>
          ) : (
            <span className="truncate text-muted-foreground">{t("proxies.selectFromPool")}</span>
          )}
        </span>
        <ChevronDown className={cn("size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-180")} />
      </button>
      {dropdown}
    </div>
  );
}
