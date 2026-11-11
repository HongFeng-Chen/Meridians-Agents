import { useEffect, useState } from 'react';
import { Alert, Badge, Spinner } from './UI.jsx';
import { LLMCallsTable, fmtTokens } from './LLMCalls.jsx';
import { Markdown } from './Markdown.jsx';
import { v2 } from '../api.js';

// TaskDetailDrawer 任务链路抽屉（参考 Dify/n8n 运行详情与 SkyWalking 链路视图）：
// 右侧滑出弹层，概要 → 输入/答案 → Trace 时间线（执行事件 + 操作历史合并）→ LLM 调用记录 → 关联调用历史。
// 任务中心与会话页（内嵌任务链路）共用；width 可自定义（会话页用 50vw 靠右）。

export const STATUS_TONES = {
  succeeded: 'ok',
  running: 'indigo',
  pending: 'warn',
  paused: 'warn',
  retry_wait: 'warn',
  failed: 'error',
  canceled: 'default',
  timeout: 'error',
};
export const STATUS_LABELS = {
  pending: '排队中',
  running: '执行中',
  paused: '待审批',
  retry_wait: '等待重试',
  succeeded: '成功',
  failed: '失败',
  canceled: '已取消',
  timeout: '执行超时',
};

export const EVENT_LABELS = {
  status: '状态',
  thought: '思考',
  tool_call: '调用',
  tool_result: '结果',
  delegate: '委派',
  plan: '计划',
  orchestrate: '编排',
  evaluate: '评估',
  reflect: '反思',
  route: '路由',
  node: '节点',
  supervise: '监察',
  risk_assess: '风评',
  paused: '暂停',
  approval: '审批',
  resume: '续跑',
  final: '答案',
  error: '错误',
};
export const EVENT_TONES = {
  thought: 'default',
  tool_call: 'indigo',
  tool_result: 'teal',
  delegate: 'violet',
  plan: 'indigo',
  orchestrate: 'violet',
  evaluate: 'warn',
  reflect: 'violet',
  route: 'indigo',
  node: 'default',
  supervise: 'violet',
  risk_assess: 'warn',
  paused: 'warn',
  approval: 'violet',
  resume: 'teal',
  final: 'ok',
  error: 'error',
  status: 'warn',
};

export const HISTORY_KIND_META = {
  mcp_call: { label: 'MCP 调用', tone: 'indigo', store: '外部工具' },
  skill_use: { label: '技能使用', tone: 'violet', store: '技能库' },
  memory_recall: { label: '记忆召回', tone: 'teal', store: 'Redis 短期 · MySQL 长期' },
  rag_recall: { label: 'RAG 召回', tone: 'blue', store: 'Milvus 向量' },
  graph_recall: { label: 'GraphRAG 召回', tone: 'warn', store: 'Milvus 图谱' },
};

export function formatTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return v;
  return d.toLocaleString('zh-CN', { hour12: false });
}

