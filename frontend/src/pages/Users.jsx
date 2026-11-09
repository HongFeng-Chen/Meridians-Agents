import { useCallback, useEffect, useState } from 'react';
import { admin } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import { IconRefresh, IconTrash, IconUser } from '../components/Icons.jsx';

// 角色取值与后端 pkg/authz 的规范角色一一对应；展示用中文，提交用英文。
const ROLES = [
  { value: 'platform_admin', label: '平台管理员', hint: '平台级配置（租户、MCP、模型数据源）' },
  { value: 'tenant_admin', label: '租户管理员', hint: '本租户成员、配额与全部租户内资源' },
  { value: 'developer', label: '开发者', hint: 'Agent / 提示词 / 技能的开发与调试' },
  { value: 'operator', label: '运维', hint: '任务提交、取消、重试与审批' },
  { value: 'auditor', label: '审计', hint: '只读执行记录与审计事件' },
  { value: 'user', label: '普通用户', hint: '使用被授权的 Agent' },
];
const ROLE_LABEL = Object.fromEntries(ROLES.map((r) => [r.value, r.label]));
const ROLE_TONE = {
  platform_admin: 'violet',
  tenant_admin: 'indigo',
  developer: 'teal',
  operator: 'default',
  auditor: 'default',
  user: 'default',
};
const STATUS_TONE = { active: 'ok', disabled: 'warn' };
const STATUS_TEXT = { active: '启用', disabled: '停用' };

const EMPTY = { tenant_id: '', username: '', display_name: '', email: '', role: 'user', status: 'active' };

