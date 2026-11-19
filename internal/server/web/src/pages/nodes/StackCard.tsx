// «Версии и обновления» (ADR 0008): последние релизы telemt и mtg-multi на GitHub и
// версии на каждой машине. Обновление - по кнопке: мастер скачивает релиз и публикует
// машине ресурс stack, агент меняет бинарь и сам откатывает, если сервис не поднялся.
// Своя сборка (суффикс после X.Y.Z, например 1.15.0-webalias) обновляется только с
// подтверждением (force): её правки пропадут. Пока обновление идёт - опрос раз в 5 с,
// и второе обновление на той же машине (любого продукта) не запускается: мастер ответит
// 409 update_in_progress, поэтому кнопки обоих продуктов машины выключены.

import { useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, App, Button, Card, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { CloudDownloadOutlined, SyncOutlined } from '@ant-design/icons';

import { describeError } from '@/api/errors';
import { ApiError } from '@/api/http';
import { useCheckStack, useIsMaster, useStack, useUpdateMachineProduct } from '@/api/queries';
import type { Product, StackMachine, StackProduct, StackProductState } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { KindTag } from '@/components/FleetTags';
import { usePaging } from '@/hooks/usePaging';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatDate, formatDateTime, productName } from '@/utils/format';

// Коды 409, которые снимаются подтверждением с force.
const FORCEABLE = new Set(['custom_build', 'not_newer']);

export type VersionView = 'none' | 'unknown' | 'pending' | 'failed' | 'available' | 'custom' | 'current';

// На машине идёт обновление хоть одного продукта.
export function machineBusy(m: StackMachine): boolean {
  return m.telemt?.update?.status === 'pending' || m.mtg?.update?.status === 'pending';
}

// Что показать в ячейке продукта: идущее или упавшее обновление важнее доступного.
export function versionView(s: StackProductState | null): VersionView {
  if (!s) return 'none';
  if (s.update?.status === 'pending') return 'pending';
  if (s.update?.status === 'failed' && s.current !== s.update.to) return 'failed';
  if (!s.current) return 'unknown';
  if (s.update_available) return 'available';
  if (s.custom_build) return 'custom';
  return 'current';
}

function ProductCell({ m, product }: { m: StackMachine; product: Product }) {
  const { t } = useTranslation();
  const { message, modal } = App.useApp();
  const isMaster = useIsMaster();
  const update = useUpdateMachineProduct();
  const s = m[product];
  const view = versionView(s);
  if (!s) return <Typography.Text type="secondary">-</Typography.Text>;

  const run = (force: boolean) =>
    update.mutate(
      { code: m.code, body: { product, force } },
      {
        onSuccess: (u) => message.success(t('stack.started', { code: m.code, product: productName(product), to: u?.to ?? s.latest ?? '' })),
        onError: (e) => {
          if (e instanceof ApiError && e.status === 409 && FORCEABLE.has(e.code) && !force) {
            modal.confirm({
              title: t(`stack.force.${e.code}`),
              content: e.message,
              okText: t('stack.forceOk'),
              okButtonProps: { danger: true },
              cancelText: t('common.cancel'),
              onOk: () => run(true),
            });
            return;
          }
          // Нет файла под архитектуру машины - повтор не поможет: текст в окне.
          if (e instanceof ApiError && e.code === 'asset_missing') {
            modal.error({ title: t('stack.assetMissing', { code: m.code, product: productName(product) }), content: e.message });
            return;
          }
          message.error(describeError(e, t));
        },
      },
    );

  // Своя сборка - сразу с предупреждением и force; обычное обновление - простое подтверждение.
  const ask = () =>
    s.custom_build
      ? modal.confirm({
          title: t('stack.customTitle', { code: m.code, product: productName(product) }),
          content: t('stack.customText', { current: s.current, latest: s.latest }),
          okText: t('stack.forceOk'),
          okButtonProps: { danger: true },
          cancelText: t('common.cancel'),
          onOk: () => run(true),
        })
      : modal.confirm({
          title: t('stack.confirmTitle', { code: m.code, product: productName(product), to: s.latest }),
          content: t('stack.confirmText'),
          okText: t('stack.update'),
          cancelText: t('common.cancel'),
          onOk: () => run(false),
        });

  const busy = machineBusy(m);
  const canUpdate = isMaster && m.enabled && !!s.latest && !busy;
  const hint = !isMaster
    ? t('role.writeOnMaster')
    : !m.enabled
      ? t('stack.machineDisabled')
      : busy
        ? t('stack.busy')
        : undefined;
  const showButton = view === 'available' || view === 'failed' || (view === 'custom' && !!s.latest);

  return (
    <Space size={6} wrap data-testid={`stack-${m.code}-${product}`} data-view={view}>
      {s.current ? <span className="mono">{s.current}</span> : <Typography.Text type="secondary">{t('stack.unknown')}</Typography.Text>}
      {s.custom_build && (
        <Tooltip title={t('stack.customHint')}>
          <Tag>{t('stack.custom')}</Tag>
        </Tooltip>
      )}
      {view === 'pending' && (
        <Tag icon={<SyncOutlined spin />} color="processing">
          {t('stack.pending', { to: s.update?.to })}
        </Tag>
      )}
      {view === 'failed' && (
        <Tooltip title={s.update?.error}>
          <Tag color="error">{t('stack.failed', { to: s.update?.to })}</Tag>
        </Tooltip>
      )}
      {view === 'available' && <Tag color="warning">{t('stack.available', { latest: s.latest })}</Tag>}
      {showButton && (
        <Tooltip title={hint}>
          <Button
            size="small"
            icon={<CloudDownloadOutlined />}
            disabled={!canUpdate}
            loading={update.isPending}
            onClick={ask}
            aria-label={t('stack.updateAria', { code: m.code, product: productName(product) })}
          >
            {view === 'custom' ? t('stack.updateAnyway') : t('stack.update')}
          </Button>
        </Tooltip>
      )}
    </Space>
  );
}

