import { useCallback, useEffect, useState } from 'react';
import { skill } from '../api.js';
import { Alert, Badge, EmptyState, Spinner } from './UI.jsx';
import SkillExecutionForm from './SkillExecutionForm.jsx';
import SkillInstanceTable from './SkillInstanceTable.jsx';
import {
  EXEC_LABELS,
  MODE_LABELS,
  MODE_TONES,
  execTone,
  fmtLatency,
  isExecutableMode,
  shortTime,
} from './skillRuntime.js';

// SkillDetailDrawer 技能详情：运行配置 / 文件 / 执行记录三个视角。
//
// 之前这些内容都堆在页面里的一个 Panel 中，长了会顶到页面底部；
// 改成右侧抽屉后，列表与详情各自滚动，配置表单也有足够宽度。

const TABS = [
  { key: 'runtime', label: '运行配置' },
  { key: 'files', label: '文件' },
  { key: 'executions', label: '执行记录' },
];

export default function SkillDetailDrawer({ detail, files, onClose, onChanged, onRun, onOpenExecution, onOpenAllExecutions }) {
  const [tab, setTab] = useState('runtime');
  const [instances, setInstances] = useState([]);
  const [executions, setExecutions] = useState([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [fileBody, setFileBody] = useState(null);

  const mode = detail.execution?.mode || 'local';
  const executable = isExecutableMode(mode);

  const loadRuntime = useCallback(async () => {
    if (!executable) {
      setInstances([]);
      setExecutions([]);
      return;
    }
    setLoading(true);
    setError('');
    try {
      const [inst, exec] = await Promise.all([
        skill.listSkillInstances({ name: detail.name }).catch(() => ({ instances: [] })),
        skill.listSkillExecutions({ skill: detail.name, limit: 20 }).catch(() => ({ executions: [] })),
      ]);
      setInstances(inst.instances || []);
      setExecutions(exec.executions || []);
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  }, [detail.name, executable]);

  useEffect(() => {
    loadRuntime();
  }, [loadRuntime]);

  const readFile = async (path) => {
    setFileBody(null);
    try {
      setFileBody(await skill.readSkillFile({ name: detail.name, path }));
    } catch (err) {
      setError(err.message);
    }
  };

  return (
    <div className="drawer-mask" role="dialog" aria-modal="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="drawer" style={{ width: 820 }}>
        <div className="drawer-header">
          <div style={{ flex: 1, minWidth: 0 }}>
            <h3 style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
              {detail.title || detail.name}
              <span className={`badge ${MODE_TONES[mode]}`}>{MODE_LABELS[mode]}</span>
              {detail.enabled ? <Badge tone="ok">启用</Badge> : <Badge tone="warn">停用</Badge>}
              {detail.category && <Badge>{detail.category}</Badge>}
            </h3>
            <div className="drawer-sub">
              <span className="mono">{detail.name}</span>
              {detail.version ? ` v${detail.version}` : ''} · 来源 {detail.source} · 更新 {shortTime(detail.updated_at || detail.installed_at)}
              {detail.description ? ` · ${detail.description}` : ''}
            </div>
          </div>
          <button className="btn ghost sm" type="button" onClick={() => onRun(detail)} disabled={!detail.enabled}>
            试跑
          </button>
          <button className="btn ghost sm" type="button" onClick={onClose}>关闭</button>
        </div>

        <div className="drawer-body">
          {error && <Alert type="error">{error}</Alert>}

          <div className="chips" style={{ marginBottom: 14 }}>
            {TABS.map((t) => (
              <button key={t.key} type="button" className={`chip${tab === t.key ? ' active' : ''}`} onClick={() => setTab(t.key)}>
                {t.label}
              </button>
            ))}
            <button className="btn ghost sm" type="button" style={{ marginLeft: 'auto' }} onClick={loadRuntime} disabled={loading}>
              {loading ? <Spinner text="刷新" /> : '刷新运行时'}
            </button>
          </div>

          {tab === 'runtime' && (
            <>
              <div className="panel">
                <div className="panel-head" style={{ padding: 0, marginBottom: 10 }}>
                  <div>
                    <h2 className="panel-title">执行配置</h2>
                    <p className="panel-desc">
                      决定 Agent 调用这个技能时平台怎么执行；改完立即生效，不影响已安装的文件。
                    </p>
                  </div>
                </div>
                <SkillExecutionForm
                  name={detail.name}
                  spec={detail.execution}
                  instances={instances}
                  onSaved={() => { loadRuntime(); if (onChanged) onChanged(); }}
                  onError={setError}
                />
              </div>

              <div className="panel">
                <div className="panel-head" style={{ padding: 0, marginBottom: 10 }}>
                  <div>
                    <h2 className="panel-title">Worker 实例</h2>
                    <p className="panel-desc">
                      {executable
                        ? '集群模式由 Worker 注册实例并周期心跳，调度器按「权重 × 空闲度 ÷ 延迟」挑实例；心跳过期、排空中、连续失败达阈值的实例不参与。'
                        : '本地模式不经过 Worker，没有实例；需要远程执行时把上面的执行模式改成远程或异步。'}
                    </p>
                  </div>
                </div>
                {executable ? (
                  <SkillInstanceTable name={detail.name} instances={instances} onChanged={loadRuntime} onError={setError} />
                ) : (
                  <EmptyState title="本地模式无需实例" desc="改成远程/异步后，这里会出现 Worker 注册的实例列表" />
                )}
              </div>
            </>
          )}

          {tab === 'files' && (
            <div className="grid-2 align-start">
              <div>
                <div className="field-label">文件清单</div>
                <ul className="list-reset">
                  <li>
                    <button className="btn ghost sm" type="button" onClick={() => readFile('SKILL.md')}>
                      SKILL.md
                    </button>
                    <span className="muted mono" style={{ marginLeft: 'auto' }}>
                      {files?.size ?? '—'} B
                    </span>
                  </li>
                  {(files?.files || []).map((f) => (
                    <li key={f.path}>
                      <button className="btn ghost sm" type="button" onClick={() => readFile(f.path)}>
                        {f.path}
                      </button>
                      <span className="muted mono" style={{ marginLeft: 'auto' }}>
                        {f.size} B
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
              <div>
                <div className="field-label">{fileBody ? fileBody.path : '文件内容'}</div>
                <pre className="block mono" style={{ maxHeight: 420, overflow: 'auto' }}>
                  {fileBody ? fileBody.content : '点击左侧文件查看内容'}
                </pre>
              </div>
            </div>
          )}

          {tab === 'executions' && (
            <>
              <div className="panel-head" style={{ padding: 0, marginBottom: 10 }}>
                <div>
                  <h2 className="panel-title">最近执行</h2>
                  <p className="panel-desc">
                    {executable ? '该技能最近 20 条执行记录，点行看输入输出与时间线' : '本地模式不写执行记录（使用痕迹在「技能使用历史」里）'}
                  </p>
                </div>
                <div className="panel-actions">
                  {onOpenAllExecutions && (
                    <button className="btn ghost sm" type="button" onClick={() => onOpenAllExecutions(detail.name)}>
                      查看全部
                    </button>
                  )}
                </div>
              </div>

              {!executable ? (
                <EmptyState title="无执行记录" desc="本地模式只返回 SKILL.md，不产生执行账本" />
              ) : executions.length === 0 ? (
                <EmptyState title="还没有执行记录" desc="点右上角「试跑」手动调一次，或等 Agent 任务调用该技能" />
              ) : (
                <div className="table-wrap">
                  <table className="table">
                    <thead>
                      <tr>
                        <th>执行 ID</th>
                        <th>状态</th>
                        <th className="num">尝试</th>
                        <th className="num">耗时</th>
                        <th>落点</th>
                        <th>时间</th>
                      </tr>
                    </thead>
                    <tbody>
                      {executions.map((e) => (
                        <tr key={e.id} className="row-click" onClick={() => onOpenExecution && onOpenExecution(e.id)}>
                          <td className="mono" style={{ fontSize: 12 }}>{e.id}</td>
                          <td><span className={`badge ${execTone(e.status)}`}>{EXEC_LABELS[e.status] || e.status}</span></td>
                          <td className="num">{e.attempts || 1}</td>
                          <td className="num">{fmtLatency(e.latency_ms)}</td>
                          <td className="mono" style={{ fontSize: 12 }}>{e.worker_id || '—'}</td>
                          <td className="mono" style={{ fontSize: 12 }}>{shortTime(e.created_at)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </div>
  );
}
