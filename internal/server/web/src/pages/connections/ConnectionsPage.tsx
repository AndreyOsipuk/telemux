// «Подключения»: сверху сводка по машинам и продуктам (telemt, mtg) - подключений,
// пользователей онлайн, ответил ли API продукта, свежесть снимка; ниже - пользователи
// онлайн по последним снимкам с фильтрами и поиском. Обновление раз в 15 секунд.

import { useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useIsFetching, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined } from '@ant-design/icons';

import { useConnectionsSummary } from '@/api/queries';
import type { ConnectionSummary, Product } from '@/api/types';
import ConnectionsTable, { ProductTag } from '@/components/ConnectionsTable';
import ErrorAlert from '@/components/ErrorAlert';
import { usePaging } from '@/hooks/usePaging';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

function Summary() {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const paging = usePaging(20);
  const summary = useConnectionsSummary(paging.pageSize, paging.offset);
  const { syncTotal } = paging;
  const total = summary.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const columns = useMemo<ColumnsType<ConnectionSummary>>(
    () => [
      { title: t('connections.machine'), dataIndex: 'machine', width: 130, render: (v: string) => <span className="mono">{v}</span> },
      { title: t('connections.product'), dataIndex: 'product', width: 100, render: (v: Product) => <ProductTag product={v} /> },
      {
        title: t('connections.connections'),
        dataIndex: 'connections_total',
        width: 120,
        align: 'right',
        render: (v: number) => <span className="mono">{v}</span>,
      },
      {
        title: t('connections.usersOnline'),
        dataIndex: 'users_online',
        width: 110,
        align: 'right',
        render: (v: number) => <span className="mono">{v}</span>,
      },
      {
        title: t('connections.api'),
        dataIndex: 'api_ok',
        width: 220,
        render: (v: boolean, s) =>
          v ? (
            <Tag color="success">{t('drawer.apiOk')}</Tag>
          ) : (
            <Space size={6} wrap>
              <Tag color="error">{t('drawer.apiDown')}</Tag>
              {s.error && <Typography.Text type="danger">{s.error}</Typography.Text>}
            </Space>
          ),
      },
      {
        title: t('connections.freshness'),
        dataIndex: 'seen_at',
        width: 190,
        render: (v: string, s) => (
          <Space size={6}>
            <Tooltip title={formatDateTime(v)}>{relative(secondsSince(v, now))}</Tooltip>
            {s.stale && <Tag color="warning">{t('connections.stale')}</Tag>}
          </Space>
        ),
      },
    ],
    [t, now, relative],
  );

  return (
    <Card title={t('connections.summaryTitle')} style={{ marginBottom: 16 }} data-testid="connections-summary">
      {summary.isError && <ErrorAlert error={summary.error} onRetry={() => summary.refetch()} />}
      <Table<ConnectionSummary>
        rowKey={(s) => `${s.machine}/${s.product}`}
        size="small"
        columns={columns}
        dataSource={summary.data?.data ?? []}
        loading={summary.isPending || (summary.isFetching && summary.isPlaceholderData)}
        scroll={{ x: 870 }}
        locale={{ emptyText: summary.isSuccess ? t('connections.summaryEmpty') : <span /> }}
        pagination={paging.pagination(total, { size: 'small' })}
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
