// Источник: форк 3x-ui, frontend/src/pages/api-docs/ApiDocsPage.tsx (GPL-3.0). Адаптировано:
// спека /api/openapi.json загружается заранее, чтобы на 404 (спеки ещё нет) показать
// понятное сообщение вместо пустого swagger-ui; убраны вкладки WebSocket-событий.

import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Button, Card, Result, Spin } from 'antd';
import SwaggerUI from 'swagger-ui-react';
import 'swagger-ui-react/swagger-ui.css';

import { request, ApiError } from '@/api/http';
import { describeError } from '@/api/errors';
import { useTheme } from '@/hooks/useTheme';
import './ApiDocsPage.css';

export const OPENAPI_URL = '/api/openapi.json';

export default function ApiDocsPage() {
  const { t } = useTranslation();
  const { isDark } = useTheme();
  const spec = useQuery({
    queryKey: ['openapi'],
    queryFn: () => request<Record<string, unknown>>(OPENAPI_URL),
    staleTime: Infinity,
    retry: false,
  });

  if (spec.isPending) {
    return (
      <div className="page-loading">
        <Spin size="large" />
      </div>
    );
  }
  if (spec.isError) {
    const missing = spec.error instanceof ApiError && spec.error.status === 404;
    return (
      <Card>
        <Result
          status={missing ? 'info' : 'warning'}
          title={missing ? t('apiDocs.missingTitle') : t('apiDocs.failedTitle')}
          subTitle={missing ? t('apiDocs.missingHint', { url: OPENAPI_URL }) : describeError(spec.error, t)}
          extra={<Button onClick={() => spec.refetch()}>{t('common.retry')}</Button>}
        />
      </Card>
    );
  }
  return (
    <div className={`api-docs${isDark ? ' is-dark' : ''}`} role="region" aria-label={t('menu.apiDocs')}>
      <SwaggerUI spec={spec.data} docExpansion="list" deepLinking={false} tryItOutEnabled />
    </div>
  );
}
