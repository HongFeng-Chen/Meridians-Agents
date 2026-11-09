import { useCallback, useEffect, useState } from 'react';
import { admin } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import { IconRefresh, IconTenant, IconTrash } from '../components/Icons.jsx';

// 新建租户的默认表单。key 一旦创建就不可修改：它已经写进 agent_tasks 等表的 tenant_id。
const EMPTY = { key: '', name: '', status: 'active', remark: '' };

const STATUS_TONE = { active: 'ok', disabled: 'warn' };
const STATUS_TEXT = { active: '启用', disabled: '停用' };

export default function Tenants() {
  const [items, setItems] = useState([]);
  const [q, setQ] = useState('');
  const [status, setStatus] = useState('');
  const [form, setForm] = useState(EMPTY);
  const [showForm, setShowForm] = useState(false);
  // 编辑中的租户键；null = 新增模式
  const [editing, setEditing] = useState(null);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  const pg = usePagedList(items, { size: 10, resetKey: `${q}|${status}` });

  const load = useCallback(async () => {
    setError('');
    try {
      const res = await admin.listTenants({ q, status });
      setItems(res.items || []);
    } catch (err) {
      setError(err.message);
    }
  }, [q, status]);

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
    setForm(EMPTY);
    setShowForm(true);
  };

  const openEdit = (t) => {
    setEditing(t.key);
    setForm({ key: t.key, name: t.name || '', status: t.status || 'active', remark: t.remark || '' });
    setShowForm(true);
  };

  const closeForm = () => {
    setShowForm(false);
    setEditing(null);
    setForm(EMPTY);
  };

  const submit = async () => {
    if (!form.key.trim()) {
      setError('租户键必填（小写字母开头，2-32 位）');
      return;
    }
    if (editing) {
      // 只提交可变字段：key 是路径参数，改名会制造孤儿数据
      const ok = await act(
        'save',
        () => admin.updateTenant({ key: editing, name: form.name, status: form.status, remark: form.remark }),
        `${editing} 已更新`,
      );
      if (ok) closeForm();
      return;
    }
    const ok = await act(
      'save',
      () => admin.createTenant({ key: form.key.trim(), name: form.name, remark: form.remark }),
      `租户 ${form.key.trim()} 已创建`,
    );
    if (ok) closeForm();
  };

  const remove = (t) => {
    const tip = t.builtin
      ? `${t.key} 是内置租户，不可删除`
      : `删除租户 ${t.key}？\n成员需先移除；历史任务与调用记录会保留。`;
    if (t.builtin || !window.confirm(tip)) return;
    act(`del-${t.key}`, () => admin.deleteTenant({ key: t.key }), `${t.key} 已删除`);
  };

  const enabled = items.filter((t) => t.status === 'active').length;
  const users = items.reduce((n, t) => n + (t.user_count || 0), 0);
  const tasks = items.reduce((n, t) => n + Number(t.task_count || 0), 0);

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="租户" value={items.length} hint="隔离域总数（tenants）" tone="blue" />
        <StatCard label="启用中" value={enabled} hint="停用只是标记，不阻断既有数据" tone="teal" />
        <StatCard label="名册成员" value={users} hint="users 表（仅平台管理员可见全部）" tone="indigo" />
        <StatCard label="历史任务" value={tasks} hint="agent_tasks 按 tenant_id 聚合" tone="violet" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <Panel
        title="租户列表"
        desc="租户键即隔离键，创建后不可改；public 是内置租户（开发态身份与历史数据归属）"
        actions={
          <>
            <button className="btn ghost sm" type="button" onClick={openCreate}>
              新建租户
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
            <input
              className="input"
              placeholder="租户键 / 名称"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
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
            icon={IconTenant}
            title="没有匹配的租户"
            desc="换个关键字，或新建一个租户；public 会在首次启动时自动创建"
          />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>租户键</th>
                  <th>名称</th>
                  <th>状态</th>
                  <th className="num">成员</th>
                  <th className="num">任务</th>
                  <th>备注</th>
                  <th>创建时间</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {pg.paged.map((t) => (
                  <tr key={t.key}>
                    <td className="mono">
                      {t.key}
                      {t.builtin && <Badge tone="indigo">内置</Badge>}
                    </td>
                    <td>{t.name || '—'}</td>
                    <td>
                      <Badge tone={STATUS_TONE[t.status] || 'default'}>{STATUS_TEXT[t.status] || t.status}</Badge>
                    </td>
                    <td className="num">{t.user_count || 0}</td>
                    <td className="num">{t.task_count || 0}</td>
                    <td className="ellipsis" title={t.remark} style={{ maxWidth: 220 }}>
                      {t.remark || '—'}
                    </td>
                    <td className="mono">{fmtDate(t.created_at)}</td>
                    <td className="num">
                      <button className="btn ghost sm" type="button" onClick={() => openEdit(t)}>
                        编辑
                      </button>
                      <button
                        className="btn danger sm"
                        type="button"
                        disabled={t.builtin || busy === `del-${t.key}`}
                        title={t.builtin ? '内置租户不可删除' : '删除租户（需先移除成员）'}
                        onClick={() => remove(t)}
                      >
                        {busy === `del-${t.key}` ? <Spinner text="删除" /> : <IconTrash size={14} />}
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
          title={editing ? `编辑租户 · ${editing}` : '新建租户'}
          desc={editing ? '租户键不可修改（历史数据的隔离键）；停用只是标记' : '租户键创建后不可修改：它会写进任务、记忆等表的 tenant_id'}
          onClose={closeForm}
        >
          <div className="form">
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">租户键</span>
                <input
                  className="input mono"
                  placeholder="acme"
                  value={form.key}
                  disabled={!!editing}
                  onChange={(e) => setForm({ ...form, key: e.target.value })}
                />
                <span className="cell-sub" style={{ marginTop: 4 }}>
                  小写字母开头，2-32 位；用作 JWT 的 tenant_id
                </span>
              </label>
              <label className="field inline">
                <span className="field-label">名称</span>
                <input
                  className="input"
                  placeholder="Acme 公司"
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                />
              </label>
            </div>
            {editing && (
              <label className="field inline">
                <span className="field-label">状态</span>
                <select className="input" value={form.status} onChange={(e) => setForm({ ...form, status: e.target.value })}>
                  <option value="active">启用</option>
                  <option value="disabled">停用</option>
                </select>
              </label>
            )}
            <label className="field">
              <span className="field-label">备注</span>
              <textarea
                className="input"
                rows={3}
                placeholder="归属团队 / 联系人或用途说明"
                value={form.remark}
                onChange={(e) => setForm({ ...form, remark: e.target.value })}
              />
            </label>
            <div className="form-actions">
              <button className="btn ghost sm" type="button" onClick={closeForm}>
                取消
              </button>
              <button className="btn primary sm" type="button" disabled={busy === 'save'} onClick={submit}>
                {busy === 'save' ? <Spinner text="保存" /> : editing ? '保存修改' : '创建'}
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
