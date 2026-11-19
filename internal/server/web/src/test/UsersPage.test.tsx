// Тесты экрана «Пользователи»: серверная пагинация, запись только на мастере, понятный 409.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import type { User } from '@/api/types';
import ru from '@/i18n/ru.json';
import UsersPage, { renewedExpiration } from '@/pages/users/UsersPage';
import { mockFetch, renderWithProviders } from './test-utils';

function makeUser(i: number): User {
  return {
    username: `user-${i}`,
    secret: `secret-${i}`,
    expiration_at: null,
    max_tcp_conns: null,
    enabled: true,
    created_at: '2026-01-01T00:00:00Z',
  };
}

// Бэкенд с N пользователями: отдаёт страницу по limit/offset из URL.
function usersBackend(total: number, role: { role: string; is_master: boolean }) {
  const all = Array.from({ length: total }, (_, i) => makeUser(i + 1));
  return (url: string, init?: RequestInit) => {
    if (url === '/api/role') return { body: role };
    if (url.startsWith('/api/users') && (init?.method ?? 'GET') === 'GET') {
      const q = new URL(url, 'http://x').searchParams;
      const limit = Number(q.get('limit'));
      const offset = Number(q.get('offset'));
      return { body: { data: all.slice(offset, offset + limit), paging: { total, limit, offset } } };
    }
    return undefined;
  };
}

const MASTER = { role: 'master', is_master: true };
const REPLICA = { role: 'replica', is_master: false };

