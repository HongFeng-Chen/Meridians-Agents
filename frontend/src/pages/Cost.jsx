// 成本看板：/api/v2/usage/summary 的可视化。
//
// 数据侧口径（不要在这里二次加工，改口径去 biz/usage）：
//   - 只统计已写入 usage_records 的调用；LLM 之外的成本（embedding / mcp / skill）
//     目前要靠手动上报，所以「单任务成本」等于「已上报部分」，不是全量账单。
//   - cost_micros 是整数微单位（1 元 = 1e6），前端只在展示层换算，不参与累加。
//   - 租户隔离由服务端按调用方身份强制（p.TenantID），前端不传 tenant 参数。
import { useCallback, useEffect, useMemo, useState } from 'react';
import { v2 } from '../api.js';
import { Panel, StatCard, Alert, EmptyState, Spinner, Badge } from '../components/UI.jsx';
import { IconClock, IconSpark } from '../components/Icons.jsx';

// 分组维度：与服务端 groupExpr 白名单一一对应（前端多传会被拒，少传则报未知分组）
const GROUPS = [
  { key: 'day', label: '按天', note: '每天一条，适合看趋势与突增' },
  { key: 'agent', label: '按 Agent', note: '谁在花钱；空值表示会话直连或未标注 agent' },
  { key: 'model', label: '按模型', note: '模型维度的单价差异' },
  { key: 'kind', label: '按来源', note: 'llm / embedding / rerank / mcp / skill' },
  { key: 'task', label: '按任务', note: '异步任务成本；空值表示非任务链路' },
  { key: 'ref', label: '按归属', note: 'harness_run:<id> 这类跨任务的业务归属' },
];

const KINDS = [
  { key: '', label: '全部来源' },
  { key: 'llm', label: 'LLM' },
  { key: 'embedding', label: 'Embedding' },
  { key: 'rerank', label: 'Rerank' },
  { key: 'mcp', label: 'MCP' },
  { key: 'skill', label: 'Skill' },
  { key: 'worker', label: 'Worker' },
];

const KIND_LABELS = Object.fromEntries(KINDS.filter((k) => k.key).map((k) => [k.key, k.label]));

const WINDOWS = [
  { key: 'today', label: '今天' },
  { key: '7d', label: '近 7 天' },
  { key: '30d', label: '近 30 天' },
  { key: 'month', label: '本月' },
];

/** 本地自然日边界 → 绝对值时刻（until 是开区间，取次日零点） */
function windowRange(key) {
  const now = new Date();
  const dayStart = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const end = dayStart(now);
  end.setDate(end.getDate() + 1);
  let start;
  if (key === 'today') start = dayStart(now);
  else if (key === '7d') {
    start = dayStart(now);
    start.setDate(start.getDate() - 6);
  } else if (key === '30d') {
    start = dayStart(now);
    start.setDate(start.getDate() - 29);
  } else {
    start = new Date(now.getFullYear(), now.getMonth(), 1);
    end.setTime(new Date(now.getFullYear(), now.getMonth() + 1, 1).getTime());
  }
  return { since: start.toISOString(), until: end.toISOString() };
}

/** 微元 → 元。token 级成本常小于 1 分，固定 4 位小数便于横向比较 */
function fmtCost(micros) {
  const v = Number(micros || 0) / 1_000_000;
  if (!v) return '¥0.0000';
  if (v < 0.0001) return '¥<0.0001';
  return `¥${v.toFixed(4)}`;
}

function fmtInt(n) {
  return Number(n || 0).toLocaleString('zh-CN');
}

