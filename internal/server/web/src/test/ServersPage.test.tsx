// Экран «Серверы» (ADR 0005): список с точками входа и живыми цифрами, фильтры, форма
// создания и правки, настройки из каталога полей, подтверждение опасных правок, удаление.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import type { ServerInput } from '@/api/types';
import ru from '@/i18n/ru.json';
import ServersPage from '@/pages/servers/ServersPage';
import { linkPreview, riskyChanges, toServerInput, emptyFormValues } from '@/pages/servers/serverModel';
import { urlPath } from './fleet-fixtures';
import { FIELDS, endpoint, server, serversBackend } from './server-fixtures';
import { mockFetch, renderWithProviders } from './test-utils';

type User = ReturnType<typeof userEvent.setup>;

afterEach(() => {
  vi.unstubAllGlobals();
});

const calls = (fn: ReturnType<typeof mockFetch>, method: string, path?: string) =>
  fn.mock.calls.filter(
    ([u, init]) => (init?.method ?? 'GET') === method && (path === undefined || urlPath(String(u)) === path),
  );
const bodyOf = (call: unknown[]) => JSON.parse(String((call[1] as RequestInit).body)) as ServerInput;
const listCalls = (fn: ReturnType<typeof mockFetch>) =>
  calls(fn, 'GET', '/api/v1/servers').map(([u]) => String(u));

function row(no: number): HTMLElement {
  return document.querySelector<HTMLElement>(`tr[data-row-key="${no}"]`)!;
}

async function pick(user: User, combobox: HTMLElement, title: string) {
  await user.click(combobox);
  const options = await screen.findAllByTitle(title);
  await user.click(options.at(-1)!);
}

async function openCreate(user: User) {
  const btn = await screen.findByRole('button', { name: new RegExp(ru.servers.add) });
  await waitFor(() => expect(btn).toBeEnabled());
  await user.click(btn);
  const dialog = await screen.findByRole('dialog');
  await within(dialog).findByLabelText(ru.servers.name);
  return dialog;
}

async function openEdit(user: User, no: number) {
  await screen.findByText(`Страна ${no}`);
  await user.click(within(row(no)).getByRole('button', { name: ru.servers.edit }));
  const dialog = await screen.findByRole('dialog');
  await within(dialog).findByLabelText(ru.servers.name);
  return dialog;
}

const saveButton = (dialog: HTMLElement, name: string) => within(dialog).getByRole('button', { name });

describe('модель сервера', () => {
  it('превью ссылки без секрета', () => {
    expect(linkPreview({ protocol: 'ee', host: 'a.example.com', port: 8443 })).toBe(
      'tg://proxy?server=a.example.com&port=8443&secret=ee…',
    );
    expect(linkPreview({ protocol: 'dd', host: 'd.example.com', port: 443 })).toBe(
      'tg://proxy?server=d.example.com&port=443&secret=dd…',
    );
    // web - как у бота: tg://webproxy без порта, секрет dd.
    expect(linkPreview({ protocol: 'web', host: 'w.example.com', port: 443 })).toBe(
      'tg://webproxy?server=w.example.com&secret=dd…',
    );
  });

  it('страна уходит в нижнем регистре (бэкенд: ^[a-z]{2}$)', () => {
    expect(toServerInput({ ...emptyFormValues(), name: 'X', country: 'NL' }, FIELDS).country).toBe('nl');
    expect(toServerInput({ ...emptyFormValues(), name: 'X', country: '' }, FIELDS).country).toBe('');
  });

  it('пустые настройки не пишутся, чужие для движка - тоже, неизвестные каталогу - сохраняются', () => {
    const v = {
      ...emptyFormValues(),
      no: 5,
      name: ' Германия ',
      settings: { 'censorship.mask_port': null, 'general.fast_mode': undefined, 'general.secret_mode': '', 'mtg.concurrency': 10 },
    };
    const body = toServerInput(v, FIELDS, { 'legacy.key': 'x' });
    expect(body.settings).toEqual({ 'legacy.key': 'x' });
    expect(body.name).toBe('Германия');
    expect(toServerInput({ ...v, settings: { 'general.fast_mode': false, 'censorship.mask_port': 8443 } }, FIELDS).settings).toEqual({
      'general.fast_mode': '0',
      'censorship.mask_port': '8443',
    });
  });

  it('подтверждение: опасное поле, домен, ee-точка, выключение; dd и подпись - без подтверждения', () => {
    const before = server(1);
    const same = toServerInput({ ...emptyFormValues() }, FIELDS);
    expect(riskyChanges(before, { ...before, endpoints: before.endpoints.map((e) => (e.protocol === 'dd' ? { ...e, port: 9 } : e)) }, FIELDS)).toEqual([]);
    expect(riskyChanges(before, { ...before, note: 'x' }, FIELDS)).toEqual([]);
    const kinds = riskyChanges(
      before,
      {
        ...before,
        state: 'disabled',
        tls_domain: 'other.example.org',
        settings: { 'general.secret_mode': 'secure' },
        endpoints: before.endpoints.filter((e) => e.protocol !== 'ee'),
      },
      FIELDS,
    ).map((c) => c.kind);
    expect(kinds).toEqual(['danger', 'tlsDomain', 'eeEndpoints', 'disable']);
    expect(same.settings).toEqual({});
  });
});

