// Ошибки журнала telemt за последние 24 часа (/api/v1/log-errors): сумма счётчиков
// по пользователю и виду ошибки, сначала самые частые. Постранично; machine задан -
// только эта машина.

import { useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';

import { useLogErrors } from '@/api/queries';
import type { LogErrorKind, LogErrorSum } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { usePaging } from '@/hooks/usePaging';

const KIND_COLOR: Record<LogErrorKind, string> = {
  limit: 'orange',
  handshake: 'default',
  auth: 'red',
  timeout: 'gold',
  other: 'default',
};

const rowKey = (e: LogErrorSum) => `${e.machine}/${e.key}/${e.kind}`;

export default function LogErrorsTable({ machine }: { machine?: string }) {
  const { t } = useTranslation();
  const paging = usePaging(20);
  const log = useLogErrors(machine, paging.pageSize, paging.offset);
  const { syncTotal } = paging;
  const total = log.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const columns = useMemo<ColumnsType<LogErrorSum>>(() => {
    const cols: ColumnsType<LogErrorSum> = [];
    if (!machine) {
      cols.push({ title: t('logErrors.machine'), dataIndex: 'machine', width: 120, render: (v: string) => <span className="mono">{v}</span> });
    }
    cols.push(
      {
        title: t('logErrors.user'),
        dataIndex: 'key',
        render: (v: string) =>
          v ? <span className="mono">{v}</span> : <Typography.Text type="secondary">{t('logErrors.noUser')}</Typography.Text>,
      },
      {
        title: t('logErrors.kind'),
        dataIndex: 'kind',
        width: 190,
        render: (v: LogErrorKind) => <Tag color={KIND_COLOR[v] ?? 'default'}>{t(`logKind.${v}`, { defaultValue: v })}</Tag>,
      },
      { title: t('logErrors.count'), dataIndex: 'count', width: 110, align: 'right', render: (v: number) => <span className="mono">{v}</span> },
    );
    return cols;
  }, [t, machine]);

  return (
    <>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
        {t('logErrors.window')}
      </Typography.Paragraph>
      {log.isError && <ErrorAlert error={log.error} onRetry={() => log.refetch()} />}
      <Table<LogErrorSum>
        rowKey={rowKey}
        size="small"
        columns={columns}
        dataSource={log.data?.data ?? []}
        loading={log.isPending || (log.isFetching && log.isPlaceholderData)}
        scroll={{ x: 520 }}
        locale={{ emptyText: log.isSuccess ? t('logErrors.empty') : <span /> }}
        pagination={paging.pagination(total, { size: 'small' })}
      />
    </>
  );
}
