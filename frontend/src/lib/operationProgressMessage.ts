export interface OperationProgressMessageEvent {
  type: "start" | "progress" | "complete";
  action?: string;
  error?: string;
  message?: string;
  account_email?: string;
  account_name?: string;
}

const CLEAN_DONE_MESSAGE = /Cleaned up \d+ accounts?|accounts? cleared/i;

function currentAccountLabel(event: OperationProgressMessageEvent): string | undefined {
  return event.account_email?.trim() || event.account_name?.trim() || undefined;
}

// While cleanup is running, only the current account or the failure reason is shown; the summary appears after the whole batch finishes.
export function operationProgressMessage(
  event: OperationProgressMessageEvent,
  previous?: string,
): string | undefined {
  if (event.type === "complete") {
    return event.error || event.message || previous;
  }
  if (event.action === "clean") {
    return event.error || currentAccountLabel(event);
  }
  const next = event.error || event.message;
  if (next && CLEAN_DONE_MESSAGE.test(next)) {
    return currentAccountLabel(event) || previous;
  }
  return next || previous;
}
