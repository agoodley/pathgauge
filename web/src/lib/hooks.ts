import { useCallback, useEffect, useRef, useState } from 'react';
import { api, errorMessage } from './api';

export interface Async<T> {
  data: T | undefined;
  error: string | null;
  loading: boolean;
  reload: () => void;
  setData: (v: T) => void;
}

/** Fetch a GET endpoint; refetches when the path changes. Pass null to skip. */
export function useApi<T>(path: string | null): Async<T> {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState<boolean>(path !== null);
  const seq = useRef(0);
  const pathRef = useRef(path);
  pathRef.current = path;

  const load = useCallback((quiet: boolean) => {
    const p = pathRef.current;
    if (p === null) return;
    const n = ++seq.current;
    if (!quiet) setLoading(true);
    api
      .get<T>(p)
      .then((d) => {
        if (n !== seq.current) return;
        setData(d);
        setError(null);
      })
      .catch((e) => {
        if (n !== seq.current) return;
        setError(errorMessage(e));
      })
      .finally(() => {
        if (n === seq.current) setLoading(false);
      });
  }, []);

  useEffect(() => {
    if (path === null) {
      setData(undefined);
      setLoading(false);
      return;
    }
    setData(undefined);
    setError(null);
    load(false);
  }, [path, load]);

  const reload = useCallback(() => load(true), [load]);
  return { data, error, loading, reload, setData };
}

/** Re-render every `ms` (for elapsed timers and "x min ago" labels). */
export function useTick(ms: number, enabled = true): number {
  const [n, setN] = useState(0);
  useEffect(() => {
    if (!enabled) return;
    const t = window.setInterval(() => setN((x) => x + 1), ms);
    return () => window.clearInterval(t);
  }, [ms, enabled]);
  return n;
}

/** Call fn every `ms` while enabled. */
export function useInterval(fn: () => void, ms: number, enabled = true) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    if (!enabled) return;
    const t = window.setInterval(() => ref.current(), ms);
    return () => window.clearInterval(t);
  }, [ms, enabled]);
}

export function useDocumentTitle(title: string) {
  useEffect(() => {
    document.title = title ? `${title} · PathGauge` : 'PathGauge';
  }, [title]);
}
