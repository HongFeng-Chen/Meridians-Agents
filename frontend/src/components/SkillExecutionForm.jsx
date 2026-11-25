import { useMemo, useState } from 'react';
import { skill } from '../api.js';
import { Alert, Spinner } from './UI.jsx';
import { MODE_DESC, MODE_LABELS, registrationSnippet } from './skillRuntime.js';

// SkillExecutionForm 编辑技能的执行配置（skills.execution_spec）。
//
// 只改配置、不重装技能：后端 SetExecutionSpec 不动 content 与 installed_at，
// 所以「把本地技能改成远程」是一次运维动作，而不是一次升级。
//
// 远程/异步有两种 Worker 接入方式：
// - 直连：endpoint 写死在技能配置里，实例全失效时兜底，适合单机部署；
// - 集群：Worker 启动时调用注册接口自报地址并周期心跳（注册发现），
//   调度器按「权重 × 空闲度 ÷ 延迟」挑实例，扩缩容不用改技能配置。
// 切到 local 时保留其余字段（回切不用重填），只是运行时不再用它们。

const MODES = ['local', 'remote', 'async'];

function toForm(spec) {
  const s = spec || {};
  const endpoint = s.endpoint || '';
  return {
    mode: s.mode || 'local',
    // 配置里写过 endpoint 视为直连；否则按集群注册理解（运行时也是这个口径）
    access: endpoint ? 'direct' : 'cluster',
    endpoint,
    handler: s.handler || '',
    timeout_sec: s.timeout_sec || '',
    retry: s.retry ?? '',
    concurrency: s.concurrency || '',
    idempotent: Boolean(s.idempotent),
    input_schema: s.input_schema || '',
    output_schema: s.output_schema || '',
    required_scopes: (s.required_scopes || []).join(','),
  };
}

