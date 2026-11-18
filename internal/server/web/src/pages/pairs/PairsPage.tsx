// «Пары» - главный экран (docs/PLAN.md, раздел 3): релей, два его сервера, два выхода;
// желаемое против применённого, версии и канарейки. Мультивыбор машин с общим действием
// «применить сейчас» - как bulk-обновление нод в NodesPage форка 3x-ui.

import { useCallback, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { App, Button, Card, Empty, Space, Tag, Typography } from 'antd';
import { CheckCircleOutlined, ReloadOutlined, ThunderboltOutlined, WarningOutlined } from '@ant-design/icons';

import { api, USES_MOCKS } from '@/api/client';
import { describeError } from '@/api/errors';
import { qk, useIsMaster, usePairs } from '@/api/queries';
import type { Pair, Target } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import MockBanner from '@/components/MockBanner';
import PairTable, { isDrifting, pairRows } from './PairTable';

function PairSummary({ pair }: { pair: Pair }) {
  const { t } = useTranslation();
  const rows = pairRows(pair);
  const drift = rows.filter(isDrifting).length;
  const down = rows.filter((r) => r.status === 'offline').length;
  return (
    <Space size={4} wrap>
      {drift === 0 && down === 0 && (
        <Tag icon={<CheckCircleOutlined />} color="success">
          {t('pairs.allApplied')}
        </Tag>
      )}
      {drift > 0 && (
        <Tag icon={<WarningOutlined />} color="warning">
          {t('pairs.driftCount', { count: drift })}
        </Tag>
      )}
      {down > 0 && <Tag color="error">{t('pairs.offlineCount', { count: down })}</Tag>}
    </Space>
  );
}

export default function PairsPage() {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const qc = useQueryClient();
  const isMaster = useIsMaster();
  const pairs = usePairs();
  // Выбор - по каждой паре отдельно, общий список - для bulk-действия.
  const [selection, setSelection] = useState<Record<string, string[]>>({});
  const [busy, setBusy] = useState<Set<string>>(new Set());

  const selected = useMemo(() => Object.values(selection).flat(), [selection]);

  const mark = (codes: string[], on: boolean) =>
    setBusy((prev) => {
      const next = new Set(prev);
      codes.forEach((c) => (on ? next.add(c) : next.delete(c)));
      return next;
    });

  const apply = useMutation({
    mutationFn: async (codes: string[]) => {
      mark(codes, true);
      try {
        // Ошибка на одной машине не должна отменять остальные.
        return await Promise.allSettled(codes.map((c) => api.applyTarget(c)));
      } finally {
        mark(codes, false);
      }
    },
    onSuccess: (results, codes) => {
      const failed = results.filter((r) => r.status === 'rejected') as PromiseRejectedResult[];
      if (failed.length === 0) {
        message.success(t('pairs.applyQueued', { count: codes.length }));
      } else {
        message.error(`${t('pairs.applyFailed', { count: failed.length })}: ${describeError(failed[0].reason, t)}`);
      }
      setSelection({});
      void qc.invalidateQueries({ queryKey: qk.pairs });
    },
  });

  const retry = useMutation({
    mutationFn: (code: string) => api.retryTarget(code),
    onSuccess: (_r, code) => {
      message.success(t('pairs.retryQueued', { name: code }));
      void qc.invalidateQueries({ queryKey: qk.pairs });
    },
    onError: (e) => message.error(describeError(e, t)),
  });

  // mutate у react-query стабилен между рендерами - колонки таблиц не пересобираются.
  const applyMutate = apply.mutate;
  const retryMutate = retry.mutate;
  const onApply = useCallback((tg: Target) => applyMutate([tg.code]), [applyMutate]);
  const onRetry = useCallback((tg: Target) => retryMutate(tg.code), [retryMutate]);

  return (
    <>
      <div className="page-header">
        <h1>{t('menu.pairs')}</h1>
        <Space wrap>
          {selected.length > 0 && (
            <Typography.Text type="secondary">{t('pairs.selected', { count: selected.length })}</Typography.Text>
          )}
          <Button
            type="primary"
            icon={<ThunderboltOutlined />}
            disabled={!isMaster || selected.length === 0}
            loading={apply.isPending}
            onClick={() => apply.mutate(selected)}
          >
            {t('pairs.applySelected')}
          </Button>
          <Button icon={<ReloadOutlined />} onClick={() => pairs.refetch()} loading={pairs.isFetching}>
            {t('common.refresh')}
          </Button>
        </Space>
      </div>
      {USES_MOCKS.pairs && <MockBanner />}
      {pairs.isError && <ErrorAlert error={pairs.error} onRetry={() => pairs.refetch()} />}
      {pairs.isPending && <Card loading />}
      {pairs.data?.length === 0 && (
        <Card>
          <Empty description={t('pairs.empty')} />
        </Card>
      )}
      <Space orientation="vertical" size={16} style={{ width: '100%' }}>
        {pairs.data?.map((p) => (
          <Card
            key={p.code}
            size="small"
            title={
              <Space wrap>
                <span>{t('pairs.pairTitle', { name: p.name })}</span>
                <Typography.Text type="secondary" className="mono">
                  {p.code}
                </Typography.Text>
              </Space>
            }
            extra={<PairSummary pair={p} />}
          >
            <PairTable
              pair={p}
              isMaster={isMaster}
              selected={selection[p.code] ?? []}
              onSelectionChange={(codes) => setSelection((s) => ({ ...s, [p.code]: codes }))}
              onApply={onApply}
              onRetry={onRetry}
              busy={busy}
            />
          </Card>
        ))}
      </Space>
    </>
  );
}
