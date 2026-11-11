import { useEffect, useMemo, useState } from 'react';
import { api, agentfactory, llm, mcp } from '../api.js';
import { Panel, StatCard, EmptyState, Badge, Alert } from '../components/UI.jsx';
import { LLMCallsTable, fmtTokens } from '../components/LLMCalls.jsx';
import { IconArrowRight, IconDatabase, IconDoc, IconSearch, IconUpload, IconBrain, IconSpark, IconLink } from '../components/Icons.jsx';

// 仪表盘：知识库规模 + Agent 平台运行指标 + 最近动态。
// 数据来自六个域的统计接口，Promise.allSettled 并行拉取，单域失败不影响整体。

const STATUS_TONES = { succeeded: 'ok', running: 'indigo', pending: 'warn', paused: 'warn', retry_wait: 'warn', failed: 'error', canceled: 'default', timeout: 'error' };
const STATUS_LABELS = { pending: '排队', running: '执行中', paused: '待审批', retry_wait: '等待重试', succeeded: '成功', failed: '失败', canceled: '取消', timeout: '超时' };

function formatTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? v : d.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false });
}

export default function Dashboard({ onNavigate }) {
  const [rag, setRag] = useState(null); // 知识库 stats（chunks/docs/entities/relations/环境信息）
  const [docs, setDocs] = useState([]);
  const [taskStats, setTaskStats] = useState(null);
  const [tasks, setTasks] = useState([]);
  const [agents, setAgents] = useState([]);
  const [llmStats, setLlmStats] = useState(null);
  const [llmCallStats, setLlmCallStats] = useState(null);
  const [llmCalls, setLlmCalls] = useState([]);
  const [mcpServers, setMcpServers] = useState([]);
  const [error, setError] = useState('');

  useEffect(() => {
    let alive = true;
    (async () => {
      const [s, d, ts, tl, ag, ls, lcs, lc, ms] = await Promise.allSettled([
        api.stats(),
        api.docs(),
        agentfactory.getTaskStats(),
        agentfactory.listTasks({ root_only: true, limit: 6 }),
        agentfactory.listAgentDefs({ limit: 200 }),
        llm.getLLMStats(),
        llm.getLLMCallStats({}),
        llm.listLLMCalls({ limit: 500 }),
        mcp.listMCPServers({ limit: 100 }),
      ]);
      if (!alive) return;
      if (s.status === 'fulfilled') setRag(s.value);
      if (d.status === 'fulfilled') setDocs(d.value.docs || []);
      if (ts.status === 'fulfilled') setTaskStats(ts.value);
      if (tl.status === 'fulfilled') setTasks(tl.value.tasks || []);
      if (ag.status === 'fulfilled') setAgents(ag.value.agents || []);
      if (ls.status === 'fulfilled') setLlmStats(ls.value);
      if (lcs.status === 'fulfilled') setLlmCallStats(lcs.value);
      if (lc.status === 'fulfilled') setLlmCalls(lc.value.calls || []);
      if (ms.status === 'fulfilled') setMcpServers(ms.value.servers || []);
      const failed = [s, d, ts, tl, ag, ls, lcs, lc, ms].filter((r) => r.status === 'rejected');
      if (failed.length) setError(failed.map((f) => f.reason?.message || '加载失败').join('；'));
    })();
    return () => {
      alive = false;
    };
  }, []);

  const totalTools = mcpServers.reduce((n, s) => n + (s.tool_count ?? 0), 0);
  // 在线 = 最近 10 分钟内有探活心跳
  const mcpOnline = mcpServers.filter((s) => {
    const ping = s.status?.last_ping_at ? new Date(s.status.last_ping_at).getTime() : 0;
    return ping > 0 && Date.now() - ping < 10 * 60 * 1000;
  }).length;
  const agentLlm = agents.filter((a) => a.source === 'llm').length;
  const agentEnabled = agents.filter((a) => a.enabled).length;
  const taskDone = (taskStats?.succeeded ?? 0) + (taskStats?.failed ?? 0) + (taskStats?.canceled ?? 0) + (taskStats?.timeout ?? 0);
  const failRate = taskDone ? `${Math.round(((taskStats?.failed ?? 0) / taskDone) * 100)}% 失败` : '暂无终态';
  // 用量趋势：把调用记录按时间分桶（跨度 >48h 按天，否则按小时）
  // —— 用量趋势筛选：模型多选 + 时间范围 ——
  const [trendModels, setTrendModels] = useState([]); // 选中的模型（空 = 全部）
  const [rangeKey, setRangeKey] = useState('all'); // 24h / 3d / 7d / 30d / all / custom
  const [customSince, setCustomSince] = useState('');
  const [customUntil, setCustomUntil] = useState('');
  const [trendCalls, setTrendCalls] = useState([]);
  const [trendLoading, setTrendLoading] = useState(true);

  const modelOptions = useMemo(() => (llmCallStats?.by_model || []).map((m) => m.model).filter(Boolean), [llmCallStats]);

  // 计算时间范围（RFC3339；since/until 空 = 不限；输入不完整时跳过该端）
  const range = useMemo(() => {
    const toISO = (v) => {
      if (!v) return undefined;
      const d = new Date(v);
      return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
    };
    if (rangeKey === 'custom') {
      return { since: toISO(customSince), until: toISO(customUntil) };
    }
    const spans = { '24h': 24 * 3600e3, '3d': 3 * 86400e3, '7d': 7 * 86400e3, '30d': 30 * 86400e3 };
    const ms = spans[rangeKey];
    return ms ? { since: new Date(Date.now() - ms).toISOString() } : {};
  }, [rangeKey, customSince, customUntil]);
  const rangeText = rangeKey === 'custom' ? `${customSince || '…'} ~ ${customUntil || '…'}` : ({ '24h': '近 24 小时', '3d': '近 3 天', '7d': '近 7 天', '30d': '近 30 天', all: '全部时间' })[rangeKey];

  // 筛选条件变化 → 服务端拉取对应范围的调用记录
  useEffect(() => {
    let alive = true;
    setTrendLoading(true);
    llm
      .listLLMCalls({
        models: trendModels.length ? trendModels : undefined,
        since: range.since,
        until: range.until,
        limit: 1000,
      })
      .then((res) => {
        if (alive) setTrendCalls(res.calls || []);
      })
      .catch(() => {
        if (alive) setTrendCalls([]);
      })
      .finally(() => {
        if (alive) setTrendLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [trendModels, range.since, range.until]);

  const usage = buildUsageBuckets(trendCalls);

  const quickActions = [
    { key: 'kb-assets', hash: 'knowledge?tab=assets', title: '实时库', desc: '一份文档 · 向量 + 图谱双索引', icon: IconDatabase },
    { key: 'kb-ingest', hash: 'knowledge?tab=ingest', title: '接入数据', desc: '向量 / 图谱 / 双索引可选', icon: IconUpload },
    { key: 'kb-ask', hash: 'knowledge?tab=ask', title: '检索问答', desc: 'RAG 与 GraphRAG 双路对比', icon: IconSearch },
    { key: 'agents', title: 'Agent 工厂', desc: '低代码定义 + LLM 元编程', icon: IconBrain },
    { key: 'tasks', title: '任务中心', desc: '异步执行 · 链路追踪', icon: IconSpark },
    { key: 'llm', title: '模型数据源', desc: '多源路由 · 调用历史', icon: IconDoc },
    { key: 'mcp', title: 'MCP 工具', desc: '外部工具接入与同步', icon: IconLink },
  ];

  return (
    <div className="stack">
      {error && <Alert type="error">部分指标加载失败：{error}</Alert>}

      {/* 知识库规模 */}
      <div className="stat-grid">
        <StatCard label="向量分块" value={rag?.chunks ?? 0} hint="Milvus 主集合实体数" tone="indigo" />
        <StatCard label="已入库文档" value={rag?.docs ?? 0} hint="按 doc_id 聚合" tone="blue" />
        <StatCard label="图谱实体" value={rag?.entities ?? 0} hint="实体集合记录数" tone="teal" />
        <StatCard label="图谱关系" value={rag?.relations ?? 0} hint={`扩展跳数 ${rag?.graph_hops ?? '—'}`} tone="violet" />
      </div>

      {/* Agent 平台运行 */}
      <div className="stat-grid">
        <StatCard
          label="Agent"
          value={agents.length}
          hint={`启用 ${agentEnabled} · LLM 自造 ${agentLlm}`}
          tone="violet"
        />
        <StatCard
          label="任务总数"
          value={taskStats?.tasks ?? 0}
          hint={`执行中 ${taskStats?.running ?? 0} · 排队 ${taskStats?.pending ?? 0} · ${failRate}`}
          tone="indigo"
        />
        <StatCard
          label="MCP 服务"
          value={mcpServers.length}
          hint={`在线 ${mcpOnline} · 工具 ${totalTools} 个`}
          tone="teal"
        />
        <StatCard
          label="数据源"
          value={llmStats?.providers ?? 0}
          hint={`启用 ${llmStats?.enabled ?? 0} · 默认 ${llmStats?.default || '—'}`}
          tone="blue"
        />
      </div>

      <Panel title="快捷入口" desc="实时库里同一批文档可建向量与图谱两种索引；平台能力覆盖 Agent / 任务 / 模型 / 工具">
        <div className="action-grid">
          {quickActions.map((a) => {
            const Icon = a.icon;
            return (
              <button
                key={a.key}
                className="action-card"
                type="button"
                onClick={() => (a.hash ? (window.location.hash = `#/${a.hash}`) : onNavigate(a.key))}
              >
                <span className="action-icon">
                  <Icon size={18} />
                </span>
                <span className="action-text">
                  <strong>{a.title}</strong>
                  <small>{a.desc}</small>
                </span>
                <IconArrowRight size={16} className="action-arrow" />
              </button>
            );
          })}
        </div>
      </Panel>

      {/* LLM 用量趋势（时间分桶折线图：每个模型一种颜色，实线=输入 / 虚线=输出；支持模型多选与自定义时间范围） */}
      <Panel
        title="LLM 用量趋势"
        desc={`${rangeText} · 按${usage?.grain === 'hour' ? '小时' : '天'}聚合 ${trendCalls.length} 次调用的 token 消耗${trendCalls.length >= 1000 ? '（已达单次上限 1000 条）' : ''}`}
        actions={
          usage?.models?.length ? (
            <div style={{ display: 'flex', gap: 12, alignItems: 'center', flexWrap: 'wrap' }}>
              {usage.models.map((m) => (
                <span key={m} style={{ display: 'inline-flex', alignItems: 'center', gap: 5, fontSize: 12 }} className="mono">
                  <i style={{ width: 10, height: 10, borderRadius: 2, background: modelColorMap(usage.models)[m], display: 'inline-block' }} /> {m}
                </span>
              ))}
              <span className="muted" style={{ fontSize: 12 }}>实线=输入 · 虚线=输出</span>
            </div>
          ) : null
        }
      >
        {/* 筛选工具条：模型多选 chips + 时间范围 */}
        <div className="trend-toolbar">
          <div className="trend-chips">
            <button
              type="button"
              className={`chip${trendModels.length === 0 ? ' active' : ''}`}
              onClick={() => setTrendModels([])}
            >
              全部模型
            </button>
            {modelOptions.map((m) => {
              const on = trendModels.includes(m);
              return (
                <button
                  key={m}
                  type="button"
                  className={`chip mono${on ? ' active' : ''}`}
                  onClick={() => setTrendModels((prev) => (on ? prev.filter((x) => x !== m) : [...prev, m]))}
                >
                  {m}
                </button>
              );
            })}
          </div>
          <div className="trend-range">
            <select className="input sm" value={rangeKey} onChange={(e) => setRangeKey(e.target.value)}>
              <option value="all">全部时间</option>
              <option value="24h">近 24 小时</option>
              <option value="3d">近 3 天</option>
              <option value="7d">近 7 天</option>
              <option value="30d">近 30 天</option>
              <option value="custom">自定义</option>
            </select>
            {rangeKey === 'custom' && (
              <>
                <input type="datetime-local" className="input sm" value={customSince} onChange={(e) => setCustomSince(e.target.value)} />
                <span className="muted">至</span>
                <input type="datetime-local" className="input sm" value={customUntil} onChange={(e) => setCustomUntil(e.target.value)} />
              </>
            )}
          </div>
        </div>
        {trendLoading ? (
          <div className="muted" style={{ padding: '32px 0', textAlign: 'center' }}>加载中…</div>
        ) : (
          <UsageChart usage={usage} />
        )}
      </Panel>

      <div className="grid-2 align-start">
        {/* 最近任务 */}
        <Panel
          title="最近任务"
          desc="仅根任务；子任务与完整链路在任务中心查看"
          actions={
            <button className="btn ghost sm" type="button" onClick={() => onNavigate('tasks')}>
              任务中心 <IconArrowRight size={13} />
            </button>
          }
        >
          {tasks.length === 0 ? (
            <EmptyState icon={IconSpark} title="还没有任务" desc="到「Agent 工厂」试运行或提交一个任务" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th className="num">#</th>
                    <th>任务</th>
                    <th>Agent</th>
                    <th>状态</th>
                    <th>时间</th>
                  </tr>
                </thead>
                <tbody>
                  {tasks.map((t) => (
                    <tr key={t.id}>
                      <td className="num mono">{t.id}</td>
                      <td>
                        <div className="cell-main ellipsis" style={{ maxWidth: 260 }} title={t.title}>{t.title}</div>
                      </td>
                      <td className="mono">{t.agent_key}</td>
                      <td>
                        <Badge tone={STATUS_TONES[t.status] || 'default'}>{STATUS_LABELS[t.status] || t.status}</Badge>
                        {t.archived && <Badge tone="default" style={{ marginLeft: 4 }}>历史</Badge>}
                      </td>
                      <td className="cell-sub">{formatTime(t.created_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        {/* LLM 调用概览 */}
        <Panel
          title="LLM 调用概览"
          desc="每次数据源尝试一条记录，含 token 用量"
          actions={
            <button className="btn ghost sm" type="button" onClick={() => onNavigate('llm')}>
              调用历史 <IconArrowRight size={13} />
            </button>
          }
        >
          {llmCallStats && (
            <div className="kv" style={{ marginBottom: 12 }}>
              <div>
                <span className="muted">调用次数</span>{' '}
                <span className="mono">
                  {llmCallStats.calls ?? 0}
                  {(llmCallStats.failures ?? 0) > 0 && <span className="muted">（失败 {llmCallStats.failures}）</span>}
                </span>
              </div>
              <div><span className="muted">输入 tokens</span> <span className="mono">{fmtTokens(llmCallStats.prompt_tokens)}</span></div>
              <div><span className="muted">输出 tokens</span> <span className="mono">{fmtTokens(llmCallStats.completion_tokens)}</span></div>
              <div><span className="muted">总计</span> <span className="mono" style={{ fontWeight: 600 }}>{fmtTokens(llmCallStats.total_tokens)}</span></div>
            </div>
          )}
          {llmCallStats?.by_model?.length > 0 && (
            <div style={{ marginBottom: 12, display: 'flex', gap: 8, flexWrap: 'wrap' }}>
              {llmCallStats.by_model.map((m) => (
                <Badge key={m.model} tone="violet">
                  {m.model} · {m.calls} 次 · {fmtTokens(m.total_tokens ?? (m.prompt_tokens ?? 0) + (m.completion_tokens ?? 0))} tok
                </Badge>
              ))}
            </div>
          )}
          {llmCalls.length === 0 ? (
            <EmptyState title="暂无调用记录" desc="发起对话或提交 Agent 任务后这里会出现调用明细" />
          ) : (
            <div className="table-wrap">
              <LLMCallsTable calls={llmCalls.slice(0, 6)} showTask />
            </div>
          )}
        </Panel>
      </div>

      <div className="grid-2 align-start">
        {/* 最近入库文档 */}
        <Panel title="最近入库文档" desc="按文档聚合的分块数量">
          {docs.length === 0 ? (
            <EmptyState icon={IconDoc} title="暂无文档" desc="前往「实时库 · 数据接入」写入第一篇文档" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>文档 ID</th>
                    <th>来源</th>
                    <th className="num">分块</th>
                  </tr>
                </thead>
                <tbody>
                  {docs.slice(0, 6).map((d) => (
                    <tr key={d.doc_id}>
                      <td className="mono ellipsis" title={d.doc_id}>{d.doc_id}</td>
                      <td className="ellipsis" title={d.source}>{d.source || '—'}</td>
                      <td className="num">{d.chunks}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        {/* 运行环境 */}
        <Panel title="运行环境" desc="当前后端连接信息">
          <div className="kv">
            <div>
              <span>Milvus</span>
              <b className="mono">{rag?.milvus_uri || '—'}</b>
            </div>
            <div>
              <span>集合</span>
              <b className="mono">{rag?.collection || '—'}</b>
            </div>
            <div>
              <span>Embedding</span>
              <b>{rag?.embed_model || '—'}{rag?.embed_dim ? ` · ${rag.embed_dim}d` : ''}</b>
            </div>
            <div>
              <span>LLM</span>
              <b>{rag?.llm_model || '—'}</b>
            </div>
            <div>
              <span>图谱集合</span>
              <b className="mono">{rag?.collection ? `${rag.collection}_graph_entities` : '—'}</b>
            </div>
          </div>
          <div className="badges-row">
            <Badge tone="ok">BM25 稀疏索引</Badge>
            <Badge tone="ok">HNSW 稠密索引</Badge>
            <Badge tone="ok">RRF 融合</Badge>
          </div>
        </Panel>
      </div>
    </div>
  );
}

// buildUsageBuckets 把调用记录按时间分桶：
//   时间跨度 > 48h → 按天聚合（最多 31 桶）；否则按小时聚合（最多 48 桶）。
//   每桶除输入/输出总量外，还按模型分别统计（models 字典），供逐模型折线使用。
//   纯前端计算，无后端改动。
function buildUsageBuckets(calls) {
  const parsed = (calls || [])
    .map((c) => ({
      t: new Date(c.created_at).getTime(),
      in: c.prompt_tokens ?? 0,
      out: c.completion_tokens ?? 0,
      model: c.model || 'unknown',
    }))
    .filter((c) => !Number.isNaN(c.t));
  if (parsed.length === 0) return null;

  const newest = Math.max(...parsed.map((c) => c.t));
  const oldest = Math.min(...parsed.map((c) => c.t));
  const byHour = newest - oldest <= 48 * 3600e3;

  // 对齐桶起点：小时桶抹掉分秒，天桶抹掉时分秒
  const start = new Date(byHour ? oldest : oldest);
  if (byHour) start.setMinutes(0, 0, 0);
  else start.setHours(0, 0, 0, 0);
  const step = byHour ? 3600e3 : 24 * 3600e3;
  const n = Math.min(byHour ? 48 : 31, Math.floor((newest - start.getTime()) / step) + 1);

  const buckets = Array.from({ length: n }, (_, i) => ({
    start: start.getTime() + i * step,
    in: 0,
    out: 0,
    calls: 0,
    models: {},
  }));
  for (const c of parsed) {
    const idx = Math.floor((c.t - start.getTime()) / step);
    if (idx < 0 || idx >= n) continue;
    const b = buckets[idx];
    b.in += c.in;
    b.out += c.out;
    b.calls += 1;
    const m = (b.models[c.model] ??= { in: 0, out: 0, calls: 0 });
    m.in += c.in;
    m.out += c.out;
    m.calls += 1;
  }
  // 出现过的模型按总 token 降序：消耗最大的拿色板第一个颜色，图例顺序与视觉主次一致
  const totals = {};
  for (const c of parsed) {
    totals[c.model] = (totals[c.model] || 0) + c.in + c.out;
  }
  const models = Object.keys(totals).sort((a, b2) => totals[b2] - totals[a]);
  return { grain: byHour ? 'hour' : 'day', buckets, models };
}

// 模型配色板：逐模型取色，超出色板长度循环（同色系重复时靠图例顺序区分）
const MODEL_PALETTE = ['#6366f1', '#14b8a6', '#f59e0b', '#ef4444', '#8b5cf6', '#3b82f6', '#10b981', '#f97316', '#ec4899', '#06b6d4'];

function modelColorMap(models) {
  const map = {};
  (models || []).forEach((m, i) => {
    map[m] = MODEL_PALETTE[i % MODEL_PALETTE.length];
  });
  return map;
}

// UsageChart 纯 SVG 折线图（无第三方依赖）：横轴时间桶，纵轴 tokens。
// 每个模型一种颜色：实线 = 输入，虚线 = 输出（同色同模型）；
// 单模型时保留线下渐变面积，多模型时去掉面积避免互相覆盖。
function UsageChart({ usage }) {
  if (!usage || usage.buckets.length === 0) {
    return <EmptyState title="暂无可绘制的调用数据" desc="发起一次对话或 Agent 任务后这里会出现 token 用量折线图" />;
  }
  const { grain, buckets, models } = usage;
  const colors = modelColorMap(models);
  const W = 680;
  const H = 220;
  const padL = 46;
  const padR = 12;
  const padT = 14;
  const padB = 26;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const baseY = padT + plotH;
  const maxVal = Math.max(
    1,
    ...buckets.flatMap((b) => Object.values(b.models).flatMap((m) => [m.in, m.out]))
  );
  const n = buckets.length;
  // 单桶时把点画在中间，避免折线退化
  const xAt = (i) => (n === 1 ? padL + plotW / 2 : padL + (plotW * i) / (n - 1));
  const yAt = (v) => baseY - (v / maxVal) * plotH;
  const toPath = (model, dir) =>
    buckets
      .map((b, i) => `${i === 0 ? 'M' : 'L'}${xAt(i).toFixed(1)},${yAt(b.models[model]?.[dir] ?? 0).toFixed(1)}`)
      .join(' ');
  const fmt = (v) => (v >= 10000 ? `${(v / 1000).toFixed(1)}k` : String(v));
  const labelEvery = Math.max(1, Math.ceil(n / 10));
  const fmtTime = (b) => {
    const d = new Date(b.start);
    return grain === 'hour'
      ? d.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', hour12: false })
      : d.toLocaleDateString('zh-CN');
  };

  // 折线系列：每个模型两条（输入实线 / 输出虚线）
  const lines = models.flatMap((m) => [
    { model: m, dir: 'in', color: colors[m], dash: null, label: '输入' },
    { model: m, dir: 'out', color: colors[m], dash: '5 4', label: '输出' },
  ]);

  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} style={{ width: '100%', height: 'auto', display: 'block' }} role="img" aria-label="LLM token 用量趋势图">
        <defs>
          {models.length === 1 && (
            <linearGradient id="grad-single" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={colors[models[0]]} stopOpacity={0.18} />
              <stop offset="100%" stopColor={colors[models[0]]} stopOpacity={0.02} />
            </linearGradient>
          )}
        </defs>

        {[0, 0.5, 1].map((f) => {
          const y = padT + plotH * (1 - f);
          return (
            <g key={f}>
              <line x1={padL} y1={y} x2={W - padR} y2={y} stroke="#e5e7eb" strokeWidth={1} />
              <text x={padL - 6} y={y + 4} textAnchor="end" fontSize={10} fill="#9aa1ad">
                {fmt(maxVal * f)}
              </text>
            </g>
          );
        })}

        {models.length === 1 && (
          <path
            d={`${toPath(models[0], 'in')} L${xAt(n - 1).toFixed(1)},${baseY} L${xAt(0).toFixed(1)},${baseY} Z`}
            fill="url(#grad-single)"
            stroke="none"
          />
        )}

        {lines.map((s) => (
          <g key={`${s.model}-${s.dir}`}>
            <path
              d={toPath(s.model, s.dir)}
              fill="none"
              stroke={s.color}
              strokeWidth={2}
              strokeDasharray={s.dash || undefined}
              strokeLinejoin="round"
              strokeLinecap="round"
            />
            {buckets.map((b, i) => {
              const m = b.models[s.model];
              const tip = `${s.model} · ${fmtTime(b)} · ${m?.calls ?? 0} 次 · ${s.label} ${m?.[s.dir] ?? 0} tok`;
              return (
                <g key={`${s.model}-${s.dir}-${b.start}`}>
                  <title>{tip}</title>
                  <circle cx={xAt(i)} cy={yAt(m?.[s.dir] ?? 0)} r={3} fill="#fff" stroke={s.color} strokeWidth={2} />
                </g>
              );
            })}
          </g>
        ))}

        {buckets.map((b, i) => {
          if (i % labelEvery !== 0 && i !== n - 1) return null;
          const d = new Date(b.start);
          const label = grain === 'hour' ? `${String(d.getHours()).padStart(2, '0')}:00` : `${d.getMonth() + 1}/${d.getDate()}`;
          return (
            <text key={`x-${b.start}`} x={xAt(i)} y={H - 8} textAnchor="middle" fontSize={10} fill="#9aa1ad">
              {label}
            </text>
          );
        })}
        <line x1={padL} y1={baseY} x2={W - padR} y2={baseY} stroke="#d1d5db" strokeWidth={1} />
      </svg>
      <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
        每个模型一种颜色：实线 = 输入，虚线 = 输出 · 共 {buckets.reduce((s, b) => s + b.calls, 0)} 次调用 · 窗口 {buckets.length} 个{grain === 'hour' ? '小时' : '天'}；悬停数据点查看明细
      </div>
    </div>
  );
}
