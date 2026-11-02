import { useCallback, useEffect, useState } from 'react';
import { history } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, usePagedList } from './UI.jsx';
import { IconRefresh, IconSearch } from './Icons.jsx';
import Subgraph from './Subgraph.jsx';

// HistoryPanel 通用「调用 / 召回历史」面板：
// 同一个 op_histories 表，按 kind 过滤后内嵌到各功能页
//   RAG 页 → rag_recall；GraphRAG 页 → graph_recall；
//   记忆页 → memory_recall；MCP 工具页 → mcp_call；技能页 → skill_use。
// 任务内产生的记录自动带 task_id / agent_key（AgentFactory 执行时塞入 context），
// task_id = 0 表示控制台手动触发。
// 召回类记录（rag_recall / memory_recall / graph_recall）带 detail 明细 JSON，
// 点击行弹出弹层展示当时的完整数据。

const KIND_META = {
  mcp_call: { label: 'MCP 调用', tone: 'indigo' },
  skill_use: { label: '技能使用', tone: 'violet' },
  memory_recall: { label: '记忆召回', tone: 'teal' },
  rag_recall: { label: 'RAG 召回', tone: 'default' },
  graph_recall: { label: 'GraphRAG 召回', tone: 'warn' },
};

// 有明细可展开的类型
const DETAIL_KINDS = new Set(['rag_recall', 'memory_recall', 'graph_recall']);

function formatTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return v;
  return d.toLocaleString('zh-CN', { hour12: false });
}

// parseDetail 解析 detail JSON（历史记录可能为空或格式异常）
function parseDetail(s) {
  if (!s) return null;
  try {
    return JSON.parse(s);
  } catch {
    return null;
  }
}

// 统一取出命中数组：RAG 是 hits，记忆是 memories
function hitsOf(detail) {
  if (!detail) return [];
  return detail.hits || detail.memories || [];
}

// ScoreBar 匹配度条：宽度按本次召回最高分归一化，右侧给原始分
function ScoreBar({ score, max }) {
  const pct = max > 0 ? Math.max(2, Math.min(100, (score / max) * 100)) : 0;
  return (
    <div className="score-row">
      <div className="score-track">
        <div className="score-fill" style={{ width: `${pct}%` }} />
      </div>
      <span className="score-num mono">{(score ?? 0).toFixed(4)}</span>
    </div>
  );
}

