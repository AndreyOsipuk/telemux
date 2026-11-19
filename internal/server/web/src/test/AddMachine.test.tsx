// Тесты добавления машины (POST /api/v1/targets) и повторной команды установки
// (POST /api/v1/targets/{code}/join) по схемам NewTarget/JoinCommand из api/openapi.yaml.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import dayjs from 'dayjs';

import { qk } from '@/api/queries';
import type { FirewallStatus, JoinCommand, Machine, NewTarget, Pair } from '@/api/types';
import ru from '@/i18n/ru.json';
import { IPV4_RE, isSocks, toNewTarget } from '@/pages/nodes/AddMachineModal';
import NodesPage from '@/pages/nodes/NodesPage';
import PairsPage from '@/pages/pairs/PairsPage';
import { machine, pageOf, pairMachine, urlPath } from './fleet-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

const COMMAND = "curl -fsS --pinnedpubkey 'sha256//abc=' https://198.51.100.1:18443/v1/join/tok123 | bash";
const EXPIRES = '2026-11-19T13:45:00Z';

function joinCommand(over: Partial<JoinCommand> = {}): JoinCommand {
  return { target: 'exit-new', command: COMMAND, expires_at: EXPIRES, firewall: 'opened', ...over };
}

const RELAYS: Pair[] = [
  { relay: pairMachine('relay-a', { kind: 'relay' }), exits: [pairMachine('exit-a1', { relay_code: 'relay-a' })] },
  { relay: pairMachine('relay-b', { kind: 'relay' }), exits: [] },
  { relay: null, exits: [pairMachine('panel-1', { kind: 'panel' })] },
];

type Reply = { status?: number; body?: unknown };

interface BackendOpts {
  machines?: Machine[];
  pairs?: Pair[];
  create?: (body: NewTarget) => Reply;
  join?: (code: string) => Reply;
  master?: boolean;
}