// 任务耗时：开始 → 结束；未结束且传入 nowTs 时按当前时刻计算（调用方用定时器刷新 nowTs 实现跳动）。
// 优雅降级：<1s 显示 ms，<1min 显示秒，更长显示分/小时。
export function formatDuration(startedAt, finishedAt, nowTs) {
  if (!startedAt) return '—';
  const end = finishedAt ? new Date(finishedAt) : nowTs ? new Date(nowTs) : new Date();
  const ms = end - new Date(startedAt);
  if (Number.isNaN(ms) || ms < 0) return '—';
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${Math.round(s % 60)}s`;
  return `${Math.floor(m / 60)}h${m % 60}m`;
}

// 事件 payload 美化成一行可读摘要
function eventSummary(type, payload) {
  let p = {};
  try {
    p = JSON.parse(payload || '{}');
  } catch {
    return payload || '';
  }
  switch (type) {
    case 'status':
      return `${p.from || '—'} → ${p.to}`;
    case 'thought':
      return p.parse_error ? `解析失败：${p.parse_error}（原始输出：${p.thought || ''}）` : p.thought || '';
    case 'tool_call':
      return `${p.name} ${JSON.stringify(p.args ?? {})}`;
    case 'tool_result':
      return `${p.name}${p.is_error ? ' [失败]' : ''}：${p.result ?? ''}`;
    case 'delegate':
      return `委派给 ${p.agent}（子任务 #${p.child_task_id}）：${p.input ?? ''}`;
    case 'plan': {
      if (Array.isArray(p.questions)) {
        return `拆出 ${p.questions.length} 个子问题：${p.questions.join(' / ')}`;
      }
      const steps = Array.isArray(p.steps) ? p.steps : [];
      const head = steps.map((s, i) => `${i + 1}.${s.goal ?? s}${s.agent ? `（${s.agent}）` : ''}`).join(' → ');
      return `${p.revised ? '重规划' : '计划'} ${steps.length} 步：${head}`;
    }
    case 'orchestrate': {
      const ms = Array.isArray(p.members) ? p.members : [];
      const who = ms.map((m) => `${m.name || m.agent_key}${m.duty ? `（${m.duty}）` : ''}`).join(' / ');
      const extra = [];
      if (Array.isArray(p.created) && p.created.length) extra.push(`新建 ${p.created.length}`);
      if (Array.isArray(p.reused) && p.reused.length) extra.push(`复用 ${p.reused.length}`);
      return `选用 ${p.graph_type}${p.reason ? `（${p.reason}）` : ''} · ${ms.length} 个成员${who ? `：${who}` : ''}${extra.length ? ` · ${extra.join(' / ')}` : ''}`;
    }
    case 'evaluate':
      return `第 ${p.round} 轮评分 ${p.score ?? '—'} · ${p.pass ? '通过' : '未通过'}${p.issues ? `：${p.issues}` : ''}`;
    case 'reflect':
      return `第 ${p.round} 轮教训：${p.lesson ?? ''}`;
    case 'route':
      return p.target && p.target !== 'none'
        ? `路由到 ${p.target}${p.reason ? `（${p.reason}）` : ''}`
        : `不路由技能，直接作答${p.reason ? `（${p.reason}）` : ''}`;
    case 'node':
      return `[${p.kind || '节点'}] ${p.node || ''}${p.agent ? ` · ${p.agent}` : ''} · ${p.status || ''}${p.output ? `：${p.output}` : ''}`;
    case 'supervise':
      return `${p.supervisor || '监察'} 审查 ${p.agent || ''}：${p.passed ? '通过' : `未通过 ${p.issues || ''}`}`;
    case 'risk_assess':
      return `${p.tool || '工具'} 风险 ${p.level || '—'} · ${p.allow ? '放行' : '拦截'}${p.reason ? `：${p.reason}` : ''}`;
    case 'paused':
      return `${p.name || p.ability || '能力'} ${JSON.stringify(p.args ?? {})}${p.reason ? `（${p.reason === 'risk_assess_high' ? `高危转人工：${p.risk_reason || ''}` : p.reason}）` : ''}，等待人工审批`;
    case 'approval':
      return `${p.decision === 'approve' ? '批准' : p.decision === 'reject' ? '驳回' : p.decision || '—'}${p.by ? ` · ${p.by}` : ''}${p.note ? `：${p.note}` : ''}`;
    case 'resume':
      return `从 checkpoint #${p.seq ?? '—'} 续跑（游标 ${p.cursor || '—'}）`;
    case 'final':
      return p.answer ?? '';
    case 'error':
      return p.message ?? '';
    default:
      return JSON.stringify(p);
  }
}

function prettyPayload(payload) {
  try {
    return JSON.stringify(JSON.parse(payload || '{}'), null, 2);
  } catch {
    return payload || '';
  }
}

