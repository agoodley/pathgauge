// The dashboard's single WebSocket to /api/live. It reconnects with
// exponential backoff, fans messages out to subscribers, and buffers run
// events per run so a live run page opened right after starting a run sees
// the events that arrived before it mounted.
import { useEffect, useRef, useSyncExternalStore } from 'react';
import type { EngineEvent, LiveMessage } from './types';

export type LiveStatus = 'idle' | 'connecting' | 'open' | 'closed';
type Listener = (m: LiveMessage) => void;

const MAX_EVENTS_PER_RUN = 20000;

/** Server-assigned sequence number of each buffered run event. */
const seqOf = new WeakMap<EngineEvent, number>();

class LiveClient {
  private ws: WebSocket | null = null;
  private status: LiveStatus = 'idle';
  private listeners = new Set<Listener>();
  private statusListeners = new Set<() => void>();
  private runBuffers = new Map<string, { events: EngineEvent[]; version: number; touched: number }>();
  private runListeners = new Set<() => void>();
  private backoff = 1000;
  private timer: number | undefined;
  private wanted = false;
  /** Increments on every (re)connect so pages can refetch missed state. */
  connects = 0;

  start() {
    this.wanted = true;
    if (!this.ws) this.connect();
  }

  stop() {
    this.wanted = false;
    window.clearTimeout(this.timer);
    this.ws?.close();
    this.ws = null;
    this.setStatus('idle');
  }

  getStatus = () => this.status;

  private setStatus(s: LiveStatus) {
    this.status = s;
    this.statusListeners.forEach((l) => l());
  }

  private connect() {
    window.clearTimeout(this.timer);
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.setStatus('connecting');
    let ws: WebSocket;
    try {
      ws = new WebSocket(`${proto}//${window.location.host}/api/live`);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;
    ws.onopen = () => {
      this.backoff = 1000;
      this.connects++;
      this.setStatus('open');
    };
    ws.onmessage = (ev) => {
      let m: LiveMessage;
      try {
        m = JSON.parse(String(ev.data)) as LiveMessage;
      } catch {
        return;
      }
      if (m.type === 'run_event' && m.event) this.bufferEvent(m.run_id, m.event, m.seq);
      this.listeners.forEach((l) => {
        try {
          l(m);
        } catch (e) {
          console.warn('live listener failed', e);
        }
      });
    };
    ws.onclose = () => {
      if (this.ws === ws) this.ws = null;
      if (this.wanted) {
        this.setStatus('closed');
        this.scheduleReconnect();
      }
    };
    ws.onerror = () => {
      /* onclose follows */
    };
  }

  private scheduleReconnect() {
    if (!this.wanted) return;
    const delay = this.backoff + Math.random() * 500;
    this.backoff = Math.min(this.backoff * 2, 30000);
    this.timer = window.setTimeout(() => this.connect(), delay);
  }

  private bufferEvent(runId: string, e: EngineEvent, seq?: number) {
    let b = this.runBuffers.get(runId);
    if (!b) {
      b = { events: [], version: 0, touched: Date.now() };
      this.runBuffers.set(runId, b);
      this.prune();
    }
    if (seq !== undefined) seqOf.set(e, seq);
    if (b.events.length < MAX_EVENTS_PER_RUN || e.type !== 'progress') b.events.push(e);
    b.version++;
    b.touched = Date.now();
    this.runListeners.forEach((l) => l());
  }

  private prune() {
    const cutoff = Date.now() - 3 * 3600 * 1000;
    for (const [id, b] of this.runBuffers) if (b.touched < cutoff) this.runBuffers.delete(id);
  }

  /**
   * Merge a run's event history from the server (GET /api/runs/{id}/events)
   * with events already received live, ordered by sequence number, so a
   * page opened mid-run shows the earlier steps too.
   */
  seed(runId: string, history: { seq: number; event: EngineEvent }[]) {
    if (!history.length) return;
    let b = this.runBuffers.get(runId);
    if (!b) {
      b = { events: [], version: 0, touched: Date.now() };
      this.runBuffers.set(runId, b);
    }
    const have = new Set<number>();
    for (const e of b.events) {
      const s = seqOf.get(e);
      if (s !== undefined) have.add(s);
    }
    const merged = [...b.events];
    for (const h of history) {
      if (have.has(h.seq)) continue;
      seqOf.set(h.event, h.seq);
      merged.push(h.event);
    }
    merged.sort((a, c) => (seqOf.get(a) ?? Number.MAX_SAFE_INTEGER) - (seqOf.get(c) ?? Number.MAX_SAFE_INTEGER));
    b.events = merged;
    b.version++;
    b.touched = Date.now();
    this.runListeners.forEach((l) => l());
  }

  runEvents(runId: string): EngineEvent[] {
    return this.runBuffers.get(runId)?.events ?? [];
  }

  runVersion(runId: string): number {
    return this.runBuffers.get(runId)?.version ?? 0;
  }

  subscribe(l: Listener) {
    this.listeners.add(l);
    return () => {
      this.listeners.delete(l);
    };
  }

  subscribeStatus = (l: () => void) => {
    this.statusListeners.add(l);
    return () => {
      this.statusListeners.delete(l);
    };
  };

  subscribeRuns = (l: () => void) => {
    this.runListeners.add(l);
    return () => {
      this.runListeners.delete(l);
    };
  };
}

export const live = new LiveClient();

/** Call handler for every live message (latest handler wins; no resubscribe churn). */
export function useLive(handler: Listener) {
  const ref = useRef(handler);
  ref.current = handler;
  useEffect(() => live.subscribe((m) => ref.current(m)), []);
}

export function useLiveStatus(): LiveStatus {
  return useSyncExternalStore(live.subscribeStatus, live.getStatus);
}

/**
 * Buffered engine events for one run; re-renders as new events arrive. The
 * array grows in place, so memoise derived values on the version number.
 */
export function useRunEvents(runId: string): [EngineEvent[], number] {
  const version = useSyncExternalStore(live.subscribeRuns, () => live.runVersion(runId));
  return [live.runEvents(runId), version];
}

/**
 * Run `fn` (debounced) whenever a live message matches `filter`, and after a
 * reconnect (messages may have been missed while disconnected).
 */
export function useLiveRefresh(filter: (m: LiveMessage) => boolean, fn: () => void, delay = 800) {
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const filterRef = useRef(filter);
  filterRef.current = filter;
  const status = useLiveStatus();
  const seen = useRef(live.connects);
  useEffect(() => {
    let t: number | undefined;
    const unsub = live.subscribe((m) => {
      if (!filterRef.current(m)) return;
      window.clearTimeout(t);
      t = window.setTimeout(() => fnRef.current(), delay);
    });
    return () => {
      unsub();
      window.clearTimeout(t);
    };
  }, [delay]);
  useEffect(() => {
    if (status === 'open' && live.connects > 1 && live.connects !== seen.current) {
      seen.current = live.connects;
      fnRef.current();
    }
  }, [status]);
}
