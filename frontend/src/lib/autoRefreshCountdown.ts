// One step of the auto-refresh countdown: given a deadline, returns the remaining seconds to display, and how long until the next
// change in the displayed seconds (or the deadline itself). It computes from the deadline instead of decrementing,
// so the display stays accurate when timers are delayed or the page is throttled.
export function autoRefreshCountdownStep(
  deadline: number,
  now: number,
): { remaining: number; delay: number } {
  const left = deadline - now;
  if (left <= 0) return { remaining: 0, delay: 0 };
  const remaining = Math.ceil(left / 1000);
  return { remaining, delay: left - (remaining - 1) * 1000 };
}