// MdBlock：Markdown 内容块，带「渲染/原文」切换（与调用详情弹层同款交互）。
// 默认渲染视图解析 Markdown；切到原文按等宽纯文本展示。
function MdBlock({ text, fallback = '—', defaultView = 'md' }) {
  const [view, setView] = useState(defaultView);
  return (
    <div className="call-detail-block">
      <div className="call-detail-label">
        <span className="muted" style={{ fontWeight: 400 }}>{text ? `${text.length} 字符` : ''}</span>
        <span className="io-view-toggle" role="group" aria-label="视图切换">
          <button type="button" className={view === 'md' ? 'active' : ''} onClick={() => setView('md')}>渲染</button>
          <button type="button" className={view === 'raw' ? 'active' : ''} onClick={() => setView('raw')}>原文</button>
        </span>
      </div>
      {view === 'md' ? (
        text ? (
          <div className="call-detail-md">
            <Markdown text={text} />
          </div>
        ) : (
          <div className="call-detail-md muted">{fallback}</div>
        )
      ) : (
        <pre>{text || fallback}</pre>
      )}
    </div>
  );
}

// 从任务中心直达来源会话：切到会话页并打开指定会话（Chats 页监听 wb:open-conv）
export function openConversation(convId) {
  window.location.hash = '#/chats';
  setTimeout(() => window.dispatchEvent(new CustomEvent('wb:open-conv', { detail: convId })), 60);
}

// Span 类型配色：与后端 span.kind 属性一一对应（task / llm / tool / rag / graphrag）
const SPAN_KINDS = {
  task: { label: '任务', tone: 'indigo' },
  llm: { label: 'LLM', tone: 'violet' },
  tool: { label: '工具', tone: 'teal' },
  rag: { label: 'RAG', tone: 'blue' },
  graphrag: { label: '图谱', tone: 'warn' },
  other: { label: '其他', tone: 'default' },
};

// buildSpanTree 按 parent_id 组装层级（父不在样本里的当根：采样丢失时也要能显示）
function buildSpanTree(spans) {
  const byId = new Map(spans.map((s) => [s.span_id, { ...s, children: [] }]));
  const roots = [];
  byId.forEach((node) => {
    const parent = node.parent_id ? byId.get(node.parent_id) : null;
    if (parent) parent.children.push(node);
    else roots.push(node);
  });
  const sortByStart = (list) => list.sort((a, b) => new Date(a.start) - new Date(b.start));
  sortByStart(roots);
  byId.forEach((n) => sortByStart(n.children));
  return roots;
}

// fmtMicros 微元 → 元（1 元 = 1_000_000 微元）：成本一律按整数微元存储，避免浮点累加误差
export function fmtMicros(micros) {
  const v = Number(micros || 0) / 1e6;
  if (v === 0) return '¥0';
  if (v < 0.01) return `¥${v.toFixed(4)}`;
  return `¥${v.toFixed(2)}`;
}

