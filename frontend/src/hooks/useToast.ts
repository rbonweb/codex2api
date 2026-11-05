import { useToastContext } from '../components/ToastProvider'

// Compatible with existing callers: `const { toast, showToast } = useToast()` is unchanged.
// The timeoutMs parameter is deprecated: the global ToastProvider uses one default duration;
// for a one-off custom duration, call `showToast(msg, type, ms)` directly.
export function useToast(_timeoutMs?: number) {
  void _timeoutMs
  return useToastContext()
}
