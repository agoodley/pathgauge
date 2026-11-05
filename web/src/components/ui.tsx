// Small shared UI pieces: cards, buttons, status badges (always icon +
// label, never colour alone), tables, dialogs, copy buttons and toasts.
import {
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type ButtonHTMLAttributes,
  type ReactNode,
} from 'react';
import type { StatusKind } from '../lib/domain';
import { scoreStatus } from '../lib/domain';

export function Card({
  title,
  actions,
  children,
  className = '',
  id,
}: {
  title?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  id?: string;
}) {
  return (
    <section className={`card ${className}`} id={id}>
      {(title || actions) && (
        <header className="card-head">
          {title && <h2>{title}</h2>}
          {actions && <div className="card-actions">{actions}</div>}
        </header>
      )}
      {children}
    </section>
  );
}

export function PageHeader({ title, sub, actions }: { title: ReactNode; sub?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="page-head">
      <div className="page-title">
        <h1>{title}</h1>
        {sub && <div className="page-sub">{sub}</div>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  );
}

type BtnProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: 'primary' | 'default' | 'danger' | 'ghost';
  small?: boolean;
  busy?: boolean;
};

export function Button({ variant = 'default', small, busy, className = '', children, disabled, type, ...rest }: BtnProps) {
  return (
    <button
      type={type ?? 'button'}
      className={`btn btn-${variant}${small ? ' btn-sm' : ''} ${className}`}
      disabled={disabled || busy}
      aria-busy={busy || undefined}
      {...rest}
    >
      {busy && <span className="spinner" aria-hidden="true" />}
      {children}
    </button>
  );
}

/** An anchor styled as a button (for report links and downloads). */
export function LinkButton({
  href,
  children,
  small,
  variant = 'default',
  download,
  newTab,
}: {
  href: string;
  children: ReactNode;
  small?: boolean;
  variant?: 'primary' | 'default';
  download?: boolean;
  newTab?: boolean;
}) {
  return (
    <a
      className={`btn btn-${variant}${small ? ' btn-sm' : ''}`}
      href={href}
      download={download ? '' : undefined}
      target={newTab ? '_blank' : undefined}
      rel={newTab ? 'noopener' : undefined}
    >
      {children}
    </a>
  );
}

export function Badge({ kind, icon, label, title }: { kind: StatusKind; icon?: string; label: ReactNode; title?: string }) {
  return (
    <span className={`badge badge-${kind}`} title={title}>
      {icon && (
        <span className="badge-icon" aria-hidden="true">
          {icon}
        </span>
      )}
      <span>{label}</span>
    </span>
  );
}

/** Status dot + label (for agent connectivity). */
export function StatusDot({ kind, label, icon }: { kind: StatusKind; label: string; icon?: string }) {
  return (
    <span className={`status status-${kind}`}>
      <span className="status-dot" aria-hidden="true">
        {icon ?? '●'}
      </span>
      {label}
    </span>
  );
}

