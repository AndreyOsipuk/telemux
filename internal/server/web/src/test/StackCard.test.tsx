// «Версии и обновления» (ADR 0008): релизы GitHub, версии на машинах, обновление с
// подтверждением, своя сборка - только с force, 409 not_newer - повтор с force,
// идущее обновление - частый опрос, старый мастер без ручки (404) - подсказка.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { STACK_PENDING_REFRESH_MS, hasPendingUpdate } from '@/api/queries';
import type { StackMachine, StackProductState, StackStatus, StackUpdateState } from '@/api/types';
import ru from '@/i18n/ru.json';
import StackCard, { machineBusy, versionView } from '@/pages/nodes/StackCard';
import { urlPath } from './fleet-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

afterEach(() => {
  vi.unstubAllGlobals();
});

const MASTER = { body: { role: 'master', is_master: true } };

function upd(over: Partial<StackUpdateState> = {}): StackUpdateState {
  return {
    code: 'pr1',
    product: 'telemt',
    from: '3.5.16',
    to: '3.5.17',
    revision: 1,
    status: 'pending',
    error: null,
    requested_at: '2026-06-01T12:00:00Z',
    finished_at: null,
    ...over,
  };
}

function st(over: Partial<StackProductState> = {}): StackProductState {
  return { current: '3.5.17', latest: '3.5.17', update_available: false, custom_build: false, update: null, ...over };
}

function machineOf(code: string, telemt: StackProductState | null, mtg: StackProductState | null = null): StackMachine {
  return { code, kind: 'relay', enabled: true, telemt, mtg };
}

function status(data: StackMachine[]): StackStatus {
  return {
    products: [
      { product: 'telemt', repo: 'telemt/telemt', latest: { version: '3.5.17', tag: '3.5.17', published_at: '2026-06-01T00:00:00Z' }, checked_at: '2026-06-01T11:00:00Z', error: null },
      { product: 'mtg', repo: 'dolonet/mtg-multi', latest: { version: '1.16.0', tag: 'v1.16.0', published_at: '2026-06-01T00:00:00Z' }, checked_at: '2026-06-01T11:00:00Z', error: null },
    ],
    data,
    paging: { total: data.length, limit: 20, offset: 0 },
  };
}

describe('versionView', () => {
  it.each([
    { name: 'продукта нет', s: null, want: 'none' },
    { name: 'версия не пришла', s: st({ current: null }), want: 'unknown' },
    { name: 'последняя', s: st(), want: 'current' },
    { name: 'есть новее', s: st({ current: '3.5.16', update_available: true }), want: 'available' },
    { name: 'своя сборка на том же ядре', s: st({ current: '1.16.0-webalias', custom_build: true }), want: 'custom' },
    { name: 'идёт обновление важнее доступного', s: st({ update_available: true, update: upd() }), want: 'pending' },
    { name: 'упало и версия не сменилась', s: st({ current: '3.5.16', update: upd({ status: 'failed', error: 'boom' }) }), want: 'failed' },
    { name: 'старый провал, версия уже новая', s: st({ update: upd({ status: 'failed' }) }), want: 'current' },
  ])('$name -> $want', ({ s, want }) => {
    expect(versionView(s)).toBe(want);
  });

  it('hasPendingUpdate видит идущее обновление любого продукта', () => {
    expect(hasPendingUpdate(status([machineOf('a', st())]))).toBe(false);
    expect(hasPendingUpdate(status([machineOf('a', st(), st({ update: upd({ product: 'mtg' }) }))]))).toBe(true);
    expect(STACK_PENDING_REFRESH_MS).toBeLessThanOrEqual(10_000);
    expect(machineBusy(machineOf('a', st(), st({ update: upd({ product: 'mtg' }) })))).toBe(true);
    expect(machineBusy(machineOf('a', st({ update: upd({ status: 'applied' }) }), null))).toBe(false);
  });
});

