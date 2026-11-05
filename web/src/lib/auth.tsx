import { createContext, useContext } from 'react';
import type { Me, Role } from './types';

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };

export interface AuthState {
  me: Me;
  logout: () => void;
}

export const AuthContext = createContext<AuthState | null>(null);

export function useAuth(): AuthState {
  const a = useContext(AuthContext);
  if (!a) throw new Error('useAuth outside AuthContext');
  return a;
}

/** Whether the signed-in user has at least `role`. */
export function useCan(role: Role): boolean {
  const { me } = useAuth();
  return (rank[me.role] ?? 0) >= rank[role];
}
