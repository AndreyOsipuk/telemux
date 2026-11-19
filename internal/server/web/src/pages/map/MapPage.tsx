// «Карта»: как клиенты доходят до машин за рубежом. Слева - релеи (и «напрямую» для
// прямых точек входа), справа - машины за рубежом, линии - серверы: сервер N идёт от
// своего релея (или напрямую) к своей машине. Связи строятся из серверов, а не из
// targets.relay_code: одна машина бывает за разными релеями у разных серверов.
// Средняя колонка - серверы строго по номеру (путь «через релей» и «напрямую» - отдельные
// строки), релеи и машины за рубежом - напротив своих серверов. Цвет линии - состояние
// сервера, пунктир - все точки этого пути выключены. Наведение на машину или сервер
// подсвечивает его пути, остальное затемняется. Клик по машине - карточка, по серверу - форма.

import { useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Card, Empty, Space, Tag, Tooltip, Typography, theme } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';

import { useOverview, useServers } from '@/api/queries';
import type { Machine, Server, ServerState } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import MachineDrawer from '@/components/MachineDrawer';
import { RuntimeStatusCell } from '@/components/MachineCells';
import ServerForm from '@/pages/servers/ServerForm';
import { productName } from '@/utils/format';
import './MapPage.css';

// Псевдо-релей для точек входа напрямую и псевдо-машина для серверов с выведенной
// машиной за рубежом (node_retired): ребро такого сервера идёт только до релея.
export const DIRECT = '__direct__';
export const RETIRED = '__retired__';

export interface MapEdge {
  from: string; // код релея или DIRECT
  to: string; // код машины за рубежом
  servers: { server: Server; enabled: boolean }[];
}

// Путь одного сервера: от релея (или напрямую) к машине за рубежом.
export interface MapPath {
  id: string;
  server: Server;
  from: string;
  to: string;
  enabled: boolean;
}

export interface MapModel {
  relays: string[];
  exits: string[];
  edges: MapEdge[];
  // Пути по номеру сервера, через релей раньше прямого.
  paths: MapPath[];
  // Серверы на машине-релее (relay_code) - подпись узла: «telemt 1 · mtg 5».
  relayServers: Record<string, Server[]>;
}

// Имя релея в ссылках сервера: первая метка хоста точки через релей (pr5.example.org,
// pr5-ee.example.org -> pr5). Сервер N+4 (mtg) живёт на той же машине, что релей N.
export function relayDomain(s: Server): string | undefined {
  const host = s.endpoints.find((e) => e.via_relay && e.host)?.host;
  if (!host || /^[\d.]+$/.test(host)) return undefined;
  return host.split('.')[0].replace(/-(ee|dd|web)$/, '');
}

// Граф карты из серверов: ребро на каждый путь «релей -> машина» (через релей) и
// «напрямую -> машина»; серверы с одинаковым путём - на одном ребре.
export function buildMap(servers: Server[]): MapModel {
  const edges = new Map<string, MapEdge>();
  const relays = new Set<string>();
  const exits = new Set<string>();
  const add = (from: string, to: string, server: Server, enabled: boolean) => {
    const k = `${from}->${to}`;
    let e = edges.get(k);
    if (!e) {
      e = { from, to, servers: [] };
      edges.set(k, e);
    }
    const had = e.servers.find((x) => x.server.no === server.no);
    if (had) had.enabled ||= enabled;
    else e.servers.push({ server, enabled });
  };
  const relayServers: Record<string, Server[]> = {};
  for (const s of servers) {
    if (s.relay_code) (relayServers[s.relay_code] ??= []).push(s);
    if (!s.node_code) continue;
    const to = s.node_retired ? RETIRED : s.node_code;
    for (const ep of s.endpoints) {
      const from = ep.via_relay ? s.relay_code : DIRECT;
      // У выведенной машины прямых точек нет: клиенту некуда идти напрямую.
      if (!from || (from === DIRECT && s.node_retired)) continue;
      relays.add(from);
      exits.add(to);
      add(from, to, s, ep.enabled && s.state !== 'disabled');
    }
  }
  for (const list of Object.values(relayServers)) list.sort((a, b) => a.no - b.no);
  for (const e of edges.values()) e.servers.sort((a, b) => a.server.no - b.server.no);
  const paths: MapPath[] = [...edges.values()]
    .flatMap((e) => e.servers.map(({ server, enabled }) => ({ id: `${server.no}:${e.from}:${e.to}`, server, from: e.from, to: e.to, enabled })))
    .sort((a, b) => a.server.no - b.server.no || Number(a.from === DIRECT) - Number(b.from === DIRECT));
  // Релеи и машины за рубежом - по наименьшему номеру своего сервера, так узлы стоят
  // напротив своих серверов; «напрямую» и «выход выведен» - в конце колонки.
  const firstNo = (code: string, side: 'from' | 'to') => Math.min(...paths.filter((p) => p[side] === code).map((p) => p.server.no));
  const order = (xs: Set<string>, side: 'from' | 'to', last: string) => {
    const out = [...xs].filter((x) => x !== last).sort((a, b) => firstNo(a, side) - firstNo(b, side) || a.localeCompare(b, undefined, { numeric: true }));
    if (xs.has(last)) out.push(last);
    return out;
  };
  const rs = order(relays, 'from', DIRECT);
  const xs = order(exits, 'to', RETIRED);
  return { relays: rs, exits: xs, edges: [...edges.values()], paths, relayServers };
}

