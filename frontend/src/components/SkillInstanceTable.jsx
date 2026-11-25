import { useState } from 'react';
import { skill } from '../api.js';
import { Alert, EmptyState, Spinner } from './UI.jsx';
import { INSTANCE_LABELS, fmtLatency, instanceTone, relativeTime, shortTime } from './skillRuntime.js';

// SkillInstanceTable Worker 实例列表 + 手工登记/摘除。
//
// 常规部署由 Worker 启动时自注册、按周期心跳（POST /instances/{id}/heartbeat）；
// 这里的手工登记主要给「先配好环境、Worker 稍后上线」和排障场景用，
// 因此表单默认折叠，不抢主流程。

export default function SkillInstanceTable({ name, instances, onChanged, onError }) {
  const [showForm, setShowForm] = useState(false);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [form, setForm] = useState({ id: '', endpoint: '', weight: '', max_concurrency: '', region: '', labels: '' });

  const reload = async () => {
    try {
      if (onChanged) await onChanged();
    } catch (err) {
      setError(err.message);
    }
  };

  const register = async () => {
    setError('');
    setBusy('register');
    try {
      await skill.registerSkillInstance({
        name,
        id: form.id.trim(),
        endpoint: form.endpoint.trim(),
        region: form.region.trim(),
        labels: form.labels
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean),
        weight: Number(form.weight) || 0,
        max_concurrency: Number(form.max_concurrency) || 0,
      });
      setForm({ id: '', endpoint: '', weight: '', max_concurrency: '', region: '', labels: '' });
      setShowForm(false);
      await reload();
    } catch (err) {
      setError(err.message);
      if (onError) onError(err.message);
    } finally {
      setBusy('');
    }
  };

  const remove = async (id) => {
    if (!window.confirm(`摘除实例 ${id}？之后的调度不会再选到它（Worker 重新注册即恢复）。`)) return;
    setError('');
    setBusy(`rm-${id}`);
    try {
      await skill.deleteSkillInstance({ name, id });
      await reload();
    } catch (err) {
      setError(err.message);
      if (onError) onError(err.message);
    } finally {
      setBusy('');
    }
  };

  return (
    <div>
      {error && <Alert type="error">{error}</Alert>}

      {(!instances || instances.length === 0) ? (
        <EmptyState
          title="暂无 Worker 实例"
          desc="Worker 启动后调用注册接口自报地址；只有配置了 endpoint 时会走直连兜底，否则执行会报「没有可用的技能执行实例」"
        />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>实例</th>
                <th>地址</th>
                <th>状态</th>
                <th className="num">在途</th>
                <th className="num">成功 / 连续失败</th>
                <th className="num">延迟</th>
                <th>心跳</th>
                <th className="num">操作</th>
              </tr>
            </thead>
            <tbody>
              {instances.map((i) => (
                <tr key={i.id}>
                  <td className="mono" style={{ fontSize: 12.5 }}>
                    {i.id}
                    {i.region ? <div className="cell-sub">{i.region}</div> : null}
                    {(i.labels || []).length > 0 && <div className="cell-sub">{(i.labels || []).join(' · ')}</div>}
                  </td>
                  <td className="mono" style={{ fontSize: 12 }}>{i.endpoint}</td>
                  <td>
                    <span className={`badge ${instanceTone(i.status)}`}>{INSTANCE_LABELS[i.status] || i.status}</span>
                    {(i.failures || 0) >= 3 && <div className="cell-sub">已熔断</div>}
                  </td>
                  <td className="num">
                    {i.inflight || 0} / {i.max_concurrency || 1}
                  </td>
                  <td className="num">
                    {i.successes || 0} / {i.failures || 0}
                  </td>
                  <td className="num">{fmtLatency(i.latency_ms)}</td>
                  <td className="mono" style={{ fontSize: 12 }}>
                    {shortTime(i.last_heartbeat)}
                    {relativeTime(i.last_heartbeat) && <div className="cell-sub">{relativeTime(i.last_heartbeat)}</div>}
                  </td>
                  <td className="num">
                    <button
                      className="btn ghost sm"
                      type="button"
                      disabled={busy === `rm-${i.id}`}
                      onClick={() => remove(i.id)}
                    >
                      {busy === `rm-${i.id}` ? <Spinner text="摘除" /> : '摘除'}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {emptyHint(instances)}

      <div className="form" style={{ marginTop: 12 }}>
        <div className="form-actions" style={{ justifyContent: 'flex-start' }}>
          <button className="btn ghost sm" type="button" onClick={() => setShowForm((v) => !v)}>
            {showForm ? '收起登记表单' : '手工登记实例'}
          </button>
        </div>
        {showForm && (
          <>
            <div className="form-grid">
              <label className="field">
                <span className="field-label">实例 ID</span>
                <input
                  className="input mono"
                  placeholder="worker-9101（留空用地址）"
                  value={form.id}
                  onChange={(e) => setForm({ ...form, id: e.target.value })}
                />
              </label>
              <label className="field">
                <span className="field-label">地址（endpoint）</span>
                <input
                  className="input mono"
                  placeholder="http://127.0.0.1:9101"
                  value={form.endpoint}
                  onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
                />
              </label>
            </div>
            <div className="form-grid">
              <label className="field">
                <span className="field-label">权重</span>
                <input
                  className="input mono"
                  type="number"
                  min="0"
                  placeholder="1（越大越容易被选中）"
                  value={form.weight}
                  onChange={(e) => setForm({ ...form, weight: e.target.value })}
                />
              </label>
              <label className="field">
                <span className="field-label">并发上限</span>
                <input
                  className="input mono"
                  type="number"
                  min="0"
                  placeholder="4"
                  value={form.max_concurrency}
                  onChange={(e) => setForm({ ...form, max_concurrency: e.target.value })}
                />
              </label>
            </div>
            <div className="form-grid">
              <label className="field">
                <span className="field-label">区域（可选）</span>
                <input
                  className="input mono"
                  placeholder="cn-north-1"
                  value={form.region}
                  onChange={(e) => setForm({ ...form, region: e.target.value })}
                />
              </label>
              <label className="field">
                <span className="field-label">标签（逗号分隔，可选）</span>
                <input
                  className="input mono"
                  placeholder="gpu,slow"
                  value={form.labels}
                  onChange={(e) => setForm({ ...form, labels: e.target.value })}
                />
              </label>
            </div>
            <div className="form-actions">
              <button
                className="btn primary sm"
                type="button"
                disabled={!form.endpoint.trim() || busy === 'register'}
                onClick={register}
              >
                {busy === 'register' ? <Spinner text="登记中" /> : '登记'}
              </button>
              <span className="muted" style={{ fontSize: 12.5 }}>
                手工登记的实例没有心跳，约 45 秒后会被判为离线
              </span>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

// 实例为空时的补充说明放在空状态下方（空状态组件本身不适合放接口提示类文案）
function emptyHint(instances) {
  if (instances && instances.length) return null;
  return (
    <div className="muted" style={{ fontSize: 12.5, marginTop: 8 }}>
      注册接口：<code className="mono">POST /api/skills/{'{name}'}/instances</code>；
      心跳：<code className="mono">POST /api/skills/{'{name}'}/instances/{'{id}'}/heartbeat</code>。
      参考实现见 <code className="mono">examples/skill-worker-python/worker.py</code>。
    </div>
  );
}
