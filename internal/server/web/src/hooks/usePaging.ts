// Серверная пагинация {data, paging}: номер страницы, размер, offset для запроса
// и свойства antd Pagination. Если total уменьшился и текущей страницы больше
// нет - уходим на новую последнюю, иначе offset за пределами total покажет пустоту.

import { useEffect, useState } from 'react';
import type { PaginationProps } from 'antd';
import { useTranslation } from 'react-i18next';

export const PAGE_SIZES = [20, 50, 100];

export function usePaging(defaultSize = 20) {
  const { t } = useTranslation();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(defaultSize);
  const [total, setTotal] = useState<number | undefined>(undefined);

  useEffect(() => {
    if (total === undefined) return;
    const last = Math.max(1, Math.ceil(total / pageSize));
    if (page > last) setPage(last);
  }, [total, page, pageSize]);

  const pagination = (count: number | undefined, opts: { size?: 'small' } = {}): PaginationProps => ({
    current: page,
    pageSize,
    total: count ?? 0,
    size: opts.size,
    showSizeChanger: true,
    pageSizeOptions: PAGE_SIZES,
    showTotal: (n) => t('common.total', { count: n }),
    onChange: (p, size) => {
      setPage(size !== pageSize ? 1 : p);
      setPageSize(size);
    },
  });

  return {
    page,
    pageSize,
    offset: (page - 1) * pageSize,
    reset: () => setPage(1),
    // Вызывать с paging.total из ответа.
    syncTotal: setTotal,
    pagination,
  };
}