const NODE_H = 60;
const GAP = 14;
const PAD = 12;
const LABEL_H = 24;
const ROW = 40; // шаг строк серверов в средней колонке

// Центры узлов колонки: каждый узел - на среднем уровне своих серверов, без наложений.
function nodeYs(codes: string[], paths: MapPath[], labelY: Map<string, number>, side: 'from' | 'to'): number[] {
  const want = codes.map((c) => {
    const ys = paths.filter((p) => p[side] === c).map((p) => labelY.get(p.id)!);
    return ys.reduce((a, y) => a + y, 0) / Math.max(ys.length, 1);
  });
  return spread(want.map((y) => Math.max(y, PAD + NODE_H / 2)), NODE_H + GAP);
}

// Подписи по середине линий без наложений: сдвигаем вниз, если наезжают.
export function spread(ys: number[], gap: number): number[] {
  const order = ys.map((y, i) => ({ y, i })).sort((a, b) => a.y - b.y);
  const out = [...ys];
  let last = -Infinity;
  for (const o of order) {
    const y = Math.max(o.y, last + gap);
    out[o.i] = y;
    last = y;
  }
  return out;
}

const STATE_COLOR: Record<ServerState, 'colorPrimary' | 'colorWarning' | 'colorInfo' | 'colorTextQuaternary'> = {
  active: 'colorPrimary',
  maintenance: 'colorWarning',
  test: 'colorInfo',
  disabled: 'colorTextQuaternary',
};

function MachineNode({
  code,
  m,
  onOpen,
  direct,
  serversOn,
}: {
  code: string;
  m?: Machine;
  onOpen?: () => void;
  direct?: boolean;
  serversOn?: Server[];
}): ReactNode {
  const { t } = useTranslation();
  if (code === RETIRED)
    return (
      <div className="map-node map-node-direct" data-testid="map-node-retired">
        <Typography.Text strong>{t('map.retired')}</Typography.Text>
        <Typography.Text type="secondary" className="map-node-sub">
          {t('map.retiredHint')}
        </Typography.Text>
      </div>
    );
  if (direct)
    return (
      <div className="map-node map-node-direct" data-testid="map-node-direct">
        <Typography.Text strong>{t('map.direct')}</Typography.Text>
        <Typography.Text type="secondary" className="map-node-sub">
          {t('map.directHint')}
        </Typography.Text>
      </div>
    );
  return (
    <div
      className={`map-node${onOpen ? ' map-node-click' : ''}${m && !m.enabled ? ' map-node-off' : ''}`}
      onClick={onOpen}
      data-testid={`map-node-${code}`}
    >
      <Space size={6}>
        <Typography.Text strong className="mono">
          {code}
        </Typography.Text>
        {m ? <RuntimeStatusCell m={m} /> : <Tag>{t('map.unknownMachine')}</Tag>}
      </Space>
      {serversOn && serversOn.length > 0 ? (
        <Tooltip
          title={serversOn.map((s) => (
            <div key={s.no}>
              {t('map.relayServerTip', { no: s.no, engine: productName(s.engine), name: s.name })}
              {relayDomain(s) ? ` - ${relayDomain(s)}` : ''}
            </div>
          ))}
        >
          <Typography.Text type="secondary" className="map-node-sub" data-testid={`map-relay-servers-${code}`}>
            {serversOn.map((s) => `${productName(s.engine)} ${s.no}`).join(' · ')}
          </Typography.Text>
        </Tooltip>
      ) : (
        m && (
          <Typography.Text type="secondary" className="map-node-sub">
            {t('map.load', { conns: m.connections ?? 0, users: m.users_online ?? 0 })}
          </Typography.Text>
        )
      )}
    </div>
  );
}

