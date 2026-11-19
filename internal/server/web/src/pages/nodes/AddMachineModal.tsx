// «Добавить машину» (createTarget): код, роль, ресурсы; для выхода - релей, публичный
// IPv4 и socks-вход на релее. Успех - одноразовая команда установки агента (JoinCommand).

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Alert, Button, Checkbox, Form, Input, Modal, Radio, Select, Space } from 'antd';
import type { FormInstance } from 'antd';

import { ApiError } from '@/api/http';
import { useCreateTarget, useRelays } from '@/api/queries';
import type { JoinCommand, NewTarget, NewTargetKind, TargetResource } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import { JoinCommandView } from '@/components/JoinCommand';

export const CODE_RE = /^[a-z][a-z0-9-]{1,31}$/;
const OCTET = '(25[0-5]|2[0-4]\\d|1\\d\\d|[1-9]?\\d)';
export const IPV4_RE = new RegExp(`^${OCTET}(\\.${OCTET}){3}$`);
// socks5://<хост>:<порт>, порт 1-65535.
const SOCKS_RE = /^socks5h?:\/\/[^\s/:]+:(\d{1,5})$/;
export const SOCKS_PREFIX = 'socks5://127.0.0.1:';
const RESOURCES: TargetResource[] = ['telemt.users', 'mtg.config'];

export function isSocks(v: string): boolean {
  const m = SOCKS_RE.exec(v);
  if (!m) return false;
  const port = Number(m[1]);
  return port >= 1 && port <= 65535;
}

interface FormValues {
  code: string;
  kind: NewTargetKind;
  resources: TargetResource[];
  relay_code?: string;
  address?: string;
  bridge_socks?: string;
}

export function toNewTarget(v: FormValues): NewTarget {
  const body: NewTarget = { code: v.code.trim(), kind: v.kind, resources: v.resources ?? [] };
  if (v.kind === 'exit') {
    body.relay_code = v.relay_code;
    body.address = v.address?.trim();
    body.bridge_socks = v.bridge_socks?.trim();
  }
  return body;
}

// 400 от сервера - текст под полем, если по тексту ясно, к какому полю он относится.
// Иначе (и для остальных ошибок) - баннер над формой.
const FIELD_HINTS: [keyof FormValues, RegExp][] = [
  ['bridge_socks', /bridge_socks|socks/i],
  ['relay_code', /relay_code|релей|relay/i],
  ['address', /address|адрес|ipv4/i],
  ['code', /\bcode\b|код/i],
];

function fieldOf(err: unknown): keyof FormValues | null {
  if (!(err instanceof ApiError) || err.status !== 400) return null;
  for (const [field, re] of FIELD_HINTS) if (re.test(err.message)) return field;
  return null;
}

