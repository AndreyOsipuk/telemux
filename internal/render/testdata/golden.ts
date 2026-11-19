// Эталоны сборщиков telemux из кода бота (tg-bots, apps/mtproxy).
//
// Запуск (из каталога tg-bots после npm ci; синтетические входы - cases.json):
//   DATABASE_URL=postgres://x@127.0.0.1:1/x BOT_DIR=$PWD \
//     npx tsx <telemux>/internal/render/testdata/golden.ts <telemux>/internal/render/testdata
//
// Каждый случай cases.json в формате telemux (сервер, машина, пользователи) переводится
// во входы бота: telemt - Server для generateMainConfig (релей - через relayTargets),
// mtg - окружение MTG_<CODE>_* для parseMtgNodes и mtgDesiredConfigs. Результат -
// <name>.golden рядом с cases.json.

import { readFileSync, writeFileSync } from 'fs';
import { join } from 'path';

type Endpoint = { protocol: string; host: string; enabled?: boolean };
type CaseServer = {
  no: number;
  tls_domain: string;
  extra_tls_domains: string[];
  node_retired?: boolean;
  relay?: boolean;
  settings: Record<string, string>;
  endpoints?: Endpoint[];
};
type Case = {
  name: string;
  engine: 'telemt' | 'mtg';
  server: CaseServer;
  machine: { code: string; kind: 'exit' | 'relay'; address: string };
};
type CaseUser = {
  key: string;
  secret: string;
  expires_at: string;
  previous_secret?: string;
  previous_secret_expires_at?: string;
  bot: { id: number; source: string; sourceId: number; username: string };
};

async function main() {
  const dir = process.argv[2];
  const bot = process.env.BOT_DIR;
  if (!dir || !bot) throw new Error('нужны каталог testdata и BOT_DIR');
  const src = join(bot, 'apps/mtproxy/src');
  const pm = await import(join(src, 'proxy-manager.ts'));
  const links = await import(join(src, 'proxy-links.ts'));
  const sync = await import(join(src, 'mtg-sync.ts'));
  const keys = await import(join(src, 'users-toml-builder.ts'));

  const input = JSON.parse(readFileSync(join(dir, 'cases.json'), 'utf-8')) as {
    now: string;
    users: CaseUser[];
    cases: Case[];
  };
  const nowMs = new Date(input.now).getTime();
  const subs = input.users.map((u) => {
    const sub = {
      ...u.bot,
      secret: u.secret,
      expiresAt: u.expires_at,
      previousSecret: u.previous_secret ?? null,
      previousSecretRevokeAt: u.previous_secret_expires_at ?? null,
    };
    if (keys.buildUserKey(sub) !== u.key) throw new Error(`ключ ${u.key} не совпал с buildUserKey`);
    return sub;
  });

  for (const c of input.cases) {
    const s = c.server;
    let text: string;
    if (c.engine === 'telemt') {
      const server = {
        id: s.no,
        code: `ps${s.no}`,
        isActive: true,
        port: parseInt(s.settings.port ?? '443', 10),
        tlsDomain: s.tls_domain,
        extraTlsDomains: s.extra_tls_domains.join(','),
        publicTlsDomain: null,
        configPath: '/etc/telemt/telemt.toml',
        telemtIpv6: s.settings.ipv6 === '1',
        nodeRetired: !!s.node_retired,
        relaySecretMode: s.settings.relay_secret_mode ?? 'dd',
        ip: c.machine.kind === 'exit' ? c.machine.address : '203.0.113.250',
        publicIp: c.machine.kind === 'relay' ? c.machine.address : null,
        sshHost: '203.0.113.250',
      };
      const target = c.machine.kind === 'relay' ? links.relayTargets([server])[0] : server;
      text = pm.generateMainConfig(target);
    } else {
      const P = 'MTG_X1';
      const env: Record<string, string> = {
        [`${P}_HOST`]: c.machine.kind === 'exit' ? c.machine.address : '203.0.113.250',
        [`${P}_DOMAIN`]: s.tls_domain,
      };
      for (const [k, v] of Object.entries(s.settings)) env[`${P}_${k}`] = v;
      const web = (s.endpoints ?? []).find((e) => e.protocol === 'web' && e.enabled !== false);
      if (web) env[`${P}_WEB_DOMAIN`] = web.host;
      if (s.relay || c.machine.kind === 'relay') {
        env[`${P}_RELAY_HOST`] = c.machine.kind === 'relay' ? c.machine.address : '203.0.113.251';
      }
      const [node] = sync.parseMtgNodes(env);
      const kind = c.machine.kind === 'relay' ? 'mtg-relay' : 'mtg';
      const desired = sync.mtgDesiredConfigs(node, subs, nowMs).find((d: { kind: string }) => d.kind === kind);
      if (!desired) throw new Error(`${c.name}: бот не собрал ${kind}`);
      text = desired.text;
    }
    writeFileSync(join(dir, `${c.name}.golden`), text);
    console.log(`${c.name}.golden: ${text.length} байт`);
  }
  process.exit(0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