function backend(o: BackendOpts = {}) {
  const machines = o.machines ?? [];
  const pairs = o.pairs ?? RELAYS;
  return (url: string, init?: RequestInit): Reply | undefined => {
    const path = urlPath(url);
    const method = init?.method ?? 'GET';
    if (url === '/api/role') return { body: { role: 'master', is_master: o.master ?? true } };
    if (url === '/api/nodes') return { body: { nodes: [] } };
    if (path === '/api/v1/machines') return { body: pageOf(machines, url) };
    if (path === '/api/v1/pairs') return { body: pageOf(pairs, url) };
    if (path === '/api/v1/targets' && method === 'POST') {
      const body = JSON.parse(String(init?.body)) as NewTarget;
      return o.create ? o.create(body) : { status: 201, body: joinCommand({ target: body.code }) };
    }
    const m = /^\/api\/v1\/targets\/([^/]+)\/join$/.exec(path);
    if (m && method === 'POST') {
      const code = decodeURIComponent(m[1]);
      return o.join ? o.join(code) : { body: joinCommand({ target: code, firewall: 'not_needed' }) };
    }
    return undefined;
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

async function openForm(user: ReturnType<typeof userEvent.setup>) {
  const btn = await screen.findByRole('button', { name: new RegExp(ru.addMachine.open) });
  await waitFor(() => expect(btn).toBeEnabled());
  await user.click(btn);
  return screen.findByRole('dialog');
}

async function chooseRelay(user: ReturnType<typeof userEvent.setup>, dialog: HTMLElement, code: string) {
  await user.click(within(dialog).getByRole('combobox', { name: ru.addMachine.relay }));
  const option = await screen.findByTitle(code);
  await user.click(option);
}

async function fillExit(user: ReturnType<typeof userEvent.setup>, dialog: HTMLElement, code = 'exit-new') {
  await user.type(within(dialog).getByLabelText(ru.addMachine.code), code);
  await chooseRelay(user, dialog, 'relay-a');
  await user.type(within(dialog).getByLabelText(ru.addMachine.address), '203.0.113.10');
  await user.type(within(dialog).getByLabelText(ru.addMachine.socks), '10872');
}

const posts = (fn: ReturnType<typeof mockFetch>, path: string) =>
  fn.mock.calls.filter(([u, init]) => urlPath(String(u)) === path && init?.method === 'POST');

describe('правила полей', () => {
  it.each([
    ['203.0.113.10', true],
    ['0.0.0.0', true],
    ['255.255.255.255', true],
    ['256.1.1.1', false],
    ['1.2.3', false],
    ['01.2.3.4', false],
    ['1.2.3.4 ', false],
    ['exit.example.com', false],
  ])('IPv4 %s -> %s', (v, ok) => {
    expect(IPV4_RE.test(v)).toBe(ok);
  });

  it.each([
    ['socks5://127.0.0.1:10872', true],
    ['socks5://127.0.0.1:', false],
    ['socks5://127.0.0.1:0', false],
    ['socks5://127.0.0.1:70000', false],
    ['http://127.0.0.1:10872', false],
  ])('socks %s -> %s', (v, ok) => {
    expect(isSocks(v)).toBe(ok);
  });

  it('релею не отправляются поля выхода', () => {
    expect(
      toNewTarget({ code: ' relay-x ', kind: 'relay', resources: [], relay_code: 'r', address: '1.1.1.1', bridge_socks: 's' }),
    ).toEqual({ code: 'relay-x', kind: 'relay', resources: [] });
  });
});

describe('Добавить машину: форма', () => {
  it('поля выхода есть только у выхода; релеи - из /api/v1/pairs', async () => {
    mockFetch(backend());
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    expect(within(dialog).getByLabelText(ru.addMachine.code)).toBeInTheDocument();
    expect(within(dialog).getByLabelText(ru.addMachine.resource.telemt)).toBeChecked();
    expect(within(dialog).getByLabelText(ru.addMachine.resource.mtg)).not.toBeChecked();
    // По умолчанию - выход.
    expect(within(dialog).getByLabelText(ru.addMachine.address)).toBeInTheDocument();
    expect(within(dialog).getByLabelText(ru.addMachine.socks)).toHaveValue('socks5://127.0.0.1:');
    expect(within(dialog).getByText(ru.addMachine.socksHint)).toBeInTheDocument();

    await user.click(within(dialog).getByRole('combobox', { name: ru.addMachine.relay }));
    expect(await screen.findByTitle('relay-a')).toBeInTheDocument();
    expect(screen.getByTitle('relay-b')).toBeInTheDocument();
    expect(screen.queryByTitle('panel-1')).not.toBeInTheDocument();
    await user.click(screen.getByTitle('relay-b'));

    await user.click(within(dialog).getByText(ru.kinds.relay));
    expect(within(dialog).queryByLabelText(ru.addMachine.address)).not.toBeInTheDocument();
    expect(within(dialog).queryByLabelText(ru.addMachine.socks)).not.toBeInTheDocument();
    expect(within(dialog).queryByRole('combobox', { name: ru.addMachine.relay })).not.toBeInTheDocument();

    await user.click(within(dialog).getByText(ru.kinds.exit));
    expect(within(dialog).getByLabelText(ru.addMachine.address)).toBeInTheDocument();
  });

  it('релей: в теле только код, роль и ресурсы', async () => {
    const fn = mockFetch(backend());
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await user.type(within(dialog).getByLabelText(ru.addMachine.code), 'relay-c');
    await user.click(within(dialog).getByText(ru.kinds.relay));
    await user.click(within(dialog).getByLabelText(ru.addMachine.resource.mtg));
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    await screen.findByTestId('join-command');
    const [[, init]] = posts(fn, '/api/v1/targets');
    expect(JSON.parse(String(init?.body))).toEqual({ code: 'relay-c', kind: 'relay', resources: ['telemt.users', 'mtg.config'] });
  });

  it('клиентская проверка кода, IP и socks - запрос не уходит', async () => {
    const fn = mockFetch(backend());
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));
    expect(await within(dialog).findByText(ru.addMachine.codeRequired)).toBeInTheDocument();
    expect(within(dialog).getByText(ru.addMachine.relayRequired)).toBeInTheDocument();
    expect(within(dialog).getByText(ru.addMachine.addressRequired)).toBeInTheDocument();
    expect(within(dialog).getByText(ru.addMachine.socksInvalid)).toBeInTheDocument();

    await user.type(within(dialog).getByLabelText(ru.addMachine.code), 'Exit_1');
    await user.type(within(dialog).getByLabelText(ru.addMachine.address), '300.1.1.1');
    expect(await within(dialog).findByText(ru.addMachine.codeInvalid)).toBeInTheDocument();
    expect(await within(dialog).findByText(ru.addMachine.addressInvalid)).toBeInTheDocument();

    await user.clear(within(dialog).getByLabelText(ru.addMachine.code));
    await user.type(within(dialog).getByLabelText(ru.addMachine.code), '1exit');
    expect(await within(dialog).findByText(ru.addMachine.codeInvalid)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    expect(posts(fn, '/api/v1/targets')).toHaveLength(0);
  });
});

describe('Добавить машину: результат', () => {
  // Новый релей без выходов: машина заведена, но команды нет (command: "").
  it('релей без выходов: вместо команды - подсказка добавить выход', async () => {
    mockFetch(
      backend({
        create: (body) => ({ status: 201, body: joinCommand({ target: body.code, command: '', firewall: 'not_needed' }) }),
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);
    expect(await screen.findByText(ru.nodes.fleetEmpty)).toBeInTheDocument();
    const dialog = await openForm(user);
    await user.type(within(dialog).getByLabelText(ru.addMachine.code), 'relay-new');
    await user.click(within(dialog).getByText(ru.kinds.relay));
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));
    const note = await screen.findByTestId('join-no-command');
    expect(note).toHaveTextContent(ru.join.noExitsTitle.replace('{{code}}', 'relay-new'));
    expect(screen.queryByTestId('join-command-text')).not.toBeInTheDocument();
  });

  it('выход: тело запроса, команда, срок, инструкция, копирование; списки обновляются', async () => {
    const created: Machine[] = [];
    const fn = mockFetch(
      backend({
        machines: created,
        create: (body) => {
          created.push(machine(body.code, { relay_code: body.relay_code, seen_at: null, observed: [] }));
          return { status: 201, body: joinCommand({ target: body.code }) };
        },
      }),
    );
    const user = userEvent.setup();
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const { queryClient } = renderWithProviders(<NodesPage />);
    expect(await screen.findByText(ru.nodes.fleetEmpty)).toBeInTheDocument();
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries');

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    const view = await screen.findByTestId('join-command');
    const [[, init]] = posts(fn, '/api/v1/targets');
    expect(JSON.parse(String(init?.body))).toEqual({
      code: 'exit-new',
      kind: 'exit',
      resources: ['telemt.users'],
      relay_code: 'relay-a',
      address: '203.0.113.10',
      bridge_socks: 'socks5://127.0.0.1:10872',
    });
    expect(within(view).getByTestId('join-command-text')).toHaveTextContent(COMMAND);
    expect(within(view).getByTestId('join-expires')).toHaveTextContent(
      ru.join.expires.replace('{{time}}', dayjs(EXPIRES).format('HH:mm')),
    );
    expect(within(view).getByText(ru.join.instruction)).toBeInTheDocument();
    expect(screen.getByText(ru.addMachine.doneTitle.replace('{{code}}', 'exit-new'))).toBeInTheDocument();

    await user.click(within(view).getByRole('button', { name: new RegExp(ru.join.copy) }));
    expect(writeText).toHaveBeenCalledWith(COMMAND);
    expect(await screen.findByText(ru.join.copied)).toBeInTheDocument();

    // Машина сразу появляется в «Машинах флота»: machines и pairs инвалидированы.
    const matched = new Set<unknown>();
    for (const [filters] of invalidate.mock.calls) {
      for (const q of queryClient.getQueryCache().findAll(filters)) matched.add(q.queryKey[0]);
    }
    expect([...matched]).toEqual(expect.arrayContaining(['machines', 'pairs']));
    expect(await within(screen.getByTestId('fleet-machines')).findByText('exit-new')).toBeInTheDocument();
  });

  it('буфер обмена недоступен - команда выделяется для ручного копирования', async () => {
    mockFetch(backend());
    const user = userEvent.setup();
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));
    const view = await screen.findByTestId('join-command');

    await user.click(within(view).getByRole('button', { name: new RegExp(ru.join.copy) }));
    expect(window.getSelection()?.toString()).toBe(COMMAND);
    expect(await screen.findByText(ru.join.copyManually)).toBeInTheDocument();
  });

  it.each<[FirewallStatus | undefined, string | null, string | null]>([
    ['opened', ru.join.firewall.opened, 'opened'],
    ['not_configured', ru.join.firewall.not_configured, 'not_configured'],
    ['failed', ru.join.firewall.failed, 'failed'],
    ['not_needed', null, null],
    [undefined, null, null],
  ])('файрвол %s', async (firewall, text, attr) => {
    mockFetch(backend({ create: (b) => ({ status: 201, body: joinCommand({ target: b.code, firewall }) }) }));
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));
    const view = await screen.findByTestId('join-command');

    if (text === null) {
      expect(within(view).queryByTestId('join-firewall')).not.toBeInTheDocument();
    } else {
      const note = within(view).getByTestId('join-firewall');
      expect(note).toHaveTextContent(text);
      expect(note).toHaveAttribute('data-firewall', attr);
    }
  });
});

