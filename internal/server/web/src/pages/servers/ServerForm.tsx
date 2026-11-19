// Форма сервера (ADR 0005): основное, точки входа, маскировка и лимиты, настройки движка
// из каталога полей. Перед сохранением правки, меняющей ссылки пользователей или рвущей
// подключения, - подтверждение с перечнем того, что изменится.

import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import {
  App,
  Button,
  Card,
  Col,
  Drawer,
  Form,
  Input,
  InputNumber,
  Row,
  Segmented,
  Select,
  Space,
  Spin,
  Switch,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { FormInstance } from 'antd';
import { DeleteOutlined, PlusOutlined } from '@ant-design/icons';

import { ApiError } from '@/api/http';
import { useIsMaster, useMachinesByKind, useSaveServer, useServerFields } from '@/api/queries';
import type { EndpointProtocol, Server, ServerField, ServerFieldApply } from '@/api/types';
import ErrorAlert from '@/components/ErrorAlert';
import {
  ENGINES,
  PROTOCOLS,
  STATES,
  emptyFormValues,
  isTrue,
  linkPreview,
  newEndpoint,
  riskyChanges,
  toFormValues,
  toServerInput,
  type Change,
  type ServerFormValues,
  type SettingValue,
} from './serverModel';
import RenderTab from './RenderTab';
import { productName } from '@/utils/format';

type TabKey = 'main' | 'endpoints' | 'limits' | 'settings' | 'render';

// Коды ошибок записи из ADR 0005: заголовок над формой и вкладка, где искать причину.
const ERROR_TAB: Record<string, TabKey> = {
  unknown_machine: 'main',
  engine_immutable: 'main',
  bad_endpoint: 'endpoints',
  unknown_setting: 'settings',
  bad_setting: 'settings',
};

// Поле с ошибкой проверки -> вкладка, где оно лежит.
function tabOfField(name: string | number): TabKey {
  if (name === 'endpoints') return 'endpoints';
  if (name === 'settings') return 'settings';
  if (name === 'tls_domain' || name === 'extra_tls_domains' || name === 'user_max_tcp_conns' || name === 'user_max_unique_ips')
    return 'limits';
  return 'main';
}

const APPLY_COLOR: Record<ServerFieldApply, string> = {
  instant: 'green',
  reload: 'blue',
  restart: 'orange',
  firewall: 'purple',
};

export const PROTOCOL_COLOR: Record<EndpointProtocol, string> = { ee: 'blue', dd: 'geekblue', web: 'cyan' };

export function ApplyTag({ apply }: { apply: ServerFieldApply }) {
  const { t } = useTranslation();
  return (
    <Tag color={APPLY_COLOR[apply]} data-apply={apply}>
      {t(`servers.apply.${apply}`)}
    </Tag>
  );
}

// Флаг с «по умолчанию»: пока не трогали - ключ не пишется, переключатель показывает default.
function BoolSetting({
  id,
  value,
  onChange,
  def,
}: {
  id?: string;
  value?: SettingValue;
  onChange?: (v: boolean | undefined) => void;
  def: string;
}) {
  const { t } = useTranslation();
  const set = value !== undefined && value !== null;
  return (
    <Space size={8}>
      <Switch id={id} checked={set ? Boolean(value) : isTrue(def)} onChange={(on) => onChange?.(on)} />
      {set ? (
        <Button type="link" size="small" onClick={() => onChange?.(undefined)}>
          {t('servers.form.resetDefault')}
        </Button>
      ) : (
        <Typography.Text type="secondary">{t('servers.form.byDefault')}</Typography.Text>
      )}
    </Space>
  );
}

function SettingControl({ field, ...rest }: { field: ServerField; id?: string; value?: SettingValue; onChange?: (v: unknown) => void }) {
  const placeholder = field.default;
  switch (field.type) {
    case 'bool':
      return <BoolSetting {...rest} def={field.default} />;
    case 'int':
      return (
        <InputNumber
          {...(rest as object)}
          min={field.min}
          max={field.max}
          placeholder={placeholder}
          style={{ width: '100%' }}
        />
      );
    case 'enum':
      return (
        <Select
          {...(rest as object)}
          allowClear
          placeholder={placeholder}
          options={(field.enum ?? []).map((v) => ({ value: v, label: v }))}
        />
      );
    default:
      return <Input {...(rest as object)} allowClear placeholder={placeholder} />;
  }
}

function SettingsTab({ fields, lang }: { fields: ServerField[]; lang: 'ru' | 'en' }) {
  const { t } = useTranslation();
  const groups = useMemo(() => {
    const out = new Map<string, ServerField[]>();
    for (const f of fields) out.set(f.group, [...(out.get(f.group) ?? []), f]);
    return [...out.entries()];
  }, [fields]);

  if (fields.length === 0) return <Typography.Text type="secondary">{t('servers.form.noSettings')}</Typography.Text>;
  return (
    <>
      <Typography.Paragraph type="secondary">{t('servers.form.settingsHint')}</Typography.Paragraph>
      {groups.map(([group, list]) => (
        <section key={group} data-testid={`settings-group-${group}`}>
          <Typography.Title level={5} style={{ marginTop: 8 }}>
            {group}
          </Typography.Title>
          {list.map((f) => {
            const hint = lang === 'en' ? f.hint_en : f.hint_ru;
            return (
              <Form.Item
                key={f.key}
                name={['settings', f.key]}
                tooltip={hint || undefined}
                label={
                  <Space size={6} wrap>
                    <span>{lang === 'en' ? f.title_en : f.title_ru}</span>
                    <Typography.Text type="secondary" className="mono" style={{ fontSize: 12 }}>
                      {f.key}
                    </Typography.Text>
                    <ApplyTag apply={f.apply} />
                    {f.danger && <Tag color="red">{t('servers.apply.danger')}</Tag>}
                  </Space>
                }
              >
                <SettingControl field={f} />
              </Form.Item>
            );
          })}
        </section>
      ))}
    </>
  );
}

function EndpointCard({
  form,
  name,
  onRemove,
}: {
  form: FormInstance<ServerFormValues>;
  name: number;
  onRemove: () => void;
}) {
  const { t } = useTranslation();
  const ep = Form.useWatch(['endpoints', name], form);
  const protocol: EndpointProtocol = ep?.protocol ?? 'ee';
  return (
    <Card
      size="small"
      style={{ marginBottom: 12 }}
      data-testid={`endpoint-${name}`}
      title={
        <Typography.Text className="mono" type="secondary" copyable={false} style={{ fontSize: 12 }}>
          {linkPreview({ protocol, host: ep?.host, port: ep?.port })}
        </Typography.Text>
      }
      extra={
        <Button size="small" danger type="text" icon={<DeleteOutlined />} aria-label={t('servers.form.removeEndpoint')} onClick={onRemove} />
      }
    >
      <Row gutter={12}>
        <Col xs={24} sm={6}>
          <Form.Item name={[name, 'protocol']} label={t('servers.endpoint.protocol')}>
            <Select options={PROTOCOLS.map((p) => ({ value: p, label: p }))} />
          </Form.Item>
        </Col>
        <Col xs={24} sm={protocol === 'web' ? 18 : 12}>
          <Form.Item
            name={[name, 'host']}
            label={t('servers.endpoint.host')}
            rules={[{ required: true, whitespace: true, message: t('servers.form.required') }]}
          >
            <Input placeholder="proxy.example.com" />
          </Form.Item>
        </Col>
        {protocol !== 'web' && (
          <Col xs={24} sm={6}>
            <Form.Item name={[name, 'port']} label={t('servers.endpoint.port')} rules={[{ required: true, message: t('servers.form.required') }]}>
              <InputNumber min={1} max={65535} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
        )}
      </Row>
      {protocol === 'ee' && (
        <Form.Item
          name={[name, 'tls_domain']}
          label={t('servers.endpoint.tlsDomain')}
          rules={[{ required: true, whitespace: true, message: t('servers.form.eeNeedsDomain') }]}
        >
          <Input placeholder="cdn.example.org" />
        </Form.Item>
      )}
      <Row gutter={12}>
        <Col xs={24} sm={10}>
          <Form.Item name={[name, 'label']} label={t('servers.endpoint.label')}>
            <Input placeholder={t('servers.endpoint.labelPlaceholder')} />
          </Form.Item>
        </Col>
        <Col xs={8} sm={5}>
          <Form.Item name={[name, 'sort_order']} label={t('servers.endpoint.sortOrder')}>
            <InputNumber style={{ width: '100%' }} />
          </Form.Item>
        </Col>
        <Col xs={8} sm={5}>
          <Form.Item name={[name, 'via_relay']} label={t('servers.endpoint.viaRelay')} valuePropName="checked">
            <Switch />
          </Form.Item>
        </Col>
        <Col xs={8} sm={4}>
          <Form.Item name={[name, 'enabled']} label={t('servers.endpoint.enabled')} valuePropName="checked">
            <Switch />
          </Form.Item>
        </Col>
      </Row>
    </Card>
  );
}

function EndpointsTab({ form }: { form: FormInstance<ServerFormValues> }) {
  const { t } = useTranslation();
  return (
    <Form.List name="endpoints">
      {(items, { add, remove }) => (
        <>
          {items.length === 0 && (
            <Typography.Paragraph type="secondary">{t('servers.form.noEndpoints')}</Typography.Paragraph>
          )}
          {items.map((item) => (
            <EndpointCard key={item.key} form={form} name={item.name} onRemove={() => remove(item.name)} />
          ))}
          <Button
            type="dashed"
            block
            icon={<PlusOutlined />}
            onClick={() => add(newEndpoint('ee', form.getFieldValue('tls_domain') ?? ''))}
          >
            {t('servers.form.addEndpoint')}
          </Button>
        </>
      )}
    </Form.List>
  );
}

function MainTab({ creating, machineOptions }: { creating: boolean; machineOptions: { relay: Opt[]; exit: Opt[] } }) {
  const { t } = useTranslation();
  return (
    <>
      <Row gutter={12}>
        {creating && (
          <Col xs={24} sm={6}>
            <Form.Item name="no" label={t('servers.no')} rules={[{ required: true, message: t('servers.form.required') }]}>
              <InputNumber min={1} max={99} precision={0} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
        )}
        <Col xs={24} sm={creating ? 10 : 14}>
          <Form.Item
            name="name"
            label={t('servers.name')}
            rules={[{ required: true, whitespace: true, message: t('servers.form.required') }]}
          >
            <Input placeholder={t('servers.form.namePlaceholder')} />
          </Form.Item>
        </Col>
        <Col xs={12} sm={4}>
          <Form.Item name="emoji" label={t('servers.emoji')}>
            <Input />
          </Form.Item>
        </Col>
        <Col xs={12} sm={4}>
          <Form.Item
            name="country"
            label={t('servers.country')}
            normalize={(v?: string) => v?.toLowerCase()}
            rules={[{ pattern: /^[a-z]{2}$/, message: t('servers.form.countryRule') }]}
          >
            <Input maxLength={2} placeholder="de" />
          </Form.Item>
        </Col>
      </Row>
      <Form.Item
        name="engine"
        label={t('servers.engine')}
        extra={creating ? t('servers.form.engineOnce') : t('servers.form.engineImmutable')}
      >
        <Segmented options={ENGINES.map((e) => ({ value: e, label: productName(e) }))} disabled={!creating} />
      </Form.Item>
      <Row gutter={12}>
        <Col xs={24} sm={12}>
          <Form.Item name="relay_code" label={t('servers.relay')}>
            <Select allowClear showSearch options={machineOptions.relay} placeholder={t('servers.form.noMachine')} />
          </Form.Item>
        </Col>
        <Col xs={24} sm={12}>
          <Form.Item name="node_code" label={t('servers.node')}>
            <Select allowClear showSearch options={machineOptions.exit} placeholder={t('servers.form.noMachine')} />
          </Form.Item>
        </Col>
      </Row>
      <Form.Item name="state" label={t('servers.state')} extra={t('servers.form.stateHint')}>
        <Select options={STATES.map((s) => ({ value: s, label: t(`servers.states.${s}`) }))} />
      </Form.Item>
      <Row gutter={12}>
        <Col xs={24} sm={12}>
          <Form.Item name="relay_only" label={t('servers.relayOnly')} valuePropName="checked" extra={t('servers.form.relayOnlyHint')}>
            <Switch />
          </Form.Item>
        </Col>
        <Col xs={24} sm={12}>
          <Form.Item name="node_retired" label={t('servers.nodeRetired')} valuePropName="checked" extra={t('servers.form.nodeRetiredHint')}>
            <Switch />
          </Form.Item>
        </Col>
      </Row>
      <Form.Item name="note" label={t('servers.note')}>
        <Input.TextArea autoSize={{ minRows: 2, maxRows: 6 }} />
      </Form.Item>
    </>
  );
}

function LimitsTab() {
  const { t } = useTranslation();
  return (
    <>
      <Form.Item name="tls_domain" label={t('servers.tlsDomain')} extra={t('servers.form.tlsDomainHint')}>
        <Input placeholder="cdn.example.org" />
      </Form.Item>
      <Form.Item name="extra_tls_domains" label={t('servers.extraTlsDomains')} extra={t('servers.form.extraTlsHint')}>
        <Select mode="tags" tokenSeparators={[',', ' ']} open={false} suffix={null} />
      </Form.Item>
      <Row gutter={12}>
        <Col xs={24} sm={12}>
          <Form.Item name="user_max_tcp_conns" label={t('servers.maxTcpConns')}>
            <InputNumber min={1} precision={0} placeholder={t('servers.form.byDefault')} style={{ width: '100%' }} />
          </Form.Item>
        </Col>
        <Col xs={24} sm={12}>
          <Form.Item name="user_max_unique_ips" label={t('servers.maxUniqueIps')}>
            <InputNumber min={1} precision={0} placeholder={t('servers.form.noLimit')} style={{ width: '100%' }} />
          </Form.Item>
        </Col>
      </Row>
    </>
  );
}

interface Opt {
  value: string;
  label: string;
}

// Варианты машин; текущее значение остаётся в списке, даже если машины нет в выборке.
function machineOpts(codes: string[], current: string | null | undefined): Opt[] {
  const all = current && !codes.includes(current) ? [current, ...codes] : codes;
  return all.map((c) => ({ value: c, label: c }));
}

function ChangeItem({ change, fields, lang }: { change: Change; fields: ServerField[]; lang: 'ru' | 'en' }) {
  const { t } = useTranslation();
  switch (change.kind) {
    case 'danger': {
      const f = fields.find((x) => x.key === change.key);
      const title = f ? (lang === 'en' ? f.title_en : f.title_ru) : change.key;
      return <li>{t('servers.confirm.danger', { title, from: change.from || '-', to: change.to || '-' })}</li>;
    }
    case 'tlsDomain':
      return <li>{t('servers.confirm.tlsDomain', { from: change.from || '-', to: change.to || '-' })}</li>;
    case 'eeEndpoints':
      return <li>{t('servers.confirm.eeEndpoints')}</li>;
    case 'disable':
      return <li>{t('servers.confirm.disable')}</li>;
  }
}

function saveErrorTitle(err: unknown, t: (k: string) => string): string {
  if (err instanceof ApiError && Object.hasOwn(ERROR_TAB, err.code)) return t(`servers.errorCode.${err.code}`);
  return t('servers.form.saveFailed');
}

export default function ServerForm({ server, onClose }: { server: Server | null; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const { message, modal } = App.useApp();
  const lang: 'ru' | 'en' = i18n.language?.startsWith('en') ? 'en' : 'ru';
  const isMaster = useIsMaster();
  const creating = server === null;
  const [form] = Form.useForm<ServerFormValues>();
  const [tab, setTab] = useState<TabKey>('main');

  const fieldsQ = useServerFields();
  const relays = useMachinesByKind('relay');
  const exits = useMachinesByKind('exit');
  const save = useSaveServer();

  const catalog = useMemo(() => fieldsQ.data?.data ?? [], [fieldsQ.data]);
  const engine = Form.useWatch('engine', form) ?? server?.engine ?? 'telemt';
  const engineFields = useMemo(() => catalog.filter((f) => f.engine === engine), [catalog, engine]);
  const relayCode = Form.useWatch('relay_code', form);
  const nodeCode = Form.useWatch('node_code', form);
  const machineOptions = {
    relay: machineOpts((relays.data?.data ?? []).map((m) => m.code), relayCode ?? server?.relay_code),
    exit: machineOpts((exits.data?.data ?? []).map((m) => m.code), nodeCode ?? server?.node_code),
  };

  // Форма строится после каталога: иначе настройки правки не разложились бы по полям.
  const ready = !fieldsQ.isPending;
  const initialValues = useMemo(
    () => (server ? toFormValues(server, catalog) : emptyFormValues()),
    // Только при открытии и после загрузки каталога: дальнейшие обновления списка не сбрасывают ввод.
    [server?.no, ready],
  );

  const doSave = (body: ReturnType<typeof toServerInput>) =>
    save.mutate(
      { no: server?.no, body },
      {
        onSuccess: () => {
          message.success(creating ? t('servers.created', { no: body.no }) : t('common.saved'));
          onClose();
        },
        onError: (err) => {
          if (err instanceof ApiError && Object.hasOwn(ERROR_TAB, err.code)) setTab(ERROR_TAB[err.code]);
        },
      },
    );

  const onFinish = () => {
    // Все значения формы, включая не показанные поля (id точек входа).
    const values = form.getFieldsValue(true) as ServerFormValues;
    const body = toServerInput(values, catalog, server?.settings ?? {});
    if (!server) {
      doSave(body);
      return;
    }
    body.no = server.no;
    const changes = riskyChanges(server, body, catalog);
    if (changes.length === 0) {
      doSave(body);
      return;
    }
    modal.confirm({
      title: t('servers.confirm.title', { no: server.no }),
      width: 560,
      content: (
        <ul style={{ paddingInlineStart: 18, margin: 0 }} data-testid="server-changes">
          {changes.map((c, i) => (
            <ChangeItem key={i} change={c} fields={catalog} lang={lang} />
          ))}
        </ul>
      ),
      okText: t('servers.confirm.ok'),
      okButtonProps: { danger: true },
      cancelText: t('common.cancel'),
      onOk: () => doSave(body),
    });
  };

  const tabs: { key: TabKey; label: ReactNode; children: ReactNode; forceRender: boolean }[] = [
    { key: 'main', label: t('servers.form.tabMain'), children: <MainTab creating={creating} machineOptions={machineOptions} />, forceRender: true },
    { key: 'endpoints', label: t('servers.form.tabEndpoints'), children: <EndpointsTab form={form} />, forceRender: true },
    { key: 'limits', label: t('servers.form.tabLimits'), children: <LimitsTab />, forceRender: true },
    {
      key: 'settings',
      label: t('servers.form.tabSettings', { engine: productName(engine) }),
      children: fieldsQ.isError ? (
        <ErrorAlert error={fieldsQ.error} onRetry={() => fieldsQ.refetch()} title={t('servers.form.fieldsFailed')} />
      ) : (
        <SettingsTab fields={engineFields} lang={lang} />
      ),
      forceRender: true,
    },
    // Сборка есть только у сохранённого сервера; полей формы во вкладке нет.
    ...(server ? [{ key: 'render' as const, label: t('servers.form.tabRender'), children: <RenderTab server={server} />, forceRender: false }] : []),
  ];

  return (
    <Drawer
      open
      size={760}
      title={creating ? t('servers.form.createTitle') : t('servers.form.editTitle', { no: server.no, name: server.name })}
      onClose={onClose}
      destroyOnHidden
      footer={
        <Space style={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button onClick={onClose}>{t('common.cancel')}</Button>
          <Tooltip title={isMaster ? undefined : t('role.writeOnMaster')}>
            <Button type="primary" onClick={() => form.submit()} loading={save.isPending} disabled={!isMaster || !ready}>
              {creating ? t('servers.form.create') : t('servers.form.save')}
            </Button>
          </Tooltip>
        </Space>
      }
    >
      {save.isError && <ErrorAlert error={save.error} title={saveErrorTitle(save.error, t)} />}
      {!ready ? (
        <Spin />
      ) : (
        <Form<ServerFormValues>
          form={form}
          layout="vertical"
          requiredMark={false}
          initialValues={initialValues}
          onFinish={onFinish}
          onFinishFailed={({ errorFields }) => {
            const first = errorFields[0]?.name[0];
            if (first !== undefined) setTab(tabOfField(first));
          }}
          aria-label={t('servers.form.aria')}
        >
          <Tabs activeKey={tab} onChange={(k) => setTab(k as TabKey)} items={tabs} />
        </Form>
      )}
    </Drawer>
  );
}
