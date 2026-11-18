import { Alert, Button } from 'antd';
import { useTranslation } from 'react-i18next';

import { describeError } from '@/api/errors';

// Ошибка загрузки данных экрана с кнопкой «повторить».
export default function ErrorAlert({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const { t } = useTranslation();
  return (
    <Alert
      type="error"
      showIcon
      style={{ marginBottom: 16 }}
      title={t('errors.loadFailed')}
      description={describeError(error, t)}
      action={onRetry ? <Button size="small" onClick={onRetry}>{t('common.retry')}</Button> : undefined}
    />
  );
}