// renderSpanNodes 递归展开 span 树：缩进表示层级，属性只挑对排查有用的几项。
// 不折叠：一次任务的 span 通常几十个，折叠反而多一次点击。
function renderSpanNodes(nodes, depth = 0, out = []) {
  nodes.forEach((n) => {
    const meta = SPAN_KINDS[n.kind] || SPAN_KINDS.other;
    const attrs = n.attrs || {};
    out.push(
      <div key={n.span_id} style={{ padding: `3px 0 3px ${depth * 14}px` }}>
        <div style={{ display: 'flex', gap: 8, alignItems: 'baseline', flexWrap: 'wrap' }}>
          <Badge tone={meta.tone}>{meta.label}</Badge>
          <span className="mono" style={{ fontSize: 12 }}>{n.name}</span>
          <span className="muted mono" style={{ fontSize: 11 }}>
            {n.duration_ms > 0 ? `${n.duration_ms}ms` : '<1ms'}
          </span>
          {n.status === 'error' && <Badge tone="error">失败</Badge>}
          {attrs['mcp.replayed'] === true && <Badge tone="warn">幂等回放</Badge>}
          {attrs['task.snapshot_id'] ? (
            <span className="muted mono" style={{ fontSize: 11 }}>snap#{attrs['task.snapshot_id']}</span>
          ) : null}
          {attrs['llm.model'] ? (
            <span className="muted mono" style={{ fontSize: 11 }}>
              {attrs['llm.model']}
              {attrs['llm.prompt_tokens'] ? ` · ${attrs['llm.prompt_tokens']}/${attrs['llm.completion_tokens'] ?? 0} tok` : ''}
            </span>
          ) : null}
          {attrs['tool.name'] ? (
            <span className="muted mono" style={{ fontSize: 11 }}>{attrs['tool.name']}</span>
          ) : null}
          {attrs['rag.hits'] !== undefined ? (
            <span className="muted mono" style={{ fontSize: 11 }}>命中 {attrs['rag.hits']}</span>
          ) : null}
        </div>
        {n.error && <div className="cell-sub" style={{ color: 'var(--error, #dc2626)' }}>{n.error}</div>}
      </div>,
    );
    if (n.children?.length) renderSpanNodes(n.children, depth + 1, out);
  });
  return out;
}

