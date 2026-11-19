// «Сборка конфига» (ADR 0006): telemux сам собирает telemt.config / mtg.config машин
// сервера и раз в минуту сравнивает с текущими ревизиями (их пока публикует бот).
// Здесь - итог сравнения по каждой машине и ресурсу с diff «текущий → telemux» и
// переключатель источника: на telemux - только при полном совпадении (иначе 409
// render_mismatch и подтверждение принудительного переключения), обратно на бота - всегда.

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, App, Button, Collapse, Descriptions, Empty, Space, Spin, Tag, Tooltip, Typography } from 'antd';

import { describeError } from '@/api/errors';
import { ApiError } from '@/api/http';
import { useIsMaster, useServerRender, useSetConfigSource } from '@/api/queries';
import type { ConfigSource, Server, ServerRenderItem } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { formatDateTime } from '@/utils/format';
import './RenderTab.css';

// Код ошибки сборки из строки «код: текст».
export function renderErrorCode(error: string | null | undefined): string | null {
  if (!error) return null;
  const i = error.indexOf(':');
  return (i > 0 ? error.slice(0, i) : error).trim();
}

// Строки unified diff с подсветкой: добавленное, удалённое, заголовки ханков.
export function DiffView({ diff }: { diff: string }) {
  return (
    <pre className="diff-view" data-testid="render-diff">
      {diff.split('\n').map((line, i) => {
        const cls = line.startsWith('+++') || line.startsWith('---') ? 'diff-file' : line.startsWith('@@') ? 'diff-hunk' : line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-del' : undefined;
        return (
          <div key={i} className={cls}>
            {line || ' '}
          </div>
        );
      })}
    </pre>
  );
}

function ItemStatus({ it }: { it: ServerRenderItem }) {
  const { t } = useTranslation();
  const code = renderErrorCode(it.error);
  if (it.equal) return <Tag color="success">{t('render.equal')}</Tag>;
  if (code === 'not_assigned') return <Tag>{t('render.errors.not_assigned')}</Tag>;
  if (code) return <Tag color="error">{t(`render.errors.${code}`, { defaultValue: code })}</Tag>;
  return <Tag color="warning">{t('render.differs')}</Tag>;
}

const sha = (v: string | null | undefined) => (v ? <span className="mono">{v.slice(0, 12)}</span> : <Typography.Text type="secondary">-</Typography.Text>);

function ItemBody({ it }: { it: ServerRenderItem }) {
  const { t } = useTranslation();
  return (
    <Space orientation="vertical" size={12} style={{ width: '100%' }}>
      {it.error && <Alert type={renderErrorCode(it.error) === 'not_assigned' ? 'info' : 'error'} showIcon title={it.error} />}
      <Descriptions
        size="small"
        column={1}
        items={[
          { key: 'cur', label: t('render.currentSha'), children: sha(it.current_sha) },
          { key: 'cand', label: t('render.candidateSha'), children: sha(it.candidate_sha) },
          { key: 'at', label: t('render.renderedAt'), children: formatDateTime(it.rendered_at) },
        ]}
      />
      {it.diff ? <DiffView diff={it.diff} /> : <Typography.Text type="secondary">{it.equal ? t('render.noDiff') : t('render.noCandidate')}</Typography.Text>}
    </Space>
  );
}

export default function RenderTab({ server }: { server: Server }) {
  const { t } = useTranslation();
  const { message, modal } = App.useApp();
  const isMaster = useIsMaster();
  const render = useServerRender(server.no);
  const setSource = useSetConfigSource();
  // Источник после переключения - из ответа ручки, до обновления списка серверов.
  const [source, setLocal] = useState<ConfigSource>(server.config_source ?? 'bot');
  const items = render.data?.data ?? [];
  const allEqual = items.length > 0 && items.every((i) => i.equal);

  const apply = (next: ConfigSource, force = false) =>
    setSource.mutate(
      { no: server.no, source: next, force },
      {
        onSuccess: (s) => {
          setLocal(s?.config_source ?? next);
          message.success(t(next === 'telemux' ? 'render.switchedTelemux' : 'render.switchedBot', { no: server.no }));
        },
        onError: (e) => {
          if (e instanceof ApiError && e.code === 'render_mismatch' && !force) {
            modal.confirm({
              title: t('render.mismatchTitle'),
              content: (
                <Space orientation="vertical">
                  <span>{e.message}</span>
                  <span>{t('render.mismatchForce')}</span>
                </Space>
              ),
              okText: t('render.forceOk'),
              okButtonProps: { danger: true },
              cancelText: t('common.cancel'),
              onOk: () => apply(next, true),
            });
            return;
          }
          message.error(describeError(e, t));
        },
      },
    );

  const ask = (next: ConfigSource) =>
    modal.confirm({
      title: t(next === 'telemux' ? 'render.toTelemuxTitle' : 'render.toBotTitle', { no: server.no }),
      content: t(next === 'telemux' ? 'render.toTelemuxText' : 'render.toBotText'),
      okText: t('render.switch'),
      cancelText: t('common.cancel'),
      onOk: () => apply(next),
    });

  const next: ConfigSource = source === 'telemux' ? 'bot' : 'telemux';

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }} data-testid="server-render">
      <Descriptions
        size="small"
        bordered
        column={1}
        items={[
          {
            key: 'source',
            label: t('render.source'),
            children: (
              <Space wrap>
                <Tag color={source === 'telemux' ? 'blue' : 'default'} data-testid="config-source">
                  {t(`render.sources.${source}`)}
                </Tag>
                <Tooltip title={isMaster ? undefined : t('role.writeOnMaster')}>
                  <Button size="small" disabled={!isMaster} loading={setSource.isPending} onClick={() => ask(next)}>
                    {t(next === 'telemux' ? 'render.toTelemux' : 'render.toBot')}
                  </Button>
                </Tooltip>
              </Space>
            ),
          },
          {
            key: 'summary',
            label: t('render.summary'),
            children: render.isPending ? (
              <Spin size="small" />
            ) : items.length === 0 ? (
              <Typography.Text type="secondary">{t('render.never')}</Typography.Text>
            ) : allEqual ? (
              <Tag color="success">{t('render.allEqual')}</Tag>
            ) : (
              <Tag color="warning">{t('render.notAllEqual', { n: items.filter((i) => !i.equal).length, total: items.length })}</Tag>
            ),
          },
        ]}
      />
      <Typography.Text type="secondary">{t('render.hint')}</Typography.Text>
      {render.isError && <ErrorAlert error={render.error} onRetry={() => render.refetch()} />}
      {render.isSuccess && items.length === 0 && <Empty description={t('render.never')} />}
      {items.length > 0 && (
        <Collapse
          size="small"
          items={items.map((it) => ({
            key: `${it.target}/${it.resource}`,
            label: (
              <Space wrap>
                <span className="mono">{it.target}</span>
                <span className="mono">{it.resource}</span>
                <ItemStatus it={it} />
              </Space>
            ),
            children: <ItemBody it={it} />,
          }))}
        />
      )}
    </Space>
  );
}
