import { Alert, Button } from 'antd';
import { useTranslation } from 'react-i18next';

import { describeError } from '@/api/errors';

// Ошибка загрузки данных экрана с кнопкой «повторить». title - для ошибок записи
// («не удалось добавить машину»); по умолчанию - «не удалось загрузить данные».
export default function ErrorAlert({ error, onRetry, title }: { error: unknown; onRetry?: () => void; title?: string }) {
  const { t } = useTranslation();
  return (
    <Alert
      type="error"
      showIcon
      style={{ marginBottom: 16 }}
      title={title ?? t('errors.loadFailed')}
      description={describeError(error, t)}
      action={onRetry ? <Button size="small" onClick={onRetry}>{t('common.retry')}</Button> : undefined}
    />
  );
}