function ProductRelease({ p }: { p: StackProduct }) {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  return (
    <Space size={6} wrap data-testid={`release-${p.product}`}>
      <Typography.Text strong>{productName(p.product)}</Typography.Text>
      {p.latest ? (
        <Tooltip title={t('stack.publishedAt', { date: formatDate(p.latest.published_at) })}>
          <Tag color="blue" className="mono">
            {p.latest.version}
          </Tag>
        </Tooltip>
      ) : (
        <Tag>{t('stack.noRelease')}</Tag>
      )}
      <Typography.Text type="secondary" className="mono">
        {p.repo}
      </Typography.Text>
      {p.checked_at && (
        <Tooltip title={formatDateTime(p.checked_at)}>
          <Typography.Text type="secondary">{t('stack.checked', { ago: relative(secondsSince(p.checked_at, now)) })}</Typography.Text>
        </Tooltip>
      )}
      {p.error && (
        <Tooltip title={p.error}>
          <Tag color="error">{t('stack.checkError')}</Tag>
        </Tooltip>
      )}
    </Space>
  );
}

export default function StackCard() {
  const { t } = useTranslation();
  const { message } = App.useApp();
  const isMaster = useIsMaster();
  const paging = usePaging(20);
  const stack = useStack(paging.pageSize, paging.offset);
  const check = useCheckStack();
  const { syncTotal } = paging;
  const total = stack.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);
  // Старый мастер без ручки обновлений (404) - подсказка вместо ошибки.
  const unsupported = stack.error instanceof ApiError && stack.error.status === 404;

  const columns = useMemo<ColumnsType<StackMachine>>(
    () => [
      {
        title: t('nodes.code'),
        dataIndex: 'code',
        width: 150,
        render: (v: string, m) => (
          <Space size={6}>
            <KindTag kind={m.kind} />
            <Typography.Text strong className="mono">
              {v}
            </Typography.Text>
            {!m.enabled && <Tag>{t('nodes.disabled')}</Tag>}
          </Space>
        ),
      },
      { title: 'telemt', key: 'telemt', render: (_v, m) => <ProductCell m={m} product="telemt" /> },
      { title: 'mtg-multi', key: 'mtg', render: (_v, m) => <ProductCell m={m} product="mtg" /> },
    ],
    [t],
  );

  return (
    <Card
      title={t('stack.title')}
      extra={
        <Tooltip title={isMaster ? t('stack.checkHint') : t('role.writeOnMaster')}>
          <Button
            icon={<SyncOutlined />}
            disabled={!isMaster || unsupported}
            loading={check.isPending}
            onClick={() =>
              check.mutate(undefined, {
                onSuccess: () => message.success(t('stack.checkedOk')),
                onError: (e) =>
                  message.error(e instanceof ApiError && e.status === 429 ? t('stack.tooOften') : describeError(e, t)),
              })
            }
          >
            {t('stack.check')}
          </Button>
        </Tooltip>
      }
      style={{ marginBottom: 16 }}
      data-testid="stack-card"
    >
      {unsupported ? (
        <Alert type="info" showIcon title={t('stack.unsupported')} data-testid="stack-unsupported" />
      ) : (
        stack.isError && <ErrorAlert error={stack.error} onRetry={() => stack.refetch()} />
      )}
      {stack.data && (
        <Space orientation="vertical" size={4} style={{ marginBottom: 12 }}>
          {stack.data.products.map((p) => (
            <ProductRelease key={p.product} p={p} />
          ))}
        </Space>
      )}
      {!unsupported && (
      <Table<StackMachine>
        rowKey="code"
        size="small"
        columns={columns}
        dataSource={stack.data?.data ?? []}
        loading={stack.isPending}
        scroll={{ x: 760 }}
        locale={{ emptyText: stack.isSuccess ? t('stack.empty') : <span /> }}
        pagination={paging.pagination(total, { size: 'small' })}
      />
      )}
    </Card>
  );
}