export default function SkillExecutionForm({ name, spec, instances, onSaved, onError }) {
  const [form, setForm] = useState(() => toForm(spec));
  const [busy, setBusy] = useState(false);
  const [localError, setLocalError] = useState('');
  const [notice, setNotice] = useState('');

  const baseline = useMemo(() => JSON.stringify(toForm(spec)), [spec]);
  const dirty = JSON.stringify(form) !== baseline;

  const set = (patch) => {
    setForm((prev) => ({ ...prev, ...patch }));
    setNotice('');
  };

  const save = async () => {
    setLocalError('');
    setNotice('');
    if (form.mode !== 'local' && form.access === 'direct' && !form.endpoint.trim()) {
      setLocalError('直连模式需要填写 Worker 地址；如果要靠实例注册发现，请切换到集群模式');
      return;
    }
    for (const key of ['input_schema', 'output_schema']) {
      const text = form[key].trim();
      if (!text) continue;
      try {
        JSON.parse(text);
      } catch (err) {
        setLocalError(`${key === 'input_schema' ? '参数 Schema' : '输出约定'} 不是合法 JSON：${err.message}`);
        return;
      }
    }
    const payload = {
      mode: form.mode,
      protocol: 'http',
      // 集群模式不下发 endpoint：调度只认注册实例，避免残留地址造成隐性兜底
      endpoint: form.access === 'cluster' ? '' : form.endpoint.trim(),
      handler: form.handler.trim(),
      timeout_sec: Number(form.timeout_sec) || 0,
      retry: form.retry === '' ? 0 : Number(form.retry),
      concurrency: Number(form.concurrency) || 0,
      idempotent: form.idempotent,
      input_schema: form.input_schema,
      output_schema: form.output_schema,
      required_scopes: form.required_scopes
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean),
    };
    setBusy(true);
    try {
      await skill.setSkillExecution({ name, execution: payload });
      setNotice('执行配置已保存');
      if (onSaved) onSaved();
    } catch (err) {
      setLocalError(err.message);
      if (onError) onError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const remote = form.mode !== 'local';
  const cluster = remote && form.access === 'cluster';
  const instList = instances || [];
  const onlineCount = instList.filter((i) => i.status === 'online').length;

  return (
    <div className="form">
      {localError && <Alert type="error">{localError}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <div className="field">
        <span className="field-label">执行模式</span>
        <div className="mode-cards">
          {MODES.map((m) => (
            <button
              key={m}
              type="button"
              className={`mode-card${form.mode === m ? ' active' : ''}`}
              onClick={() => set({ mode: m })}
            >
              <b>{MODE_LABELS[m]}</b>
              <span>{MODE_DESC[m]}</span>
            </button>
          ))}
        </div>
      </div>

      {remote && (
        <>
          <div className="field">
            <span className="field-label">Worker 接入方式</span>
            <div className="mode-cards">
              <button
                type="button"
                className={`mode-card${form.access === 'direct' ? ' active' : ''}`}
                onClick={() => set({ access: 'direct' })}
              >
                <b>单实例直连</b>
                <span>endpoint 写在配置里；实例全部失效时回退到它。适合单机部署与开发调试。</span>
              </button>
              <button
                type="button"
                className={`mode-card${form.access === 'cluster' ? ' active' : ''}`}
                onClick={() => set({ access: 'cluster' })}
              >
                <b>集群注册发现</b>
                <span>Worker 启动时自报地址并周期心跳，调度器按负载挑实例；扩缩容不用改配置。</span>
              </button>
            </div>
          </div>

          {cluster ? (
            <>
              {instList.length === 0 ? (
                <Alert type="error">集群模式还没有注册任何 Worker 实例：让 Worker 按下面的引导注册并保持心跳，否则执行会报「没有可用的技能执行实例」。</Alert>
              ) : (
                <Alert type="success">
                  已注册 {instList.length} 个实例（{onlineCount} 个在线）；调度只认注册实例，心跳过期会被自动判离线。
                </Alert>
              )}
              <div className="field">
                <span className="field-label">注册与心跳（Worker 侧接入）</span>
                <pre className="block mono" style={{ fontSize: 12, maxHeight: 200, overflow: 'auto' }}>
                  {registrationSnippet(name)}
                </pre>
                <span className="field-hint">
                  注册成功返回实例 ID；Worker 每 15 秒心跳一次并上报在途数，超过 45 秒没有心跳会被判为离线（可用 SKILL_INSTANCE_TTL 调整）。
                  参考实现见 examples/skill-worker-python/worker.py。
                </span>
              </div>
            </>
          ) : (
            <div className="form-grid">
              <label className="field">
                <span className="field-label">Worker 地址（endpoint）</span>
                <input
                  className="input mono"
                  placeholder="http://127.0.0.1:9101"
                  value={form.endpoint}
                  onChange={(e) => set({ endpoint: e.target.value })}
                />
                <span className="field-hint">
                  平台会优先调度注册过的实例，这里只在没有任何可用实例时兜底；多实例部署请选「集群注册发现」。
                </span>
              </label>
              <label className="field">
                <span className="field-label">Handler（Worker 侧实现名）</span>
                <input
                  className="input mono"
                  placeholder="缺省等于技能名"
                  value={form.handler}
                  onChange={(e) => set({ handler: e.target.value })}
                />
                <span className="field-hint">
                  平台技能名是用户看到的能力标识，handler 是 Worker 代码里的实现名，两者可不同。
                </span>
              </label>
            </div>
          )}

          {cluster && (
            <label className="field">
              <span className="field-label">Handler（Worker 侧实现名）</span>
              <input
                className="input mono"
                placeholder="缺省等于技能名"
                value={form.handler}
                onChange={(e) => set({ handler: e.target.value })}
              />
              <span className="field-hint">
                平台技能名是用户看到的能力标识，handler 是 Worker 代码里的实现名，两者可不同。
              </span>
            </label>
          )}

          <div className="form-grid cols-3">
            <label className="field">
              <span className="field-label">超时（秒）</span>
              <input
                className="input mono"
                type="number"
                min="0"
                placeholder={form.mode === 'async' ? '300' : '60'}
                value={form.timeout_sec}
                onChange={(e) => set({ timeout_sec: e.target.value })}
              />
            </label>
            <label className="field">
              <span className="field-label">失败重试次数</span>
              <input
                className="input mono"
                type="number"
                min="-1"
                placeholder="1"
                value={form.retry}
                onChange={(e) => set({ retry: e.target.value })}
              />
              <span className="field-hint">0 取默认（1 次）；-1 不重试</span>
            </label>
            <label className="field">
              <span className="field-label">单实例并发上限</span>
              <input
                className="input mono"
                type="number"
                min="0"
                placeholder="4"
                value={form.concurrency}
                onChange={(e) => set({ concurrency: e.target.value })}
              />
              <span className="field-hint">调度器按在途数做背压</span>
            </label>
          </div>

          <label className="field">
            <span className="field-label">参数 Schema（JSON Schema，进 Agent 能力描述）</span>
            <textarea
              className="textarea mono"
              rows={4}
              placeholder='{"type":"object","properties":{"file_id":{"type":"string"}},"required":["file_id"]}'
              value={form.input_schema}
              onChange={(e) => set({ input_schema: e.target.value })}
            />
            <span className="field-hint">填了之后，模型知道该传什么参数；试跑也会按它生成示例</span>
          </label>

          <div className="form-grid">
            <label className="field">
              <span className="field-label">输出约定（说明性）</span>
              <textarea
                className="textarea mono"
                rows={3}
                placeholder='{"type":"object","properties":{"summary":{"type":"string"}}}'
                value={form.output_schema}
                onChange={(e) => set({ output_schema: e.target.value })}
              />
            </label>
            <label className="field">
              <span className="field-label">权限范围（逗号分隔，预留）</span>
              <input
                className="input mono"
                placeholder="file:read,kb:search"
                value={form.required_scopes}
                onChange={(e) => set({ required_scopes: e.target.value })}
              />
              <span className="field-hint">留给网关/调用方校验，平台本身不拦截</span>
            </label>
          </div>

          <label className="chip" style={{ cursor: 'pointer', alignSelf: 'flex-start' }}>
            <input type="checkbox" checked={form.idempotent} onChange={(e) => set({ idempotent: e.target.checked })} />{' '}
            幂等去重：同幂等键回放上次成功结果（按租户隔离）
          </label>
        </>
      )}

      <div className="form-actions">
        <button className="btn primary sm" type="button" onClick={save} disabled={!dirty || busy}>
          {busy ? <Spinner text="保存中" /> : '保存执行配置'}
        </button>
        {dirty && (
          <button className="btn ghost sm" type="button" onClick={() => setForm(toForm(spec))} disabled={busy}>
            还原
          </button>
        )}
        <span className="muted" style={{ fontSize: 12.5 }}>
          只改配置，不动 SKILL.md 内容与安装时间
        </span>
      </div>
    </div>
  );
}
