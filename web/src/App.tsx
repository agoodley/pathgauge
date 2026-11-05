import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { api, ApiError, errorMessage, setUnauthorizedHandler } from './lib/api';
import { AuthContext, useAuth, useCan } from './lib/auth';
import { live, useLiveStatus } from './lib/live';
import { Link, matchPath, navigate, useLocation } from './lib/router';
import { setThemePref, useTheme, type ThemePref } from './lib/theme';
import type { Me } from './lib/types';
import { Icon } from './components/Icon';
import { Button, Loading, Toasts } from './components/ui';
import { Login } from './pages/Login';
import { Overview } from './pages/Overview';
import { AgentsPage } from './pages/Agents';
import { AgentDetail } from './pages/AgentDetail';
import { NewRun } from './pages/NewRun';
import { RunPage } from './pages/RunPage';
import { Compare } from './pages/Compare';
import { Monitoring } from './pages/Monitoring';
import { Schedules } from './pages/Schedules';
import { Alerts } from './pages/Alerts';
import { Reports } from './pages/Reports';
import { Admin } from './pages/Admin';

const HINT = 'pathgauge-signed-in';
function hasSessionHint(): boolean {
  return localStorage.getItem(HINT) === '1';
}
function setSessionHint(on: boolean) {
  if (on) localStorage.setItem(HINT, '1');
  else localStorage.removeItem(HINT);
}

type Boot = { phase: 'loading' } | { phase: 'setup' } | { phase: 'login' } | { phase: 'ready'; me: Me } | { phase: 'error'; error: string };

export function App() {
  const [boot, setBoot] = useState<Boot>({ phase: 'loading' });

  const start = useCallback(async () => {
    try {
      const s = await api.get<{ needs_setup: boolean }>('/api/setup');
      if (s.needs_setup) {
        setBoot({ phase: 'setup' });
        return;
      }
      // The session cookie is HttpOnly; a local hint avoids a pointless 401
      // round trip (and a console error) when we know we are signed out.
      if (!hasSessionHint()) {
        setBoot({ phase: 'login' });
        return;
      }
      const me = await api.get<Me>('/api/auth/me');
      setBoot({ phase: 'ready', me });
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        setSessionHint(false);
        setBoot({ phase: 'login' });
      } else setBoot({ phase: 'error', error: errorMessage(e) });
    }
  }, []);

  useEffect(() => {
    void start();
    setUnauthorizedHandler(() => {
      setSessionHint(false);
      setBoot({ phase: 'login' });
    });
    return () => setUnauthorizedHandler(null);
  }, [start]);

  useEffect(() => {
    if (boot.phase === 'ready') live.start();
    else live.stop();
  }, [boot.phase]);

  const logout = useCallback(async () => {
    try {
      await api.post('/api/auth/logout');
    } catch {
      /* the session is gone either way */
    }
    live.stop();
    setSessionHint(false);
    setBoot({ phase: 'login' });
  }, []);

  switch (boot.phase) {
    case 'loading':
      return (
        <div className="center-screen">
          <Loading />
        </div>
      );
    case 'error':
      return (
        <div className="center-screen">
          <div className="auth-card">
            <h1>PathGauge</h1>
            <p className="alert alert-critical">
              <span aria-hidden="true">✖</span> {boot.error}
            </p>
            <Button variant="primary" onClick={() => void start()}>
              Try again
            </Button>
          </div>
        </div>
      );
    case 'setup':
    case 'login':
      return (
        <>
          <Login
            setup={boot.phase === 'setup'}
            onDone={(me) => {
              setSessionHint(true);
              setBoot({ phase: 'ready', me });
            }}
          />
          <Toasts />
        </>
      );
    case 'ready':
      return (
        <AuthContext.Provider value={{ me: boot.me, logout }}>
          <Shell />
          <Toasts />
        </AuthContext.Provider>
      );
  }
}

interface NavItem {
  to: string;
  label: string;
  icon: string;
  match: (p: string) => boolean;
  role?: 'operator' | 'admin';
}

const NAV: NavItem[] = [
  { to: '/', label: 'Overview', icon: 'overview', match: (p) => p === '/' },
  { to: '/agents', label: 'Agents', icon: 'agents', match: (p) => p.startsWith('/agents') },
  { to: '/runs/new', label: 'New run', icon: 'run', match: (p) => p === '/runs/new', role: 'operator' },
  { to: '/monitoring', label: 'Monitoring', icon: 'monitoring', match: (p) => p.startsWith('/monitoring') },
  { to: '/reports', label: 'Runs & reports', icon: 'reports', match: (p) => p.startsWith('/reports') || (p.startsWith('/runs/') && p !== '/runs/new') },
  { to: '/compare', label: 'Compare', icon: 'compare', match: (p) => p.startsWith('/compare') },
  { to: '/schedules', label: 'Schedules', icon: 'schedules', match: (p) => p.startsWith('/schedules') },
  { to: '/alerts', label: 'Alerts', icon: 'alerts', match: (p) => p.startsWith('/alerts') },
  { to: '/admin/users', label: 'Admin', icon: 'admin', match: (p) => p.startsWith('/admin'), role: 'admin' },
];

