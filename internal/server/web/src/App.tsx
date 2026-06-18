import { useEffect, useState, useCallback } from 'react';
import { api, type Role, type SyncStatus, type Node } from './api';

export function App() {
  const [version, setVersion] = useState('…');
  const [role, setRole] = useState<Role | null>(null);
  const [users, setUsers] = useState<number | null>(null);
  const [sync, setSync] = useState<SyncStatus | null>(null);
  const [nodes, setNodes] = useState<Node[] | null>(null);
  const [status, setStatus] = useState('');

  const refresh = useCallback(async () => {
    try {
      const [v, r, u, s] = await Promise.all([api.version(), api.role(), api.users(), api.syncStatus()]);
      setVersion(v.version); setRole(r); setUsers(u.total); setSync(s); setStatus('');
    } catch (e) { setStatus('ошибка: ' + (e as Error).message); }
    try { setNodes((await api.nodes()).nodes); } catch { setNodes(null); }
  }, []);

  useEffect(() => { refresh(); const t = setInterval(refresh, 15000); return () => clearInterval(t); }, [refresh]);

  const syncNow = async () => {
    setStatus('синхронизация…');
    try { setSync(await api.syncNow()); setStatus('готово'); }
    catch (e) { setStatus('ошибка: ' + (e as Error).message); }
  };

  const at = sync?.at && !sync.at.startsWith('0001') ? new Date(sync.at).toLocaleString() : '—';

  return (
    <>
      <header className="header">
        <h1>telemux</h1>
        <span className="badge">{version}</span>
        {role && <span className="badge">{role.role}</span>}
      </header>
      <main className="main">
        <div className="grid">
          <div className="card"><div className="k">Роль ноды</div><div className={'v ' + (role?.role ?? '')}>{role?.role ?? '…'}</div></div>
          <div className="card"><div className="k">Юзеров (desired)</div><div className="v">{users ?? '…'}</div></div>
          <div className="card"><div className="k">Последняя синхра</div><div className="v" style={{ fontSize: 15 }}>{at}</div></div>
          <div className="card"><div className="k">Diff (C/P/D)</div><div className="v">{sync ? `${sync.creates}/${sync.patches}/${sync.deletes}` : '—'}</div></div>
        </div>

        <div className="card">
          <div className="row">
            <button className="primary" onClick={syncNow}>Синхронизировать сейчас</button>
            <button onClick={refresh}>Обновить</button>
            <span className="muted">{status}</span>
          </div>
        </div>

        {nodes && (
          <div className="card">
            <div className="k">Ноды кластера</div>
            <table>
              <thead><tr><th>code</th><th>роль</th><th>адрес</th><th>last seen</th></tr></thead>
              <tbody>
                {nodes.length === 0 && <tr><td colSpan={4} className="muted">нод пока нет</td></tr>}
                {nodes.map((n) => (
                  <tr key={n.code}>
                    <td>{n.code}</td>
                    <td className={n.role}>{n.role}</td>
                    <td>{n.address}</td>
                    <td>{n.last_seen_at ? new Date(n.last_seen_at).toLocaleString() : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <div className="card">
          <div className="k">Статус синхронизации</div>
          <pre>{sync ? JSON.stringify(sync, null, 2) : '—'}</pre>
        </div>

        <p className="muted">telemux — панель управления кластером telemt.</p>
      </main>
    </>
  );
}