describe('StackCard', () => {
  it('релизы, версии; обновление - через подтверждение, без force', async () => {
    const user = userEvent.setup();
    const posts: unknown[] = [];
    mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/stack') return { body: status([machineOf('pr1', st({ current: '3.5.16', update_available: true }))]) };
      if (urlPath(url) === '/api/v1/machines/pr1/update' && init?.method === 'POST') {
        posts.push(JSON.parse(String(init.body)));
        return { status: 202, body: upd() };
      }
      return undefined;
    });
    renderWithProviders(<StackCard />);
    expect(await screen.findByTestId('release-telemt')).toHaveTextContent('telemt/telemt');
    const cell = await screen.findByTestId('stack-pr1-telemt');
    expect(cell).toHaveTextContent('доступна 3.5.17');
    const btn = within(cell).getByRole('button', { name: 'Обновить telemt на pr1' });
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);
    await user.click(await screen.findByRole('button', { name: ru.stack.update }));
    await waitFor(() => expect(posts).toEqual([{ product: 'telemt', force: false }]));
  });

  it('своя сборка - «Обновить всё равно» с предупреждением и force', async () => {
    const user = userEvent.setup();
    const posts: unknown[] = [];
    mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/stack')
        return { body: status([machineOf('pr1', null, st({ current: '1.16.0-webalias', latest: '1.16.0', custom_build: true }))]) };
      if (urlPath(url) === '/api/v1/machines/pr1/update') {
        posts.push(JSON.parse(String(init?.body)));
        return { status: 202, body: upd({ product: 'mtg', to: '1.16.0' }) };
      }
      return undefined;
    });
    renderWithProviders(<StackCard />);
    const cell = await screen.findByTestId('stack-pr1-mtg');
    expect(within(cell).getByText(ru.stack.custom)).toBeInTheDocument();
    const btn = within(cell).getByRole('button', { name: 'Обновить mtg-multi на pr1' });
    expect(btn).toHaveTextContent(ru.stack.updateAnyway);
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);
    expect(await screen.findByText(/Правки своей сборки/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: ru.stack.forceOk }));
    await waitFor(() => expect(posts).toEqual([{ product: 'mtg', force: true }]));
  });

  it('409 not_newer - подтверждение и повтор с force', async () => {
    const user = userEvent.setup();
    const posts: { force: boolean }[] = [];
    mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/stack') return { body: status([machineOf('pr1', st({ current: '3.5.16', update_available: true }))]) };
      if (urlPath(url) === '/api/v1/machines/pr1/update') {
        const b = JSON.parse(String(init?.body)) as { force: boolean };
        posts.push(b);
        return b.force ? { status: 202, body: upd() } : { status: 409, body: { code: 'not_newer', message: 'версия 3.5.17 не новее' } };
      }
      return undefined;
    });
    renderWithProviders(<StackCard />);
    const btn = await screen.findByRole('button', { name: 'Обновить telemt на pr1' });
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);
    await user.click(await screen.findByRole('button', { name: ru.stack.update }));
    expect((await screen.findAllByText(ru.stack.force.not_newer)).length).toBeGreaterThan(0);
    await user.click(screen.getByRole('button', { name: ru.stack.forceOk }));
    await waitFor(() => expect(posts.map((p) => p.force)).toEqual([false, true]));
  });

  it('идёт обновление - метка и кнопки нет; провал - текст ошибки', async () => {
    mockFetch((url) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/stack')
        return {
          body: status([
            machineOf('pr1', st({ current: '3.5.16', update_available: true, update: upd() })),
            machineOf('pr2', st({ current: '3.5.16', update_available: true, update: upd({ code: 'pr2', status: 'failed', error: 'сервис не поднялся' }) })),
          ]),
        };
      return undefined;
    });
    renderWithProviders(<StackCard />);
    const c1 = await screen.findByTestId('stack-pr1-telemt');
    expect(c1).toHaveTextContent('обновляется до 3.5.17');
    expect(within(c1).queryByRole('button')).not.toBeInTheDocument();
    const c2 = screen.getByTestId('stack-pr2-telemt');
    expect(c2.dataset.view).toBe('failed');
    expect(within(c2).getByRole('button', { name: 'Обновить telemt на pr2' })).toBeInTheDocument();
  });

  it('обновление одного продукта идёт - кнопка другого продукта машины выключена', async () => {
    mockFetch((url) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/stack')
        return {
          body: status([
            machineOf(
              'pr1',
              st({ current: '3.5.16', update_available: true, update: upd() }),
              st({ current: '1.15.0', latest: '1.16.0', update_available: true }),
            ),
          ]),
        };
      return undefined;
    });
    renderWithProviders(<StackCard />);
    const mtg = await screen.findByTestId('stack-pr1-mtg');
    const btn = within(mtg).getByRole('button', { name: 'Обновить mtg-multi на pr1' });
    await waitFor(() => expect(screen.queryByRole('button', { name: new RegExp(ru.stack.check) })).toBeEnabled());
    expect(btn).toBeDisabled();
  });

  it('409 asset_missing - окно с текстом, без повтора с force', async () => {
    const user = userEvent.setup();
    const posts: unknown[] = [];
    mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/stack') return { body: status([machineOf('pr1', st({ current: '3.5.16', update_available: true }))]) };
      if (urlPath(url) === '/api/v1/machines/pr1/update') {
        posts.push(JSON.parse(String(init?.body)));
        return { status: 409, body: { code: 'asset_missing', message: 'нет файла под arm64' } };
      }
      return undefined;
    });
    renderWithProviders(<StackCard />);
    const btn = await screen.findByRole('button', { name: 'Обновить telemt на pr1' });
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);
    await user.click(await screen.findByRole('button', { name: ru.stack.update }));
    expect(await screen.findByText('нет файла под arm64')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: ru.stack.forceOk })).not.toBeInTheDocument();
    expect(posts).toHaveLength(1);
  });

  it('проверка релизов чаще раза в минуту - понятное сообщение', async () => {
    const user = userEvent.setup();
    mockFetch((url, init) => {
      if (url === '/api/role') return MASTER;
      if (urlPath(url) === '/api/v1/stack') return { body: status([]) };
      if (urlPath(url) === '/api/v1/stack/check' && init?.method === 'POST')
        return { status: 429, body: { code: 'too_many_requests', message: 'too many' } };
      return undefined;
    });
    renderWithProviders(<StackCard />);
    const btn = await screen.findByRole('button', { name: new RegExp(ru.stack.check) });
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);
    expect(await screen.findByText(ru.stack.tooOften)).toBeInTheDocument();
  });

  it('старый мастер без ручки (404) - подсказка, а не ошибка', async () => {
    mockFetch((url) => (url === '/api/role' ? MASTER : undefined));
    renderWithProviders(<StackCard />);
    expect(await screen.findByTestId('stack-unsupported')).toHaveTextContent(ru.stack.unsupported);
    expect(screen.getByRole('button', { name: new RegExp(ru.stack.check) })).toBeDisabled();
  });
});
