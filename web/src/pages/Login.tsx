import { useState, type FormEvent } from 'react';
import { api, errorMessage } from '../lib/api';
import type { Me } from '../lib/types';
import { Button, Field } from '../components/ui';
import { useDocumentTitle } from '../lib/hooks';

/** Sign-in form, or the first-administrator form on a fresh server. */
export function Login({ setup, onDone }: { setup: boolean; onDone: (me: Me) => void }) {
  useDocumentTitle(setup ? 'Set up' : 'Sign in');
  const [username, setUsername] = useState(setup ? 'admin' : '');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (setup) {
      if (password.length < 10) {
        setError('The password must be at least 10 characters.');
        return;
      }
      if (password !== confirm) {
        setError('The passwords do not match.');
        return;
      }
    }
    setBusy(true);
    try {
      const me = await api.post<Me>(setup ? '/api/setup' : '/api/auth/login', { username: username.trim(), password });
      onDone(me);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="center-screen">
      <form className="auth-card" onSubmit={submit} noValidate>
        <div className="auth-brand">
          <img src="/favicon.svg" alt="" width={36} height={36} />
          <span>PathGauge</span>
        </div>
        <h1>{setup ? 'Create the first administrator' : 'Sign in'}</h1>
        {setup && <p className="muted">This server has no users yet. The account you create here has full administrator rights.</p>}
        <Field label="Username">
          <input autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required autoFocus={!setup} name="username" />
        </Field>
        <Field label="Password" hint={setup ? 'At least 10 characters.' : undefined}>
          <input
            type="password"
            autoComplete={setup ? 'new-password' : 'current-password'}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            minLength={setup ? 10 : undefined}
            autoFocus={setup}
            name="password"
          />
        </Field>
        {setup && (
          <Field label="Confirm password">
            <input type="password" autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required name="confirm" />
          </Field>
        )}
        {error && (
          <p className="alert alert-critical" role="alert">
            <span aria-hidden="true">✖</span> {error}
          </p>
        )}
        <Button type="submit" variant="primary" busy={busy} className="btn-block">
          {setup ? 'Create administrator' : 'Sign in'}
        </Button>
      </form>
    </div>
  );
}
