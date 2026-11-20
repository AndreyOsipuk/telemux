// Путь «релей - своя нода» (спецификация relay-via-exit, R3): статус пары из
// /api/v1/pairs в ячейке «Релей» таблицы машин. У выхода - его релей и статус
// пары (ok, relay_silent, exit_down), у релея - статус пары (down, no_exit),
// чужой мост и через мост какого выхода пришёл последний отчёт (relay_via).

import { useTranslation } from 'react-i18next';
import { Space, Tag, Tooltip, Typography } from 'antd';

import { usePairs } from '@/api/queries';
import type { Machine, Pair, PairPathStatus, RelayStatus } from '@/api/types';
import { formatDateTime } from '@/utils/format';

export interface RelayPath {
  relayStatus?: RelayStatus;
  relayLastSeen?: string | null;
  pairStatus?: PairPathStatus;
  foreignBridge?: boolean;
}

// relayPaths - статусы пар по коду машины: выход -> его статус в паре, релей -> статус пары.
export function relayPaths(pairs: Pair[]): Map<string, RelayPath> {
  const out = new Map<string, RelayPath>();
  for (const p of pairs) {
    if (!p.relay) continue;
    out.set(p.relay.code, { pairStatus: p.status, foreignBridge: p.foreign_bridge });
    for (const e of p.exits) {
      if (e.relay_status) out.set(e.code, { relayStatus: e.relay_status, relayLastSeen: e.relay_last_seen });
    }
  }
  return out;
}

// Пары флота для таблицы машин: страница в 100 пар - это 100 релеев.
export function useRelayPaths(): Map<string, RelayPath> {
  const q = usePairs(100, 0);
  return relayPaths(q.data?.data ?? []);
}

const RELAY_COLOR: Record<RelayStatus, string> = { ok: 'success', relay_silent: 'error', exit_down: 'default' };
const PAIR_COLOR: Record<PairPathStatus, string> = { ok: 'success', down: 'error', no_exit: 'warning' };

export function RelayPathCell({ m, path }: { m: Machine; path?: RelayPath }) {
  const { t } = useTranslation();
  if (m.kind === 'relay') {
    const bad = path?.pairStatus && path.pairStatus !== 'ok' ? path.pairStatus : undefined;
    if (!bad && !path?.foreignBridge && !m.relay_via) return <Typography.Text type="secondary">-</Typography.Text>;
    return (
      <Space size={4} wrap>
        {bad && <Tag color={PAIR_COLOR[bad]}>{t(`fleet.pairStatus.${bad}`)}</Tag>}
        {path?.foreignBridge && <Tag color="error">{t('fleet.foreignBridge')}</Tag>}
        {m.relay_via && (
          <Tooltip title={m.relay_via_at ? `${t('fleet.relayVia')}: ${formatDateTime(m.relay_via_at)}` : t('fleet.relayVia')}>
            <span className="mono">{m.relay_via}</span>
          </Tooltip>
        )}
      </Space>
    );
  }
  if (!m.relay_code) return <Typography.Text type="secondary">-</Typography.Text>;
  return (
    <Space size={4} wrap>
      <span className="mono">{m.relay_code}</span>
      {path?.relayStatus && (
        <Tooltip
          title={t('fleet.relayLastSeen', { when: path.relayLastSeen ? formatDateTime(path.relayLastSeen) : '-' })}
        >
          <Tag color={RELAY_COLOR[path.relayStatus]}>{t(`fleet.relayStatus.${path.relayStatus}`)}</Tag>
        </Tooltip>
      )}
    </Space>
  );
}
