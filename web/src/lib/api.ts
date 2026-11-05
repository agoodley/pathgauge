// Thin fetch wrapper for the PathGauge REST API. Every state-changing
// request carries the X-PathGauge header (the server's CSRF guard).

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

type UnauthorizedHandler = () => void;
let onUnauthorized: UnauthorizedHandler | null = null;

/** Register the handler called when the session has expired (HTTP 401). */
export function setUnauthorizedHandler(h: UnauthorizedHandler | null) {
  onUnauthorized = h;
}

async function request<T>(method: string, path: string, body?: unknown, raw = false): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  let payload: BodyInit | undefined;
  if (method !== 'GET' && method !== 'HEAD') {
    headers['X-PathGauge'] = '1';
    headers['Content-Type'] = 'application/json';
    if (body !== undefined) payload = raw ? (body as BodyInit) : JSON.stringify(body);
  }
  let res: Response;
  try {
    res = await fetch(path, { method, headers, body: payload, credentials: 'same-origin' });
  } catch {
    throw new ApiError(0, 'Cannot reach the PathGauge server. Check your connection.');
  }
  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    if (res.status === 401 && !path.startsWith('/api/auth/login') && onUnauthorized) onUnauthorized();
    const msg =
      data && typeof data === 'object' && 'error' in data && typeof (data as { error: unknown }).error === 'string'
        ? (data as { error: string }).error
        : `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body ?? {}),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body ?? {}),
  del: <T>(path: string) => request<T>('DELETE', path),
  /** POST a pre-serialised body (e.g. an imported results file). */
  postRaw: <T>(path: string, body: string) => request<T>('POST', path, body, true),
};

export function errorMessage(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

/** URL of a run's HTML report. */
export function reportURL(runId: string, template: 'technical' | 'executive', download = false): string {
  return `/api/runs/${encodeURIComponent(runId)}/report.html?template=${template}${download ? '&download=1' : ''}`;
}
