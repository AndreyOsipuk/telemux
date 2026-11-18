// Источник: форк 3x-ui, frontend/src/pages/nodes/NodeList.tsx (GPL-3.0). Адаптировано:
// строки - машины пары (релей, два сервера, два выхода), колонки - статус, ревизия
// желаемая/применённая, версия бинаря с отметкой канарейки, «видели N секунд назад»,
// мост, «применить сейчас» и «повторить»; раскрывающаяся строка - история применений.

import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ExclamationCircleOutlined, RedoOutlined, ThunderboltOutlined } from '@ant-design/icons';

import type { Pair, Target } from '@/api/types';
import StatusCell from '@/components/StatusDot';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';
import TargetHistory from './TargetHistory';

export interface PairRow extends Target {
  pair: string;
}

// Порядок строк: релей, его серверы, выходы.
export function pairRows(p: Pair): PairRow[] {
  return [p.relay, ...p.servers, ...p.exits].map((t) => ({ ...t, pair: p.code }));
}

export const isDrifting = (t: Target) => t.applied_rev !== t.desired_rev;

const ROLE_COLOR: Record<Target['role'], string> = { relay: 'purple', server: 'blue', exit: 'cyan' };

interface PairTableProps {
  pair: Pair;
  isMaster: boolean;
  selected: string[];
  onSelectionChange: (codes: string[]) => void;
  onApply: (t: Target) => void;
  onRetry: (t: Target) => void;
  busy: Set<string>;
}

export default function PairTable({
  pair,
  isMaster,
  selected,
  onSelectionChange,
  onApply,
  onRetry,
  busy,
}: PairTableProps) {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const rows = useMemo(() => pairRows(pair), [pair]);
  const writeHint = isMaster ? undefined : t('role.writeOnMaster');

  const columns = useMemo<ColumnsType<PairRow>>(
    () => [
      {
        title: t('pairs.machine'),
        width: 190,
        dataIndex: 'name',
        render: (_v, r) => (
          <Space size={8}>
            <Tag color={ROLE_COLOR[r.role]} style={{ margin: 0, minWidth: 64, textAlign: 'center' }}>
              {t(`roles.${r.role}`)}
            </Tag>
            <Typography.Text strong style={{ whiteSpace: 'nowrap' }}>
              {r.name}
            </Typography.Text>
          </Space>
        ),
      },
      {
        title: t('pairs.status'),
        width: 150,
        dataIndex: 'status',
        render: (_v, r) => (
          <Space size={4}>
            <StatusCell status={r.status} />
            {r.last_error && (
              <Tooltip title={r.last_error}>
                <ExclamationCircleOutlined style={{ color: 'var(--ant-color-warning)' }} aria-label={r.last_error} />
              </Tooltip>
            )}
          </Space>
        ),
      },
      {
        title: (
          <Tooltip title={t('pairs.revisionHint')}>
            <span>{t('pairs.revision')}</span>
          </Tooltip>
        ),
        width: 100,
        align: 'center',
        render: (_v, r) =>
          isDrifting(r) ? (
            <Tooltip title={t('pairs.revisionDrift', { desired: r.desired_rev, applied: r.applied_rev ?? '-' })}>
              <Tag color="error" className="mono" data-testid={`rev-${r.code}`}>
                {r.applied_rev ?? '-'} → {r.desired_rev}
              </Tag>
            </Tooltip>
          ) : (
            <Tag color="success" className="mono" data-testid={`rev-${r.code}`}>
              {r.desired_rev}
            </Tag>
          ),
      },
      {
        title: t('pairs.version'),
        width: 230,
        render: (_v, r) => (
          <Space size={4} wrap>
            <Tag className="mono" style={{ margin: 0 }}>
              {r.binary.name} {r.binary.version}
            </Tag>
            {r.binary.canary && (
              <Tooltip title={t('pairs.canaryHint')}>
                <Tag color="orange" style={{ margin: 0 }}>
                  {t('pairs.canary')}
                </Tag>
              </Tooltip>
            )}
          </Space>
        ),
      },
      {
        title: t('pairs.lastSeen'),
        width: 140,
        dataIndex: 'last_seen_at',
        render: (v: string | null) => (
          <Tooltip title={v ? formatDateTime(v) : undefined}>{relative(secondsSince(v, now))}</Tooltip>
        ),
      },
      {
        title: t('pairs.bridge'),
        width: 120,
        dataIndex: 'via_bridge',
        render: (v: string | null, r) =>
          r.role !== 'relay' ? (
            <Typography.Text type="secondary">-</Typography.Text>
          ) : v ? (
            <Tag className="mono">{v}</Tag>
          ) : (
            <Typography.Text type="secondary">{t('pairs.direct')}</Typography.Text>
          ),
      },
      {
        title: t('common.actions'),
        width: 90,
        align: 'right',
        render: (_v, r) => (
          <Space size={4}>
            <Tooltip title={writeHint ?? t('pairs.applyNow')}>
              <Button
                size="small"
                type="text"
                icon={<ThunderboltOutlined />}
                aria-label={`${t('pairs.applyNow')} ${r.name}`}
                disabled={!isMaster}
                loading={busy.has(r.code)}
                onClick={() => onApply(r)}
              />
            </Tooltip>
            <Tooltip title={writeHint ?? t('pairs.retry')}>
              <Button
                size="small"
                type="text"
                icon={<RedoOutlined />}
                aria-label={`${t('pairs.retry')} ${r.name}`}
                disabled={!isMaster || (!r.last_error && !isDrifting(r))}
                onClick={() => onRetry(r)}
              />
            </Tooltip>
          </Space>
        ),
      },
    ],
    [t, now, relative, isMaster, writeHint, busy, onApply, onRetry],
  );

  return (
    <Table<PairRow>
      rowKey="code"
      size="small"
      columns={columns}
      dataSource={rows}
      pagination={false}
      tableLayout="fixed"
      scroll={{ x: 1000 }}
      rowSelection={{
        selectedRowKeys: selected,
        onChange: (keys) => onSelectionChange(keys.map(String)),
        getCheckboxProps: () => ({ disabled: !isMaster }),
      }}
      expandable={{
        expandedRowRender: (r) => <TargetHistory target={r} />,
        rowExpandable: () => true,
      }}
    />
  );
}
