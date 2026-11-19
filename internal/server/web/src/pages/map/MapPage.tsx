// «Карта»: как клиенты доходят до машин за рубежом. Слева - релеи (и «напрямую» для
// прямых точек входа), справа - машины за рубежом, линии - серверы: сервер N идёт от
// своего релея (или напрямую) к своей машине. Связи строятся из серверов, а не из
// targets.relay_code: одна машина бывает за разными релеями у разных серверов.
// Цвет линии - состояние сервера, пунктир - все точки этого пути выключены.
// Клик по машине открывает её карточку, по подписи линии - форму сервера.

import { useLayoutEffect, useMemo, useRef, useState } from 'react';
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

export interface MapModel {
  relays: string[];
  exits: string[];
  edges: MapEdge[];
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
  const sorted = (xs: Set<string>) => [...xs].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
  const rs = sorted(relays).filter((r) => r !== DIRECT);
  if (relays.has(DIRECT)) rs.push(DIRECT);
  // Машины за рубежом - напротив своих релеев (средний номер релея в колонке), при
  // равенстве - по наименьшему номеру сервера: так линии почти не пересекаются.
  const relayIdx = new Map(rs.map((r, i) => [r, i]));
  relayIdx.set(DIRECT, rs.length);
  const exitKey = new Map<string, { pos: number; no: number }>();
  for (const x of exits) {
    const es = [...edges.values()].filter((e) => e.to === x);
    const pos = es.reduce((a, e) => a + (relayIdx.get(e.from) ?? rs.length), 0) / Math.max(es.length, 1);
    const no = Math.min(...es.flatMap((e) => e.servers.map((v) => v.server.no)));
    exitKey.set(x, { pos, no });
  }
  const xs = [...exits]
    .filter((x) => x !== RETIRED)
    .sort((a, b) => {
      const ka = exitKey.get(a)!;
      const kb = exitKey.get(b)!;
      return ka.pos - kb.pos || ka.no - kb.no || a.localeCompare(b, undefined, { numeric: true });
    });
  if (exits.has(RETIRED)) xs.push(RETIRED);
  for (const e of edges.values()) e.servers.sort((a, b) => a.server.no - b.server.no);
  return { relays: rs, exits: xs, edges: [...edges.values()], relayServers };
}

const NODE_H = 60;
const GAP = 18;
const PAD = 12;
const LABEL_H = 24;

// Вертикальные центры узлов колонки; короткая колонка - по центру.
function column(n: number, total: number): number[] {
  const h = total - 2 * PAD;
  const used = n * NODE_H + (n - 1) * GAP;
  const top = PAD + Math.max(0, (h - used) / 2);
  return Array.from({ length: n }, (_, i) => top + i * (NODE_H + GAP) + NODE_H / 2);
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
}) {
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

  const rows = Math.max(model.relays.length, model.exits.length, 1);
  const height = rows * NODE_H + (rows - 1) * GAP + 2 * PAD;
  const ry = column(model.relays.length, height);
  const ey = column(model.exits.length, height);
  const yOf = (list: string[], ys: number[], code: string) => ys[list.indexOf(code)];
  const nodeW = Math.min(220, Math.max(140, width * 0.24));
  const x1 = nodeW;
  const x2 = width - nodeW;
  const midY = model.edges.map((e) => (yOf(model.relays, ry, e.from) + yOf(model.exits, ey, e.to)) / 2);
  const labelY = spread(midY, LABEL_H + 4);
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
        {servers.isSuccess && model.edges.length === 0 ? (
          <Empty description={t('map.empty')} />
        ) : (
          <div ref={boxRef} className="map-box" style={{ height }} data-testid="map">
            <svg width={width} height={height} className="map-svg" aria-hidden>
              {model.edges.map((e, i) => {
                const ya = yOf(model.relays, ry, e.from);
                const yb = yOf(model.exits, ey, e.to);
                const on = e.servers.some((s) => s.enabled);
                const state = e.servers.find((s) => s.enabled)?.server.state ?? 'disabled';
                const color = token[STATE_COLOR[state]];
                const cx = (x1 + x2) / 2;
                return (
                  <path
                    key={`${e.from}-${e.to}`}
                    d={`M ${x1} ${ya} C ${cx} ${ya}, ${cx} ${labelY[i]}, ${cx} ${labelY[i]} S ${cx} ${yb}, ${x2} ${yb}`}
                    fill="none"
                    stroke={color}
                    strokeWidth={on ? 2 : 1.5}
                    strokeDasharray={on ? undefined : '5 5'}
                    opacity={on ? 0.9 : 0.6}
                  />
                );
              })}
            </svg>
            {model.relays.map((r, i) => (
              <div key={r} className="map-slot" style={{ left: 0, top: ry[i] - NODE_H / 2, width: nodeW, height: NODE_H }}>
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
              <div key={x} className="map-slot" style={{ left: x2, top: ey[i] - NODE_H / 2, width: nodeW, height: NODE_H }}>
                <MachineNode code={x} m={machines.get(x)} onOpen={machines.get(x) ? () => setOpened(machines.get(x)!) : undefined} />
              </div>
            ))}
            {model.edges.map((e, i) => (
              <div
                key={`label-${e.from}-${e.to}`}
                className="map-label"
                style={{ left: (x1 + x2) / 2, top: labelY[i] - LABEL_H / 2, height: LABEL_H }}
                data-testid={`map-edge-${e.from === DIRECT ? 'direct' : e.from}-${e.to}`}
              >
                {e.servers.map(({ server: s, enabled }) => (
                  <Tooltip
                    key={s.no}
                    title={
                      <div>
                        <div>
                          {t('map.serverTip', { no: s.no, name: s.name, engine: productName(s.engine) })}
                        </div>
                        <div>{t(`servers.states.${s.state}`)}</div>
                        {relayDomain(s) && <div>{t('map.domainTip', { domain: relayDomain(s) })}</div>}
                        {!enabled && <div>{t('map.pathOff')}</div>}
                      </div>
                    }
                  >
                    <Tag
                      className="map-tag"
                      color={enabled ? (s.engine === 'mtg' ? 'purple' : 'blue') : undefined}
                      style={enabled ? undefined : { opacity: 0.6 }}
                      onClick={() => setEditing(s)}
                    >
                      {s.emoji ? `${s.emoji} ` : ''}
                      {s.no} · {productName(s.engine)}
                    </Tag>
                  </Tooltip>
                ))}
              </div>
            ))}
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
