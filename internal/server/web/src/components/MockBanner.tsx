import { Alert } from 'antd';
import { useTranslation } from 'react-i18next';

// Экран показывает заглушки: ручек на мастере ещё нет.
export default function MockBanner() {
  const { t } = useTranslation();
  return <Alert type="info" showIcon style={{ marginBottom: 16 }} title={t('common.mockData')} />;
}