export default function MapPage() {
  const { t } = useTranslation();
  const { token } = theme.useToken();
  const servers = useServers({}, 100, 0);
  const overview = useOverview();
  const [opened, setOpened] = useState<Machine | null>(null);
  const [editing, setEditing] = useState<Server | null>(null);
  const boxRef = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(900);

  useLayoutEffect(() => {
    const el = boxRef.current;
    if (!el) return;
    setWidth(el.clientWidth || 900);
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => setWidth(el.clientWidth || 900));
    ro.observe(el);
    return () => ro.disconnect();
  }, [servers.isSuccess]);

  const list = useMemo(() => servers.data?.data ?? [], [servers.data]);
  const model = useMemo(() => buildMap(list), [list]);
  const machines = useMemo(() => new Map((overview.data?.data ?? []).map((m) => [m.code, m])), [overview.data]);

  // Наведение: машина (код) или путь сервера (id) - его линии и узлы ярче, остальное темнее.
  const [hover, setHover] = useState<{ node?: string; path?: string } | null>(null);
  const pathOn = (p: MapPath) => !hover || hover.path === p.id || hover.node === p.from || hover.node === p.to;
  const lit = new Set(model.paths.filter(pathOn).flatMap((p) => [p.from, p.to, p.id]));
  const dim = (key: string) => (hover && !lit.has(key) ? 'map-dim' : '');

  const labelY = new Map(model.paths.map((p, i) => [p.id, PAD + LABEL_H / 2 + i * ROW]));
  const ry = nodeYs(model.relays, model.paths, labelY, 'from');
  const ey = nodeYs(model.exits, model.paths, labelY, 'to');
  const height = Math.max(PAD * 2 + LABEL_H + (model.paths.length - 1) * ROW, ...ry.map((y) => y + NODE_H / 2 + PAD), ...ey.map((y) => y + NODE_H / 2 + PAD), NODE_H);
  const yOf = (list: string[], ys: number[], code: string) => ys[list.indexOf(code)];
  const nodeW = Math.min(220, Math.max(140, width * 0.22));
  const x1 = nodeW;
  const x2 = width - nodeW;
  const cx = width / 2;
  const labelW = Math.min(220, width * 0.2);
  const unplaced = (overview.data?.data ?? []).filter((m) => m.kind === 'exit' && !model.exits.includes(m.code));

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.map')}</h1>
        <Button
          icon={<ReloadOutlined />}
          onClick={() => {
            void servers.refetch();
            void overview.refetch();
          }}
          loading={servers.isFetching || overview.isFetching}
        >
          {t('common.refresh')}
        </Button>
      </div>
      <Alert type="info" showIcon style={{ marginBottom: 16 }} title={t('map.hint')} />
      {servers.isError && <ErrorAlert error={servers.error} onRetry={() => servers.refetch()} />}
      <Card>
        <div className="map-columns">
          <Typography.Text type="secondary">{t('map.relays')}</Typography.Text>
          <Typography.Text type="secondary">{t('map.servers')}</Typography.Text>
          <Typography.Text type="secondary">{t('map.exits')}</Typography.Text>
        </div>
        {servers.isSuccess && model.paths.length === 0 ? (
          <Empty description={t('map.empty')} />
        ) : (
          <div ref={boxRef} className="map-box" style={{ height }} data-testid="map">
            <svg width={width} height={height} className="map-svg" aria-hidden>
              {model.paths.map((p) => {
                const ya = yOf(model.relays, ry, p.from);
                const yb = yOf(model.exits, ey, p.to);
                const ly = labelY.get(p.id)!;
                const color = token[STATE_COLOR[p.enabled ? p.server.state : 'disabled']];
                const la = cx - labelW / 2;
                const lb = cx + labelW / 2;
                const ma = (x1 + la) / 2;
                const mb = (lb + x2) / 2;
                return (
                  <path
                    key={p.id}
                    className={dim(p.id)}
                    d={`M ${x1} ${ya} C ${ma} ${ya}, ${ma} ${ly}, ${la} ${ly} L ${lb} ${ly} C ${mb} ${ly}, ${mb} ${yb}, ${x2} ${yb}`}
                    fill="none"
                    stroke={color}
                    strokeWidth={hover && lit.has(p.id) ? 3 : p.enabled ? 2 : 1.5}
                    strokeDasharray={p.enabled ? undefined : '5 5'}
                    opacity={p.enabled ? 0.9 : 0.6}
                    data-testid={`map-path-${p.id}`}
                  />
                );
              })}
            </svg>
            {model.relays.map((r, i) => (
              <div
                key={r}
                className={`map-slot ${dim(r)}`}
                style={{ left: 0, top: ry[i] - NODE_H / 2, width: nodeW, height: NODE_H }}
                onMouseEnter={() => setHover({ node: r })}
                onMouseLeave={() => setHover(null)}
              >
                {r === DIRECT ? (
                  <MachineNode code={r} direct />
                ) : (
                  <MachineNode
                    code={r}
                    m={machines.get(r)}
                    serversOn={model.relayServers[r]}
                    onOpen={machines.get(r) ? () => setOpened(machines.get(r)!) : undefined}
                  />
                )}
              </div>
            ))}
            {model.exits.map((x, i) => (
              <div
                key={x}
                className={`map-slot ${dim(x)}`}
                style={{ left: x2, top: ey[i] - NODE_H / 2, width: nodeW, height: NODE_H }}
                onMouseEnter={() => setHover({ node: x })}
                onMouseLeave={() => setHover(null)}
              >
                <MachineNode code={x} m={machines.get(x)} onOpen={machines.get(x) ? () => setOpened(machines.get(x)!) : undefined} />
              </div>
            ))}
            {model.paths.map((p) => {
              const s = p.server;
              return (
                <div
                  key={`label-${p.id}`}
                  className={`map-label ${dim(p.id)}`}
                  style={{ left: cx, top: labelY.get(p.id)! - LABEL_H / 2, height: LABEL_H, width: labelW }}
                  data-testid={`map-edge-${p.from === DIRECT ? 'direct' : p.from}-${p.to}`}
                  onMouseEnter={() => setHover({ path: p.id })}
                  onMouseLeave={() => setHover(null)}
                >
                  <Tooltip
                    title={
                      <div>
                        <div>{t('map.serverTip', { no: s.no, name: s.name, engine: productName(s.engine) })}</div>
                        <div>{t(`servers.states.${s.state}`)}</div>
                        {relayDomain(s) && <div>{t('map.domainTip', { domain: relayDomain(s) })}</div>}
                        {!p.enabled && <div>{t('map.pathOff')}</div>}
                      </div>
                    }
                  >
                    <Tag
                      className="map-tag"
                      color={p.enabled ? (s.engine === 'mtg' ? 'purple' : 'blue') : undefined}
                      style={p.enabled ? undefined : { opacity: 0.6 }}
                      onClick={() => setEditing(s)}
                    >
                      {s.emoji ? `${s.emoji} ` : ''}
                      {s.no} · {productName(s.engine)}
                      {p.from === DIRECT ? ` · ${t('map.directShort')}` : ''}
                    </Tag>
                  </Tooltip>
                </div>
              );
            })}
          </div>
        )}
        {unplaced.length > 0 && (
          <div className="map-unplaced">
            <Typography.Text type="secondary">{t('map.unplaced')}</Typography.Text>{' '}
            {unplaced.map((m) => (
              <Tag key={m.code} className="mono map-tag" onClick={() => setOpened(m)}>
                {m.code}
              </Tag>
            ))}
          </div>
        )}
        <div className="map-legend">
          <span>
            <span className="map-line" style={{ borderColor: token.colorPrimary }} /> {t('servers.states.active')}
          </span>
          <span>
            <span className="map-line" style={{ borderColor: token.colorWarning }} /> {t('servers.states.maintenance')}
          </span>
          <span>
            <span className="map-line" style={{ borderColor: token.colorInfo }} /> {t('servers.states.test')}
          </span>
          <span>
            <span className="map-line map-line-dashed" style={{ borderColor: token.colorTextQuaternary }} /> {t('map.legendOff')}
          </span>
        </div>
      </Card>
      <MachineDrawer machine={opened && (machines.get(opened.code) ?? opened)} onClose={() => setOpened(null)} />
      {editing && <ServerForm key={editing.no} server={editing} onClose={() => setEditing(null)} />}
    </>
  );
}
