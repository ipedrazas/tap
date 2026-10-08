// Shared helper for the Frankfurter API (https://frankfurter.dev, ECB reference rates).
// api.frankfurter.app 301-redirects to api.frankfurter.dev/v1, so we call that directly.
export const BASE = "https://api.frankfurter.dev/v1";

export type Result = { ok: true; data: any } | { ok: false; error: Record<string, unknown> };

export async function get(url: URL): Promise<Result> {
  let res: Response;
  try {
    res = await fetch(url, { headers: { accept: "application/json" }, signal: AbortSignal.timeout(10_000) });
  } catch (e) {
    return { ok: false, error: { error: "upstream_unreachable", detail: String((e as Error).message ?? e) } };
  }
  let body: any = null;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  if (res.status === 404) {
    return { ok: false, error: { error: "unknown_currency", detail: "one of the currencies is not published by the ECB" } };
  }
  if (res.status === 422) {
    return { ok: false, error: { error: "invalid_request", detail: body?.message ?? "rejected by upstream" } };
  }
  if (!res.ok || body === null || typeof body !== "object") {
    return { ok: false, error: { error: "upstream", status: res.status } };
  }
  return { ok: true, data: body };
}

export function out(v: unknown): void {
  process.stdout.write(JSON.stringify(v));
}