function AddMachineForm({ form, onSubmit }: { form: FormInstance<FormValues>; onSubmit: (v: FormValues) => void }) {
  const { t } = useTranslation();
  const kind = Form.useWatch('kind', form);
  const isExit = kind === 'exit';
  const relays = useRelays(isExit);
  const relayOptions = (relays.data ?? []).map((r) => ({ value: r.code, label: r.code }));

  return (
    <Form<FormValues>
      form={form}
      layout="vertical"
      requiredMark={false}
      initialValues={{ kind: 'exit', resources: ['telemt.users'], bridge_socks: SOCKS_PREFIX }}
      onFinish={onSubmit}
    >
      <Form.Item
        name="code"
        label={t('addMachine.code')}
        extra={
          <span>
            {t('addMachine.codeHint')} <code>{'^[a-z][a-z0-9-]{1,31}$'}</code>
          </span>
        }
        rules={[
          { required: true, message: t('addMachine.codeRequired') },
          { pattern: CODE_RE, message: t('addMachine.codeInvalid') },
        ]}
      >
        <Input className="mono" autoFocus autoComplete="off" placeholder="exit-de1" />
      </Form.Item>
      <Form.Item name="kind" label={t('addMachine.kind')}>
        <Radio.Group
          optionType="button"
          buttonStyle="solid"
          options={[
            { value: 'relay', label: t('kinds.relay') },
            { value: 'exit', label: t('kinds.exit') },
          ]}
        />
      </Form.Item>
      <Form.Item name="resources" label={t('addMachine.resources')} extra={t('addMachine.resourcesHint')}>
        <Checkbox.Group
          options={RESOURCES.map((r) => ({ value: r, label: t(`addMachine.resource.${r === 'telemt.users' ? 'telemt' : 'mtg'}`) }))}
        />
      </Form.Item>
      {isExit && (
        <>
          <Form.Item
            name="relay_code"
            label={t('addMachine.relay')}
            extra={relays.isSuccess && relayOptions.length === 0 ? t('addMachine.noRelays') : undefined}
            rules={[{ required: true, message: t('addMachine.relayRequired') }]}
          >
            <Select
              options={relayOptions}
              loading={relays.isFetching}
              placeholder={t('addMachine.relayPlaceholder')}
              aria-label={t('addMachine.relay')}
              showSearch
            />
          </Form.Item>
          {relays.isError && <ErrorAlert error={relays.error} onRetry={() => relays.refetch()} />}
          <Form.Item
            name="address"
            label={t('addMachine.address')}
            extra={t('addMachine.addressHint')}
            rules={[
              { required: true, message: t('addMachine.addressRequired') },
              {
                validator: (_r, v?: string) =>
                  !v || IPV4_RE.test(v.trim()) ? Promise.resolve() : Promise.reject(new Error(t('addMachine.addressInvalid'))),
              },
            ]}
          >
            <Input className="mono" autoComplete="off" placeholder="203.0.113.10" />
          </Form.Item>
          <Form.Item
            name="bridge_socks"
            label={t('addMachine.socks')}
            extra={t('addMachine.socksHint')}
            rules={[
              {
                validator: (_r, v?: string) =>
                  v && isSocks(v.trim()) ? Promise.resolve() : Promise.reject(new Error(t('addMachine.socksInvalid'))),
              },
            ]}
          >
            <Input className="mono" autoComplete="off" />
          </Form.Item>
        </>
      )}
    </Form>
  );
}

// Тело окна живёт, пока окно открыто (destroyOnHidden): новое открытие - чистая форма.
function AddMachineBody({ onClose, onResult }: { onClose: () => void; onResult: (j: JoinCommand) => void }) {
  const { t } = useTranslation();
  const [form] = Form.useForm<FormValues>();
  const create = useCreateTarget();
  const [result, setResult] = useState<JoinCommand | null>(null);
  // Ошибка, которую показали под полем, - баннер над формой её не повторяет.
  const [fieldError, setFieldError] = useState(false);

  const submit = (v: FormValues) => {
    setFieldError(false);
    create.mutate(toNewTarget(v), {
      onSuccess: (join) => {
        setResult(join);
        onResult(join);
      },
      onError: (err) => {
        const field = fieldOf(err);
        if (field) {
          setFieldError(true);
          form.setFields([{ name: field, errors: [(err as ApiError).message] }]);
        }
      },
    });
  };

  if (result) {
    return (
      <>
        <JoinCommandView join={result} />
        <div className="modal-actions">
          <Button type="primary" onClick={onClose}>
            {t('common.close')}
          </Button>
        </div>
      </>
    );
  }

  return (
    <>
      {create.isError && !fieldError && <ErrorAlert error={create.error} title={t('addMachine.failed')} />}
      {create.isError && fieldError && (
        <Alert type="error" showIcon style={{ marginBottom: 16 }} title={t('addMachine.checkFields')} />
      )}
      <AddMachineForm form={form} onSubmit={submit} />
      <div className="modal-actions">
        <Space>
          <Button onClick={onClose}>{t('common.cancel')}</Button>
          <Button type="primary" loading={create.isPending} onClick={() => form.submit()}>
            {t('addMachine.submit')}
          </Button>
        </Space>
      </div>
    </>
  );
}

export default function AddMachineModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { t } = useTranslation();
  const [done, setDone] = useState<JoinCommand | null>(null);
  const close = () => {
    setDone(null);
    onClose();
  };
  return (
    <Modal
      open={open}
      title={done ? t('addMachine.doneTitle', { code: done.target }) : t('addMachine.title')}
      onCancel={close}
      destroyOnHidden
      width={done ? 720 : 560}
      footer={null}
    >
      <AddMachineBody onClose={close} onResult={setDone} />
    </Modal>
  );
}
