// Сертификаты WEB-доменов релеев (/api/v1/certs): домен, машина, назначен ли
// tls.web, срок, выпуск, последняя попытка и её ошибка. Меньше CERT_WARN_DAYS дней
// до конца срока (или уже истёк) - строка подсвечена. Постранично; machine задан -
// только сертификаты этой машины (вкладка карточки машины).

import { useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';

import { ApiError } from '@/api/http';
import { useCerts } from '@/api/queries';
import type { CertInfo } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { usePaging } from '@/hooks/usePaging';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { daysUntil, formatDate, formatDateTime } from '@/utils/format';
import './CertsTable.css';

export const CERT_WARN_DAYS = 14;

export type CertState = 'missing' | 'expired' | 'expiring' | 'ok';

export function certState(c: Pick<CertInfo, 'not_after'>, now = Date.now()): CertState {
  if (!c.not_after) return 'missing';
  if (Date.parse(c.not_after) <= now) return 'expired';
  return daysUntil(c.not_after, now) < CERT_WARN_DAYS ? 'expiring' : 'ok';
}

const certKey = (c: CertInfo) => `${c.machine}/${c.domain}`;

export default function CertsTable({ machine }: { machine?: string }) {
  const { t } = useTranslation();
  const now = useNow(60_000);
  const relative = useRelativeTime();
  const paging = usePaging(20);
  const certs = useCerts(machine, paging.pageSize, paging.offset);
  const { syncTotal } = paging;
  const total = certs.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const columns = useMemo<ColumnsType<CertInfo>>(() => {
    const cols: ColumnsType<CertInfo> = [
      { title: t('certs.domain'), dataIndex: 'domain', width: 220, render: (v: string) => <span className="mono">{v}</span> },
    ];
    if (!machine) {
      cols.push({ title: t('certs.machine'), dataIndex: 'machine', width: 110, render: (v: string) => <span className="mono">{v}</span> });
    }
    cols.push(
      {
        title: (
          <Tooltip title={t('certs.assignedHint')}>
            <span>tls.web</span>
          </Tooltip>
        ),
        dataIndex: 'assigned',
        width: 120,
        render: (v: boolean) => <Tag color={v ? 'success' : 'default'}>{v ? t('certs.assigned') : t('certs.notAssigned')}</Tag>,
      },
      {
        title: t('certs.notAfter'),
        dataIndex: 'not_after',
        width: 200,
        render: (v: string | null, c) => {
          const st = certState(c, now);
          if (!v) return <Tag color="warning">{t('certs.noCert')}</Tag>;
          const days = daysUntil(v, now);
          return (
            <span data-cert-state={st}>
              <Tooltip title={formatDateTime(v)}>{formatDate(v)}</Tooltip>{' '}
              {st === 'expired' ? (
                <Tag color="error">{t('certs.expired')}</Tag>
              ) : (
                <Tag color={st === 'expiring' ? 'error' : 'default'}>{t('certs.daysLeft', { days: t('time.days', { count: days }) })}</Tag>
              )}
            </span>
          );
        },
      },
      {
        title: t('certs.issuedAt'),
        dataIndex: 'issued_at',
        width: 120,
        render: (v: string | null) => (v ? formatDate(v) : <Typography.Text type="secondary">-</Typography.Text>),
      },
      {
        title: t('certs.lastTry'),
        dataIndex: 'last_try_at',
        width: 150,
        render: (v: string | null) =>
          v ? <Tooltip title={formatDateTime(v)}>{relative(secondsSince(v, now))}</Tooltip> : <Typography.Text type="secondary">-</Typography.Text>,
      },
      {
        title: t('certs.lastError'),
        dataIndex: 'last_error',
        ellipsis: true,
        render: (v: string | undefined) => (v ? <Typography.Text type="danger">{v}</Typography.Text> : '-'),
      },
    );
    return cols;
  }, [t, machine, now, relative]);

  // 404 - мастер не ведёт сертификаты (нет схемы базы): это не ошибка, а режим.
  if (certs.error instanceof ApiError && certs.error.status === 404) {
    return <Alert type="info" showIcon message={t('certs.off')} description={t('certs.offHint')} />;
  }

  return (
    <>
      {certs.isError && <ErrorAlert error={certs.error} onRetry={() => certs.refetch()} />}
      <Table<CertInfo>
        rowKey={certKey}
        size="small"
        columns={columns}
        dataSource={certs.data?.data ?? []}
        loading={certs.isPending || (certs.isFetching && certs.isPlaceholderData)}
        rowClassName={(c) => {
          const st = certState(c, now);
          return st === 'expired' || st === 'expiring' ? 'cert-row-warn' : '';
        }}
        scroll={{ x: machine ? 860 : 970 }}
        locale={{ emptyText: certs.isSuccess ? t('certs.empty') : <span /> }}
        pagination={paging.pagination(total, { size: 'small' })}
      />
    </>
  );
}
