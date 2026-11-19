// История применений и проверок машины (/api/v1/targets/{code}/apply-log), новые
// первыми, постранично. Образец - NodeHistoryPanel из форка 3x-ui; код написан заново.

import { useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';

import { useApplyLog } from '@/api/queries';
import type { ApplyLogEntry } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { usePaging } from '@/hooks/usePaging';
import { formatDateTime } from '@/utils/format';

const STATUS_COLOR: Record<string, string> = {
  applied: 'success',
  ok: 'success',
  pending: 'processing',
  compared: 'gold',
  failed: 'error',
};

const KNOWN_STATUS = new Set(Object.keys(STATUS_COLOR));
const CHECK_PREFIX = 'check:';

export default function TargetHistory({ code }: { code: string }) {
  const { t } = useTranslation();
  const paging = usePaging(20);
  const log = useApplyLog(code, paging.pageSize, paging.offset);
  const { syncTotal } = paging;
  const total = log.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const columns: ColumnsType<ApplyLogEntry> = [
    {
      title: t('history.at'),
      dataIndex: 'at',
      width: 170,
      render: (v: string) => <Typography.Text type="secondary">{formatDateTime(v)}</Typography.Text>,
    },
    {
      title: t('history.resource'),
      dataIndex: 'resource',
      render: (v: string) =>
        v.startsWith(CHECK_PREFIX) ? (
          <span>
            {t('history.check')} <span className="mono">{v.slice(CHECK_PREFIX.length)}</span>
          </span>
        ) : (
          <span className="mono">{v}</span>
        ),
    },
    {
      title: t('history.revision'),
      dataIndex: 'revision',
      width: 90,
      align: 'center',
      render: (v: number | null | undefined) => (v === null || v === undefined ? '-' : <span className="mono">{v}</span>),
    },
    {
      title: t('history.status'),
      dataIndex: 'status',
      width: 120,
      render: (v: string) => (
        <Tag color={STATUS_COLOR[v] ?? 'default'}>{KNOWN_STATUS.has(v) ? t(`applyStatus.${v}`) : v}</Tag>
      ),
    },
    {
      title: t('history.error'),
      dataIndex: 'error',
      ellipsis: true,
      render: (v: string | undefined) => (v ? <Typography.Text type="danger">{v}</Typography.Text> : '-'),
    },
  ];

  return (
    <>
      {log.isError && <ErrorAlert error={log.error} onRetry={() => log.refetch()} />}
      <Table<ApplyLogEntry>
        rowKey={(e) => `${e.at}/${e.resource}/${e.status}`}
        size="small"
        columns={columns}
        dataSource={log.data?.data ?? []}
        loading={log.isPending || (log.isFetching && log.isPlaceholderData)}
        scroll={{ x: 640 }}
        locale={{ emptyText: log.isSuccess ? t('history.empty') : <span /> }}
        pagination={paging.pagination(total, { size: 'small' })}
      />
    </>
  );
}
