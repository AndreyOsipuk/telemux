// «Пары» - главный экран (docs/PLAN.md, раздел 3): релей и его выходы; у каждой
// машины сводный статус, желаемое против применённого по ресурсам, версия агента,
// мост и проверки. Клик по машине - история применений. Данные - /api/v1/pairs,
// постранично по релеям; псевдо-пара «без релея» (relay: null) - в конце списка.

import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button, Card, Drawer, Empty, Pagination, Space, Tag, Typography } from 'antd';
import { CheckCircleOutlined, ReloadOutlined } from '@ant-design/icons';

import { usePairs } from '@/api/queries';
import type { Pair, PairMachine, PairStatus } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { usePaging } from '@/hooks/usePaging';
import PairTable, { pairKey, pairMachines } from './PairTable';
import TargetHistory from './TargetHistory';

const SUMMARY_COLOR: Record<Exclude<PairStatus, 'ok'>, string> = {
  behind: 'processing',
  failed: 'error',
  silent: 'warning',
  no_agent: 'default',
  disabled: 'default',
};

// Сколько машин пары в каждом статусе, кроме ok.
export function statusCounts(p: Pair): Partial<Record<PairStatus, number>> {
  const out: Partial<Record<PairStatus, number>> = {};
  for (const m of pairMachines(p)) out[m.status] = (out[m.status] ?? 0) + 1;
  return out;
}

function PairSummary({ pair }: { pair: Pair }) {
  const { t } = useTranslation();
  const counts = statusCounts(pair);
  const problems = (Object.keys(SUMMARY_COLOR) as (keyof typeof SUMMARY_COLOR)[]).filter((s) => counts[s]);
  if (problems.length === 0) {
    return (
      <Tag icon={<CheckCircleOutlined />} color="success">
        {t('pairs.allOk')}
      </Tag>
    );
  }
  return (
    <Space size={4} wrap>
      {problems.map((s) => (
        <Tag key={s} color={SUMMARY_COLOR[s]}>
          {t(`status.${s}`)}: {counts[s]}
        </Tag>
      ))}
    </Space>
  );
}

export default function PairsPage() {
  const { t } = useTranslation();
  const paging = usePaging(20);
  const pairs = usePairs(paging.pageSize, paging.offset);
  const [opened, setOpened] = useState<PairMachine | null>(null);

  const { syncTotal } = paging;
  const total = pairs.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const onOpen = useCallback((m: PairMachine) => setOpened(m), []);
  const list = pairs.data?.data;

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.pairs')}</h1>
        <Button icon={<ReloadOutlined />} onClick={() => pairs.refetch()} loading={pairs.isFetching}>
          {t('common.refresh')}
        </Button>
      </div>
      {pairs.isError && <ErrorAlert error={pairs.error} onRetry={() => pairs.refetch()} />}
      {pairs.isPending && <Card loading />}
      {list?.length === 0 && (
        <Card>
          <Empty description={t('pairs.empty')} />
        </Card>
      )}
      <Space orientation="vertical" size={16} style={{ width: '100%' }}>
        {list?.map((p) => (
          <Card
            key={pairKey(p)}
            size="small"
            data-testid={`pair-${pairKey(p)}`}
            title={
              p.relay ? (
                <Space wrap>
                  <span>{t('pairs.relayTitle')}</span>
                  <Typography.Text strong className="mono">
                    {p.relay.code}
                  </Typography.Text>
                </Space>
              ) : (
                <Space wrap>
                  <span>{t('pairs.noRelay')}</span>
                  <Typography.Text type="secondary">{t('pairs.noRelayHint')}</Typography.Text>
                </Space>
              )
            }
            extra={<PairSummary pair={p} />}
          >
            <PairTable pair={p} onOpen={onOpen} />
          </Card>
        ))}
        {list !== undefined && (total ?? 0) > 0 && (
          <Pagination align="end" {...paging.pagination(total)} />
        )}
      </Space>
      <Drawer
        open={opened !== null}
        onClose={() => setOpened(null)}
        size="large"
        destroyOnHidden
        title={opened ? t('history.title', { code: opened.code }) : undefined}
      >
        {opened && <TargetHistory code={opened.code} />}
      </Drawer>
    </>
  );
}
