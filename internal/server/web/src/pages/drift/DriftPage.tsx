// «Дрейф» (docs/PLAN.md, раздел 3): строки (машина, ресурс), где применённое
// расходится с желаемым дольше порога (/api/v1/drift). Причины: behind - машина
// отстаёт от желаемой ревизии, failed - агент сообщил об отказе, silent - агент
// молчит дольше порога (строка на машину, ресурса нет). Постранично по paging.total.

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Card, Result, Segmented, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined } from '@ant-design/icons';

import { useDrift } from '@/api/queries';
import type { DriftItem, DriftReason } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { ErrorText, KindTag } from '@/components/FleetTags';
import { usePaging } from '@/hooks/usePaging';
import { secondsSince, useDuration, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

export const THRESHOLDS = [60, 300, 600, 1800, 3600] as const;
export const DEFAULT_THRESHOLD = 600;

const REASON_COLOR: Record<DriftReason, string> = { behind: 'processing', failed: 'error', silent: 'warning' };

export const driftKey = (d: DriftItem) => `${d.target}/${d.reason}/${d.resource}`;

export default function DriftPage() {
  const { t } = useTranslation();
  const now = useNow();
  const duration = useDuration();
  const relative = useRelativeTime();
  const [threshold, setThreshold] = useState<number>(DEFAULT_THRESHOLD);
  const paging = usePaging(20);
  const drift = useDrift(threshold, paging.pageSize, paging.offset);

  const { syncTotal } = paging;
  const total = drift.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const columns: ColumnsType<DriftItem> = [
    {
      title: t('fleet.machine'),
      width: 190,
      dataIndex: 'target',
      render: (v: string, d) => (
        <Space size={8}>
          <KindTag kind={d.kind} />
          <span>
            <Typography.Text strong className="mono">
              {v}
            </Typography.Text>
            {d.relay_code && (
              <Typography.Text type="secondary" className="mono" style={{ display: 'block', fontSize: 12 }}>
                {t('drift.relayOf', { code: d.relay_code })}
              </Typography.Text>
            )}
          </span>
        </Space>
      ),
    },
    {
      title: t('drift.reason'),
      width: 140,
      dataIndex: 'reason',
      render: (v: DriftReason) => (
        <Tooltip title={t(`driftReasonHint.${v}`)}>
          <Tag color={REASON_COLOR[v]}>{t(`driftReason.${v}`)}</Tag>
        </Tooltip>
      ),
    },
    {
      title: t('drift.resource'),
      width: 150,
      dataIndex: 'resource',
      render: (v: string) => (v ? <span className="mono">{v}</span> : <Typography.Text type="secondary">-</Typography.Text>),
    },
    {
      title: (
        <Tooltip title={t('fleet.resourcesHint')}>
          <span>{t('drift.revisions')}</span>
        </Tooltip>
      ),
      width: 120,
      align: 'center',
      render: (_v, d) =>
        d.reason === 'silent' ? (
          <Typography.Text type="secondary">-</Typography.Text>
        ) : (
          <Tag color="error" className="mono" data-testid={`rev-${driftKey(d)}`}>
            {d.observed_revision ?? '-'} / {d.desired_revision}
          </Tag>
        ),
    },
    {
      title: t('drift.since'),
      width: 130,
      dataIndex: 'since',
      render: (v: string) => <Tooltip title={formatDateTime(v)}>{duration(secondsSince(v, now) ?? 0)}</Tooltip>,
    },
    {
      title: t('fleet.lastSeen'),
      width: 140,
      dataIndex: 'seen_at',
      render: (v: string | null | undefined) =>
        v ? (
          <Tooltip title={formatDateTime(v)}>{relative(secondsSince(v, now))}</Tooltip>
        ) : (
          <Typography.Text type="secondary">{t('fleet.noAgent')}</Typography.Text>
        ),
    },
    {
      title: t('drift.error'),
      dataIndex: 'error',
      render: (v: string | undefined) =>
        v ? <ErrorText text={v} /> : <Typography.Text type="secondary">{t('drift.noError')}</Typography.Text>,
    },
  ];

  const empty = drift.isSuccess && total === 0;

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.drift')}</h1>
        <Space wrap>
          <Typography.Text type="secondary">{t('drift.threshold')}</Typography.Text>
          <Segmented<number>
            value={threshold}
            onChange={(v) => {
              setThreshold(v);
              paging.reset();
            }}
            options={THRESHOLDS.map((s) => ({ value: s, label: duration(s) }))}
          />
          <Button icon={<ReloadOutlined />} onClick={() => drift.refetch()} loading={drift.isFetching}>
            {t('common.refresh')}
          </Button>
        </Space>
      </div>
      {drift.isError && <ErrorAlert error={drift.error} onRetry={() => drift.refetch()} />}
      <Card>
        {empty ? (
          <Result status="success" title={t('drift.none')} subTitle={t('drift.noneHint', { threshold: duration(threshold) })} />
        ) : (
          <Table<DriftItem>
            rowKey={driftKey}
            size="middle"
            columns={columns}
            dataSource={drift.data?.data ?? []}
            loading={drift.isPending || (drift.isFetching && drift.isPlaceholderData)}
            scroll={{ x: 1000 }}
            // Пока грузится или при ошибке - не «расхождений нет», а пусто.
            locale={{ emptyText: drift.isSuccess ? t('drift.none') : <span /> }}
            pagination={paging.pagination(total)}
          />
        )}
      </Card>
    </>
  );
}
