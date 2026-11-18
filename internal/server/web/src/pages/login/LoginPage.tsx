import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Card, Form, Input, Typography } from 'antd';
import { LockOutlined, UserOutlined } from '@ant-design/icons';

import { api } from '@/api/client';
import { ApiError } from '@/api/http';
import { describeError } from '@/api/errors';
import { useTheme } from '@/hooks/useTheme';
import './LoginPage.css';

interface LoginForm {
  username: string;
  password: string;
}

export default function LoginPage({ onSuccess }: { onSuccess: () => void }) {
  const { t } = useTranslation();
  const { isDark } = useTheme();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const submit = async ({ username, password }: LoginForm) => {
    setBusy(true);
    setError('');
    try {
      await api.login(username, password);
      onSuccess();
    } catch (e) {
      setError(e instanceof ApiError && e.status === 401 ? t('auth.badCredentials') : describeError(e, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={`login-page${isDark ? ' is-dark' : ''}`}>
      <Card className="login-card">
        <Typography.Title level={3} className="login-title">
          telemux
        </Typography.Title>
        <Typography.Paragraph type="secondary" className="login-subtitle">
          {t('auth.subtitle')}
        </Typography.Paragraph>
        <Form<LoginForm> layout="vertical" onFinish={submit} requiredMark={false} disabled={busy}>
          <Form.Item
            name="username"
            label={t('auth.username')}
            rules={[{ required: true, message: t('auth.usernameRequired') }]}
          >
            <Input prefix={<UserOutlined />} autoComplete="username" autoFocus />
          </Form.Item>
          <Form.Item
            name="password"
            label={t('auth.password')}
            rules={[{ required: true, message: t('auth.passwordRequired') }]}
          >
            <Input.Password prefix={<LockOutlined />} autoComplete="current-password" />
          </Form.Item>
          {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
          <Button type="primary" htmlType="submit" block loading={busy}>
            {t('auth.submit')}
          </Button>
        </Form>
      </Card>
    </div>
  );
}