function fmtTokens(n) {
  const v = Number(n || 0);
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(2)}M`;
  if (v >= 1_000) return `${(v / 1_000).toFixed(1)}k`;
  return String(v);
}

/** 分组键渲染：空的 agent_id / task_id 是真实语义（未标注），不要写成 0 */
function groupKeyCell(key, groupBy) {
  if (!key) return <span className="cost-empty">未归属</span>;
  if (groupBy === 'kind') {
    return <Badge tone={key === 'llm' ? 'indigo' : 'default'}>{KIND_LABELS[key] || key}</Badge>;
  }
  if (groupBy === 'task') return <span className="cost-mono">#{key}</span>;
  if (groupBy === 'ref') {
    const i = key.indexOf(':');
    if (i > 0) {
      return (
        <span className="cost-ref">
          <span className="cost-mono">{key.slice(0, i)}</span>
          <span className="cost-ref-id">{key.slice(i + 1)}</span>
        </span>
      );
    }
  }
  return <span className="cost-mono">{key}</span>;
}

export default function Cost() {
  const [windowKey, setWindowKey] = useState('7d');
  const [groupBy, setGroupBy] = useState('day');
  const [kind, setKind] = useState('');
  const [buckets, setBuckets] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const range = useMemo(() => windowRange(windowKey), [windowKey]);

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const res = await v2.usageSummary({
        group_by: groupBy,
        kind,
        since: range.since,
        until: range.until,
      });
      setBuckets(res?.buckets || []);
    } catch (err) {
      // 403 是权限问题（非管理员 / 非本租户），不要当成「没有数据」
      setError(
        err?.status === 403
          ? '没有权限查看该租户的成本：需要管理员身份，或确认登录身份属于同一租户。'
          : err?.message || '加载失败',
      );
      setBuckets([]);
    } finally {
      setLoading(false);
    }
  }, [groupBy, kind, range.since, range.until]);

  useEffect(() => {
    load();
  }, [load]);

  const totals = useMemo(
    () =>
      buckets.reduce(
        (acc, b) => ({
          cost: acc.cost + Number(b.cost_micros || 0),
          tokens: acc.tokens + Number(b.total_tokens || 0),
          calls: acc.calls + Number(b.calls || 0),
        }),
        { cost: 0, tokens: 0, calls: 0 },
      ),
    [buckets],
  );

  const maxCost = useMemo(
    () => buckets.reduce((m, b) => Math.max(m, Number(b.cost_micros || 0)), 0),
    [buckets],
  );

  const groupMeta = GROUPS.find((g) => g.key === groupBy);
  // 按成本降序是通用口径；只有「按天」按时间正序——日期乱序看不出趋势
  const sorted = useMemo(() => {
    const rows = [...buckets].sort(
      (a, b) => Number(b.cost_micros || 0) - Number(a.cost_micros || 0),
    );
    if (groupBy === 'day') rows.sort((a, b) => String(a.key).localeCompare(String(b.key)));
    return rows;
  }, [buckets, groupBy]);

  return (
    <div className="cost-page">
      <div className="cost-toolbar">
        <div className="seg">
          {WINDOWS.map((w) => (
            <button
              key={w.key}
              type="button"
              className={`seg-item${windowKey === w.key ? ' active' : ''}`}
              onClick={() => setWindowKey(w.key)}
            >
              {w.label}
            </button>
          ))}
        </div>
        <div className="cost-spacer" />
        <div className="field cost-inline">
          <span className="field-label">来源</span>
          <select className="input sm" value={kind} onChange={(e) => setKind(e.target.value)}>
            {KINDS.map((k) => (
              <option key={k.key || 'all'} value={k.key}>
                {k.label}
              </option>
            ))}
          </select>
        </div>
        <button className="btn ghost sm" type="button" onClick={load} disabled={loading}>
          重新汇总
        </button>
      </div>

      {error ? <Alert type="error">{error}</Alert> : null}

      <div className="stat-grid">
        <StatCard label="总成本" value={fmtCost(totals.cost)} hint="已上报部分（LLM 之外靠主动上报）" tone="indigo" />
        <StatCard label="总 Tokens" value={fmtTokens(totals.tokens)} hint={`${fmtInt(totals.tokens)} tokens`} />
        <StatCard label="调用次数" value={fmtInt(totals.calls)} hint="一次真实请求 = 一条用量记录" />
        <StatCard label="分组数" value={fmtInt(buckets.length)} hint={groupMeta?.note || ''} />
      </div>

      <Panel
        title="成本分布"
        desc={groupMeta ? `${groupMeta.label}${kind ? ` · ${KIND_LABELS[kind] || kind}` : ''}` : ''}
        actions={
          <div className="seg">
            {GROUPS.map((g) => (
              <button
                key={g.key}
                type="button"
                className={`seg-item${groupBy === g.key ? ' active' : ''}`}
                onClick={() => setGroupBy(g.key)}
              >
                {g.label}
              </button>
            ))}
          </div>
        }
      >
        {loading ? (
          <Spinner text="汇总中…" />
        ) : buckets.length === 0 ? (
          <EmptyState
            icon={IconSpark}
            title="这个时间窗内没有用量记录"
            desc="成本中心只统计已写入的记录。LLM 调用自动记账；embedding / MCP / Skill 需要调用 POST /api/v2/usage/records 上报。"
          />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th style={{ width: 240 }}>{groupMeta?.label.replace('按', '') || '分组'}</th>
                  <th className="num" style={{ width: 80 }}>
                    调用
                  </th>
                  <th className="num" style={{ width: 110 }}>
                    输入
                  </th>
                  <th className="num" style={{ width: 110 }}>
                    输出
                  </th>
                  <th className="num" style={{ width: 120 }}>
                    成本
                  </th>
                  <th style={{ width: 200 }}>占比</th>
                </tr>
              </thead>
              <tbody>
                {sorted.map((b) => {
                  const cost = Number(b.cost_micros || 0);
                  const pct = totals.cost > 0 ? (cost / totals.cost) * 100 : 0;
                  const barW = maxCost > 0 ? (cost / maxCost) * 100 : 0;
                  return (
                    <tr key={b.key || '__empty'}>
                      <td>{groupKeyCell(b.key, groupBy)}</td>
                      <td className="num">{fmtInt(b.calls)}</td>
                      <td className="num cost-dim">{fmtTokens(b.input_tokens)}</td>
                      <td className="num cost-dim">{fmtTokens(b.output_tokens)}</td>
                      <td className="num cost-value">{fmtCost(cost)}</td>
                      <td>
                        <div className="cost-bar">
                          <div className="cost-bar-fill" style={{ width: `${Math.max(barW, 1.5)}%` }} />
                          <span className="cost-bar-text">{pct.toFixed(1)}%</span>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      <Panel title="口径与已知边界" desc="看数字之前先确认这三条，避免把局部当全量">
        <ul className="cost-notes">
          <li>
            <strong>只覆盖已上报成本</strong>：LLM 由调用链路自动记账；embedding / rerank / MCP / Skill 仍需手动上报，
            所以这里的合计通常小于云厂商账单。
          </li>
          <li>
            <strong>异步任务可能缺归属</strong>：<code>ref</code> 挂在调用链 context 上，队列异步取起的任务拿不到，
            表现为「按归属」里出现空键（任务详情仍能按 task_id 查到）。
          </li>
          <li>
            <strong>历史不重算</strong>：计价规则版本随记录落库（<code>price_version</code>），模型调价只影响新记录，
            不回写历史账单。
          </li>
        </ul>
      </Panel>

      <div className="cost-foot">
        <IconClock size={14} />
        <span>
          时间窗（本地时区）：{new Date(range.since).toLocaleString('zh-CN', { hour12: false })} —{' '}
          {new Date(new Date(range.until).getTime() - 1).toLocaleString('zh-CN', { hour12: false })}
        </span>
      </div>
    </div>
  );
}