describe('ServersPage: список', () => {
  it('баннер «справочное», строки, точки входа и живые цифры', async () => {
    const fetchFn = mockFetch(serversBackend());
    renderWithProviders(<ServersPage />);

    expect(await screen.findByText('Страна 1')).toBeInTheDocument();
    expect(screen.getByText('Страна 2')).toBeInTheDocument();
    expect(screen.getByText(ru.servers.referenceOnly)).toBeInTheDocument();
    expect(listCalls(fetchFn)[0]).toBe('/api/v1/servers?limit=20&offset=0');

    const eps = within(row(1)).getByTestId('endpoints-1');
    // Включённые точки - строками «протокол, путь, порт»; выключенные - одной серой строкой.
    const lines = Array.from(eps.querySelectorAll<HTMLElement>('[data-endpoint]'));
    expect(lines.map((l) => l.dataset.endpoint)).toEqual(['ee', 'dd']);
    expect(lines[0]).toHaveTextContent(/ee.*:443/);
    expect(within(row(1)).getByTestId('endpoints-off-1')).toHaveTextContent('выключено: 1');

    const live = within(row(1)).getByTestId('live-1');
    expect(live).toHaveTextContent('42 / 9');
    expect(live.dataset.status).toBe('online');
    expect(within(row(1)).getByText('relay-a')).toBeInTheDocument();
    expect(within(row(1)).getByText('exit-1')).toBeInTheDocument();
    expect(within(row(1)).getByText(ru.servers.states.active)).toBeInTheDocument();
    expect(within(row(2)).getByText(ru.servers.states.disabled)).toBeInTheDocument();
  });

  it('давно не видели машину - серый статус; выведенная машина помечена', async () => {
    const old = new Date(Date.now() - 3_600_000).toISOString();
    mockFetch(
      serversBackend({
        servers: [
          server(1, {
            node_retired: true,
            live: { relay: { code: 'relay-a', seen_at: old, connections: 3, users_online: 1 }, node: null },
          }),
        ],
      }),
    );
    renderWithProviders(<ServersPage />);
    await screen.findByText('Страна 1');
    expect(within(row(1)).getByTestId('live-1').dataset.status).toBe('offline');
    expect(within(row(1)).getByText(ru.servers.retired)).toBeInTheDocument();
  });

  it('фильтры движка и состояния уходят в запрос и сбрасывают страницу', async () => {
    const fetchFn = mockFetch(serversBackend());
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    await screen.findByText('Страна 1');

    await pick(user, screen.getByRole('combobox', { name: ru.servers.filterEngine }), 'mtg-multi');
    await waitFor(() => expect(listCalls(fetchFn)).toContain('/api/v1/servers?engine=mtg&limit=20&offset=0'));
    await waitFor(() => expect(screen.queryByText('Страна 1')).not.toBeInTheDocument());

    await pick(user, screen.getByRole('combobox', { name: ru.servers.filterState }), ru.servers.states.disabled);
    await waitFor(() =>
      expect(listCalls(fetchFn)).toContain('/api/v1/servers?engine=mtg&state=disabled&limit=20&offset=0'),
    );
  });
});

