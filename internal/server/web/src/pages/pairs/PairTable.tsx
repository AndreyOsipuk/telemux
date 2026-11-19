// Источник: форк 3x-ui, frontend/src/pages/nodes/NodeList.tsx (GPL-3.0). Адаптировано:
// строки - машины пары (релей, затем его выходы), колонки - сводный статус, версия
// агента, мост, «видели N назад», ресурсы (применённая/желаемая ревизия) и проверки.
// Клик по машине открывает историю применений; действие «Команда установки» выдаёт
// новую одноразовую команду агента.

import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Space, Table, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';

import type { Pair, PairMachine } from '@/api/types';
import { CheckTags, KindTag, ResourceTags, resourceRows } from '@/components/FleetTags';
import { JoinCommandButton } from '@/components/JoinCommand';
import StatusCell from '@/components/StatusDot';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

// Порядок строк: релей (если есть), затем выходы.
export function pairMachines(p: Pair): PairMachine[] {
  return p.relay ? [p.relay, ...p.exits] : p.exits;
}

// Ключ пары: код релея; у псевдо-пары релея нет.
export const NO_RELAY_KEY = '__no_relay__';
export const pairKey = (p: Pair) => p.relay?.code ?? NO_RELAY_KEY;

interface PairTableProps {
  pair: Pair;
  onOpen: (m: PairMachine) => void;
}

export default function PairTable({ pair, onOpen }: PairTableProps) {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const rows = useMemo(() => pairMachines(pair), [pair]);

  const columns = useMemo<ColumnsType<PairMachine>>(
    () => [
      {
        title: t('fleet.machine'),
        width: 190,
        dataIndex: 'code',
        render: (v: string, m) => (
          <Space size={8}>
            <KindTag kind={m.kind} />
            <Tooltip title={t('fleet.openHistory')}>
              <Button
                type="link"
                size="small"
                style={{ padding: 0, fontWeight: 600 }}
                aria-label={t('fleet.historyOf', { code: v })}
                onClick={(e) => {
                  e.stopPropagation();
                  onOpen(m);
                }}
              >
                {v}
              </Button>
            </Tooltip>
          </Space>
        ),
      },
      {
        title: t('fleet.status'),
        width: 140,
        dataIndex: 'status',
        render: (_v, m) => <StatusCell status={m.status} />,
      },
      {
        title: t('fleet.agent'),
        width: 120,
        dataIndex: 'agent_version',
        render: (v: string | undefined) =>
          v ? <span className="mono">{v}</span> : <Typography.Text type="secondary">-</Typography.Text>,
      },
      {
        title: t('fleet.bridge'),
        width: 110,
        dataIndex: 'via',
        render: (v: string | undefined, m) =>
          v ? (
            <span className="mono">{v}</span>
          ) : (
            <Typography.Text type="secondary">{m.seen_at ? t('fleet.direct') : '-'}</Typography.Text>
          ),
      },
      {
        title: t('fleet.lastSeen'),
        width: 140,
        dataIndex: 'seen_at',
        render: (v: string | null) =>
          v ? (
            <Tooltip title={formatDateTime(v)}>{relative(secondsSince(v, now))}</Tooltip>
          ) : (
            <Typography.Text type="secondary">{t('fleet.noAgent')}</Typography.Text>
          ),
      },
      {
        title: (
          <Tooltip title={t('fleet.resourcesHint')}>
            <span>{t('fleet.resources')}</span>
          </Tooltip>
        ),
        render: (_v, m) => <ResourceTags rows={resourceRows(m.resources, m.observed, m.desired)} />,
      },
      {
        title: t('fleet.checks'),
        width: 200,
        render: (_v, m) => <CheckTags checks={m.checks} />,
      },
      {
        title: t('common.actions'),
        width: 190,
        render: (_v, m) => <JoinCommandButton code={m.code} hasAgent={m.seen_at !== null} />,
      },
    ],
    [t, now, relative, onOpen],
  );

  return (
    <Table<PairMachine>
      rowKey="code"
      size="small"
      columns={columns}
      dataSource={rows}
      pagination={false}
      tableLayout="fixed"
      scroll={{ x: 1290 }}
      locale={{ emptyText: t('pairs.noMachines') }}
      onRow={(m) => ({ onClick: () => onOpen(m), style: { cursor: 'pointer' } })}
    />
  );
}
