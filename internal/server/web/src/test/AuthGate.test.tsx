// Тесты AuthGate: экран входа по /api/me, вход, возврат на вход по 401 от любого запроса.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useQuery } from '@tanstack/react-query';

import AuthGate from '@/auth/AuthGate';
import { request } from '@/api/http';
import ru from '@/i18n/ru.json';
import { mockFetch, renderWithProviders } from './test-utils';

// Маленький «экран» за входом: грузит пользователей через общий HTTP-клиент.
function UsersProbe() {
  const q = useQuery({
    queryKey: ['users', 20, 0],
    queryFn: () => request<{ data: unknown[] }>('/api/users?limit=20&offset=0'),
    retry: false,
  });
  if (q.isError) return <div>ошибка загрузки</div>;
  return <div>{q.isSuccess ? 'пользователи загружены' : 'загрузка'}</div>;
}

const loginTitle = () => screen.findByRole('button', { name: ru.auth.submit });

async function submitLogin(username: string, password: string) {
  const user = userEvent.setup();
  await user.type(await screen.findByLabelText(ru.auth.username), username);
  await user.type(screen.getByLabelText(ru.auth.password), password);
  await user.click(screen.getByRole('button', { name: ru.auth.submit }));
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('AuthGate', () => {
  it('вход включён и не выполнен - экран входа, дети не видны', async () => {
    mockFetch((url) => (url === '/api/me' ? { body: { auth_enabled: true, authed: false } } : undefined));

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );

    expect(await loginTitle()).toBeInTheDocument();
    expect(screen.queryByText('секретная зона')).not.toBeInTheDocument();
  });

  it('вход выключен - сразу дети', async () => {
    mockFetch((url) => (url === '/api/me' ? { body: { auth_enabled: false, authed: false } } : undefined));

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );

    expect(await screen.findByText('секретная зона')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: ru.auth.submit })).not.toBeInTheDocument();
  });

  it('уже вошёл - дети', async () => {
    mockFetch((url) => (url === '/api/me' ? { body: { auth_enabled: true, authed: true } } : undefined));

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );

    expect(await screen.findByText('секретная зона')).toBeInTheDocument();
  });

  it('/api/me упал (500) - экран «не удалось проверить вход», а не вход и не дети', async () => {
    mockFetch((url) => (url === '/api/me' ? { status: 500, body: { error: 'db down' } } : undefined));

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );

    expect(await screen.findByText(ru.auth.meFailed)).toBeInTheDocument();
    expect(screen.getByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText('секретная зона')).not.toBeInTheDocument();
  });

  it('/api/me ответил 401 - экран входа, а не «не удалось проверить вход»', async () => {
    mockFetch((url) => (url === '/api/me' ? { status: 401, body: { error: 'unauthorized' } } : undefined));

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );

    expect(await loginTitle()).toBeInTheDocument();
    expect(screen.queryByText(ru.auth.meFailed)).not.toBeInTheDocument();
    expect(screen.queryByText('секретная зона')).not.toBeInTheDocument();
  });

  it('/api/me ответил 401, затем удачный вход - дети', async () => {
    mockFetch((url) => {
      if (url === '/api/me') return { status: 401, body: {} };
      if (url === '/api/login') return { body: { ok: true } };
      return undefined;
    });

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );
    await submitLogin('admin', 'right');

    expect(await screen.findByText('секретная зона')).toBeInTheDocument();
  });

  it('неверный пароль - сообщение и экран входа остаётся, /api/me не перезапрашивается', async () => {
    const fetchFn = mockFetch((url, init) => {
      if (url === '/api/me') return { body: { auth_enabled: true, authed: false } };
      if (url === '/api/login' && init?.method === 'POST') return { status: 401, body: { error: 'bad credentials' } };
      return undefined;
    });

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );
    await submitLogin('admin', 'wrong');

    expect(await screen.findByText(ru.auth.badCredentials)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: ru.auth.submit })).toBeInTheDocument();
    expect(screen.queryByText('секретная зона')).not.toBeInTheDocument();
    // Нет цикла: 401 от логина не поднимает событие «нужен вход» и не дёргает /api/me.
    const meCalls = fetchFn.mock.calls.filter(([u]) => u === '/api/me').length;
    expect(meCalls).toBe(1);
    const loginCalls = fetchFn.mock.calls.filter(([u]) => u === '/api/login').length;
    expect(loginCalls).toBe(1);
  });

  it('логин отправляет введённые логин и пароль', async () => {
    const fetchFn = mockFetch((url) => {
      if (url === '/api/me') return { body: { auth_enabled: true, authed: false } };
      if (url === '/api/login') return { body: { ok: true } };
      return undefined;
    });

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );
    await submitLogin('admin', 's3cret');

    await screen.findByText('секретная зона');
    const call = fetchFn.mock.calls.find(([u]) => u === '/api/login');
    expect(JSON.parse(String(call?.[1]?.body))).toEqual({ username: 'admin', password: 's3cret' });
  });

  it('пустая форма - подсказки валидации, запрос на вход не уходит', async () => {
    const fetchFn = mockFetch((url) => (url === '/api/me' ? { body: { auth_enabled: true, authed: false } } : undefined));

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );
    await userEvent.setup().click(await loginTitle());

    expect(await screen.findByText(ru.auth.usernameRequired)).toBeInTheDocument();
    expect(await screen.findByText(ru.auth.passwordRequired)).toBeInTheDocument();
    expect(fetchFn.mock.calls.some(([u]) => u === '/api/login')).toBe(false);
  });

  it('вход упал по сети - понятный текст, а не «неверный пароль»', async () => {
    mockFetch((url) => (url === '/api/me' ? { body: { auth_enabled: true, authed: false } } : undefined));
    // /api/login - обрыв сети; /api/me уже загружен.
    const original = globalThis.fetch;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        if (String(input) === '/api/login') throw new TypeError('Failed to fetch');
        return original(input, init);
      }),
    );

    renderWithProviders(
      <AuthGate>
        <div>секретная зона</div>
      </AuthGate>,
    );
    await submitLogin('admin', 'x');

    expect(await screen.findByText(ru.errors.network)).toBeInTheDocument();
    expect(screen.queryByText(ru.auth.badCredentials)).not.toBeInTheDocument();
  });

  it('удачный вход - показываются дети', async () => {
    mockFetch((url, init) => {
      if (url === '/api/me') return { body: { auth_enabled: true, authed: false } };
      if (url === '/api/login' && init?.method === 'POST') return { body: { ok: true } };
      if (url.startsWith('/api/users')) return { body: { data: [], paging: { total: 0, limit: 20, offset: 0 } } };
      return undefined;
    });

    renderWithProviders(
      <AuthGate>
        <UsersProbe />
      </AuthGate>,
    );
    await submitLogin('admin', 'right');

    expect(await screen.findByText('пользователи загружены')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: ru.auth.submit })).not.toBeInTheDocument();
  });

  it('залогинен, запрос детей получил 401 - снова экран входа', async () => {
    mockFetch((url) => {
      if (url === '/api/me') return { body: { auth_enabled: true, authed: true } };
      if (url.startsWith('/api/users')) return { status: 401, body: { error: 'unauthorized' } };
      return undefined;
    });

    renderWithProviders(
      <AuthGate>
        <UsersProbe />
      </AuthGate>,
    );

    expect(await loginTitle()).toBeInTheDocument();
    expect(screen.queryByText('пользователи загружены')).not.toBeInTheDocument();
    expect(screen.queryByText('ошибка загрузки')).not.toBeInTheDocument();
  });

  it('после 401 и повторного входа дети снова грузят данные', async () => {
    let sessionAlive = true;
    mockFetch((url) => {
      if (url === '/api/me') return { body: { auth_enabled: true, authed: true } };
      if (url === '/api/login') {
        sessionAlive = true;
        return { body: { ok: true } };
      }
      if (url.startsWith('/api/users')) {
        return sessionAlive
          ? { body: { data: [], paging: { total: 0, limit: 20, offset: 0 } } }
          : { status: 401, body: { error: 'unauthorized' } };
      }
      return undefined;
    });

    const { queryClient } = renderWithProviders(
      <AuthGate>
        <UsersProbe />
      </AuthGate>,
    );
    expect(await screen.findByText('пользователи загружены')).toBeInTheDocument();

    // Сессия истекла: следующий запрос детей получает 401.
    sessionAlive = false;
    await queryClient.refetchQueries({ queryKey: ['users'] });
    expect(await loginTitle()).toBeInTheDocument();

    await submitLogin('admin', 'right');
    expect(await screen.findByText('пользователи загружены')).toBeInTheDocument();
  });

  it('на 401 выбрасывается кэш чужой сессии, но не /api/me', async () => {
    mockFetch((url) => {
      if (url === '/api/me') return { body: { auth_enabled: true, authed: true } };
      if (url.startsWith('/api/users')) return { status: 401, body: {} };
      return undefined;
    });

    const { queryClient } = renderWithProviders(
      <AuthGate>
        <UsersProbe />
      </AuthGate>,
    );
    queryClient.setQueryData(['role'], { role: 'master', is_master: true });

    await loginTitle();
    await waitFor(() => expect(queryClient.getQueryData(['role'])).toBeUndefined());
    expect(queryClient.getQueryData(['me'])).toEqual({ auth_enabled: true, authed: false });
  });
});