describe('ServersPage: удаление', () => {
  it('удалить можно только выключенный сервер', async () => {
    const fetchFn = mockFetch(serversBackend());
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    await screen.findByText('Страна 1');

    const del1 = within(row(1)).getByRole('button', { name: ru.common.delete });
    const del2 = within(row(2)).getByRole('button', { name: ru.common.delete });
    await waitFor(() => expect(del2).toBeEnabled());
    expect(del1).toBeDisabled();

    await user.click(del2);
    const pop = (await screen.findByText(ru.servers.deleteConfirm.replace('{{no}}', '2').replace('{{name}}', 'Страна 2'))).closest<HTMLElement>(
      '.ant-popover, .ant-popconfirm',
    )!;
    await user.click(within(pop).getByRole('button', { name: ru.common.delete }));
    await waitFor(() => expect(calls(fetchFn, 'DELETE', '/api/v1/servers/2')).toHaveLength(1));
    expect(await screen.findByText(ru.servers.deleted.replace('{{no}}', '2'))).toBeInTheDocument();
  });

  it('409 при удалении - текст сервера', async () => {
    mockFetch(
      serversBackend({ remove: () => ({ status: 409, body: { code: 'conflict', message: 'сервер 2 не выключен' } }) }),
    );
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    await screen.findByText('Страна 2');
    const del2 = within(row(2)).getByRole('button', { name: ru.common.delete });
    await waitFor(() => expect(del2).toBeEnabled());
    await user.click(del2);
    const pop = (await screen.findByText(/Удалить сервер 2/)).closest<HTMLElement>('.ant-popover, .ant-popconfirm')!;
    await user.click(within(pop).getByRole('button', { name: ru.common.delete }));
    expect(await screen.findByText('сервер 2 не выключен')).toBeInTheDocument();
  });
});

