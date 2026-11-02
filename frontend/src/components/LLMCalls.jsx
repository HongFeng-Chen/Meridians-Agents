import { useState } from 'react';
import { Badge, FormDrawer } from './UI.jsx';
import { Markdown } from './Markdown.jsx';

// LLM 调用记录表格：任务详情面板与 LLM 页共用。
// proto3 零值字段在 JSON 里省略，数字一律 ?? 0 兜底。
// 传入 onDetail(call) 时表格末尾出现「详情」按钮，弹层展示输入/输出全文。

export function fmtTokens(n) {
  const v = n ?? 0;
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`;
  if (v >= 10_000) return `${(v / 1000).toFixed(1)}k`;
  return String(v);
}

export function fmtDuration(ms) {
  const v = ms ?? 0;
  if (v >= 1000) return `${(v / 1000).toFixed(1)}s`;
  return `${v}ms`;
}

const PURPOSE_LABELS = {
  'agentfactory.react': 'Agent 执行图',
  'agentfactory.simple': 'Agent 单轮',
  'agentfactory.factory': 'LLM 造 Agent',
  'llm.test': '连通测试',
};

export function purposeLabel(p) {
  return PURPOSE_LABELS[p] || p || '业务调用';
}

export function LLMCallsTable({ calls, showTask = false, onDetail }) {
  if (!calls.length) return null;
  return (
    <table className="table">
      <thead>
        <tr>
          <th>时间</th>
          <th>模型</th>
          <th>数据源</th>
          <th>用途</th>
          {showTask && <th>任务</th>}
          <th style={{ textAlign: 'right' }}>输入 tok</th>
          <th style={{ textAlign: 'right' }}>输出 tok</th>
          <th style={{ textAlign: 'right' }}>总计</th>
          <th style={{ textAlign: 'right' }}>耗时</th>
          <th>状态</th>
          {onDetail && <th style={{ width: 56 }}>详情</th>}
        </tr>
      </thead>
      <tbody>
        {calls.map((c) => {
          const ok = c.ok ?? false;
          return (
            <tr key={c.id}>
              <td className="muted mono" style={{ fontSize: 12, whiteSpace: 'nowrap' }}>
                {(c.created_at || '').replace('T', ' ').slice(5, 19)}
              </td>
              <td className="mono">{c.model || '—'}</td>
              <td>{c.provider === 'fallback' ? <Badge tone="blue">fallback</Badge> : c.provider}</td>
              <td className="muted">{purposeLabel(c.purpose)}</td>
              {showTask && (
                <td className="mono">
                  {(c.task_id ?? 0) > 0 ? (
                    <a href="#/tasks" style={{ color: 'var(--accent)' }}>#{c.task_id}</a>
                  ) : '—'}
                </td>
              )}
              <td className="mono" style={{ textAlign: 'right' }}>{fmtTokens(c.prompt_tokens)}</td>
              <td className="mono" style={{ textAlign: 'right' }}>{fmtTokens(c.completion_tokens)}</td>
              <td className="mono" style={{ textAlign: 'right', fontWeight: 600 }}>{fmtTokens(c.total_tokens)}</td>
              <td className="mono muted" style={{ textAlign: 'right' }}>{fmtDuration(c.duration_ms)}</td>
              <td>
                {ok ? (
                  <Badge tone="teal">成功</Badge>
                ) : (
                  <span title={c.error_msg}><Badge tone="error">失败</Badge></span>
                )}
              </td>
              {onDetail && (
                <td>
                  <button className="btn ghost sm" type="button" onClick={() => onDetail(c)}>详情</button>
                </td>
              )}
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

// LLMCallDetail 调用详情弹层：全部字段 + 输入/输出全文（默认 Markdown 渲染，可切回原文）
function IOBlock({ label, text, chars, truncated, fallback, view, onView }) {
  return (
    <div className="call-detail-block">
      <div className="call-detail-label">
        {label}
        <span className="muted" style={{ marginLeft: 8, fontWeight: 400 }}>
          原文 {chars ?? 0} 字符{truncated ? ' · 已截断' : ''}
        </span>
        <span className="io-view-toggle" role="group" aria-label={`${label}视图切换`}>
          <button type="button" className={view === 'md' ? 'active' : ''} onClick={() => onView('md')}>渲染</button>
          <button type="button" className={view === 'raw' ? 'active' : ''} onClick={() => onView('raw')}>原文</button>
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

export function LLMCallDetail({ call, onClose }) {
  const [view, setView] = useState({ input: 'md', output: 'md' });
  if (!call) return null;
  const ok = call.ok ?? false;
  // 后端截断标记（truncateText 追加的省略尾注）
  const isTruncated = (t) => !!t && t.includes('（已截断');
  const info = [
    ['时间', (call.created_at || '').replace('T', ' ').slice(0, 19)],
    ['数据源', call.provider === 'fallback' ? 'fallback（配置兜底）' : call.provider || '—'],
    ['模型', call.model || '—'],
    ['用途', purposeLabel(call.purpose)],
    ['任务', (call.task_id ?? 0) > 0 ? `#${call.task_id}` : '—'],
    ['Agent', call.agent_key || '—'],
    ['温度', call.temperature != null ? String(call.temperature) : '—'],
    ['耗时', fmtDuration(call.duration_ms)],
    ['输入 tokens', String(call.prompt_tokens ?? 0)],
    ['输出 tokens', String(call.completion_tokens ?? 0)],
    ['总计 tokens', String(call.total_tokens ?? 0)],
    ['状态', ok ? '成功' : '失败'],
  ];
  return (
    <FormDrawer title={`调用详情 #${call.id}`} desc={`${call.model || ''} · ${ok ? '成功' : '失败'}`} onClose={onClose} width="min(920px, 94vw)">
      <div className="call-detail">
        <div className="call-detail-grid">
          {info.map(([k, v]) => (
            <div key={k} className="call-detail-item">
              <span className="muted">{k}</span>
              <span className="mono" style={k === '状态' ? { fontWeight: 600, color: ok ? '#16a34a' : '#dc2626' } : undefined}>{v}</span>
            </div>
          ))}
        </div>
        {!ok && call.error_msg && (
          <div className="call-detail-block is-error">
            <div className="call-detail-label">错误信息</div>
            <pre>{call.error_msg}</pre>
          </div>
        )}
        <IOBlock
          label="输入（input）"
          text={call.prompt_text}
          chars={call.prompt_chars}
          truncated={isTruncated(call.prompt_text)}
          fallback="（该记录早于详情功能，未落输入内容）"
          view={view.input}
          onView={(v) => setView((s) => ({ ...s, input: v }))}
        />
        <IOBlock
          label="输出（output）"
          text={call.reply_text}
          chars={call.reply_chars}
          truncated={isTruncated(call.reply_text)}
          fallback={ok ? '（空输出）' : '（调用失败，无输出）'}
          view={view.output}
          onView={(v) => setView((s) => ({ ...s, output: v }))}
        />
      </div>
    </FormDrawer>
  );
}
