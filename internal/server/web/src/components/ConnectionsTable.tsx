// Подключения пользователей по последним снимкам машин (/api/v1/connections):
// фильтры (машина, продукт, минимум подключений, поиск по пользователю - префикс),
// серверная пагинация, активные IP раскрываются списком, автообновление раз в 15 с.
// Используется на экране «Подключения» и во вкладке карточки машины (машина задана).

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Input, InputNumber, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { SearchOutlined } from '@ant-design/icons';

import { useConnections } from '@/api/queries';
import type { ConnFilter, Connection, Product } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { useDebounced } from '@/hooks/useDebounced';
import { usePaging } from '@/hooks/usePaging';
import { secondsSince, useNow, useRelativeTime } from '@/hooks/useRelativeTime';
import { formatBytes, formatDateTime } from '@/utils/format';

export const PRODUCTS: Product[] = ['telemt', 'mtg'];
const PRODUCT_COLOR: Record<Product, string> = { telemt: 'blue', mtg: 'purple' };

export function ProductTag({ product }: { product: Product }) {
  return (
    <Tag color={PRODUCT_COLOR[product]} style={{ margin: 0 }}>
      {product}
    </Tag>
  );
}

export const connKey = (c: Connection) => `${c.machine}/${c.product}/${c.key}`;

interface ConnectionsTableProps {
  // Машина задана (карточка машины): фильтра и колонки «Машина» нет.
  fixedMachine?: string;
  // Машины для фильтра; пусто - фильтр по машине не показывается.
  machineOptions?: string[];
  // Минимум подключений по умолчанию: 1 - только пользователи онлайн.
  defaultMinConnections?: number;
}

export default function ConnectionsTable({ fixedMachine, machineOptions = [], defaultMinConnections = 1 }: ConnectionsTableProps) {
  const { t } = useTranslation();
  const now = useNow();
  const relative = useRelativeTime();
  const [machine, setMachine] = useState<string | undefined>(undefined);
  const [product, setProduct] = useState<Product | undefined>(undefined);
  const [minConns, setMinConns] = useState<number | null>(defaultMinConnections);
  const [search, setSearch] = useState('');
  const user = useDebounced(search.trim(), 300);

  const filter = useMemo<ConnFilter>(
    () => ({
      machine: fixedMachine ?? machine,
      product,
      minConnections: minConns ?? undefined,
      user: user || undefined,
    }),
    [fixedMachine, machine, product, minConns, user],
  );

  const paging = usePaging(20);
  const { reset, syncTotal } = paging;
  // Новый фильтр - с первой страницы.
  // reset не стабилен между рендерами, в зависимостях только фильтр.
  useEffect(() => reset(), [filter.machine, filter.product, filter.minConnections, filter.user]);
  const conns = useConnections(filter, paging.pageSize, paging.offset);
  const total = conns.data?.paging.total;
  useEffect(() => syncTotal(total), [syncTotal, total]);

  const columns = useMemo<ColumnsType<Connection>>(() => {
    const cols: ColumnsType<Connection> = [
      {
        title: t('connections.user'),
        dataIndex: 'key',
        width: 180,
        render: (v: string) => (
          <Typography.Text strong className="mono">
            {v}
          </Typography.Text>
        ),
      },
    ];
    if (!fixedMachine) {
      cols.push({ title: t('connections.machine'), dataIndex: 'machine', width: 120, render: (v: string) => <span className="mono">{v}</span> });
    }
    cols.push(
      { title: t('connections.product'), dataIndex: 'product', width: 90, render: (v: Product) => <ProductTag product={v} /> },
      { title: t('connections.connections'), dataIndex: 'connections', width: 110, align: 'right', render: (v: number) => <span className="mono">{v}</span> },
      {
        title: t('connections.ips'),
        width: 110,
        align: 'right',
        render: (_v, c) =>
          c.active_ips.length > 0 ? (
            <span className="mono">{c.active_ips.length}</span>
          ) : (
            <Typography.Text type="secondary">{t('connections.noIps')}</Typography.Text>
          ),
      },
      {
        title: t('connections.traffic'),
        width: 170,
        render: (_v, c) =>
          c.product === 'mtg' ? (
            <span className="mono">
              {formatBytes(c.bytes_in)} / {formatBytes(c.bytes_out)}
            </span>
          ) : (
            <Tooltip title={t('connections.trafficMtgOnly')}>
              <Typography.Text type="secondary">-</Typography.Text>
            </Tooltip>
          ),
      },
      {
        title: t('connections.lastSeen'),
        width: 170,
        render: (_v, c) => {
          const at = c.last_seen ?? c.seen_at;
          return (
            <Space size={6}>
              <Tooltip title={formatDateTime(at)}>{relative(secondsSince(at, now))}</Tooltip>
              {c.stale && <Tag color="warning">{t('connections.stale')}</Tag>}
            </Space>
          );
        },
      },
    );
    return cols;
  }, [t, fixedMachine, relative, now]);

  return (
    <>
      <Space wrap style={{ marginBottom: 12 }}>
        <Input
          allowClear
          prefix={<SearchOutlined />}
          placeholder={t('connections.search')}
          aria-label={t('connections.search')}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          style={{ width: 220 }}
        />
        {!fixedMachine && machineOptions.length > 0 && (
          <Select<string>
            allowClear
            aria-label={t('connections.machine')}
            placeholder={t('connections.allMachines')}
            value={machine}
            onChange={(v) => setMachine(v)}
            options={machineOptions.map((m) => ({ value: m, label: m }))}
            style={{ width: 160 }}
          />
        )}
        <Select<Product>
          allowClear
          aria-label={t('connections.product')}
          placeholder={t('connections.allProducts')}
          value={product}
          onChange={(v) => setProduct(v)}
          options={PRODUCTS.map((p) => ({ value: p, label: p }))}
          style={{ width: 140 }}
        />
        <Space size={6}>
          <Typography.Text type="secondary">{t('connections.minConnections')}</Typography.Text>
          <InputNumber
            min={0}
            precision={0}
            aria-label={t('connections.minConnections')}
            value={minConns}
            onChange={(v) => setMinConns(typeof v === 'number' ? v : null)}
            style={{ width: 80 }}
          />
        </Space>
      </Space>
      {conns.isError && <ErrorAlert error={conns.error} onRetry={() => conns.refetch()} />}
      <Table<Connection>
        rowKey={connKey}
        size="small"
        columns={columns}
        dataSource={conns.data?.data ?? []}
        loading={conns.isPending || (conns.isFetching && conns.isPlaceholderData)}
        scroll={{ x: fixedMachine ? 840 : 960 }}
        locale={{ emptyText: conns.isSuccess ? t('connections.empty') : <span /> }}
        pagination={paging.pagination(total, { size: 'small' })}
        expandable={{
          rowExpandable: (c) => c.active_ips.length > 0,
          expandedRowRender: (c) => (
            <Space size={[6, 6]} wrap data-testid={`ips-${connKey(c)}`}>
              {c.active_ips.map((ip) => (
                <Tag key={ip} className="mono" style={{ margin: 0 }}>
                  {ip}
                </Tag>
              ))}
            </Space>
          ),
        }}
      />
    </>
  );
}