describe('ServerForm: создание', () => {
  it('POST с корректным телом, пустые настройки не пишутся', async () => {
    const fetchFn = mockFetch(serversBackend());
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    const dialog = await openCreate(user);

    await user.type(within(dialog).getByLabelText(ru.servers.no), '7');
    await user.type(within(dialog).getByLabelText(ru.servers.name), 'Нидерланды');
    // Ввели заглавными - в поле и в запросе нижний регистр.
    await user.type(within(dialog).getByLabelText(ru.servers.country), 'NL');
    expect(within(dialog).getByLabelText(ru.servers.country)).toHaveValue('nl');
    await pick(user, within(dialog).getByRole('combobox', { name: ru.servers.relay }), 'relay-b');
    await pick(user, within(dialog).getByRole('combobox', { name: ru.servers.node }), 'exit-9');

    await user.click(within(dialog).getByRole('tab', { name: ru.servers.form.tabLimits }));
    await user.type(within(dialog).getByLabelText(ru.servers.tlsDomain), 'cdn.example.org');
    await user.type(within(dialog).getByLabelText(ru.servers.maxTcpConns), '8');

    await user.click(within(dialog).getByRole('tab', { name: ru.servers.form.tabEndpoints }));
    await user.click(within(dialog).getByRole('button', { name: new RegExp(ru.servers.form.addEndpoint) }));
    const ep = within(dialog).getByTestId('endpoint-0');
    await user.type(within(ep).getByLabelText(ru.servers.endpoint.host), 'nl.example.com');
    // Домен ee подставлен из домена маскировки сервера; превью - без секрета.
    expect(within(ep).getByLabelText(ru.servers.endpoint.tlsDomain)).toHaveValue('cdn.example.org');
    expect(within(ep).getByText('tg://proxy?server=nl.example.com&port=443&secret=ee…')).toBeInTheDocument();

    // Одну настройку задали, остальные остались пустыми.
    await user.click(within(dialog).getByRole('tab', { name: /Настройки telemt/ }));
    await user.type(within(dialog).getByLabelText(/Порт маскировки/), '8443');

    await user.click(saveButton(dialog, ru.servers.form.create));
    await waitFor(() => expect(calls(fetchFn, 'POST', '/api/v1/servers')).toHaveLength(1));
    const body = bodyOf(calls(fetchFn, 'POST', '/api/v1/servers')[0]);
    expect(body).toMatchObject({
      no: 7,
      name: 'Нидерланды',
      country: 'nl',
      engine: 'telemt',
      relay_code: 'relay-b',
      node_code: 'exit-9',
      state: 'active',
      relay_only: false,
      node_retired: false,
      tls_domain: 'cdn.example.org',
      extra_tls_domains: [],
      user_max_tcp_conns: 8,
      user_max_unique_ips: null,
      settings: { 'censorship.mask_port': '8443' },
    });
    expect(body.endpoints).toEqual([
      {
        protocol: 'ee',
        host: 'nl.example.com',
        port: 443,
        tls_domain: 'cdn.example.org',
        via_relay: true,
        label: '',
        sort_order: 100,
        enabled: true,
        note: '',
      },
    ]);
    expect(body).not.toHaveProperty('live');
    expect(await screen.findByText(ru.servers.created.replace('{{no}}', '7'))).toBeInTheDocument();
  });

  it('настройки строятся из каталога и фильтруются по движку', async () => {
    mockFetch(serversBackend());
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    const dialog = await openCreate(user);

    await user.click(within(dialog).getByRole('tab', { name: /Настройки telemt/ }));
    expect(within(dialog).getByLabelText(/Порт маскировки/)).toHaveAttribute('placeholder', '443');
    expect(within(dialog).getByLabelText(/Быстрый режим/)).toBeInTheDocument();
    expect(within(dialog).queryByLabelText(/Параллельность mtg/)).not.toBeInTheDocument();
    // Способ применения у поля: рестарт рвёт подключения.
    expect(within(dialog).getAllByText(ru.servers.apply.restart).length).toBeGreaterThan(0);
    expect(within(dialog).getByTestId('settings-group-Маскировка')).toBeInTheDocument();

    await user.click(within(dialog).getByRole('tab', { name: ru.servers.form.tabMain }));
    await user.click(within(dialog).getByText('mtg-multi'));
    await user.click(within(dialog).getByRole('tab', { name: /Настройки mtg-multi/ }));
    expect(within(dialog).getByLabelText(/Параллельность mtg/)).toHaveAttribute('placeholder', '8192');
    expect(within(dialog).queryByLabelText(/Порт маскировки/)).not.toBeInTheDocument();
  });

  it('400 от сервера показывается у формы, форма остаётся открытой', async () => {
    mockFetch(
      serversBackend({
        create: () => ({ status: 400, body: { code: 'unknown_machine', message: 'машина exit-9 не вида exit' } }),
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    const dialog = await openCreate(user);
    await user.type(within(dialog).getByLabelText(ru.servers.no), '7');
    await user.type(within(dialog).getByLabelText(ru.servers.name), 'Нидерланды');
    await user.click(saveButton(dialog, ru.servers.form.create));

    expect(await within(dialog).findByText('машина exit-9 не вида exit')).toBeInTheDocument();
    expect(within(dialog).getByText(ru.servers.errorCode.unknown_machine)).toBeInTheDocument();
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  it('пустой номер и название - запроса нет', async () => {
    const fetchFn = mockFetch(serversBackend());
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    const dialog = await openCreate(user);
    await user.click(saveButton(dialog, ru.servers.form.create));
    expect((await within(dialog).findAllByText(ru.servers.form.required)).length).toBeGreaterThan(0);
    expect(calls(fetchFn, 'POST', '/api/v1/servers')).toHaveLength(0);
  });
});

describe('ServerForm: правка', () => {
  it('PUT /api/v1/servers/{no}, движок заблокирован, номер не правится, id точек входа сохраняются', async () => {
    const fetchFn = mockFetch(
      serversBackend({ servers: [server(1, { settings: { 'censorship.mask_port': '8443', 'legacy.key': 'x' } })] }),
    );
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    const dialog = await openEdit(user, 1);

    expect(within(dialog).queryByLabelText(ru.servers.no)).not.toBeInTheDocument();
    expect(within(dialog).getByText(ru.servers.form.engineImmutable)).toBeInTheDocument();
    for (const r of within(dialog).getAllByRole('radio')) expect(r).toBeDisabled();

    const name = within(dialog).getByLabelText(ru.servers.name);
    await user.clear(name);
    await user.type(name, 'Германия');
    await user.click(saveButton(dialog, ru.servers.form.save));

    await waitFor(() => expect(calls(fetchFn, 'PUT', '/api/v1/servers/1')).toHaveLength(1));
    const body = bodyOf(calls(fetchFn, 'PUT', '/api/v1/servers/1')[0]);
    expect(body.no).toBe(1);
    expect(body.name).toBe('Германия');
    expect(body.engine).toBe('telemt');
    expect(body.settings).toEqual({ 'censorship.mask_port': '8443', 'legacy.key': 'x' });
    expect(body.endpoints.map((e) => e.id)).toEqual([11, 12, 13]);
    // Подтверждения не было: опасного не меняли.
    expect(screen.queryByTestId('server-changes')).not.toBeInTheDocument();
  });

  it('смена домена маскировки - подтверждение с перечнем, PUT только после «Сохранить»', async () => {
    const fetchFn = mockFetch(serversBackend());
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    const dialog = await openEdit(user, 1);

    await user.click(within(dialog).getByRole('tab', { name: ru.servers.form.tabLimits }));
    const domain = within(dialog).getByLabelText(ru.servers.tlsDomain);
    await user.clear(domain);
    await user.type(domain, 'new.example.org');
    await user.click(saveButton(dialog, ru.servers.form.save));

    const changes = await screen.findByTestId('server-changes');
    expect(changes).toHaveTextContent('cdn.example.org → new.example.org');
    expect(changes).toHaveTextContent('ee-ссылки всех пользователей сервера изменятся');
    expect(calls(fetchFn, 'PUT')).toHaveLength(0);

    const confirm = changes.closest<HTMLElement>('.ant-modal')!;
    await user.click(within(confirm).getByRole('button', { name: ru.servers.confirm.ok }));
    await waitFor(() => expect(calls(fetchFn, 'PUT', '/api/v1/servers/1')).toHaveLength(1));
    expect(bodyOf(calls(fetchFn, 'PUT', '/api/v1/servers/1')[0]).tls_domain).toBe('new.example.org');
  });

  it('удаление ee-точки - подтверждение', async () => {
    const fetchFn = mockFetch(
      serversBackend({ servers: [server(1, { endpoints: [endpoint('ee', { id: 1 }), endpoint('dd', { id: 2 })] })] }),
    );
    const user = userEvent.setup();
    renderWithProviders(<ServersPage />);
    const dialog = await openEdit(user, 1);
    await user.click(within(dialog).getByRole('tab', { name: ru.servers.form.tabEndpoints }));
    await user.click(within(within(dialog).getByTestId('endpoint-0')).getByRole('button', { name: ru.servers.form.removeEndpoint }));
    await user.click(saveButton(dialog, ru.servers.form.save));

    expect(await screen.findByTestId('server-changes')).toHaveTextContent(ru.servers.confirm.eeEndpoints);
    expect(calls(fetchFn, 'PUT')).toHaveLength(0);
  });
});