export function ScoreBadge({ score, large }: { score: number | null | undefined; large?: boolean }) {
  const s = scoreStatus(score);
  if (score === null || score === undefined) return <span className="muted">—</span>;
  return (
    <span className={`score score-${s.kind}${large ? ' score-lg' : ''}`} title={`Health score ${score}: ${s.label}`}>
      <strong>{score}</strong>
      <span className="score-label">
        <span aria-hidden="true">{s.icon}</span> {s.label}
      </span>
    </span>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

export function ErrorBox({ error, onRetry }: { error: string | null | undefined; onRetry?: () => void }) {
  if (!error) return null;
  return (
    <div className="alert alert-critical" role="alert">
      <span aria-hidden="true">✖</span>
      <span>{error}</span>
      {onRetry && (
        <Button small variant="ghost" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}

export function Notice({ kind = 'info', children }: { kind?: StatusKind; children: ReactNode }) {
  const icon = { good: '✔', warning: '▲', serious: '◆', critical: '✖', info: 'ℹ', neutral: '•' }[kind];
  return (
    <div className={`alert alert-${kind}`}>
      <span aria-hidden="true">{icon}</span>
      <div>{children}</div>
    </div>
  );
}

export function Loading({ label = 'Loading…' }: { label?: string }) {
  return (
    <div className="loading" role="status">
      <span className="spinner" aria-hidden="true" />
      {label}
    </div>
  );
}

/** Key/value table. */
export function KV({ rows }: { rows: [ReactNode, ReactNode][] }) {
  return (
    <table className="kv">
      <tbody>
        {rows.map(([k, v], i) => (
          <tr key={i}>
            <th scope="row">{k}</th>
            <td>{v}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** Simple data table: head labels and rows of cells. */
export function DataTable({
  head,
  rows,
  numeric,
  caption,
}: {
  head: string[];
  rows: ReactNode[][];
  /** Column indexes to right-align. */
  numeric?: number[];
  caption?: string;
}) {
  const num = new Set(numeric ?? []);
  return (
    <div className="table-wrap">
      <table className="data">
        {caption && <caption>{caption}</caption>}
        <thead>
          <tr>
            {head.map((h, i) => (
              <th key={i} className={num.has(i) ? 'num' : undefined} scope="col">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              {r.map((c, j) => (
                <td key={j} className={num.has(j) ? 'num' : undefined}>
                  {c}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // Clipboard API needs a secure context and permission; fall back.
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      try {
        document.execCommand('copy');
      } finally {
        ta.remove();
      }
    }
    setDone(true);
    window.setTimeout(() => setDone(false), 1600);
  };
  return (
    <Button small onClick={copy} aria-live="polite">
      {done ? '✔ Copied' : label}
    </Button>
  );
}

export function CodeLine({ text }: { text: string }) {
  return (
    <div className="codeline">
      <code>{text}</code>
      <CopyButton text={text} />
    </div>
  );
}

/** A button that asks for confirmation inline (no blocking browser dialogs). */
export function ConfirmButton({
  label,
  confirmLabel = 'Confirm',
  onConfirm,
  small = true,
  variant = 'danger',
  prompt,
  disabled,
}: {
  label: string;
  confirmLabel?: string;
  onConfirm: () => Promise<unknown> | void;
  small?: boolean;
  variant?: 'danger' | 'default';
  prompt?: string;
  disabled?: boolean;
}) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  if (!asking)
    return (
      <Button small={small} variant={variant === 'danger' ? 'ghost' : 'default'} className={variant === 'danger' ? 'btn-danger-text' : ''} onClick={() => setAsking(true)} disabled={disabled}>
        {label}
      </Button>
    );
  return (
    <span className="confirm">
      {prompt && <span className="confirm-prompt">{prompt}</span>}
      <Button
        small={small}
        variant="danger"
        busy={busy}
        onClick={async () => {
          setBusy(true);
          try {
            await onConfirm();
          } finally {
            setBusy(false);
            setAsking(false);
          }
        }}
      >
        {confirmLabel}
      </Button>
      <Button small={small} variant="ghost" onClick={() => setAsking(false)} disabled={busy}>
        Cancel
      </Button>
    </span>
  );
}

export function Modal({
  title,
  open,
  onClose,
  children,
  footer,
  wide,
}: {
  title: string;
  open: boolean;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal();
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      className={`modal${wide ? ' modal-wide' : ''}`}
      onClose={onClose}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      aria-label={title}
    >
      {open && (
        <>
          <header className="modal-head">
            <h2>{title}</h2>
            <button type="button" className="icon-btn" onClick={onClose} aria-label="Close">
              ✕
            </button>
          </header>
          <div className="modal-body">{children}</div>
          {footer && <footer className="modal-foot">{footer}</footer>}
        </>
      )}
    </dialog>
  );
}

export function Field({ label, hint, children, wide }: { label: string; hint?: ReactNode; children: ReactNode; wide?: boolean }) {
  return (
    <label className={`field${wide ? ' field-wide' : ''}`}>
      <span className="field-label">{label}</span>
      {children}
      {hint && <span className="field-hint">{hint}</span>}
    </label>
  );
}

export function Tabs<T extends string>({ tabs, value, onChange }: { tabs: { id: T; label: string }[]; value: T; onChange: (v: T) => void }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map((t) => (
        <button
          key={t.id}
          type="button"
          role="tab"
          aria-selected={t.id === value}
          className={`tab${t.id === value ? ' tab-active' : ''}`}
          onClick={() => onChange(t.id)}
        >
          {t.label}
        </button>
      ))}
    </div>
  );
}

export function Segmented<T extends string>({
  options,
  value,
  onChange,
  label,
}: {
  options: { id: T; label: string }[];
  value: T;
  onChange: (v: T) => void;
  label: string;
}) {
  return (
    <div className="segmented" role="radiogroup" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.id}
          type="button"
          role="radio"
          aria-checked={o.id === value}
          className={o.id === value ? 'on' : ''}
          onClick={() => onChange(o.id)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

/** Stat tile for headline numbers. */
export function Tile({ label, value, sub, status }: { label: string; value: ReactNode; sub?: ReactNode; status?: ReactNode }) {
  return (
    <div className="tile">
      <span className="tile-label">{label}</span>
      <strong className="tile-value">{value}</strong>
      {status && <span className="tile-status">{status}</span>}
      {sub && <span className="tile-sub">{sub}</span>}
    </div>
  );
}

// ---- toasts ----

interface Toast {
  id: number;
  kind: 'good' | 'critical' | 'info';
  text: string;
}
let toasts: Toast[] = [];
let toastSeq = 0;
const toastListeners = new Set<() => void>();
function emitToasts() {
  toastListeners.forEach((l) => l());
}

export function toast(text: string, kind: Toast['kind'] = 'good') {
  const t = { id: ++toastSeq, kind, text };
  toasts = [...toasts, t];
  emitToasts();
  window.setTimeout(
    () => {
      toasts = toasts.filter((x) => x.id !== t.id);
      emitToasts();
    },
    kind === 'critical' ? 7000 : 3500,
  );
}

export function Toasts() {
  const list = useSyncExternalStore(
    (l) => {
      toastListeners.add(l);
      return () => {
        toastListeners.delete(l);
      };
    },
    () => toasts,
  );
  return (
    <div className="toasts" aria-live="polite">
      {list.map((t) => (
        <div key={t.id} className={`toast toast-${t.kind}`} role={t.kind === 'critical' ? 'alert' : 'status'}>
          <span aria-hidden="true">{t.kind === 'critical' ? '✖' : t.kind === 'good' ? '✔' : 'ℹ'}</span>
          {t.text}
        </div>
      ))}
    </div>
  );
}
