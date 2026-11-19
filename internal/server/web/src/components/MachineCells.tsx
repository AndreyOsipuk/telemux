// Ячейки машины из runtime-отчёта агента: статус, нагрузка, аптайм, подключения и
// версии. Общие для «Обзора» и «Машин» - чтобы один и тот же статус и проценты
// выглядели одинаково на обоих экранах.

import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Tooltip, Typography } from 'antd';
import type { ColumnType } from 'antd/es/table';

import type { Machine, RuntimeStatus } from '@/api/types';
import StatusCell from '@/components/StatusDot';
import { useDuration } from '@/hooks/useRelativeTime';
import { formatPct } from '@/utils/format';

// Пороги подсветки нагрузки, %.
export const PCT_WARN = 75;
export const PCT_DANGER = 90;

export function PctText({ value }: { value: number | null | undefined }) {
  if (value === null || value === undefined) return <Typography.Text type="secondary">-</Typography.Text>;
  const type = value >= PCT_DANGER ? 'danger' : value >= PCT_WARN ? 'warning' : undefined;
  return (
    <Typography.Text type={type} className="mono">
      {formatPct(value)}
    </Typography.Text>
  );
}

export const runtimeStatusOf = (m: Machine): RuntimeStatus | 'unknown' => m.runtime_status ?? 'unknown';

// Какие продукты не ответили (для подсказки к degraded).
export function downProducts(m: Pick<Machine, 'telemt_api_ok' | 'mtg_api_ok'>): string[] {
  const out: string[] = [];
  if (m.telemt_api_ok === false) out.push('telemt');
  if (m.mtg_api_ok === false) out.push('mtg');
  return out;
}

export function RuntimeStatusCell({ m }: { m: Machine }) {
  const { t } = useTranslation();
  const status = runtimeStatusOf(m);
  const down = downProducts(m);
  const tip = status === 'degraded' && down.length > 0 ? t('runtime.productDown', { products: down.join(', ') }) : undefined;
  return (
    <Tooltip title={tip}>
      <span>
        <StatusCell status={status} />
      </span>
    </Tooltip>
  );
}

const dash = <Typography.Text type="secondary">-</Typography.Text>;
const mono = (v: string | undefined | null) => (v ? <span className="mono">{v}</span> : dash);

export type RuntimeColumnKey =
  | 'status'
  | 'cpu'
  | 'mem'
  | 'disk'
  | 'load'
  | 'uptime'
  | 'connections'
  | 'online'
  | 'telemt'
  | 'mtg'
  | 'agent';

// Колонки runtime-полей машины по ключам, в порядке keys.
export function useRuntimeColumns(keys: RuntimeColumnKey[]): ColumnType<Machine>[] {
  const { t } = useTranslation();
  const duration = useDuration();
  return useMemo(() => {
    const all: Record<RuntimeColumnKey, ColumnType<Machine>> = {
      status: {
        key: 'status',
        title: (
          <Tooltip title={t('runtime.statusHint')}>
            <span>{t('runtime.status')}</span>
          </Tooltip>
        ),
        width: 150,
        render: (_v, m) => <RuntimeStatusCell m={m} />,
      },
      cpu: { key: 'cpu', title: t('runtime.cpu'), width: 80, align: 'right', render: (_v, m) => <PctText value={m.host?.cpu_pct} /> },
      mem: { key: 'mem', title: t('runtime.mem'), width: 90, align: 'right', render: (_v, m) => <PctText value={m.host?.mem_pct} /> },
      disk: { key: 'disk', title: t('runtime.disk'), width: 80, align: 'right', render: (_v, m) => <PctText value={m.host?.disk_pct} /> },
      load: {
        key: 'load',
        title: t('runtime.load'),
        width: 70,
        align: 'right',
        render: (_v, m) => (m.host ? <span className="mono">{Number(m.host.load1.toFixed(2))}</span> : dash),
      },
      uptime: {
        key: 'uptime',
        title: t('runtime.uptime'),
        width: 90,
        render: (_v, m) => (m.host ? duration(m.host.uptime_s) : dash),
      },
      connections: {
        key: 'connections',
        title: t('runtime.connections'),
        width: 110,
        align: 'right',
        render: (_v, m) => (m.runtime_seen_at ? <span className="mono">{m.connections}</span> : dash),
      },
      online: {
        key: 'online',
        title: t('runtime.online'),
        width: 80,
        align: 'right',
        render: (_v, m) => (m.runtime_seen_at ? <span className="mono">{m.users_online}</span> : dash),
      },
      telemt: { key: 'telemt', title: 'telemt', width: 100, render: (_v, m) => mono(m.telemt_version) },
      mtg: { key: 'mtg', title: 'mtg', width: 100, render: (_v, m) => mono(m.mtg_version) },
      agent: { key: 'agent', title: t('fleet.agent'), width: 100, render: (_v, m) => mono(m.agent_version) },
    };
    return keys.map((k) => all[k]);
    // keys - константа экрана; join - чтобы новый массив с теми же ключами не пересобирал колонки.
  }, [t, duration, keys.join(',')]);
}