export function TaskDetailDrawer({ detail, events, calls, histories = [], busy, onClose, onOpenTask, onCancel, onRetry, onApprove, width }) {
  // ESC 关闭 + 打开期间锁定背景滚动
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      window.removeEventListener('keydown', onKey);
      document.body.style.overflow = prev;
    };
  }, [onClose]);

  const running = ['pending', 'running'].includes(detail.status);
  const paused = detail.status === 'paused';
  const fmtDuration = () => formatDuration(detail.started_at, detail.finished_at);

  // 链路与成本（V2-P4）：这两个数据都不在 v1 proto 里，随抽屉打开单独拉一次。
  // 失败一律静默降级——追踪没开 / 老部署没有 v2 接口时，抽屉其余部分照常可用。
  const [trace, setTrace] = useState(null);
  const [costRows, setCostRows] = useState(null);
  useEffect(() => {
    let alive = true;
    setTrace(null);
    setCostRows(null);
    (async () => {
      try {
        const res = await v2.traces({ task_id: detail.id, limit: 1 });
        if (alive && res?.enabled && res.traces?.length) setTrace(res.traces[0]);
      } catch {
        /* 追踪未启用或内存缓冲已淘汰：不展示链路区块 */
      }
      try {
        const res = await v2.usageSummary({ task_id: String(detail.id), group_by: 'kind' });
        if (alive && res?.buckets?.length) setCostRows(res.buckets);
      } catch {
        /* 成本中心未接线：不展示成本区块 */
      }
    })();
    return () => {
      alive = false;
    };
  }, [detail.id, detail.status]);

  // 最近一次 paused 事件：展示挂起的能力与参数（审批卡片内容）
  const pausedEvt = paused
    ? [...(events || [])].reverse().find((e) => e.type === 'paused')
    : null;
  let pausedInfo = {};
  if (pausedEvt) {
    try {
      pausedInfo = JSON.parse(pausedEvt.payload || '{}');
    } catch {
      pausedInfo = {};
    }
  }

  const decide = async (decision) => {
    if (!onApprove) return;
    let note = '';
    if (decision === 'reject') {
      note = window.prompt('驳回说明（将写回任务错误信息）', '人工驳回') ?? '';
      if (!note) return;
    }
    await onApprove(detail, decision, note);
  };

  return (
    <div className="drawer-mask" role="dialog" aria-modal="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="drawer" style={width ? { width } : undefined}>
        <div className="drawer-header">
          <div style={{ flex: 1, minWidth: 0 }}>
            <h3 style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              任务 #{detail.id} · <span className="mono">{detail.agent_key}</span>
              <Badge tone={STATUS_TONES[detail.status] || 'default'}>{STATUS_LABELS[detail.status]}</Badge>
              {detail.archived && <Badge tone="default">历史归档</Badge>}
              {running && <Badge tone="indigo"><Spinner text="实时跟中" /></Badge>}
            </h3>
            <div className="drawer-sub">
              创建 {formatTime(detail.created_at)}
              {detail.started_at ? ` · 开始 ${formatTime(detail.started_at)}` : ''}
              {detail.finished_at ? ` · 结束 ${formatTime(detail.finished_at)}` : ''}
              {' · '}耗时 {fmtDuration()}
            </div>
          </div>
          {(running || paused) && onCancel && (
            <button className="btn ghost sm" type="button" disabled={busy === `cancel-${detail.id}`} onClick={() => onCancel(detail)}>
              取消任务
            </button>
          )}
          {['succeeded', 'failed', 'canceled', 'timeout'].includes(detail.status) && onRetry && (
            <button className="btn ghost sm" type="button" disabled={busy === `retry-${detail.id}`} onClick={() => onRetry(detail)}>
              重试
            </button>
          )}
          <button className="btn ghost sm" type="button" onClick={onClose}>关闭</button>
        </div>

        <div className="drawer-body">
          {/* 审批闸门（O3）：任务挂起等待人工审批 */}
          {paused && (
            <div className="block" style={{ marginBottom: 14, borderColor: 'var(--warn, #d97706)', background: 'var(--warn-bg, #fffbeb)' }}>
              <div style={{ fontWeight: 600, marginBottom: 6 }}>⏸ 待人工审批</div>
              <div className="muted" style={{ marginBottom: 8, fontSize: 13 }}>
                任务在执行 <span className="mono">{pausedInfo.name || pausedInfo.ability || '—'}</span> 时触发审批闸门
                {pausedInfo.reason === 'risk_assess_high' ? '（风险评估为高危，转人工）' : '（该能力被 Agent 配置为需审批）'}
                ，批准后从 checkpoint 续跑，驳回则任务置为失败。
              </div>
              {(pausedInfo.args !== undefined) && (
                <pre className="mono" style={{ fontSize: 12, background: 'rgba(0,0,0,0.04)', padding: 8, borderRadius: 6, overflow: 'auto', maxHeight: 160, margin: '0 0 10px' }}>
                  {JSON.stringify(pausedInfo.args, null, 2)}
                </pre>
              )}
              {pausedInfo.risk_reason && (
                <div className="muted" style={{ fontSize: 13, marginBottom: 10 }}>风险评估：{pausedInfo.risk_reason}</div>
              )}
              {onApprove && (
                <div style={{ display: 'flex', gap: 8 }}>
                  <button className="btn sm" type="button" disabled={busy === `approve-${detail.id}`} onClick={() => decide('approve')}>
                    批准继续
                  </button>
                  <button className="btn ghost sm" type="button" disabled={busy === `approve-${detail.id}`} onClick={() => decide('reject')}>
                    驳回
                  </button>
                </div>
              )}
            </div>
          )}

          {/* 概要 */}
          <div className="kv" style={{ marginBottom: 14 }}>
            <div><span className="muted">进度</span> <span className="mono">{detail.progress}%</span></div>
            <div><span className="muted">父任务</span> {detail.parent_task_id > 0 ? `#${detail.parent_task_id}` : '—（根任务）'}</div>
            {detail.conv_id && (
              <div>
                <span className="muted">来源会话</span>{' '}
                <button className="btn ghost sm" type="button" style={{ padding: '0 6px' }} onClick={() => openConversation(detail.conv_id)}>
                  💬 {detail.conv_id}
                </button>
                <span className="muted" style={{ marginLeft: 6 }}>（会话编排调用 Agent，链路双向共用）</span>
              </div>
            )}
          </div>

          <div className="block" style={{ marginBottom: 14 }}>
            <div className="muted" style={{ marginBottom: 4 }}>输入</div>
            {/* 任务输入可能是多行 Markdown 需求描述：默认按原文展示，可一键切渲染 */}
            <MdBlock text={detail.input} fallback="（无输入）" defaultView="raw" />
          </div>

          {detail.result && (
            <div className="block" style={{ marginBottom: 14, borderColor: 'var(--ok)' }}>
              <div className="muted" style={{ marginBottom: 4 }}>最终答案</div>
              {/* 最终答案多为 Markdown 文档：默认解析渲染，可切回原文 */}
              <MdBlock text={detail.result} defaultView="md" />
            </div>
          )}
          {detail.error_msg && <Alert type="error">{detail.error_msg}</Alert>}

          {/* Trace 时间线：执行事件与操作历史（memory/RAG/GraphRAG/MCP/技能）按时间合并 */}
          {(() => {
            const storeTags = [
              { name: 'MySQL', desc: '任务 · 事件 · 调用记录 · 长期记忆', on: true },
              { name: 'Redis', desc: '队列 · 状态缓存 · 短期记忆', on: true },
              { name: 'Milvus', desc: '向量 + 图检索', on: histories.some((h) => h.kind === 'rag_recall' || h.kind === 'graph_recall') },
              { name: 'MCP', desc: '外部工具', on: histories.some((h) => h.kind === 'mcp_call') },
            ];
            const merged = [
              ...events.map((e) => ({ t: new Date(e.created_at).getTime() || 0, seq: e.id ?? 0, kind: 'event', e })),
              ...histories.map((h) => ({ t: new Date(h.created_at).getTime() || 0, seq: h.id ?? 0, kind: 'history', h })),
            ].sort((a, b) => a.t - b.t || a.seq - b.seq);
            return (
              <>
                <div className="section-title" style={{ margin: '16px 0 8px', display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
                  <span>Trace 链路（{events.length} 条事件 · {histories.length} 条操作）</span>
                  <span style={{ display: 'inline-flex', gap: 6, flexWrap: 'wrap' }}>
                    {storeTags.filter((s) => s.on).map((s) => (
                      <span key={s.name} className="store-tag" title={s.desc}>
                        <b>{s.name}</b> {s.desc}
                      </span>
                    ))}
                  </span>
                </div>
                {merged.length === 0 ? (
                  <div className="muted">排队中，worker 尚未开始执行…</div>
                ) : (
                  <div className="trace-line" style={{ marginBottom: 8 }}>
                    {merged.map((item) => {
                      if (item.kind === 'history') {
                        const h = item.h;
                        const meta = HISTORY_KIND_META[h.kind] || { label: h.kind, tone: 'default', store: '—' };
                        return (
                          <div className={`trace-node${h.ok ? '' : ' is-error'}`} key={`h-${h.id}`}>
                            <div style={{ display: 'flex', gap: 8, alignItems: 'baseline', flexWrap: 'wrap' }}>
                              <Badge tone={meta.tone}>{meta.label}</Badge>
                              <Badge tone="default" style={{ fontSize: 11 }}>📡 {meta.store}</Badge>
                              <span className="muted mono" style={{ fontSize: 12 }}>{formatTime(h.created_at)}</span>
                              <span className="muted mono" style={{ fontSize: 12 }}>{h.latency_ms ?? 0}ms</span>
                              {!h.ok && <Badge tone="error">失败</Badge>}
                            </div>
                            <div style={{ marginTop: 4, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
                              <span className="mono">{h.ref || '—'}</span>
                              {h.query && <div className="cell-sub ellipsis" title={h.query}>{h.query}</div>}
                              {h.summary && <div className="cell-sub ellipsis" title={h.summary}>{h.summary}</div>}
                            </div>
                            {h.detail && (
                              <details style={{ marginTop: 4 }}>
                                <summary className="muted" style={{ cursor: 'pointer', fontSize: 12 }}>命中明细</summary>
                                <pre className="block mono" style={{ marginTop: 4, whiteSpace: 'pre-wrap' }}>{prettyPayload(h.detail)}</pre>
                              </details>
                            )}
                          </div>
                        );
                      }
                      const e = item.e;
                      let childId = null;
                      if (e.type === 'delegate') {
                        try { childId = JSON.parse(e.payload || '{}').child_task_id; } catch { /* ignore */ }
                      }
                      const isError = e.type === 'error' || (e.type === 'tool_result' && (() => { try { return !!JSON.parse(e.payload || '{}').is_error; } catch { return false; } })());
                      return (
                        <div className={`trace-node${isError ? ' is-error' : ''}${e.type === 'final' ? ' is-final' : ''}`} key={`e-${e.id}`}>
                          <div style={{ display: 'flex', gap: 8, alignItems: 'baseline', flexWrap: 'wrap' }}>
                            <Badge tone={EVENT_TONES[e.type] || 'default'}>{EVENT_LABELS[e.type] || e.type}</Badge>
                            {e.turn > 0 && <span className="muted mono" style={{ fontSize: 12 }}>turn {e.turn ?? 0}</span>}
                            <span className="muted mono" style={{ fontSize: 12 }}>{formatTime(e.created_at)}</span>
                            {childId && onOpenTask ? (
                              <button className="btn ghost sm" type="button" onClick={() => onOpenTask(childId)}>
                                查看子任务 #{childId} →
                              </button>
                            ) : null}
                          </div>
                          {/* final 事件的答案通常是完整 Markdown 文档：默认渲染，可切原文；其余事件保持纯文本摘要 */}
                          {e.type === 'final' ? (
                            <MdBlock text={eventSummary(e.type, e.payload)} defaultView="md" />
                          ) : (
                            <div style={{ marginTop: 4, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{eventSummary(e.type, e.payload)}</div>
                          )}
                          {['tool_result', 'delegate', 'plan', 'orchestrate', 'evaluate', 'reflect', 'route', 'node', 'supervise', 'risk_assess'].includes(e.type) && (
                            <details style={{ marginTop: 4 }}>
                              <summary className="muted" style={{ cursor: 'pointer', fontSize: 12 }}>原始 payload</summary>
                              <pre className="block mono" style={{ marginTop: 4, whiteSpace: 'pre-wrap' }}>{prettyPayload(e.payload)}</pre>
                            </details>
                          )}
                        </div>
                      );
                    })}
                  </div>
                )}
              </>
            );
          })()}

          {/* LLM 调用记录 */}
          {calls.length > 0 && (
            <>
              <div className="section-title" style={{ margin: '16px 0 8px' }}>
                LLM 调用记录（{calls.length} 次 · 输入 {fmtTokens(calls.reduce((s, c) => s + (c.prompt_tokens ?? 0), 0))} tok · 输出 {fmtTokens(calls.reduce((s, c) => s + (c.completion_tokens ?? 0), 0))} tok）
              </div>
              <div className="table-wrap" style={{ marginBottom: 8 }}>
                <LLMCallsTable calls={calls} />
              </div>
            </>
          )}

          {/* 关联调用历史：MCP 调用 / 技能使用 / 记忆召回 / RAG / GraphRAG 召回 */}
          {histories.length > 0 && (
            <>
              <div className="section-title" style={{ margin: '16px 0 8px' }}>
                关联调用历史（{histories.length} 条）
              </div>
              <div className="table-wrap" style={{ marginBottom: 8 }}>
                <table className="table">
                  <thead>
                    <tr>
                      <th>时间</th>
                      <th>类型</th>
                      <th>对象</th>
                      <th>输入 / 结果</th>
                      <th className="num">耗时</th>
                      <th>状态</th>
                    </tr>
                  </thead>
                  <tbody>
                    {histories.map((h) => {
                      const meta = HISTORY_KIND_META[h.kind] || { label: h.kind, tone: 'default' };
                      return (
                        <tr key={h.id}>
                          <td className="mono cell-sub">{formatTime(h.created_at)}</td>
                          <td>
                            <Badge tone={meta.tone}>{meta.label}</Badge>
                            {h.agent_key && h.agent_key !== detail.agent_key && (
                              <div className="cell-sub mono">{h.agent_key}</div>
                            )}
                          </td>
                          <td className="mono">{h.ref || '—'}</td>
                          <td>
                            {h.query && (
                              <div className="cell-sub ellipsis" title={h.query} style={{ maxWidth: 340 }}>
                                {h.query}
                              </div>
                            )}
                            <div className="cell-sub ellipsis" title={h.summary} style={{ maxWidth: 340 }}>
                              {h.summary || '—'}
                            </div>
                          </td>
                          <td className="num mono">{h.latency_ms ?? 0}ms</td>
                          <td>{h.ok ? <Badge tone="ok">成功</Badge> : <Badge tone="error">失败</Badge>}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </>
          )}

          {/* 执行链路（OpenTelemetry span 树）：一次执行一条 Trace，覆盖 task / LLM / MCP / Skill / RAG */}
          {trace && (
            <>
              <div className="section-title" style={{ margin: '16px 0 8px', display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <span>执行链路（{trace.spans?.length || 0} 个 span{trace.running ? ' · 执行中' : ''}）</span>
                <span className="muted mono" style={{ fontSize: 11 }} title="按 trace_id 可在 Jaeger/Tempo 里查同一条链路">
                  {trace.trace_id}
                </span>
              </div>
              {trace.spans?.length > 0 ? (
                <div className="block" style={{ padding: '6px 10px', marginBottom: 8 }}>
                  {renderSpanNodes(buildSpanTree(trace.spans))}
                </div>
              ) : (
                <div className="muted" style={{ marginBottom: 8, fontSize: 13 }}>
                  根节点 task.run 已开始，span 尚未结束（结束后出现在这里）…
                </div>
              )}
            </>
          )}

          {/* 单任务成本：usage_records 里按 task_id 归集的账单（LLM 调用自动记账） */}
          {costRows && costRows.length > 0 && (() => {
            const totalCost = costRows.reduce((s, r) => s + (r.cost_micros ?? 0), 0);
            const totalIn = costRows.reduce((s, r) => s + (r.input_tokens ?? 0), 0);
            const totalOut = costRows.reduce((s, r) => s + (r.output_tokens ?? 0), 0);
            const calls = costRows.reduce((s, r) => s + (r.calls ?? 0), 0);
            return (
              <>
                <div className="section-title" style={{ margin: '16px 0 8px' }}>
                  成本（{fmtMicros(totalCost)} · {calls} 次调用 · 输入 {fmtTokens(totalIn)} tok · 输出 {fmtTokens(totalOut)} tok）
                </div>
                <div className="table-wrap">
                  <table className="table">
                    <thead>
                      <tr>
                        <th>成本来源</th>
                        <th className="num">调用</th>
                        <th className="num">输入 tok</th>
                        <th className="num">输出 tok</th>
                        <th className="num">成本</th>
                      </tr>
                    </thead>
                    <tbody>
                      {costRows.map((r) => (
                        <tr key={r.key || 'all'}>
                          <td><Badge tone="default">{r.key || 'llm'}</Badge></td>
                          <td className="num mono">{r.calls ?? 0}</td>
                          <td className="num mono">{fmtTokens(r.input_tokens ?? 0)}</td>
                          <td className="num mono">{fmtTokens(r.output_tokens ?? 0)}</td>
                          <td className="num mono">{fmtMicros(r.cost_micros)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </>
            );
          })()}
        </div>
      </div>
    </div>
  );
}
