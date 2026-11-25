import { useEffect, useRef, useState } from 'react';
import { skill } from '../api.js';
import { Alert, FormDrawer, Spinner } from './UI.jsx';
import {
  EXEC_LABELS,
  MODE_LABELS,
  execTone,
  exampleFromSchema,
  fmtLatency,
  isTerminal,
  parseJSONLoose,
} from './skillRuntime.js';

// SkillRunDrawer 手动试跑一个技能。
//
// 三种模式共用这一个抽屉，差异体现在结果区：
//   local  —— 立即返回 SKILL.md 文本（不落执行记录）
//   remote —— 等 Worker 返回，展示输出与耗时
//   async  —— 先拿 execution_id，再按 2s 轮询直到终态
//
// 试跑是排障入口，所以把「落点 Worker / 尝试次数 / 幂等回放」都摊开显示，
// 而不是只给一句成功。

const POLL_MS = 2000;
const POLL_MAX = 90; // 最多跟 3 分钟，之后交回用户手动刷新

export default function SkillRunDrawer({ target, initialInput, onClose, onOpenExecution }) {
  const spec = target?.execution || { mode: 'local' };
  const mode = spec.mode || 'local';

  const [inputText, setInputText] = useState(() => initialInput || exampleFromSchema(spec.input_schema));
  const [taskId, setTaskId] = useState('');
  const [idemKey, setIdemKey] = useState('');
  const [timeoutSec, setTimeoutSec] = useState('');
  const [forceAsync, setForceAsync] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState(null);
  const [pollTip, setPollTip] = useState('');
  const timerRef = useRef(null);

  // 抽屉关闭时把定时器停掉，避免继续请求已卸载组件
  useEffect(() => () => {
    if (timerRef.current) clearTimeout(timerRef.current);
  }, []);

  const stopPolling = () => {
    if (timerRef.current) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
  };

  const refresh = async (id) => {
    try {
      const exec = await skill.getSkillExecution({ id });
      setResult((prev) => ({ ...(prev || {}), ...exec, queued: false }));
      if (!isTerminal(exec.status)) {
        timerRef.current = setTimeout(() => refresh(id), POLL_MS);
      } else {
        setPollTip('');
      }
    } catch (err) {
      setPollTip(`查询执行状态失败：${err.message}`);
    }
  };

  const startPolling = (id) => {
    stopPolling();
    let ticks = 0;
    setPollTip('已入队，正在轮询执行状态…');
    const tick = async () => {
      ticks += 1;
      if (ticks > POLL_MAX) {
        setPollTip('轮询超时，稍后可在「执行记录」里查看结果');
        return;
      }
      try {
        const exec = await skill.getSkillExecution({ id });
        setResult((prev) => ({ ...(prev || {}), ...exec, queued: false }));
        if (isTerminal(exec.status)) {
          setPollTip('');
          return;
        }
        setPollTip(`已等待 ${ticks * 2}s，任务仍在${EXEC_LABELS[exec.status] || '执行'}…`);
        timerRef.current = setTimeout(tick, POLL_MS);
      } catch (err) {
        setPollTip(`查询执行状态失败：${err.message}`);
      }
    };
    timerRef.current = setTimeout(tick, POLL_MS);
  };

  const run = async () => {
    setError('');
    const parsed = parseJSONLoose(inputText);
    if (parsed.error) {
      setError(parsed.error);
      return;
    }
    setBusy(true);
    setResult(null);
    setPollTip('');
    stopPolling();
    try {
      const reply = await skill.executeSkill({
        name: target.name,
        input: JSON.stringify(parsed.value),
        task_id: taskId.trim(),
        idempotency_key: idemKey.trim(),
        timeout_sec: Number(timeoutSec) || 0,
        async: forceAsync,
      });
      setResult(reply);
      if (reply.queued || reply.status === 'queued') startPolling(reply.execution_id);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const status = result?.status;
  const queued = status === 'queued';

  return (
    <FormDrawer
      title={`试跑 ${target?.title || target?.name || ''}`}
      desc="按当前执行配置真实调用一次；结果会落进执行记录（local 模式除外）"
      eyebrow={MODE_LABELS[mode]}
      width={720}
      onClose={() => {
        stopPolling();
        onClose();
      }}
      footer={
        <div className="modal-footer-bar">
          <span className="footer-hint">
            {mode === 'local' ? '本地模式不经过 Worker，也不写执行记录' : `模式 ${MODE_LABELS[mode]} · 结果写入执行记录`}
          </span>
          <div className="footer-actions">
            <button className="btn ghost sm" type="button" onClick={onClose}>
              关闭
            </button>
            <button className="btn primary sm" type="button" onClick={run} disabled={busy}>
              {busy ? <Spinner text="执行中" /> : queued ? '再次提交' : '执行'}
            </button>
          </div>
        </div>
      }
    >
      <div className="form">
        {error && <Alert type="error">{error}</Alert>}

        <label className="field">
          <span className="field-label">参数（JSON 对象）</span>
          <textarea
            className="textarea mono"
            rows={7}
            value={inputText}
            onChange={(e) => setInputText(e.target.value)}
            spellCheck={false}
          />
          <span className="field-hint">
            按技能的参数 Schema 生成示例；
            <button
              className="link-btn"
              type="button"
              onClick={() => setInputText(exampleFromSchema(spec.input_schema))}
            >
              重新生成示例
            </button>
          </span>
        </label>

        <div className="form-grid cols-3">
          <label className="field">
            <span className="field-label">关联任务号（可选）</span>
            <input
              className="input mono"
              placeholder="42"
              value={taskId}
              onChange={(e) => setTaskId(e.target.value)}
            />
          </label>
          <label className="field">
            <span className="field-label">幂等键（可选）</span>
            <input
              className="input mono"
              placeholder={spec.idempotent ? '同键回放上次成功结果' : '需在配置里开幂等'}
              value={idemKey}
              onChange={(e) => setIdemKey(e.target.value)}
            />
          </label>
          <label className="field">
            <span className="field-label">超时覆盖（秒）</span>
            <input
              className="input mono"
              type="number"
              min="0"
              placeholder={spec.timeout_sec ? String(spec.timeout_sec) : '默认'}
              value={timeoutSec}
              onChange={(e) => setTimeoutSec(e.target.value)}
            />
          </label>
        </div>

        {mode === 'remote' && (
          <label className="chip" style={{ cursor: 'pointer', alignSelf: 'flex-start' }}>
            <input type="checkbox" checked={forceAsync} onChange={(e) => setForceAsync(e.target.checked)} />{' '}
            转成异步提交（先拿执行 ID，不阻塞界面）
          </label>
        )}

        {result && (
          <div className="block" style={{ marginTop: 4 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
              <span className={`badge ${execTone(result.status)}`}>{EXEC_LABELS[result.status] || result.status}</span>
              <span className="mono" style={{ fontSize: 12.5 }}>{result.execution_id || '—'}</span>
              {result.mode && <span className="badge">{MODE_LABELS[result.mode] || result.mode}</span>}
              {result.worker_id && (
                <span className="muted mono" style={{ fontSize: 12 }}>
                  落点 {result.worker_id}
                </span>
              )}
              {result.attempts > 1 && <span className="badge tone-warn">尝试 {result.attempts} 次</span>}
              {result.latency_ms != null && (
                <span className="muted" style={{ fontSize: 12 }}>耗时 {fmtLatency(result.latency_ms)}</span>
              )}
              {result.execution_id && onOpenExecution && (
                <button
                  className="link-btn"
                  type="button"
                  style={{ marginLeft: 'auto' }}
                  onClick={() => onOpenExecution(result.execution_id)}
                >
                  查看执行详情
                </button>
              )}
            </div>

            {pollTip && <div className="muted" style={{ fontSize: 12.5, marginTop: 6 }}>{pollTip}</div>}

            {result.error && (
              <pre className="mono" style={{ fontSize: 12, marginTop: 10, whiteSpace: 'pre-wrap', color: 'var(--warn-text, #b45309)' }}>
                {result.error}
              </pre>
            )}

            {result.text && !result.error && (
              <pre className="mono" style={{ fontSize: 12, marginTop: 10, whiteSpace: 'pre-wrap', maxHeight: 220, overflow: 'auto' }}>
                {result.text}
              </pre>
            )}

            {result.output && (
              <details style={{ marginTop: 8 }}>
                <summary className="muted" style={{ fontSize: 12.5, cursor: 'pointer' }}>原始输出</summary>
                <pre className="mono" style={{ fontSize: 12, marginTop: 6, whiteSpace: 'pre-wrap', maxHeight: 240, overflow: 'auto' }}>
                  {prettyJSON(result.output)}
                </pre>
              </details>
            )}
          </div>
        )}

        {!result && !busy && (
          <div className="muted" style={{ fontSize: 12.5 }}>
            {mode === 'local' && '本地模式：返回 SKILL.md 内容并注入上下文，不经过 Worker，也不写执行记录。'}
            {mode === 'remote' && '远程模式：调度到某个在线实例同步执行；失败会换实例重试，连续失败达阈值后熔断该实例。'}
            {mode === 'async' && '异步模式：提交后立即返回执行 ID，界面上会自动轮询状态直到出现终态。'}
          </div>
        )}

        {busy && <Spinner text="正在调用技能" />}
      </div>
    </FormDrawer>
  );
}

function prettyJSON(raw) {
  if (raw == null) return '';
  if (typeof raw !== 'string') return JSON.stringify(raw, null, 2);
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
