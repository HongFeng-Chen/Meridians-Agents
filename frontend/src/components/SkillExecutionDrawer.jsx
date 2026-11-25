import { useCallback, useEffect, useRef, useState } from 'react';
import { skill } from '../api.js';
import { Alert, Badge, Spinner } from './UI.jsx';
import { EXEC_LABELS, MODE_LABELS, execTone, fmtLatency, fmtTime, isTerminal } from './skillRuntime.js';

// SkillExecutionDrawer 单条执行记录的详情（账本视角）。
//
// 排队/执行中的记录会每 2 秒自刷新一次，等终态自动停下——
// 异步技能提交后用户通常就停在这里等结果。

const POLL_MS = 2000;
const POLL_MAX = 90;

export default function SkillExecutionDrawer({ executionId, initial, onClose, onRerun, onChanged }) {
  const [exec, setExec] = useState(initial || null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState('');
  const timerRef = useRef(null);

  const stopPolling = useCallback(() => {
    if (timerRef.current) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
  }, []);

  const load = useCallback(
    async (id) => {
      try {
        const data = await skill.getSkillExecution({ id });
        setExec(data);
        if (!isTerminal(data.status)) {
          timerRef.current = setTimeout(() => load(id), POLL_MS);
        }
        return data;
      } catch (err) {
        setError(err.message);
        return null;
      }
    },
    [],
  );

  useEffect(() => {
    stopPolling();
    if (!executionId) return undefined;
    load(executionId);
    return stopPolling;
  }, [executionId, load, stopPolling]);

  const cancel = async () => {
    setBusy('cancel');
    setError('');
    try {
      const next = await skill.cancelSkillExecution({ id: exec.id });
      setExec(next);
      if (onChanged) onChanged();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  if (!exec) {
    return (
      <div className="drawer-mask" role="dialog" aria-modal="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
        <div className="drawer" style={{ width: 620 }}>
          <div className="drawer-header">
            <div style={{ flex: 1 }}>
              <h3>执行详情</h3>
              <div className="drawer-sub">{error || '加载中…'}</div>
            </div>
            <button className="btn ghost sm" type="button" onClick={onClose}>关闭</button>
          </div>
          <div className="drawer-body">{error ? <Alert type="error">{error}</Alert> : <Spinner text="读取执行记录" />}</div>
        </div>
      </div>
    );
  }

  const pending = !isTerminal(exec.status);

  return (
    <div className="drawer-mask" role="dialog" aria-modal="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="drawer" style={{ width: 720 }}>
        <div className="drawer-header">
          <div style={{ flex: 1, minWidth: 0 }}>
            <h3 style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
              <span className="mono">{exec.id}</span>
              <span className={`badge ${execTone(exec.status)}`}>{EXEC_LABELS[exec.status] || exec.status}</span>
              {pending && <Badge tone="indigo"><Spinner text="跟踪中" /></Badge>}
            </h3>
            <div className="drawer-sub">
              {exec.skill_name}
              {exec.skill_version ? ` v${exec.skill_version}` : ''} · {MODE_LABELS[exec.mode] || exec.mode}
              {exec.task_id ? ` · 任务 #${exec.task_id}` : ''} · {fmtLatency(exec.latency_ms)}
              {exec.worker_id ? ` · 落点 ${exec.worker_id}` : ''}
            </div>
          </div>
          {exec.status === 'queued' && (
            <button className="btn ghost sm" type="button" disabled={busy === 'cancel'} onClick={cancel}>
              {busy === 'cancel' ? <Spinner text="取消中" /> : '取消执行'}
            </button>
          )}
          {onRerun && exec.mode !== 'local' && (
            <button className="btn ghost sm" type="button" onClick={() => onRerun(exec)}>
              按原参数重跑
            </button>
          )}
          <button className="btn ghost sm" type="button" onClick={onClose}>关闭</button>
        </div>

        <div className="drawer-body">
          {error && <Alert type="error">{error}</Alert>}

          {exec.error && <Alert type="error">{exec.error}</Alert>}

          <div className="panel">
            <div className="kv">
              <div><span>技能</span><b className="mono">{exec.skill_name}{exec.skill_version ? ` v${exec.skill_version}` : ''}</b></div>
              <div><span>模式</span><b>{MODE_LABELS[exec.mode] || exec.mode}</b></div>
              <div><span>状态</span><b>{EXEC_LABELS[exec.status] || exec.status}</b></div>
              <div><span>任务 / 租户</span><b className="mono">{exec.task_id || '—'} / {exec.tenant_id || '—'}</b></div>
              <div><span>用户 / Agent</span><b className="mono">{exec.user_id || '—'} / {exec.agent_key || '—'}</b></div>
              <div><span>落点 Worker</span><b className="mono">{exec.worker_id || '—'}</b></div>
              <div><span>尝试次数</span><b>{exec.attempts || 1}</b></div>
              <div><span>耗时</span><b>{fmtLatency(exec.latency_ms)}</b></div>
              <div><span>幂等键</span><b className="mono">{exec.idempotency_key || '—'}</b></div>
              <div><span>Trace</span><b className="mono">{exec.trace_id || '—'}</b></div>
            </div>
          </div>

          <div className="panel">
            <div className="field-label">时间线</div>
            <div className="exec-timeline">
              <div><span>入队</span><b className="mono">{fmtTime(exec.queued_at)}</b></div>
              <div><span>开始</span><b className="mono">{fmtTime(exec.started_at)}</b></div>
              <div><span>结束</span><b className="mono">{fmtTime(exec.finished_at)}</b></div>
            </div>
          </div>

          <div className="panel">
            <div className="field-label">输入参数</div>
            <pre className="block mono" style={{ maxHeight: 200, overflow: 'auto' }}>{pretty(exec.input) || '—'}</pre>
          </div>

          <div className="panel">
            <div className="field-label">输出</div>
            <pre className="block mono" style={{ maxHeight: 320, overflow: 'auto' }}>{pretty(exec.output) || '（尚未产出）'}</pre>
          </div>
        </div>
      </div>
    </div>
  );
}

function pretty(raw) {
  if (raw == null || raw === '') return '';
  if (typeof raw !== 'string') return JSON.stringify(raw, null, 2);
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
