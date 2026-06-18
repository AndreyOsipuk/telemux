import { useEffect, useState, useCallback } from 'react';
import { api, type Role, type SyncStatus, type Node, type User } from './api';

function Login({ onOk }: { onOk: () => void }) {
  const [u, setU] = useState('');
  const [p, setP] = useState('');
  const [err, setErr] = useState('');
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    try { await api.login(u, p); onOk(); }
    catch (e) { setErr((e as Error).message); }
  };
  const inp = { padding: '10px 12px', borderRadius: 8, border: '1px solid #30363d', background: '#0d1117', color: '#e6edf3', width: '100%' } as const;
  return (
    <main className="main" style={{ maxWidth: 360, marginTop: 80 }}>
      <div className="card">
        <h1 style={{ marginTop: 0, fontSize: 20 }}>telemux</h1>
        <form onSubmit={submit} style={{ display: 'grid', gap: 10 }}>
          <input style={inp} value={u} onChange={(e) => setU(e.target.value)} placeholder="логин" autoFocus />
          <input style={inp} type="password" value={p} onChange={(e) => setP(e.target.value)} placeholder="пароль" />
          <button className="primary" type="submit">Войти</button>
          {err && <span className="muted" style={{ color: '#f85149' }}>{err}</span>}
        </form>
      </div>
    </main>
  );
}

export function App() {
  const [needLogin, setNeedLogin] = useState<boolean | null>(null);

  const checkAuth = useCallback(async () => {
    try { const m = await api.me(); setNeedLogin(m.auth_enabled && !m.authed); }
    catch { setNeedLogin(false); }
  }, []);
  useEffect(() => { checkAuth(); }, [checkAuth]);

  if (needLogin === null) return null;
  if (needLogin) return <Login onOk={() => setNeedLogin(false)} />;
  return <Dashboard onLogout={() => setNeedLogin(true)} />;
}

function Dashboard({ onLogout }: { onLogout: () => void }) {
  const [version, setVersion] = useState('…');
  const [role, setRole] = useState<Role | null>(null);
  const [sync, setSync] = useState<SyncStatus | null>(null);
  const [nodes, setNodes] = useState<Node[] | null>(null);
  const [users, setUsers] = useState<User[]>([]);
  const [total, setTotal] = useState(0);
  const [status, setStatus] = useState('');
  const [newName, setNewName] = useState('');

  const isMaster = role?.is_master ?? false;

  const refresh = useCallback(async () => {
    try {
      const [v, r, s, u] = await Promise.all([api.version(), api.role(), api.syncStatus(), api.users()]);
      setVersion(v.version); setRole(r); setSync(s);
      setUsers(u.data ?? []); setTotal(u.paging?.total ?? 0); setStatus('');
    } catch (e) {
      if ((e as Error).message === 'unauthorized') { onLogout(); return; }
      setStatus('ошибка: ' + (e as Error).message);
    }
    try { setNodes((await api.nodes()).nodes); } catch { setNodes(null); }
  }, [onLogout]);

  useEffect(() => { refresh(); const t = setInterval(refresh, 15000); return () => clearInterval(t); }, [refresh]);

  const act = async (fn: () => Promise<unknown>, ok = 'готово') => {
    setStatus('…');
    try { await fn(); setStatus(ok); await refresh(); }
    catch (e) { setStatus('ошибка: ' + (e as Error).message); }
  };

  const createUser = () => {
    const name = newName.trim();
    if (!name) return;
    act(async () => { await api.createUser({ username: name }); setNewName(''); }, 'юзер создан');
  };

  const at = sync?.at && !sync.at.startsWith('0001') ? new Date(sync.at).toLocaleString() : '—';
  const fmt = (s: string | null) => (s ? new Date(s).toLocaleDateString() : '∞');

  return (
    <>
      <header className="header">
        <h1>telemux</h1>
        <span className="badge">{version}</span>
        {role && <span className="badge">{role.role}</span>}
        <button style={{ marginLeft: 'auto' }} onClick={async () => { try { await api.logout(); } finally { onLogout(); } }}>выйти</button>
      </header>
      <main className="main">
        <div className="grid">
          <div className="card"><div className="k">Роль ноды</div><div className={'v ' + (role?.role ?? '')}>{role?.role ?? '…'}</div></div>
          <div className="card"><div className="k">Юзеров</div><div className="v">{total}</div></div>
          <div className="card"><div className="k">Последняя синхра</div><div className="v" style={{ fontSize: 15 }}>{at}</div></div>
          <div className="card"><div className="k">Diff (C/P/D)</div><div className="v">{sync ? `${sync.creates}/${sync.patches}/${sync.deletes}` : '—'}</div></div>
        </div>

        <div className="card">
          <div className="row">
            <button className="primary" onClick={() => act(() => api.syncNow())}>Синхронизировать</button>
            <button onClick={refresh}>Обновить</button>
            <span className="muted">{status}</span>
          </div>
        </div>

        <div className="card">
          <div className="k">Пользователи ({total})</div>
          {isMaster ? (
            <div className="row" style={{ marginTop: 8 }}>
              <input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="username (напр. sub_ivan)"
                style={{ flex: 1, minWidth: 180, padding: '8px 10px', borderRadius: 8, border: '1px solid #30363d', background: '#0d1117', color: '#e6edf3' }} />
              <button className="primary" onClick={createUser}>+ Создать</button>
            </div>
          ) : <p className="muted">Эта нода — replica. Управление юзерами — на master.</p>}
          <table>
            <thead><tr><th>username</th><th>срок</th><th>conns</th><th>вкл</th><th></th></tr></thead>
            <tbody>
              {users.length === 0 && <tr><td colSpan={5} className="muted">пусто</td></tr>}
              {users.map((u) => (
                <tr key={u.username}>
                  <td>{u.username}</td>
                  <td>{fmt(u.expiration_at)}</td>
                  <td>{u.max_tcp_conns ?? '—'}</td>
                  <td>{u.enabled ? '✓' : '✗'}</td>
                  <td style={{ textAlign: 'right' }}>
                    {isMaster && (
                      <>
                        <button onClick={() => act(() => api.enableUser(u.username, !u.enabled))}>{u.enabled ? 'выкл' : 'вкл'}</button>{' '}
                        <button onClick={() => { if (confirm(`Удалить ${u.username}?`)) act(() => api.deleteUser(u.username), 'удалён'); }}>удалить</button>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
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
                    <td>{n.code}</td><td className={n.role}>{n.role}</td><td>{n.address}</td>
                    <td>{n.last_seen_at ? new Date(n.last_seen_at).toLocaleString() : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <p className="muted">telemux — панель управления кластером telemt.</p>
      </main>
    </>
  );
}
