// Роль машины по серверам: от неё зависит, имеют ли смысл её счётчики подключений.
// serving - клиенты подключаются к ней (релей сервера или машина с прямыми ссылками);
// exit - только выход серверов «только через релей»: клиенты считаются на релее, здесь 0;
// retired - машина за рубежом выведена у всех её серверов;
// unused - ни в одном сервере.

import type { Server } from '@/api/types';

export type MachineUsage = 'serving' | 'exit' | 'retired' | 'unused';

export function machineUsage(servers: Server[]): Map<string, MachineUsage> {
  const out = new Map<string, MachineUsage>();
  const rank: Record<MachineUsage, number> = { unused: 0, retired: 1, exit: 2, serving: 3 };
  const up = (code: string | null, u: MachineUsage) => {
    if (!code) return;
    const was = out.get(code);
    if (!was || rank[u] > rank[was]) out.set(code, u);
  };
  for (const s of servers) {
    if (s.state === 'disabled') continue;
    up(s.relay_code, 'serving');
    if (s.node_retired) up(s.node_code, 'retired');
    else if (s.relay_only || !s.endpoints.some((e) => e.enabled && !e.via_relay)) up(s.node_code, 'exit');
    else up(s.node_code, 'serving');
  }
  return out;
}

// Счётчики подключений машины показывать серыми (на ней клиентов не бывает).
export const idleUsage = (u: MachineUsage | undefined) => u !== undefined && u !== 'serving';
// Машина вне работы - вся строка серая.
export const offUsage = (u: MachineUsage | undefined) => u === 'retired' || u === 'unused';
