// Casbin 策略管理页：额外授予层的可视化。
//
// 内置角色矩阵（代码里的基线）不在这里改——这里是「增量授权」：
//   - 授权限（p 规则）：给某用户 / 角色 / 全员(*) 在某租户内授某个权限点；
//   - 绑角色（g 规则）：把某用户在某租户内绑成某平台角色（dom=* 全局）。
// 改动即时生效（authz.Allowed 先查 Casbin 再落内置矩阵）。
// 管理操作要求 platform:config；无权限时接口 403，页面只读提示。
import { useCallback, useEffect, useState } from 'react';
import { Alert, Panel, Spinner } from '../components/UI.jsx';
import { auth, ApiError } from '../api.js';

const ROLES = ['platform_admin', 'tenant_admin', 'developer', 'operator', 'auditor', 'user'];
const PERMS = ['platform:config', 'tenant:manage', 'agent:develop', 'task:operate', 'task:approve', 'audit:read'];

export default function Policies() {
  const [policies, setPolicies] = useState([]);
  const [bindings, setBindings] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  // 新增表单状态（两条独立表单，tab 切换太重，直接上下排）
  const [pSub, setPSub] = useState('');
  const [pDom, setPDom] = useState('*');
  const [pPerm, setPPerm] = useState(PERMS[0]);
  const [bUser, setBUser] = useState('');
  const [bRole, setBRole] = useState(ROLES[1]);
  const [bDom, setBDom] = useState('*');

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const [p, b] = await Promise.all([auth.listPolicies(), auth.listBindings()]);
      setPolicies(p.policies || []);
      setBindings(b.bindings || []);
    } catch (err) {
      setError(err?.body?.message || err?.message || '加载失败');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const act = async (fn, okMsg) => {
    setError('');
    setNotice('');
    try {
      await fn();
      setNotice(okMsg);
      await load();
    } catch (err) {
      setError(err?.body?.message || err?.message || '操作失败');
    }
  };

  if (loading) {
    return (
      <Panel title="授权策略">
        <Spinner text="加载策略" />
      </Panel>
    );
  }

  return (
    <div className="stack">
      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <Panel
        title="权限授予（p 规则）"
        desc="给用户 / role:角色 / 全员(*) 在租户内授予权限点；内置角色矩阵的规则标为基线，删除后会在下次启动补回"
      >
        {policies.length === 0 ? (
          <p className="muted">暂无策略。</p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>主体（sub）</th>
                <th>租户（dom）</th>
                <th>权限点（perm）</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {policies.map((r, i) => {
                const baseline = /^role:/.test(r.sub) && r.dom === '*';
                return (
                  <tr key={`${r.sub}|${r.dom}|${r.perm}|${i}`}>
                    <td className="mono">{r.sub}</td>
                    <td className="mono">{r.dom}</td>
                    <td className="mono">{r.perm}</td>
                    <td>
                      <button
                        className="btn ghost sm"
                        type="button"
                        disabled={baseline}
                        title={baseline ? '内置角色基线不可删除' : '删除'}
                        onClick={() =>
                          act(
                            () => auth.removePolicy({ rules: [{ sub: r.sub, dom: r.dom, perm: r.perm }] }),
                            '策略已删除',
                          )
                        }
                      >
                        删除
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        <form
          className="policy-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (!pSub.trim()) {
              setError('主体不能为空');
              return;
            }
            act(
              () => auth.addPolicy({ rules: [{ sub: pSub.trim(), dom: pDom.trim() || '*', perm: pPerm }] }),
              '策略已添加',
            );
          }}
        >
          <label className="field">
            <span className="field-label">主体</span>
            <input className="input" value={pSub} onChange={(e) => setPSub(e.target.value)} placeholder="用户名 / role:developer / *" />
          </label>
          <label className="field">
            <span className="field-label">租户</span>
            <input className="input" value={pDom} onChange={(e) => setPDom(e.target.value)} placeholder="*" />
          </label>
          <label className="field">
            <span className="field-label">权限点</span>
            <select className="input" value={pPerm} onChange={(e) => setPPerm(e.target.value)}>
              {PERMS.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </label>
          <button className="btn primary sm" type="submit">
            添加
          </button>
        </form>
      </Panel>

      <Panel title="角色绑定（g 规则）" desc="把用户在租户内绑成平台角色；dom 为 * 表示全局生效，权限随角色的内置矩阵走">
        {bindings.length === 0 ? (
          <p className="muted">暂无绑定。</p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>用户</th>
                <th>角色</th>
                <th>租户</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {bindings.map((b, i) => (
                <tr key={`${b.username}|${b.role}|${b.dom}|${i}`}>
                  <td className="mono">{b.username}</td>
                  <td className="mono">{b.role}</td>
                  <td className="mono">{b.dom}</td>
                  <td>
                    <button
                      className="btn ghost sm"
                      type="button"
                      onClick={() =>
                        act(
                          () => auth.removeBinding({ bindings: [{ username: b.username, role: b.role, dom: b.dom }] }),
                          '绑定已解除',
                        )
                      }
                    >
                      解绑
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        <form
          className="policy-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (!bUser.trim()) {
              setError('用户名不能为空');
              return;
            }
            act(
              () => auth.addBinding({ bindings: [{ username: bUser.trim(), role: bRole, dom: bDom.trim() || '*' }] }),
              '绑定已添加',
            );
          }}
        >
          <label className="field">
            <span className="field-label">用户名</span>
            <input className="input" value={bUser} onChange={(e) => setBUser(e.target.value)} placeholder="alice" />
          </label>
          <label className="field">
            <span className="field-label">角色</span>
            <select className="input" value={bRole} onChange={(e) => setBRole(e.target.value)}>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field-label">租户</span>
            <input className="input" value={bDom} onChange={(e) => setBDom(e.target.value)} placeholder="*" />
          </label>
          <button className="btn primary sm" type="submit">
            绑定
          </button>
        </form>
      </Panel>
    </div>
  );
}
