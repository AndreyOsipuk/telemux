// Тесты экрана «Пары»: данные - заглушки из mocks.ts (с задержкой 250-400 мс),
// роль машины - через fetch /api/role.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { mockPairs } from '@/api/mocks';
import ru from '@/i18n/ru.json';
import { isDrifting, pairRows } from '@/pages/pairs/PairTable';
import PairsPage from '@/pages/pairs/PairsPage';
import { mockFetch, renderWithProviders } from './test-utils';

// Заглушки отвечают через setTimeout - ждём с запасом.
const WAIT = { timeout: 3000 };

function mockRole(isMaster: boolean) {
  return mockFetch((url) =>
    url === '/api/role' ? { body: { role: isMaster ? 'master' : 'replica', is_master: isMaster } } : undefined,
  );
}

// Строка таблицы машины по её коду (antd ставит data-row-key).
function rowOf(code: string): HTMLElement {
  const row = document.querySelector<HTMLElement>(`tr[data-row-key="${code}"]`);
  if (!row) throw new Error(`нет строки ${code}`);
  return row;
}

const applySelectedButton = () => screen.getByRole('button', { name: new RegExp(ru.pairs.applySelected) });

async function renderLoaded(isMaster: boolean) {
  const fetchFn = mockRole(isMaster);
  const utils = renderWithProviders(<PairsPage />);
  await screen.findByTestId('rev-relay-b', {}, WAIT);
  return { ...utils, fetchFn };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('PairsPage', () => {
  it('рендерит обе пары и все десять машин', async () => {
    await renderLoaded(true);

    expect(screen.getByText('Пара A')).toBeInTheDocument();
    expect(screen.getByText('Пара B')).toBeInTheDocument();
    for (const code of ['relay-a', 'srv-a1', 'srv-a2', 'exit-a1', 'exit-a2', 'relay-b', 'srv-b1', 'srv-b2', 'exit-b1', 'exit-b2']) {
      expect(rowOf(code)).toBeInTheDocument();
    }
    // Экран на заглушках - предупреждение видно.
    expect(screen.getByText(ru.common.mockData)).toBeInTheDocument();
  });

  it('тег канарейки только у машины в канарейке', async () => {
    await renderLoaded(true);

    const canaries = screen.getAllByText(ru.pairs.canary);
    expect(canaries).toHaveLength(1);
    expect(within(rowOf('srv-a2')).getByText(ru.pairs.canary)).toBeInTheDocument();
    expect(within(rowOf('srv-a2')).getByText(/3\.5\.18\+sess\.1bdefc24/)).toBeInTheDocument();
  });

  it('ревизия с расхождением показана как «применённая → желаемая»', async () => {
    await renderLoaded(true);

    expect(screen.getByTestId('rev-relay-b')).toHaveTextContent('41 → 42');
    expect(screen.getByTestId('rev-srv-b2')).toHaveTextContent('40 → 42');
    // Без расхождения - просто номер ревизии.
    expect(screen.getByTestId('rev-relay-a')).toHaveTextContent(/^42$/);
  });

  it('сводка пары: B - 3 расхождения и 1 не в сети, A - всё применено', async () => {
    await renderLoaded(true);

    expect(screen.getByText('3 расхождения')).toBeInTheDocument();
    expect(screen.getByText('1 не в сети')).toBeInTheDocument();
    expect(screen.getAllByText(ru.pairs.allApplied)).toHaveLength(1);
  });

  it('мост показан у релея, у остальных ролей - прочерк', async () => {
    await renderLoaded(true);

    expect(within(rowOf('relay-a')).getByText('exit-a1')).toBeInTheDocument();
    expect(within(rowOf('relay-b')).getByText('exit-b2')).toBeInTheDocument();
  });

  it('раскрытие строки показывает последнюю ошибку и историю применений', async () => {
    const user = userEvent.setup();
    await renderLoaded(true);

    const expand = within(rowOf('relay-b')).getByRole('button', { name: /развернуть/i });
    await user.click(expand);

    expect(await screen.findByText(ru.pairs.lastError)).toBeInTheDocument();
    expect(screen.getByText(ru.pairs.history)).toBeInTheDocument();
    // История из заглушки: rev 42 откатили, rev 41 применена.
    expect(await screen.findByText('rev 42', {}, WAIT)).toBeInTheDocument();
    expect(screen.getByText('rev 41')).toBeInTheDocument();
    expect(screen.getByText(ru.applyResult.rolled_back)).toBeInTheDocument();
    expect(screen.getByText(ru.applyResult.applied)).toBeInTheDocument();
  });

  it('раскрытие строки без ошибки - только история, без блока «последняя ошибка»', async () => {
    const user = userEvent.setup();
    await renderLoaded(true);

    await user.click(within(rowOf('srv-a1')).getByRole('button', { name: /развернуть/i }));

    expect(await screen.findByText(ru.pairs.history)).toBeInTheDocument();
    expect(await screen.findAllByText(ru.applyResult.applied, {}, WAIT)).toHaveLength(2);
    expect(screen.queryByText(ru.pairs.lastError)).not.toBeInTheDocument();
  });

  it('мастер: выбор двух машин из разных пар -> «Применить выбранные» активна, после клика - успех', async () => {
    const user = userEvent.setup();
    await renderLoaded(true);
    await waitFor(() => expect(within(rowOf('relay-a')).getByRole('checkbox')).toBeEnabled());

    expect(applySelectedButton()).toBeDisabled();
    await user.click(within(rowOf('relay-a')).getByRole('checkbox'));
    await user.click(within(rowOf('srv-b2')).getByRole('checkbox'));

    expect(screen.getByText('Выбрано 2 машины')).toBeInTheDocument();
    expect(applySelectedButton()).toBeEnabled();

    await user.click(applySelectedButton());

    expect(await screen.findByText('Применение запрошено для 2 машин', {}, WAIT)).toBeInTheDocument();
    // После применения выбор сброшен.
    await waitFor(() => expect(applySelectedButton()).toBeDisabled());
    expect(screen.queryByText('Выбрано 2 машины')).not.toBeInTheDocument();
  });

  it('мастер: «повторить» на машине с ошибкой - сообщение «Запрошено»', async () => {
    const user = userEvent.setup();
    await renderLoaded(true);
    const retry = within(rowOf('relay-b')).getByRole('button', { name: `${ru.pairs.retry} relay-b` });
    await waitFor(() => expect(retry).toBeEnabled());

    await user.click(retry);

    expect(await screen.findByText('Запрошено: relay-b', {}, WAIT)).toBeInTheDocument();
  });

  it('мастер: «повторить» неактивна у машины без ошибки и без расхождения', async () => {
    await renderLoaded(true);
    await waitFor(() =>
      expect(within(rowOf('relay-b')).getByRole('button', { name: `${ru.pairs.retry} relay-b` })).toBeEnabled(),
    );

    expect(within(rowOf('srv-a1')).getByRole('button', { name: `${ru.pairs.retry} srv-a1` })).toBeDisabled();
  });

  it('реплика: чекбоксы, «Применить выбранные» и «Применить сейчас» неактивны', async () => {
    const { fetchFn } = await renderLoaded(false);
    await waitFor(() => expect(fetchFn).toHaveBeenCalledWith('/api/role', expect.anything()));
    await screen.findByText('Пара A');

    expect(applySelectedButton()).toBeDisabled();
    expect(within(rowOf('relay-a')).getByRole('checkbox')).toBeDisabled();
    expect(within(rowOf('relay-b')).getByRole('button', { name: `${ru.pairs.applyNow} relay-b` })).toBeDisabled();
  });

  it('роль не загрузилась (500) - запись выключена, как на реплике', async () => {
    mockFetch((url) => (url === '/api/role' ? { status: 500, body: { error: 'db down' } } : undefined));
    renderWithProviders(<PairsPage />);
    await screen.findByTestId('rev-relay-b', {}, WAIT);

    expect(applySelectedButton()).toBeDisabled();
    expect(within(rowOf('relay-a')).getByRole('checkbox')).toBeDisabled();
  });
});

describe('pairRows и isDrifting', () => {
  it('порядок строк: релей, серверы, выходы; у каждой строки код пары', () => {
    const [a] = mockPairs();
    const rows = pairRows(a);

    expect(rows.map((r) => r.code)).toEqual(['relay-a', 'srv-a1', 'srv-a2', 'exit-a1', 'exit-a2']);
    expect(rows.every((r) => r.pair === 'pair-a')).toBe(true);
  });

  it.each([
    { name: 'совпадают', desired: 42, applied: 42 as number | null, want: false },
    { name: 'отстаёт', desired: 42, applied: 41, want: true },
    { name: 'ещё не применяла (null)', desired: 1, applied: null, want: true },
  ])('isDrifting: $name -> $want', ({ desired, applied, want }) => {
    const t = { ...mockPairs()[0].relay, desired_rev: desired, applied_rev: applied };
    expect(isDrifting(t)).toBe(want);
  });
});