export default function HistoryPanel({
  kind,
  title,
  desc,
  emptyTitle = '暂无记录',
  emptyDesc = '触发一次后这里会出现记录；Agent 任务内的调用会带上任务号',
}) {
  const [list, setList] = useState([]);
  const [status, setStatus] = useState('');
  const [taskId, setTaskId] = useState('');
  const [keyword, setKeyword] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [active, setActive] = useState(null); // 弹层中的记录

  const pg = usePagedList(list, { size: 10, resetKey: `${kind}|${status}|${taskId}|${keyword}` });

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const f = { kind };
      if (status) f.status = status;
      const t = Number(taskId);
      if (t > 0) f.task_id = t;
      if (keyword.trim()) f.ref = keyword.trim();
      const data = await history.listHistories({ ...f, limit: 200 });
      setList(data.histories || []);
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  }, [kind, status, taskId, keyword]);

  useEffect(() => {
    load();
  }, [load]);

  const meta = KIND_META[kind] || { label: kind, tone: 'default' };
  const canDetail = DETAIL_KINDS.has(kind);

  return (
    <Panel
      title={title || `${meta.label}历史`}
      desc={desc || '记录每次调用/召回的输入、结果摘要、耗时与关联任务'}
      actions={
        <>
          <Badge tone={meta.tone}>{list.length} 条</Badge>
          <input
            className="input sm"
            style={{ width: 120 }}
            placeholder="任务 #ID"
            value={taskId}
            onChange={(e) => setTaskId(e.target.value.replace(/[^0-9]/g, ''))}
            onKeyDown={(e) => e.key === 'Enter' && load()}
          />
          <input
            className="input sm"
            style={{ width: 150 }}
            placeholder="对象 / 关键字"
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && load()}
          />
          <select className="input sm" style={{ width: 96 }} value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="">全部状态</option>
            <option value="ok">成功</option>
            <option value="fail">失败</option>
          </select>
          <button className="btn ghost sm" type="button" onClick={load} disabled={loading}>
            <IconSearch size={14} /> 查询
          </button>
          <button className="btn ghost sm" type="button" onClick={load} disabled={loading}>
            <IconRefresh size={15} /> 刷新
          </button>
        </>
      }
    >
      {error && <Alert type="error">{error}</Alert>}

      {list.length === 0 ? (
        <EmptyState title={emptyTitle} desc={emptyDesc} />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>时间</th>
                <th>对象</th>
                <th>输入 / 结果</th>
                <th>任务</th>
                <th className="num">耗时</th>
                <th>状态</th>
              </tr>
            </thead>
            <tbody>
              {pg.paged.map((h) => (
                <tr
                  key={h.id}
                  className={canDetail ? 'row-click' : undefined}
                  onClick={() => canDetail && setActive(h)}
                  title={canDetail ? '点击查看召回明细' : undefined}
                >
                  <td className="mono cell-sub">{formatTime(h.created_at)}</td>
                  <td className="mono">{h.ref || '—'}</td>
                  <td>
                    {h.query && (
                      <div className="cell-sub ellipsis" title={h.query} style={{ maxWidth: 360 }}>
                        {h.query}
                      </div>
                    )}
                    <div className="cell-sub ellipsis" title={h.summary} style={{ maxWidth: 360 }}>
                      {h.summary || '—'}
                    </div>
                  </td>
                  <td>
                    {h.task_id > 0 ? (
                      <span className="mono">
                        #{h.task_id}
                        {h.agent_key && <div className="cell-sub mono">{h.agent_key}</div>}
                      </span>
                    ) : (
                      <span className="muted">手动</span>
                    )}
                  </td>
                  <td className="num mono">{h.latency_ms ?? 0}ms</td>
                  <td>{h.ok ? <Badge tone="ok">成功</Badge> : <Badge tone="error">失败</Badge>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <Pagination page={pg.page} pages={pg.pages} total={pg.total} size={pg.size} onPage={pg.setPage} onSize={pg.setSize} />

      {active && <RecallDetailDrawer row={active} kind={kind} onClose={() => setActive(null)} />}
    </Panel>
  );
}

// RecallDetailDrawer 召回明细弹层：
// 概要（输入 / 命中条数 / 耗时 / 任务 / 状态）→ 逐条命中（对象 + 匹配度 + 片段正文）。
// graph_recall 走专属视图：命中实体 / 命中关系 / 检索子图可视化 / 原文证据 / 回答。
function RecallDetailDrawer({ row, kind, onClose }) {
  const detail = parseDetail(row.detail);
  const meta = KIND_META[kind] || { label: kind, tone: 'default' };

  if (kind === 'graph_recall') {
    return <GraphRecallDrawer row={row} detail={detail} meta={meta} onClose={onClose} />;
  }

  const hits = hitsOf(detail);
  const isMemory = kind === 'memory_recall';
  const maxScore = hits.reduce((m, h) => Math.max(m, h.score ?? 0), 0);

  return (
    <FormDrawer
      title={`${meta.label}明细 · #${row.id}`}
      desc={`${formatTime(row.created_at)} · 耗时 ${row.latency_ms ?? 0}ms${row.task_id > 0 ? ` · 任务 #${row.task_id}` : ' · 手动触发'}`}
      onClose={onClose}
      width="min(880px, 94vw)"
    >
      <div className="stack">
        <div className="detail-meta">
          <Badge tone={meta.tone}>{meta.label}</Badge>
          {row.ok ? <Badge tone="ok">成功</Badge> : <Badge tone="error">失败</Badge>}
          {row.agent_key && <Badge tone="indigo">{row.agent_key}</Badge>}
          <span className="muted mono">ref: {row.ref || '—'}</span>
        </div>

        <div>
          <div className="field-label">输入（完整查询）</div>
          <pre className="block mono">{row.query || '—'}</pre>
        </div>

        {!detail ? (
          <Alert type="warn">
            该记录没有明细数据（可能产生于明细功能上线之前），仅可查看摘要：{row.summary || '—'}
          </Alert>
        ) : (
          <>
            <div className="recall-summary">
              <div className="recall-stat">
                <div className="recall-stat-num">{detail.hit_count ?? hits.length}</div>
                <div className="recall-stat-label">{isMemory ? '命中记忆条数' : '召回片段条数'}</div>
              </div>
              {isMemory ? (
                <div className="recall-stat">
                  <div className="recall-stat-num">
                    {detail.graph_entities ?? 0} / {detail.graph_relations ?? 0}
                  </div>
                  <div className="recall-stat-label">图谱实体 / 关系</div>
                </div>
              ) : (
                <div className="recall-stat">
                  <div className="recall-stat-num">{maxScore ? maxScore.toFixed(4) : '—'}</div>
                  <div className="recall-stat-label">本次最高分（RRF）</div>
                </div>
              )}
              {isMemory && detail.user_id && (
                <div className="recall-stat">
                  <div className="recall-stat-num mono" style={{ fontSize: 15 }}>
                    {detail.user_id}
                  </div>
                  <div className="recall-stat-label">user_id</div>
                </div>
              )}
            </div>

            <div>
              <div className="field-label" style={{ marginBottom: 8 }}>
                逐条命中与匹配度（{hits.length} 条
                {hits.length < (detail.hit_count ?? 0) ? `，仅展示前 ${hits.length} 条` : ''}）
              </div>
              {hits.length === 0 ? (
                <EmptyState title="本次没有命中" desc="检索结果为空，可调整问题或先导入数据" />
              ) : (
                <div className="hit-list">
                  {hits.map((h, i) => (
                    <div className="hit-item" key={`${h.doc_id || h.id || 'hit'}-${i}`}>
                      <div className="hit-head">
                        <span className="hit-index">{i + 1}</span>
                        <span className="hit-title mono">
                          {isMemory ? h.id || '—' : `${h.doc_id || '—'} · chunk #${h.chunk_idx ?? 0}`}
                        </span>
                        {isMemory && h.kind && <Badge tone="teal">{h.kind}</Badge>}
                        {isMemory && h.importance > 0 && <Badge tone="violet">重要度 {h.importance}</Badge>}
                        {!isMemory && h.source && <span className="muted mono cell-sub">{h.source}</span>}
                      </div>
                      <ScoreBar score={h.score ?? 0} max={maxScore} />
                      <p className="hit-text">{h.content || h.text || '（无正文）'}</p>
                    </div>
                  ))}
                </div>
              )}
              <div className="muted" style={{ fontSize: 12, marginTop: 8 }}>
                {isMemory
                  ? '匹配度为向量相似度得分；条形按本次召回最高分归一化，便于横向比较。'
                  : 'RAG 为 Dense + BM25 的 RRF 融合分，数值偏小属正常；条形按本次召回最高分归一化。'}
              </div>
            </div>
          </>
        )}
      </div>
    </FormDrawer>
  );
}

// GraphRecallDrawer GraphRAG 召回详情弹层：
// 概要统计（命中实体 / 命中关系 / 证据 / 缓存）→ 回答 → 检索子图可视化 →
// 命中实体列表 → 命中关系表 → 原文证据，全部来自落库的 detail JSON。
function GraphRecallDrawer({ row, detail, meta, onClose }) {
  const entities = detail?.entities || [];
  const relations = detail?.relations || [];
  const evidence = detail?.evidence || [];
  return (
    <FormDrawer
      title={`${meta.label}明细 · #${row.id}`}
      desc={`${formatTime(row.created_at)} · 耗时 ${row.latency_ms ?? 0}ms${row.task_id > 0 ? ` · 任务 #${row.task_id}` : ' · 手动触发'}`}
      onClose={onClose}
      width="min(920px, 94vw)"
    >
      <div className="stack">
        <div className="detail-meta">
          <Badge tone={meta.tone}>{meta.label}</Badge>
          {row.ok ? <Badge tone="ok">成功</Badge> : <Badge tone="error">失败</Badge>}
          {row.agent_key && <Badge tone="indigo">{row.agent_key}</Badge>}
          {detail?.cached && <Badge tone="blue">缓存命中</Badge>}
          <span className="muted mono">ref: {row.ref || '—'}</span>
        </div>

        <div>
          <div className="field-label">输入（完整查询）</div>
          <pre className="block mono">{row.query || '—'}</pre>
        </div>

        {!detail ? (
          <Alert type="warn">
            该记录没有明细数据（可能产生于明细功能上线之前），仅可查看摘要：{row.summary || '—'}
          </Alert>
        ) : (
          <>
            <div className="recall-summary">
              <div className="recall-stat">
                <div className="recall-stat-num">{detail.entity_count ?? entities.length}</div>
                <div className="recall-stat-label">命中实体</div>
              </div>
              <div className="recall-stat">
                <div className="recall-stat-num">{detail.relation_count ?? relations.length}</div>
                <div className="recall-stat-label">命中关系</div>
              </div>
              <div className="recall-stat">
                <div className="recall-stat-num">{detail.evidence_count ?? evidence.length}</div>
                <div className="recall-stat-label">检索子图证据</div>
              </div>
            </div>

            {detail.answer && (
              <div>
                <div className="field-label">回答（当时生成）</div>
                <div className="block" style={{ whiteSpace: 'pre-wrap', lineHeight: 1.7 }}>{detail.answer}</div>
              </div>
            )}

            {entities.length > 0 && (
              <div>
                <div className="field-label" style={{ marginBottom: 8 }}>检索子图（环形布局 · 有向边）</div>
                <Subgraph entities={entities} relations={relations} width={820} height={420} />
              </div>
            )}

            {entities.length > 0 && (
              <div>
                <div className="field-label" style={{ marginBottom: 8 }}>命中实体（{entities.length} 个，最多展示 20）</div>
                <ul className="entity-list">
                  {entities.map((e) => (
                    <li key={e.name}>
                      <div className="entity-head">
                        <b>{e.name}</b>
                        <Badge tone="default">{e.type || '实体'}</Badge>
                        {e.freq > 0 && <Badge tone="teal">频次 {e.freq}</Badge>}
                      </div>
                      {e.description && <p className="cell-sub">{e.description}</p>}
                    </li>
                  ))}
                </ul>
              </div>
            )}

            {relations.length > 0 && (
              <div>
                <div className="field-label" style={{ marginBottom: 8 }}>命中关系（{relations.length} 条，最多展示 30）</div>
                <div className="table-wrap">
                  <table className="table">
                    <thead>
                      <tr>
                        <th>起点</th>
                        <th>关系</th>
                        <th>终点</th>
                        <th className="num">权重</th>
                        <th>描述</th>
                      </tr>
                    </thead>
                    <tbody>
                      {relations.map((r, i) => (
                        <tr key={`${r.source}-${r.relation}-${r.target}-${i}`}>
                          <td className="mono">{r.source}</td>
                          <td><Badge tone="violet">{r.relation || '关联'}</Badge></td>
                          <td className="mono">{r.target}</td>
                          <td className="num mono">{(r.weight ?? 0).toFixed(2)}</td>
                          <td className="cell-sub">{r.description || '—'}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            )}

            {evidence.length > 0 && (
              <div>
                <div className="field-label" style={{ marginBottom: 8 }}>原文证据（{evidence.length} 段，最多展示 10）</div>
                <ol className="evidence-list">
                  {evidence.map((t, i) => (
                    <li key={i}>
                      <span className="cite-index">{i + 1}</span>
                      <p>{t}</p>
                    </li>
                  ))}
                </ol>
              </div>
            )}
          </>
        )}
      </div>
    </FormDrawer>
  );
}