describe('Добавить машину: ошибки', () => {
  it('400 про адрес - текст сервера под полем', async () => {
    mockFetch(backend({ create: () => ({ status: 400, body: { code: 'bad_request', message: 'address: не публичный IPv4' } }) }));
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    const err = await within(dialog).findByText('address: не публичный IPv4');
    expect(err.closest('.ant-form-item')).toContainElement(within(dialog).getByLabelText(ru.addMachine.address));
    expect(within(dialog).getByText(ru.addMachine.checkFields)).toBeInTheDocument();
    expect(screen.queryByTestId('join-command')).not.toBeInTheDocument();
  });

  it('400 без привязки к полю - баннер над формой', async () => {
    mockFetch(backend({ create: () => ({ status: 400, body: { code: 'bad_request', message: 'неизвестный ресурс' } }) }));
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    expect(await within(dialog).findByText(ru.addMachine.failed)).toBeInTheDocument();
    expect(within(dialog).getByText('неизвестный ресурс')).toBeInTheDocument();
  });

  it.each([
    'машина exit-new уже есть',
    'у релея relay-a нет выходов с адресом и socks',
    'на мастере не задан внешний адрес (TELEMUX_AGENT_ADDR)',
  ])('409 conflict - текст сервера: %s', async (msg) => {
    mockFetch(backend({ create: () => ({ status: 409, body: { code: 'conflict', message: msg } }) }));
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    expect(await within(dialog).findByText(msg)).toBeInTheDocument();
    expect(within(dialog).getByText(ru.addMachine.failed)).toBeInTheDocument();
    // Форма остаётся - можно исправить и отправить снова.
    expect(within(dialog).getByLabelText(ru.addMachine.code)).toHaveValue('exit-new');
  });

  it('409 not_master - «эта машина не мастер» с адресом мастера', async () => {
    mockFetch(
      backend({
        create: () => ({ status: 409, body: { code: 'not_master', message: 'not master', master_url: 'https://m1:8080' } }),
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    expect(await within(dialog).findByText(ru.errors.notMasterAt.replace('{{url}}', 'https://m1:8080'))).toBeInTheDocument();
  });

  it('401 - «сессия истекла»', async () => {
    mockFetch(backend({ create: () => ({ status: 401, body: { code: 'unauthorized', message: 'unauthorized' } }) }));
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    expect(await within(dialog).findByText(ru.errors.unauthorized)).toBeInTheDocument();
  });

  it('500 - понятный текст без подробностей сервера', async () => {
    mockFetch(backend({ create: () => ({ status: 500, body: { code: 'internal', message: 'pq: boom' } }) }));
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const dialog = await openForm(user);
    await fillExit(user, dialog);
    await user.click(within(dialog).getByRole('button', { name: ru.addMachine.submit }));

    expect(await within(dialog).findByText(ru.errors.server.replace('{{status}}', '500'))).toBeInTheDocument();
    expect(screen.queryByText(/pq: boom/)).not.toBeInTheDocument();
  });

  it('на реплике кнопка выключена', async () => {
    mockFetch(backend({ master: false }));
    renderWithProviders(<NodesPage />);

    const btn = await screen.findByRole('button', { name: new RegExp(ru.addMachine.open) });
    await screen.findByText(ru.nodes.fleetEmpty);
    expect(btn).toBeDisabled();
  });
});

describe('Команда установки для существующей машины', () => {
  const join = (code: string) => ru.join.actionOf.replace('{{code}}', code);

  it('из «Машин флота»: предупреждение, новая команда, прежняя сгорела', async () => {
    const fn = mockFetch(
      backend({
        machines: [machine('exit-silent', { seen_at: null, observed: [] }), machine('relay-a', { kind: 'relay' })],
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<NodesPage />);

    const btn = await screen.findByRole('button', { name: join('exit-silent') });
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(ru.join.renewWarning)).toBeInTheDocument();
    expect(within(dialog).queryByText(ru.join.hasAgentNote)).not.toBeInTheDocument();
    expect(posts(fn, '/api/v1/targets/exit-silent/join')).toHaveLength(0);

    await user.click(within(dialog).getByRole('button', { name: ru.join.issue }));

    expect(await within(dialog).findByTestId('join-command-text')).toHaveTextContent(COMMAND);
    expect(within(dialog).getByText(ru.join.renewedNote)).toBeInTheDocument();
    expect(posts(fn, '/api/v1/targets/exit-silent/join')).toHaveLength(1);
  });

  it('из «Пар»: клик не открывает историю; 409 - текст сервера', async () => {
    mockFetch(
      backend({
        pairs: RELAYS,
        join: () => ({ status: 409, body: { code: 'conflict', message: 'у релея relay-b нет выходов с адресом и socks' } }),
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<PairsPage />);

    const btn = await screen.findByRole('button', { name: join('relay-b') });
    await waitFor(() => expect(btn).toBeEnabled());
    await user.click(btn);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(ru.join.hasAgentNote)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: ru.join.issue }));

    expect(await within(dialog).findByText('у релея relay-b нет выходов с адресом и socks')).toBeInTheDocument();
    expect(within(dialog).getByText(ru.join.failed)).toBeInTheDocument();
    // История применений (Drawer) не открылась.
    expect(screen.queryByText(ru.history.title.replace('{{code}}', 'relay-b'))).not.toBeInTheDocument();
  });

  it('у реплики действие выключено', async () => {
    mockFetch(backend({ master: false, machines: [machine('exit-1')] }));
    const { queryClient } = renderWithProviders(<NodesPage />);

    const btn = await screen.findByRole('button', { name: join('exit-1') });
    await waitFor(() => expect(queryClient.getQueryData(qk.role)).toBeDefined());
    expect(btn).toBeDisabled();
  });
});
