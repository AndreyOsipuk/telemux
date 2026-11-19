// Серверы и каталог полей по контракту docs/adr/0005-servers.md и бэкенд для mockFetch.
import type { Server, ServerEndpoint, ServerField, ServerInput } from '@/api/types';
import { machine, NOW_ISO, pageOf, urlParam, urlPath } from './fleet-fixtures';

export function endpoint(protocol: ServerEndpoint['protocol'], over: Partial<ServerEndpoint> = {}): ServerEndpoint {
  return {
    protocol,
    host: `${protocol}.example.com`,
    port: 443,
    tls_domain: protocol === 'ee' ? 'cdn.example.org' : '',
    via_relay: true,
    label: '',
    sort_order: 100,
    enabled: true,
    note: '',
    ...over,
  };
}

export function server(no: number, over: Partial<Server> = {}): Server {
  return {
    no,
    name: `Страна ${no}`,
    emoji: '🇩🇪',
    country: 'de',
    engine: 'telemt',
    relay_code: 'relay-a',
    node_code: `exit-${no}`,
    state: 'active',
    relay_only: false,
    node_retired: false,
    tls_domain: 'cdn.example.org',
    extra_tls_domains: [],
    user_max_tcp_conns: null,
    user_max_unique_ips: null,
    settings: {},
    note: '',
    endpoints: [
      endpoint('ee', { id: no * 10 + 1, sort_order: 10 }),
      endpoint('dd', { id: no * 10 + 2, sort_order: 20, via_relay: false }),
      endpoint('web', { id: no * 10 + 3, sort_order: 30, enabled: false }),
    ],
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-01T00:00:00Z',
    live: {
      relay: { code: 'relay-a', seen_at: NOW_ISO(), connections: 30, users_online: 5 },
      node: { code: `exit-${no}`, seen_at: NOW_ISO(), connections: 12, users_online: 4 },
    },
    ...over,
  };
}

const field = (over: Partial<ServerField> & Pick<ServerField, 'key' | 'engine' | 'type'>): ServerField => ({
  group: 'Общее',
  default: '',
  apply: 'reload',
  danger: false,
  title_ru: over.key,
  title_en: over.key,
  hint_ru: '',
  hint_en: '',
  ...over,
});

export const FIELDS: ServerField[] = [
  field({
    key: 'censorship.mask_port',
    engine: 'telemt',
    group: 'Маскировка',
    type: 'int',
    default: '443',
    min: 1,
    max: 65535,
    apply: 'restart',
    title_ru: 'Порт маскировки',
    title_en: 'Mask port',
    hint_ru: 'Порт сайта маскировки',
  }),
  field({
    key: 'general.fast_mode',
    engine: 'telemt',
    type: 'bool',
    default: '1',
    apply: 'reload',
    title_ru: 'Быстрый режим',
    title_en: 'Fast mode',
  }),
  field({
    key: 'general.secret_mode',
    engine: 'telemt',
    type: 'enum',
    enum: ['classic', 'secure'],
    default: 'classic',
    apply: 'instant',
    danger: true,
    title_ru: 'Режим секрета релея',
    title_en: 'Relay secret mode',
  }),
  field({
    key: 'mtg.concurrency',
    engine: 'mtg',
    group: 'Сеть',
    type: 'int',
    default: '8192',
    apply: 'restart',
    title_ru: 'Параллельность mtg',
    title_en: 'mtg concurrency',
  }),
];

export type Reply = { status?: number; body?: unknown };

export interface ServersBackendOpts {
  servers?: Server[];
  fields?: ServerField[];
  master?: boolean;
  create?: (body: ServerInput) => Reply;
  update?: (no: number, body: ServerInput) => Reply;
  remove?: (no: number) => Reply;
}

export function serversBackend(o: ServersBackendOpts = {}) {
  const servers = o.servers ?? [server(1), server(2, { engine: 'mtg', state: 'disabled' })];
  const fields = o.fields ?? FIELDS;
  return (url: string, init?: RequestInit): Reply | undefined => {
    const path = urlPath(url);
    const method = init?.method ?? 'GET';
    if (url === '/api/role') return { body: { role: 'master', is_master: o.master ?? true } };
    if (path === '/api/v1/machines') {
      const kind = urlParam(url, 'kind');
      const all = kind === 'relay' ? [machine('relay-a', { kind: 'relay' }), machine('relay-b', { kind: 'relay' })] : [machine('exit-1'), machine('exit-9')];
      return { body: pageOf(all, url) };
    }
    if (path === '/api/v1/servers/fields') return { body: { data: fields } };
    if (path === '/api/v1/servers' && method === 'GET') {
      const engine = urlParam(url, 'engine');
      const state = urlParam(url, 'state');
      const list = servers.filter((s) => (!engine || s.engine === engine) && (!state || s.state === state));
      return { body: pageOf(list, url) };
    }
    if (path === '/api/v1/servers' && method === 'POST') {
      const body = JSON.parse(String(init?.body)) as ServerInput;
      return o.create ? o.create(body) : { status: 201, body: { ...server(body.no), ...body } };
    }
    const m = /^\/api\/v1\/servers\/(\d+)$/.exec(path);
    if (m && method === 'PUT') {
      const body = JSON.parse(String(init?.body)) as ServerInput;
      return o.update ? o.update(Number(m[1]), body) : { body: { ...server(body.no), ...body } };
    }
    if (m && method === 'DELETE') return o.remove ? o.remove(Number(m[1])) : { status: 204 };
    return undefined;
  };
}
