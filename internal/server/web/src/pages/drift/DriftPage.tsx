// «Дрейф» (docs/PLAN.md, раздел 3): только машины, где желаемое не совпадает с
// применённым дольше порога.

import { useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { App, Button, Card, Empty, Result, Segmented, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined, ThunderboltOutlined } from '@ant-design/icons';

import { api, USES_MOCKS } from '@/api/client';
import { describeError } from '@/api/errors';
import { useDrift, useIsMaster } from '@/api/queries';
import type { DriftItem } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import MockBanner from '@/components/MockBanner';
import { secondsSince, useDuration, useNow } from '@/hooks/useRelativeTime';
import { formatDateTime } from '@/utils/format';

export const THRESHOLDS = [60, 300, 900, 3600] as const;

// Сервер уже отдаёт только расхождения дольше порога; фильтр на клиенте страхует
// от устаревшего кэша после смены порога.
export function overThreshold(items: DriftItem[], thresholdSec: number, now: number): DriftItem[] {
  return items.filter((d) => (secondsSince(d.since, now) ?? 0) >= thresholdSec);
}

export default function DriftPage() {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const qc = useQueryClient();
  const isMaster = useIsMaster();
  const now = useNow();
  const duration = useDuration();
  const [threshold, setThreshold] = useState<number>(300);
  const drift = useDrift(threshold);

  const apply = useMutation({
    mutationFn: (code: string) => api.applyTarget(code),
    onSuccess: (_r, code) => {
      message.success(t('pairs.retryQueued', { name: code }));
      void qc.invalidateQueries({ queryKey: ['drift'] });
    },
    onError: (e) => message.error(describeError(e, t)),
  });

  const rows = useMemo(() => overThreshold(drift.data?.data ?? [], threshold, now), [drift.data, threshold, now]);

  const columns: ColumnsType<DriftItem> = [
    {
      title: t('pairs.machine'),
      width: 200,
      dataIndex: 'name',
      render: (v: string, d) => (
        <Space size={8}>
          <Tag style={{ margin: 0 }}>{t(`roles.${d.role}`)}</Tag>
          <Typography.Text strong>{v}</Typography.Text>
        </Space>
      ),
    },
    {
      title: t('drift.pair'),
      width: 110,
      dataIndex: 'pair',
      render: (v: string) => <span className="mono">{v}</span>,
    },
    {
      title: t('pairs.revision'),
      width: 110,
      align: 'center',
      render: (_v, d) => (
        <Tag color="error" className="mono">
          {d.applied_rev ?? '-'} → {d.desired_rev}
        </Tag>
      ),
    },
    {
      title: t('drift.duration'),
      width: 130,
      dataIndex: 'since',
      defaultSortOrder: 'descend',
      sorter: (a, b) => Date.parse(b.since) - Date.parse(a.since),
      render: (v: string) => (
        <Tooltip title={formatDateTime(v)}>{duration(secondsSince(v, now) ?? 0)}</Tooltip>
      ),
    },
    {
      title: t('drift.reason'),
      dataIndex: 'last_error',
      ellipsis: true,
      render: (v: string | null) =>
        v ? <Typography.Text type="danger">{v}</Typography.Text> : <Typography.Text type="secondary">{t('drift.noError')}</Typography.Text>,
    },
    {
      title: t('common.actions'),
      width: 90,
      align: 'right',
      render: (_v, d) => (
        <Tooltip title={isMaster ? t('pairs.applyNow') : t('role.writeOnMaster')}>
          <Button
            size="small"
            icon={<ThunderboltOutlined />}
            disabled={!isMaster}
            loading={apply.isPending && apply.variables === d.target}
            onClick={() => apply.mutate(d.target)}
            aria-label={`${t('pairs.applyNow')} ${d.name}`}
          />
        </Tooltip>
      ),
    },
  ];

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.drift')}</h1>
        <Space wrap>
          <Typography.Text type="secondary">{t('drift.threshold')}</Typography.Text>
          <Segmented<number>
            value={threshold}
            onChange={setThreshold}
            options={THRESHOLDS.map((s) => ({ value: s, label: duration(s) }))}
          />
          <Button icon={<ReloadOutlined />} onClick={() => drift.refetch()} loading={drift.isFetching}>
            {t('common.refresh')}
          </Button>
        </Space>
      </div>
      {USES_MOCKS.drift && <MockBanner />}
      {drift.isError && <ErrorAlert error={drift.error} onRetry={() => drift.refetch()} />}
      <Card>
        {!drift.isPending && !drift.isError && rows.length === 0 ? (
          <Result status="success" title={t('drift.none')} subTitle={t('drift.noneHint', { threshold: duration(threshold) })} />
        ) : (
          <Table<DriftItem>
            rowKey="target"
            size="middle"
            columns={columns}
            dataSource={rows}
            loading={drift.isPending}
            pagination={false}
            scroll={{ x: 720 }}
            locale={{ emptyText: <Empty description={t('drift.none')} /> }}
          />
        )}
      </Card>
    </>
  );
}
