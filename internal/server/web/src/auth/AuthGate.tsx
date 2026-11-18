// Вход по сессии: /api/me решает, показывать ли экран входа. Любой 401 от API
// (сессия истекла, мастер сменился) возвращает на экран входа.

import { createContext, useCallback, useContext, useEffect, useMemo } from 'react';
import type { ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Result, Spin } from 'antd';
import { useTranslation } from 'react-i18next';

import { api } from '@/api/client';
import { ApiError, onUnauthorized } from '@/api/http';
import { describeError } from '@/api/errors';
import { qk } from '@/api/queries';
import LoginPage from '@/pages/login/LoginPage';

interface AuthContextValue {
  authEnabled: boolean;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue>({ authEnabled: false, logout: async () => {} });
export const useAuth = () => useContext(AuthContext);

export default function AuthGate({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const me = useQuery({ queryKey: qk.me, queryFn: api.me, staleTime: Infinity, retry: false });

  // 401 от любого запроса: помечаем «не вошёл» и выбрасываем кэш чужой сессии.
  useEffect(
    () =>
      onUnauthorized(() => {
        qc.setQueryData(qk.me, (prev: { auth_enabled: boolean } | undefined) => ({
          auth_enabled: prev?.auth_enabled ?? true,
          authed: false,
        }));
        qc.removeQueries({ predicate: (q) => q.queryKey[0] !== qk.me[0] });
      }),
    [qc],
  );

  const onLoggedIn = useCallback(() => {
    qc.setQueryData(qk.me, { auth_enabled: true, authed: true });
  }, [qc]);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      qc.clear();
      qc.setQueryData(qk.me, { auth_enabled: true, authed: false });
    }
  }, [qc]);

  const authEnabled = me.data?.auth_enabled ?? false;
  const value = useMemo(() => ({ authEnabled, logout }), [authEnabled, logout]);

  if (me.isPending) {
    return (
      <div className="page-loading">
        <Spin size="large" />
      </div>
    );
  }
  // /api/me сейчас открыт, но если его закроют сессией - 401 значит «нужен вход».
  const meUnauthorized = me.error instanceof ApiError && me.error.status === 401;
  if (me.isError && !meUnauthorized) {
    return (
      <Result
        status="warning"
        title={t('auth.meFailed')}
        subTitle={describeError(me.error, t)}
        extra={<Button onClick={() => me.refetch()}>{t('common.retry')}</Button>}
      />
    );
  }
  if (meUnauthorized || !me.data || (me.data.auth_enabled && !me.data.authed)) {
    return <LoginPage onSuccess={onLoggedIn} />;
  }
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
