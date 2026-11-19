// Общие ячейки машин флота: вид машины, ресурсы (желаемая и применённая ревизия),
// локальные проверки агента. Используются на «Парах» и «Машинах».

import { Space, Tag, Tooltip, Typography } from 'antd';
import { useTranslation } from 'react-i18next';

import type { DesiredResource, MachineKind, ObservedCheck, ObservedResource } from '@/api/types';

const KIND_COLOR: Record<MachineKind, string> = { relay: 'purple', exit: 'cyan', panel: 'geekblue' };

export function KindTag({ kind }: { kind: MachineKind }) {
  const { t } = useTranslation();
  return (
    <Tag color={KIND_COLOR[kind]} style={{ margin: 0, minWidth: 56, textAlign: 'center' }}>
      {t(`kinds.${kind}`)}
    </Tag>
  );
}

export type ResourceState = 'applied' | 'behind' | 'failed' | 'pending' | 'compared' | 'missing';

export interface ResourceRow {
  resource: string;
  // Желаемая ревизия; null - желаемого нет (экран «Машины» его не знает).
  desired: number | null;
  // Применённая (из отчёта агента); null - агент о ресурсе не сообщал.
  observed: number | null;
  state: ResourceState;
  error?: string;
}

// Сводит список ресурсов машины, желаемое и отчёт агента в строку на ресурс.
// Порядок - как в resources, затем ресурсы, известные только из desired/observed.
export function resourceRows(
  resources: string[],
  observed: ObservedResource[],
  desired: DesiredResource[] | null = null,
): ResourceRow[] {
  const names = [...resources];
  for (const r of [...(desired ?? []), ...observed]) if (!names.includes(r.resource)) names.push(r.resource);
  return names.map((resource) => {
    const o = observed.find((x) => x.resource === resource);
    const d = desired ? (desired.find((x) => x.resource === resource)?.revision ?? null) : null;
    let state: ResourceState;
    if (!o) state = 'missing';
    else if (o.status === 'failed') state = 'failed';
    else if (d !== null && o.revision < d) state = 'behind';
    else state = o.status;
    return { resource, desired: d, observed: o?.revision ?? null, state, error: o?.error };
  });
}

const STATE_COLOR: Record<ResourceState, string> = {
  applied: 'success',
  behind: 'processing',
  failed: 'error',
  pending: 'default',
  compared: 'gold',
  missing: 'default',
};

export function ResourceTags({ rows }: { rows: ResourceRow[] }) {
  const { t } = useTranslation();
  if (rows.length === 0) return <Typography.Text type="secondary">{t('fleet.noResources')}</Typography.Text>;
  return (
    <Space size={4} wrap>
      {rows.map((r) => {
        const rev = r.desired === null ? `${r.observed ?? '-'}` : `${r.observed ?? '-'}/${r.desired}`;
        const tip = [
          t(`resourceState.${r.state}`),
          r.desired !== null ? t('fleet.revisions', { observed: r.observed ?? '-', desired: r.desired }) : null,
          r.error ?? null,
        ]
          .filter(Boolean)
          .join('. ');
        return (
          <Tooltip key={r.resource} title={tip}>
            <Tag
              color={STATE_COLOR[r.state]}
              className="mono"
              style={{ margin: 0 }}
              data-testid={`res-${r.resource}`}
              data-state={r.state}
            >
              {r.resource} {rev}
            </Tag>
          </Tooltip>
        );
      })}
      {rows
        .filter((r) => r.error)
        .map((r) => (
          <ErrorText key={`err-${r.resource}`} text={`${r.resource}: ${r.error}`} />
        ))}
    </Space>
  );
}

// Текст ошибки агента: длинный обрезается, целиком - в подсказке.
export function ErrorText({ text }: { text: string }) {
  return (
    <Typography.Text type="danger" ellipsis={{ tooltip: text }} style={{ maxWidth: 260 }}>
      {text}
    </Typography.Text>
  );
}

export function CheckTags({ checks }: { checks: ObservedCheck[] }) {
  const { t } = useTranslation();
  if (checks.length === 0) return <Typography.Text type="secondary">-</Typography.Text>;
  return (
    <Space size={4} wrap>
      {checks.map((c) => (
        <Space key={c.name} size={4}>
          <Tag color={c.ok ? 'success' : 'error'} style={{ margin: 0 }} data-testid={`check-${c.name}`}>
            {c.name}: {c.ok ? t('fleet.checkOk') : t('fleet.checkFailed')}
          </Tag>
          {!c.ok && c.error && <ErrorText text={c.error} />}
        </Space>
      ))}
    </Space>
  );
}