function Shell() {
  const { pathname } = useLocation();
  const { me, logout } = useAuth();
  const isOperator = useCan('operator');
  const isAdmin = useCan('admin');
  const [navOpen, setNavOpen] = useState(false);

  useEffect(() => setNavOpen(false), [pathname]);

  const items = NAV.filter((n) => !n.role || (n.role === 'operator' ? isOperator : isAdmin));

  return (
    <div className={`shell${navOpen ? ' nav-open' : ''}`}>
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <aside className="sidebar" aria-label="Main navigation">
        <div className="brand">
          <Link to="/" className="brand-link">
            <img src="/favicon.svg" alt="" width={26} height={26} />
            <span>PathGauge</span>
          </Link>
          <button type="button" className="icon-btn nav-toggle" aria-label={navOpen ? 'Close menu' : 'Open menu'} aria-expanded={navOpen} onClick={() => setNavOpen((v) => !v)}>
            <Icon name="menu" />
          </button>
        </div>
        <nav className="nav">
          {items.map((n) => (
            <Link key={n.to} to={n.to} className={`nav-item${n.match(pathname) ? ' active' : ''}`} aria-current={n.match(pathname) ? 'page' : undefined}>
              <Icon name={n.icon} />
              <span>{n.label}</span>
            </Link>
          ))}
        </nav>
        <div className="sidebar-foot">
          <LiveIndicator />
          <ThemeSwitch />
          <div className="user">
            <div className="user-name" title={me.name}>
              {me.name}
            </div>
            <div className="user-role">{me.role}</div>
          </div>
          <Button small variant="ghost" onClick={() => void logout()} className="signout">
            <Icon name="logout" size={16} /> Sign out
          </Button>
        </div>
      </aside>
      <main id="main" className="main" tabIndex={-1}>
        <Routes pathname={pathname} />
      </main>
    </div>
  );
}

function LiveIndicator() {
  const s = useLiveStatus();
  const map = {
    open: { cls: 'good', icon: '●', label: 'Live updates on' },
    connecting: { cls: 'neutral', icon: '◌', label: 'Connecting…' },
    closed: { cls: 'warning', icon: '▲', label: 'Live updates reconnecting…' },
    idle: { cls: 'neutral', icon: '○', label: 'Live updates off' },
  }[s];
  return (
    <div className={`live live-${map.cls}`} role="status">
      <span aria-hidden="true">{map.icon}</span> {map.label}
    </div>
  );
}

function ThemeSwitch() {
  const { pref } = useTheme();
  const opts: { id: ThemePref; label: string; icon: string }[] = [
    { id: 'auto', label: 'Auto', icon: 'auto' },
    { id: 'light', label: 'Light', icon: 'sun' },
    { id: 'dark', label: 'Dark', icon: 'moon' },
  ];
  return (
    <div className="theme-switch" role="radiogroup" aria-label="Colour theme">
      {opts.map((o) => (
        <button key={o.id} type="button" role="radio" aria-checked={pref === o.id} className={pref === o.id ? 'on' : ''} onClick={() => setThemePref(o.id)} title={`${o.label} theme`}>
          <Icon name={o.icon} size={15} />
          <span>{o.label}</span>
        </button>
      ))}
    </div>
  );
}

function Routes({ pathname }: { pathname: string }) {
  let m: Record<string, string> | null;
  let page: ReactNode;
  if (pathname === '/' || pathname === '') page = <Overview />;
  else if (pathname === '/agents') page = <AgentsPage />;
  else if ((m = matchPath('/agents/:id', pathname))) page = <AgentDetail key={m.id} id={m.id} />;
  else if (pathname === '/runs/new') page = <NewRun />;
  else if ((m = matchPath('/runs/:id', pathname))) page = <RunPage key={m.id} id={m.id} />;
  else if (pathname === '/compare') page = <Compare />;
  else if (pathname === '/monitoring') page = <Monitoring />;
  else if (pathname === '/schedules') page = <Schedules />;
  else if (pathname === '/alerts') page = <Alerts />;
  else if (pathname === '/reports' || pathname === '/runs') page = <Reports />;
  else if (pathname === '/admin') page = <Redirect to="/admin/users" />;
  else if ((m = matchPath('/admin/:tab', pathname))) page = <Admin tab={m.tab} />;
  else page = <NotFound />;
  return <>{page}</>;
}

function Redirect({ to }: { to: string }) {
  useEffect(() => navigate(to, { replace: true }), [to]);
  return null;
}

function NotFound() {
  return (
    <div className="page">
      <h1>Page not found</h1>
      <p className="muted">There is nothing at this address.</p>
      <Link to="/" className="btn btn-primary">
        Go to the overview
      </Link>
    </div>
  );
}
