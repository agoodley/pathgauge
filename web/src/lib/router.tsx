// A minimal history-API router: enough for a dashboard with a handful of
// routes, without another dependency. The Go server returns index.html for
// unknown non-API paths, so deep links work.
import { useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent } from 'react';

const listeners = new Set<() => void>();

function notify() {
  listeners.forEach((l) => l());
}

window.addEventListener('popstate', notify);

function subscribe(l: () => void) {
  listeners.add(l);
  return () => listeners.delete(l);
}

const snapshot = () => window.location.pathname + window.location.search;

export function navigate(to: string, opts: { replace?: boolean; keepScroll?: boolean } = {}) {
  if (to === snapshot()) return;
  if (opts.replace) window.history.replaceState(null, '', to);
  else window.history.pushState(null, '', to);
  notify();
  if (!opts.keepScroll) window.scrollTo(0, 0);
}

/** Current path and query string; re-renders on navigation. */
export function useLocation(): { pathname: string; search: URLSearchParams; href: string } {
  const href = useSyncExternalStore(subscribe, snapshot);
  const u = new URL(href, window.location.origin);
  return { pathname: u.pathname, search: u.searchParams, href };
}

/** Read and update query parameters (replacing history so Back stays useful). */
export function useQuery(): [URLSearchParams, (patch: Record<string, string | null>) => void] {
  const { search, pathname } = useLocation();
  const set = (patch: Record<string, string | null>) => {
    const q = new URLSearchParams(window.location.search);
    for (const [k, v] of Object.entries(patch)) {
      if (v === null || v === '') q.delete(k);
      else q.set(k, v);
    }
    const qs = q.toString();
    navigate(pathname + (qs ? '?' + qs : ''), { replace: true, keepScroll: true });
  };
  return [search, set];
}

/** Match "/runs/:id" style patterns; returns params or null. */
export function matchPath(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split('/').filter(Boolean);
  const s = path.split('/').filter(Boolean);
  if (p.length !== s.length) return null;
  const out: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    if (p[i].startsWith(':')) out[p[i].slice(1)] = decodeURIComponent(s[i]);
    else if (p[i] !== s[i]) return null;
  }
  return out;
}

type LinkProps = AnchorHTMLAttributes<HTMLAnchorElement> & { to: string };

export function Link({ to, onClick, ...rest }: LinkProps) {
  const handle = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e);
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || rest.target) return;
    e.preventDefault();
    navigate(to);
  };
  return <a href={to} onClick={handle} {...rest} />;
}
