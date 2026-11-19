// «Подключения»: сверху сводка - строка на машину, telemt и mtg рядом (подключений,
// пользователей онлайн, ответил ли API продукта), свежесть снимка; ниже - пользователи
// онлайн по последним снимкам с фильтрами и поиском. Обновление раз в 15 секунд.

import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useIsFetching, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined } from '@ant-design/icons';

import { useConnectionsSummary, useMachineUsage } from '@/api/queries';
import { IdleCount, UsageTag } from '@/components/UsageTag';
import { offUsage } from '@/utils/machineUsage';
import type { ConnectionSummary } from '@/api/types';
import ConnectionsTable, { ProductTag } from '@/components/ConnectionsTable';
import ErrorAlert from '@/components/ErrorAlert';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

// Строка сводки - одна на машину: telemt и mtg рядом, а не отдельными строками.
export interface MachineSummary {
  machine: string;
  telemt?: ConnectionSummary;
  mtg?: ConnectionSummary;
  seen_at: string;
  stale: boolean;
}

export function groupSummary(rows: ConnectionSummary[]): MachineSummary[] {
  const by = new Map<string, MachineSummary>();
  for (const r of rows) {
    let m = by.get(r.machine);
    if (!m) {
      m = { machine: r.machine, seen_at: r.seen_at, stale: false };
      by.set(r.machine, m);
    }
    m[r.product] = r;
    if (r.seen_at < m.seen_at) m.seen_at = r.seen_at;
    m.stale ||= r.stale;
  }
  return [...by.values()];
}

function ProductCell({ s }: { s?: ConnectionSummary }) {
  const { t } = useTranslation();
  if (!s) return <Typography.Text type="secondary">-</Typography.Text>;
  return (
    <Space size={6} wrap>
      <span className="mono">{t('connections.cell', { conns: s.connections_total, users: s.users_online })}</span>
      {!s.api_ok && (
        <Tooltip title={s.error}>
          <Tag color="error">{t('drawer.apiDown')}</Tag>
        </Tooltip>
      )}
    </Space>
  );
}

// Сводка целиком (до 100 строк машина+продукт): флоту хватает, а строки одной машины
// не разъезжаются по страницам.
const SUMMARY_LIMIT = 100;

function Summary() {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const summary = useConnectionsSummary(SUMMARY_LIMIT, 0);
  const rows = useMemo(() => groupSummary(summary.data?.data ?? []), [summary.data]);
  const usage = useMachineUsage();

  const columns = useMemo<ColumnsType<MachineSummary>>(
    () => [
      {
        title: t('connections.machine'),
        dataIndex: 'machine',
        width: 200,
        render: (v: string) => (
          <Space size={6}>
            <span className="mono">{v}</span>
            <UsageTag usage={usage.get(v)} />
          </Space>
        ),
      },
      {
        title: <ProductTag product="telemt" />,
        key: 'telemt',
        width: 220,
        render: (_v, m) => (
          <IdleCount usage={usage.get(m.machine)}>
            <ProductCell s={m.telemt} />
          </IdleCount>
        ),
      },
      {
        title: <ProductTag product="mtg" />,
        key: 'mtg',
        width: 220,
        render: (_v, m) => (
          <IdleCount usage={usage.get(m.machine)}>
            <ProductCell s={m.mtg} />
          </IdleCount>
        ),
      },
      {
        title: t('connections.freshness'),
        dataIndex: 'seen_at',
        width: 190,
        render: (v: string, m) => (
          <Space size={6}>
            <Tooltip title={formatDateTime(v)}>{relative(secondsSince(v, now))}</Tooltip>
            {m.stale && <Tag color="warning">{t('connections.stale')}</Tag>}
          </Space>
        ),
      },
    ],
    [t, now, relative, usage],
  );

  return (
    <Card title={t('connections.summaryTitle')} style={{ marginBottom: 16 }} data-testid="connections-summary">
      {summary.isError && <ErrorAlert error={summary.error} onRetry={() => summary.refetch()} />}
      {summary.data && summary.data.paging.total > SUMMARY_LIMIT && (
        <Typography.Text type="secondary">{t('connections.summaryTruncated', { n: SUMMARY_LIMIT })}</Typography.Text>
      )}
      <Table<MachineSummary>
        rowKey="machine"
        rowClassName={(m) => (offUsage(usage.get(m.machine)) ? 'row-off' : '')}
        size="small"
        columns={columns}
        dataSource={rows}
        loading={summary.isPending}
        scroll={{ x: 760 }}
        locale={{ emptyText: summary.isSuccess ? t('connections.summaryEmpty') : <span /> }}
        pagination={rows.length > 20 ? { pageSize: 20, size: 'small' } : false}
      />
    </Card>
  );
}

// Машины для фильтра - из сводки (первые 100 строк хватает небольшому флоту).
function useSummaryMachines(): string[] {
  const all = useConnectionsSummary(100, 0);
  return useMemo(() => [...new Set((all.data?.data ?? []).map((s) => s.machine))], [all.data]);
}

export default function ConnectionsPage() {
  const { t } = useTranslation();
  const qc = useQueryClient();
  const fetching = useIsFetching({ queryKey: ['connections'] });
  const machines = useSummaryMachines();
  return (
    <>
      <div className="page-header">
        <h1>{t('menu.connections')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => void qc.invalidateQueries({ queryKey: ['connections'] })} loading={fetching > 0}>
          {t('common.refresh')}
        </Button>
      </div>
      <Summary />
      <Card title={t('connections.usersTitle')} extra={<Typography.Text type="secondary">{t('connections.usersHint')}</Typography.Text>} data-testid="connections-users">
        <ConnectionsTable machineOptions={machines} />
      </Card>
    </>
  );
}