const createButton = () => screen.getByRole('button', { name: new RegExp(ru.users.create) });

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('UsersPage', () => {
  it('таблица из {data, paging}, первый запрос limit=20&offset=0', async () => {
    const fetchFn = mockFetch(usersBackend(3, MASTER));

    renderWithProviders(<UsersPage />);

    expect(await screen.findByText('user-1')).toBeInTheDocument();
    expect(screen.getByText('user-3')).toBeInTheDocument();
    expect(screen.getByText('Всего 3')).toBeInTheDocument();
    const userCalls = fetchFn.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('/api/users'));
    expect(userCalls[0]).toBe('/api/users?limit=20&offset=0');
  });

  it('пустой список - пустая таблица без пагинатора', async () => {
    mockFetch(usersBackend(0, MASTER));

    const { container } = renderWithProviders(<UsersPage />);

    // Ждём, пока таблица перестанет грузиться.
    await waitFor(() => expect(container.querySelector('.ant-spin-spinning')).toBeNull());
    // «Нет данных» - текст пустой таблицы из локали antd ru_RU.
    expect((await screen.findAllByText('Нет данных')).length).toBeGreaterThan(0);
    expect(screen.queryByText('user-1')).not.toBeInTheDocument();
    expect(container.querySelector('.ant-pagination-item')).toBeNull();
  });

  it('переход на 2-ю страницу запрашивает offset=20', async () => {
    const fetchFn = mockFetch(usersBackend(25, MASTER));
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    await screen.findByText('user-1');
    expect(screen.queryByText('user-21')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Страница 2' }));

    expect(await screen.findByText('user-21')).toBeInTheDocument();
    expect(screen.getByText('user-25')).toBeInTheDocument();
    expect(screen.queryByText('user-1')).not.toBeInTheDocument();
    expect(fetchFn.mock.calls.map(([u]) => String(u))).toContain('/api/users?limit=20&offset=20');
  });

  it('удалили единственного на последней странице - переход на новую последнюю страницу', async () => {
    // 41 пользователь по 20 на странице: 3-я страница - только user-41.
    const all = Array.from({ length: 41 }, (_, i) => makeUser(i + 1));
    const fetchFn = mockFetch((url, init) => {
      const method = init?.method ?? 'GET';
      if (url === '/api/role') return { body: MASTER };
      if (method === 'DELETE' && url.startsWith('/api/users/')) {
        const name = decodeURIComponent(url.slice('/api/users/'.length));
        all.splice(all.findIndex((u) => u.username === name), 1);
        return { body: { ok: true } };
      }
      if (url.startsWith('/api/users')) {
        const q = new URL(url, 'http://x').searchParams;
        const limit = Number(q.get('limit'));
        const offset = Number(q.get('offset'));
        return { body: { data: all.slice(offset, offset + limit), paging: { total: all.length, limit, offset } } };
      }
      return undefined;
    });
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    await screen.findByText('user-1');
    await user.click(screen.getByRole('button', { name: 'Страница 3' }));
    expect(await screen.findByText('user-41')).toBeInTheDocument();
    const del = screen.getByRole('button', { name: ru.common.delete });
    await waitFor(() => expect(del).toBeEnabled());

    await user.click(del);
    const confirm = await screen.findByText(ru.users.deleteConfirm.replace('{{name}}', 'user-41'));
    const popover = confirm.closest<HTMLElement>('.ant-popover, .ant-popconfirm');
    expect(popover).not.toBeNull();
    await user.click(within(popover!).getByRole('button', { name: ru.common.delete }));

    // total стал 40: страница 3 исчезла, панель сама уходит на 2-ю (offset=20).
    expect(await screen.findByText('user-40')).toBeInTheDocument();
    expect(screen.getByText('user-21')).toBeInTheDocument();
    const calls = fetchFn.mock.calls.map(([u]) => String(u));
    expect(calls).toContain('/api/users/user-41');
    const lastList = calls.filter((u) => u.startsWith('/api/users?')).at(-1);
    expect(lastList).toBe('/api/users?limit=20&offset=20');
    expect(screen.queryByRole('button', { name: 'Страница 3' })).not.toBeInTheDocument();
  });

  it('ошибка загрузки списка (500) - баннер с понятным текстом и кнопкой «Повторить»', async () => {
    mockFetch((url) => {
      if (url === '/api/role') return { body: MASTER };
      if (url.startsWith('/api/users')) return { status: 500, body: { error: 'pq: connection refused' } };
      return undefined;
    });

    renderWithProviders(<UsersPage />);

    expect(await screen.findByText(ru.errors.loadFailed)).toBeInTheDocument();
    expect(screen.getByText('Ошибка сервера (500). Подробности - в журнале демона.')).toBeInTheDocument();
    expect(screen.queryByText(/connection refused/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: ru.common.retry })).toBeInTheDocument();
  });

  it('на реплике кнопка «Создать», поле ввода и переключатели неактивны', async () => {
    mockFetch(usersBackend(1, REPLICA));

    renderWithProviders(<UsersPage />);
    await screen.findByText('user-1');
    // Роль грузится отдельно - ждём, пока она придёт (поле ввода выключено и по роли).
    await waitFor(() => expect(screen.getByLabelText(ru.users.username)).toBeDisabled());

    expect(createButton()).toBeDisabled();
    expect(screen.getByRole('switch', { name: ru.users.enabled })).toBeDisabled();
    expect(screen.getByRole('button', { name: ru.common.delete })).toBeDisabled();
  });

  it('на мастере «Создать» неактивна, пока логин пуст, и активна после ввода', async () => {
    mockFetch(usersBackend(1, MASTER));
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    const input = screen.getByLabelText(ru.users.username);
    await waitFor(() => expect(input).toBeEnabled());

    expect(createButton()).toBeDisabled();
    await user.type(input, '   ');
    expect(createButton()).toBeDisabled();
    await user.clear(input);
    await user.type(input, 'new-user');
    expect(createButton()).toBeEnabled();
  });

  it('создание на мастере - POST с логином без пробелов и сообщение об успехе', async () => {
    const backend = usersBackend(1, MASTER);
    const fetchFn = mockFetch((url, init) => {
      if (url === '/api/users' && init?.method === 'POST') return { status: 201, body: makeUser(99) };
      return backend(url, init);
    });
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    const input = screen.getByLabelText(ru.users.username);
    await waitFor(() => expect(input).toBeEnabled());
    await user.type(input, '  new-user ');
    await user.click(createButton());

    expect(await screen.findByText(ru.users.created)).toBeInTheDocument();
    const post = fetchFn.mock.calls.find(([u, i]) => u === '/api/users' && i?.method === 'POST');
    expect(JSON.parse(String(post?.[1]?.body))).toEqual({ username: 'new-user' });
    await waitFor(() => expect(input).toHaveValue(''));
  });

  it('создание: бэкенд ответил 409 «replica» - понятное сообщение «не мастер», поле не очищается', async () => {
    const backend = usersBackend(1, MASTER);
    mockFetch((url, init) => {
      if (url === '/api/users' && init?.method === 'POST') {
        return { status: 409, body: { error: 'эта нода — replica; запись идёт на master' } };
      }
      return backend(url, init);
    });
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    const input = screen.getByLabelText(ru.users.username);
    await waitFor(() => expect(input).toBeEnabled());
    await user.type(input, 'new-user');
    await user.click(createButton());

    expect(await within(document.body).findByText(ru.errors.notMaster)).toBeInTheDocument();
    expect(screen.queryByText(/replica; запись/)).not.toBeInTheDocument();
    expect(input).toHaveValue('new-user');
  });

  it('создание: свежий бэкенд 409 not_master с адресом мастера - сообщение со ссылкой на мастер', async () => {
    const masterUrl = 'https://master.example.test:8080';
    const backend = usersBackend(1, MASTER);
    mockFetch((url, init) => {
      if (url === '/api/users' && init?.method === 'POST') {
        return {
          status: 409,
          headers: { 'X-Telemux-Master': masterUrl },
          body: {
            code: 'not_master',
            message: 'узел в роли replica; запись принимает только мастер',
            error: 'not_master',
            master_url: masterUrl,
          },
        };
      }
      return backend(url, init);
    });
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    const input = screen.getByLabelText(ru.users.username);
    await waitFor(() => expect(input).toBeEnabled());
    await user.type(input, 'new-user');
    await user.click(createButton());

    expect(
      await within(document.body).findByText(`Эта машина - не мастер. Изменения принимает только мастер: ${masterUrl}`),
    ).toBeInTheDocument();
    expect(screen.queryByText(/узел в роли replica/)).not.toBeInTheDocument();
    expect(input).toHaveValue('new-user');
  });

  it('создание: дубль пользователя (409 conflict) - текст сервера, а не «не мастер»', async () => {
    const backend = usersBackend(1, MASTER);
    mockFetch((url, init) => {
      if (url === '/api/users' && init?.method === 'POST') {
        return { status: 409, body: { code: 'conflict', message: 'пользователь с таким username уже существует' } };
      }
      return backend(url, init);
    });
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    const input = screen.getByLabelText(ru.users.username);
    await waitFor(() => expect(input).toBeEnabled());
    await user.type(input, 'user-1');
    await user.click(createButton());

    expect(
      await within(document.body).findByText('пользователь с таким username уже существует'),
    ).toBeInTheDocument();
    expect(screen.queryByText(/не мастер/)).not.toBeInTheDocument();
    expect(input).toHaveValue('user-1');
  });

  it('создание: 400 от бэкенда - текст сервера', async () => {
    const backend = usersBackend(1, MASTER);
    mockFetch((url, init) => {
      if (url === '/api/users' && init?.method === 'POST') {
        return { status: 400, body: { code: 'bad_username', message: 'логин содержит недопустимые символы' } };
      }
      return backend(url, init);
    });
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    const input = screen.getByLabelText(ru.users.username);
    await waitFor(() => expect(input).toBeEnabled());
    await user.type(input, 'bad$name');
    await user.click(createButton());

    expect(await within(document.body).findByText('логин содержит недопустимые символы')).toBeInTheDocument();
  });

  it('выключение пользователя - POST /disable', async () => {
    const backend = usersBackend(1, MASTER);
    const fetchFn = mockFetch((url, init) => {
      if (url === '/api/users/user-1/disable' && init?.method === 'POST') return { body: { ok: true } };
      return backend(url, init);
    });
    const user = userEvent.setup();

    renderWithProviders(<UsersPage />);
    const sw = await screen.findByRole('switch', { name: ru.users.enabled });
    await waitFor(() => expect(sw).toBeEnabled());
    await user.click(sw);

    expect(await screen.findByText(ru.common.saved)).toBeInTheDocument();
    expect(fetchFn.mock.calls.some(([u]) => u === '/api/users/user-1/disable')).toBe(true);
  });
});