export default function Users() {
  const [tenants, setTenants] = useState([]);
  const [items, setItems] = useState([]);
  const [total, setTotal] = useState(0);
  const [q, setQ] = useState('');
  const [tenantID, setTenantID] = useState('');
  const [role, setRole] = useState('');
  const [status, setStatus] = useState('');
  const [form, setForm] = useState(EMPTY);
  const [showForm, setShowForm] = useState(false);
  // 编辑中的成员 id；null = 新增模式
  const [editing, setEditing] = useState(null);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  const pg = usePagedList(items, { size: 10, resetKey: `${q}|${tenantID}|${role}|${status}` });

  const load = useCallback(async () => {
    setError('');
    try {
      const [res, tRes] = await Promise.all([
        admin.listUsers({ tenant_id: tenantID, q, role, status }),
        admin.listTenants({}),
      ]);
      setItems(res.items || []);
      setTotal(res.total ?? (res.items || []).length);
      setTenants(tRes.items || []);
    } catch (err) {
      setError(err.message);
    }
  }, [tenantID, q, role, status]);

  useEffect(() => {
    load();
  }, [load]);

  const act = async (tag, fn, okText) => {
    setBusy(tag);
    setError('');
    setNotice('');
    try {
      await fn();
      if (okText) setNotice(okText);
      await load();
      return true;
    } catch (err) {
      setError(err.message);
      return false;
    } finally {
      setBusy('');
    }
  };

  const openCreate = () => {
    setEditing(null);
    setForm({ ...EMPTY, tenant_id: tenantID || tenants[0]?.key || '' });
    setShowForm(true);
  };

  const openEdit = (u) => {
    setEditing(u.id);
    setForm({
      tenant_id: u.tenant_id,
      username: u.username,
      display_name: u.display_name || '',
      email: u.email || '',
      role: u.role || 'user',
      status: u.status || 'active',
    });
    setShowForm(true);
  };

  const closeForm = () => {
    setShowForm(false);
    setEditing(null);
    setForm(EMPTY);
  };

  const submit = async () => {
    if (!editing && !form.username.trim()) {
      setError('用户名必填：它对应令牌里的 sub');
      return;
    }
    if (editing) {
      const ok = await act(
        'save',
        () =>
          admin.updateUser({
            id: editing,
            display_name: form.display_name,
            email: form.email,
            role: form.role,
            status: form.status,
          }),
        `${form.username} 已更新`,
      );
      if (ok) closeForm();
      return;
    }
    if (!form.tenant_id) {
      setError('请选择所属租户');
      return;
    }
    const ok = await act(
      'save',
      () =>
        admin.createUser({
          tenant_id: form.tenant_id,
          username: form.username.trim(),
          display_name: form.display_name,
          email: form.email,
          role: form.role,
        }),
      `成员 ${form.username.trim()} 已登记`,
    );
    if (ok) closeForm();
  };

  const remove = (u) => {
    if (!window.confirm(`从名册移除 ${u.username}？\n只删记录，不影响已签发的令牌与历史数据。`)) return;
    act(`del-${u.id}`, () => admin.deleteUser({ id: u.id }), `${u.username} 已移除`);
  };

  const admins = items.filter((u) => u.role === 'tenant_admin' || u.role === 'platform_admin').length;
  const disabled = items.filter((u) => u.status === 'disabled').length;

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="成员" value={total} hint="当前筛选条件下的名册人数" tone="blue" />
        <StatCard label="租户" value={tenants.length} hint="可归属的隔离域" tone="teal" />
        <StatCard label="管理员" value={admins} hint="平台 / 租户管理员" tone="indigo" />
        <StatCard label="已停用" value={disabled} hint="只做标记，令牌仍由签发方决定" tone="violet" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <Panel
        title="成员名册"
        desc="记录「谁属于哪个租户、在该租户里是什么角色」。名册不参与登录认证——身份仍由 JWT / 静态令牌决定，停用这里的人不会让他的令牌立刻失效"
        actions={
          <>
            <button className="btn ghost sm" type="button" onClick={openCreate}>
              登记成员
            </button>
            <button className="btn ghost sm" type="button" onClick={load}>
              <IconRefresh size={15} /> 刷新
            </button>
          </>
        }
      >
        <div className="query-row" style={{ marginBottom: 14 }}>
          <label className="field inline">
            <span className="field-label">关键字</span>
            <input className="input" placeholder="用户名 / 显示名 / 邮箱" value={q} onChange={(e) => setQ(e.target.value)} />
          </label>
          <label className="field inline">
            <span className="field-label">租户</span>
            <select className="input" value={tenantID} onChange={(e) => setTenantID(e.target.value)}>
              <option value="">全部</option>
              {tenants.map((t) => (
                <option key={t.key} value={t.key}>
                  {t.key}
                  {t.name && t.name !== t.key ? ` · ${t.name}` : ''}
                </option>
              ))}
            </select>
          </label>
          <label className="field inline">
            <span className="field-label">角色</span>
            <select className="input" value={role} onChange={(e) => setRole(e.target.value)}>
              <option value="">全部</option>
              {ROLES.map((r) => (
                <option key={r.value} value={r.value}>
                  {r.label}
                </option>
              ))}
            </select>
          </label>
          <label className="field inline">
            <span className="field-label">状态</span>
            <select className="input" value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">全部</option>
              <option value="active">启用</option>
              <option value="disabled">停用</option>
            </select>
          </label>
        </div>

        {items.length === 0 ? (
          <EmptyState
            icon={IconUser}
            title="名册里还没有人"
            desc="登记成员后才能按租户查看归属；public 下的 default 是历史数据的归属主体"
          />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>用户名</th>
                  <th>显示名</th>
                  <th>租户</th>
                  <th>角色</th>
                  <th>状态</th>
                  <th>邮箱</th>
                  <th>登记时间</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {pg.paged.map((u) => (
                  <tr key={u.id}>
                    <td className="mono">{u.username}</td>
                    <td>{u.display_name || '—'}</td>
                    <td className="mono">{u.tenant_id}</td>
                    <td>
                      <Badge tone={ROLE_TONE[u.role] || 'default'}>{ROLE_LABEL[u.role] || u.role}</Badge>
                    </td>
                    <td>
                      <Badge tone={STATUS_TONE[u.status] || 'default'}>{STATUS_TEXT[u.status] || u.status}</Badge>
                    </td>
                    <td className="ellipsis" title={u.email} style={{ maxWidth: 200 }}>
                      {u.email || '—'}
                    </td>
                    <td className="mono">{fmtDate(u.created_at)}</td>
                    <td className="num">
                      <button className="btn ghost sm" type="button" onClick={() => openEdit(u)}>
                        编辑
                      </button>
                      <button
                        className="btn danger sm"
                        type="button"
                        disabled={u.username === 'default' || busy === `del-${u.id}`}
                        title={u.username === 'default' ? '内置主体不可删除' : '从名册移除'}
                        onClick={() => remove(u)}
                      >
                        {busy === `del-${u.id}` ? <Spinner text="移除" /> : <IconTrash size={14} />}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <Pagination page={pg.page} pages={pg.pages} total={pg.total} size={pg.size} onPage={pg.setPage} onSize={pg.setSize} />
      </Panel>

      {showForm && (
        <FormDrawer
          title={editing ? `编辑成员 · ${form.username}` : '登记成员'}
          desc={editing ? '用户名与所属租户不可修改（对应令牌里的 sub 与 tenant_id）' : '用户名对应令牌的 sub；同一租户内唯一，跨租户可同名'}
          onClose={closeForm}
        >
          <div className="form">
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">所属租户</span>
                <select
                  className="input"
                  value={form.tenant_id}
                  disabled={!!editing}
                  onChange={(e) => setForm({ ...form, tenant_id: e.target.value })}
                >
                  <option value="">请选择…</option>
                  {tenants.map((t) => (
                    <option key={t.key} value={t.key}>
                      {t.key}
                    </option>
                  ))}
                </select>
              </label>
              <label className="field inline">
                <span className="field-label">用户名</span>
                <input
                  className="input mono"
                  placeholder="alice"
                  value={form.username}
                  disabled={!!editing}
                  onChange={(e) => setForm({ ...form, username: e.target.value })}
                />
              </label>
            </div>
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">显示名</span>
                <input
                  className="input"
                  placeholder="张三"
                  value={form.display_name}
                  onChange={(e) => setForm({ ...form, display_name: e.target.value })}
                />
              </label>
              <label className="field inline">
                <span className="field-label">邮箱</span>
                <input
                  className="input mono"
                  placeholder="alice@example.com"
                  value={form.email}
                  onChange={(e) => setForm({ ...form, email: e.target.value })}
                />
              </label>
            </div>
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">角色</span>
                <select className="input" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
                  {ROLES.map((r) => (
                    <option key={r.value} value={r.value}>
                      {r.label}
                    </option>
                  ))}
                </select>
                <span className="cell-sub" style={{ marginTop: 4 }}>
                  {ROLES.find((r) => r.value === form.role)?.hint}
                </span>
              </label>
              {editing && (
                <label className="field inline">
                  <span className="field-label">状态</span>
                  <select className="input" value={form.status} onChange={(e) => setForm({ ...form, status: e.target.value })}>
                    <option value="active">启用</option>
                    <option value="disabled">停用</option>
                  </select>
                  <span className="cell-sub" style={{ marginTop: 4 }}>
                    停用只是名册标记；令牌是否可用由签发方决定
                  </span>
                </label>
              )}
            </div>
            <div className="form-actions">
              <button className="btn ghost sm" type="button" onClick={closeForm}>
                取消
              </button>
              <button className="btn primary sm" type="button" disabled={busy === 'save'} onClick={submit}>
                {busy === 'save' ? <Spinner text="保存" /> : editing ? '保存修改' : '登记'}
              </button>
            </div>
          </div>
        </FormDrawer>
      )}
    </div>
  );
}

function fmtDate(s) {
  if (!s) return '—';
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return s;
  return d.toLocaleString('zh-CN', { hour12: false });
}