describe('UsersPage: онлайн и подключения по всем машинам', () => {
  // usersBackend + /api/v1/connections/by-user: у user-1 5 подключений на 2 машинах.
  function withConns(total: number, byUser?: (url: string) => { status?: number; body?: unknown }) {
    const base = usersBackend(total, MASTER);
    return (url: string, init?: RequestInit) => {
      if (url.startsWith('/api/v1/connections/by-user')) {
        if (byUser) return byUser(url);
        const users = new URL(url, 'http://x').searchParams.getAll('user');
        return {
          body: {
            data: users.map((key) =>
              key === 'user-1'
                ? { key, connections: 5, machines: 2, online: true, seen_at: new Date().toISOString() }
                : { key, connections: 0, machines: 0, online: false, seen_at: null },
            ),
          },
        };
      }
      return base(url, init);
    };
  }

  it('онлайн и число подключений - одним запросом на страницу', async () => {
    const fetchFn = mockFetch(withConns(3));

    renderWithProviders(<UsersPage />);

    expect(await screen.findByTestId('active-user-1')).toHaveTextContent('5');
    expect(screen.getByTestId('active-user-2')).toHaveTextContent('0');
    const row1 = screen.getByTestId('active-user-1').closest('tr') as HTMLElement;
    expect(within(row1).getByText(ru.status.online)).toBeInTheDocument();
    const row2 = screen.getByTestId('active-user-2').closest('tr') as HTMLElement;
    expect(within(row2).getByText(ru.users.offline)).toBeInTheDocument();

    const calls = fetchFn.mock.calls.map(([u]) => String(u)).filter((u) => u.startsWith('/api/v1/connections/by-user'));
    expect(calls).toHaveLength(1);
    expect(calls[0]).toBe('/api/v1/connections/by-user?user=user-1&user=user-2&user=user-3');
  });

  it('пустой список пользователей - запроса подключений нет', async () => {
    const fetchFn = mockFetch(withConns(0));

    renderWithProviders(<UsersPage />);
    await waitFor(() => expect(fetchFn.mock.calls.some(([u]) => String(u).startsWith('/api/users'))).toBe(true));

    expect(fetchFn.mock.calls.some(([u]) => String(u).startsWith('/api/v1/connections/by-user'))).toBe(false);
  });

  it('ошибка подключений (500) - отдельный баннер, список пользователей виден', async () => {
    mockFetch(withConns(2, () => ({ status: 500, body: { code: 'internal', message: 'pq: boom' } })));

    renderWithProviders(<UsersPage />);

    expect(await screen.findByText(ru.users.connsFailed)).toBeInTheDocument();
    expect(screen.getByText('user-1')).toBeInTheDocument();
    expect(screen.queryByText(/pq: boom/)).not.toBeInTheDocument();
  });

  it('401 у подключений - «сессия истекла»', async () => {
    mockFetch(withConns(2, () => ({ status: 401, body: { code: 'unauthorized', message: 'unauthorized' } })));

    renderWithProviders(<UsersPage />);

    expect(await screen.findByText(ru.errors.unauthorized)).toBeInTheDocument();
  });
});

describe('renewedExpiration', () => {
  const now = new Date('2026-06-01T12:00:00Z');
  const DAY = 24 * 3600 * 1000;

  it.each([
    {
      name: 'срок в будущем - продлеваем от срока',
      current: '2026-06-10T12:00:00Z',
      want: new Date(Date.parse('2026-06-10T12:00:00Z') + 30 * DAY),
    },
    { name: 'срок прошёл - от сейчас', current: '2026-05-01T12:00:00Z', want: new Date(now.getTime() + 30 * DAY) },
    { name: 'срок ровно сейчас - от сейчас', current: now.toISOString(), want: new Date(now.getTime() + 30 * DAY) },
    { name: 'бессрочный (null) - от сейчас', current: null, want: new Date(now.getTime() + 30 * DAY) },
  ])('$name', ({ current, want }) => {
    expect(renewedExpiration(current, 30, now)).toBe(want.toISOString());
  });

  it('возвращает ISO-строку', () => {
    expect(renewedExpiration(null, 1, now)).toBe('2026-06-02T12:00:00.000Z');
  });
});
