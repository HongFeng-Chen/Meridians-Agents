import { useEffect, useState } from 'react';
import { agentfactory, conversation, llm, history } from '../api.js';
import { Alert, Badge, Spinner } from './UI.jsx';
import { TaskDetailDrawer, STATUS_TONES, STATUS_LABELS, formatTime, HISTORY_KIND_META } from './TaskDetailDrawer.jsx';

// ConvTraceDrawer 会话链路抽屉（任务中心「会话」行的链路查询入口）：
// 概要 → 会话链路时间线（消息 + 记忆/RAG/GraphRAG/MCP 能力调用按时间合并，含存储触达标注）
// → 该会话编排产生的 Agent 任务列表，每个任务可再打开任务链路详情（任务链路与会话链路双向打通）。

const ROLE_LABELS = { user: '用户', assistant: '助手' };

// 消息 meta.tools（如 "agent:issue-researcher(#12)"）提取任务号
function msgTaskIds(metaStr) {
  if (!metaStr) return [];
  try {
    const tools = JSON.parse(metaStr).tools;
    if (!Array.isArray(tools)) return [];
    const ids = [];
    for (const t of tools) {
      const m = /#(\d+)/.exec(String(t));
      if (m) ids.push(Number(m[1]));
    }
    return [...new Set(ids)];
  } catch {
    return [];
  }
}

function prettyPayload(payload) {
  try {
    return JSON.stringify(JSON.parse(payload || '{}'), null, 2);
  } catch {
    return payload || '';
  }
}

// 消息 meta 里的召回明细（全部展开用）：
// 兼容两种数据 —— 新消息带 *_list 明细；老消息只有计数字段（此时仅展示 chips）。
function msgRecallSections(metaStr) {
  if (!metaStr) return null;
  let m;
  try {
    m = JSON.parse(metaStr);
  } catch {
    return null;
  }
  const secs = [];
  // 长期记忆
  if (Array.isArray(m.memories_list) && m.memories_list.length > 0) {
    secs.push({
      key: 'mem',
      title: `🧠 长期记忆召回（${m.memories ?? m.memories_list.length} 条）`,
      store: 'Redis 短期 · MySQL 长期',
      lines: m.memories_list,
    });
  } else if (m.memories > 0) {
    secs.push({ key: 'mem', title: `🧠 长期记忆召回 ${m.memories} 条`, store: 'Redis 短期 · MySQL 长期', lines: null });
  }
  if ((Array.isArray(m.mem_graph_entities_list) && m.mem_graph_entities_list.length > 0) || (Array.isArray(m.mem_graph_relations_list) && m.mem_graph_relations_list.length > 0)) {
    secs.push({
      key: 'memgraph',
      title: `🕸 记忆图谱（${m.mem_graph_entities ?? 0} 实体 / ${m.mem_graph_relations ?? 0} 关系）`,
      store: 'Milvus 图谱',
      entities: m.mem_graph_entities_list || [],
      relations: m.mem_graph_relations_list || [],
    });
  }
  // RAG 知识库检索
  if (Array.isArray(m.rag_hits_list) && m.rag_hits_list.length > 0) {
    secs.push({
      key: 'rag',
      title: `📚 RAG 召回（${m.rag_hits ?? m.rag_hits_list.length} 条命中）`,
      store: 'Milvus 向量',
      hits: m.rag_hits_list,
    });
  } else if (m.rag_hits > 0) {
    secs.push({ key: 'rag', title: `📚 RAG 召回 ${m.rag_hits} 条`, store: 'Milvus 向量', lines: null });
  }
  // GraphRAG 图谱召回
  if ((Array.isArray(m.graph_entities_list) && m.graph_entities_list.length > 0) || (Array.isArray(m.graph_relations_list) && m.graph_relations_list.length > 0)) {
    secs.push({
      key: 'graph',
      title: `🕸 GraphRAG 召回（${m.graph_entities ?? 0} 实体 / ${m.graph_relations ?? 0} 关系）`,
      store: 'Milvus 图谱',
      entities: m.graph_entities_list || [],
      relations: m.graph_relations_list || [],
    });
  } else if (m.graph_entities > 0 || m.graph_relations > 0) {
    secs.push({ key: 'graph', title: `🕸 GraphRAG 召回 ${m.graph_entities ?? 0} 实体 / ${m.graph_relations ?? 0} 关系`, store: 'Milvus 图谱', lines: null });
  }
  // 失败提示
  for (const [k, label] of [['mem_error', '记忆'], ['rag_error', 'RAG'], ['graph_error', '图谱']]) {
    if (m[k]) secs.push({ key: k, title: `⚠️ ${label}召回失败`, error: m[k] });
  }
  return secs.length > 0 ? secs : null;
}

export default function ConvTraceDrawer({ convId, onClose }) {
  const [conv, setConv] = useState(null);
  const [messages, setMessages] = useState([]);
  const [tasks, setTasks] = useState([]);
  const [histories, setHistories] = useState([]);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);

  // 嵌套任务链路：点击任务行的「链路」打开
  const [taskDetail, setTaskDetail] = useState(null);
  const [taskEvents, setTaskEvents] = useState([]);
  const [taskCalls, setTaskCalls] = useState([]);
  const [taskHistories, setTaskHistories] = useState([]);

  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && !taskDetail && onClose();
    window.addEventListener('keydown', onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      window.removeEventListener('keydown', onKey);
      document.body.style.overflow = prev;
    };
  }, [onClose, taskDetail]);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    Promise.all([
      conversation.getConversation({ id: convId }),
      agentfactory.listTasks({ conv_id: convId, limit: 50 }).catch(() => ({ tasks: [] })),
      // 会话内的记忆/RAG/GraphRAG 召回、MCP 调用（op_histories.conv_id）
      history.listHistories({ conv_id: convId, limit: 200 }).catch(() => ({ histories: [] })),
    ])
      .then(([cv, tl, hl]) => {
        if (!alive) return;
        setConv(cv.conversation || null);
        setMessages(cv.messages || []);
        setTasks(tl.tasks || []);
        setHistories(hl.histories || []);
      })
      .catch((e) => alive && setErr(e.message || String(e)))
      .finally(() => alive && setLoading(false));
    return () => {
      alive = false;
    };
  }, [convId]);

  const openTaskTrace = async (id) => {
    try {
      const [t, ev, cl, hs] = await Promise.all([
        agentfactory.getTask({ id }),
        agentfactory.listTaskEvents({ id, limit: 300 }).catch(() => ({ events: [] })),
        llm.listLLMCalls({ task_id: id, limit: 50 }).catch(() => ({ calls: [] })),
        history.listHistories({ task_id: id, limit: 100 }).catch(() => ({ histories: [] })),
      ]);
      setTaskDetail(t);
      setTaskEvents(ev.events || []);
      setTaskCalls(cl.calls || []);
      setTaskHistories(hs.histories || []);
    } catch (e) {
      setErr(e.message || String(e));
    }
  };

  // 会话链路时间线：消息与能力调用按时间合并（升序）
  const merged = [
    ...messages.map((m) => ({ t: new Date(m.created_at).getTime() || 0, seq: m.id ?? 0, kind: 'msg', m })),
    ...histories.map((h) => ({ t: new Date(h.created_at).getTime() || 0, seq: h.id ?? 0, kind: 'hist', h })),
  ].sort((a, b) => a.t - b.t || a.seq - b.seq);

  const storeTags = [
    { name: 'MySQL', desc: '会话 · 消息 · 操作历史', on: true },
    { name: 'Redis', desc: '短期记忆', on: histories.some((h) => h.kind === 'memory_recall') || messages.some((m) => (msgRecallSections(m.meta) || []).some((s) => s.key === 'mem')) },
    { name: 'Milvus', desc: '向量 + 图检索', on: histories.some((h) => ['rag_recall', 'graph_recall', 'memory_recall'].includes(h.kind)) || messages.some((m) => (msgRecallSections(m.meta) || []).some((s) => ['rag', 'graph', 'memgraph'].includes(s.key))) },
    { name: 'MCP', desc: '外部工具', on: histories.some((h) => h.kind === 'mcp_call') },
  ];

  return (
    <div className="drawer-mask" role="dialog" aria-modal="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="drawer">
        <div className="drawer-header">
          <div style={{ flex: 1, minWidth: 0 }}>
            <h3 style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              💬 会话链路 · <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{conv?.title || convId}</span>
              {conv?.has_debate && <Badge tone="violet">辩论</Badge>}
            </h3>
            <div className="drawer-sub">
              ID <span className="mono">{convId}</span>
              {conv ? ` · ${conv.message_count ?? 0} 条消息 · 最近活跃 ${formatTime(conv.updated_at)}` : ''}
              {histories.length > 0 ? ` · 能力调用 ${histories.length} 次` : ''}
              {tasks.length > 0 ? ` · 编排任务 ${tasks.length} 个` : ''}
            </div>
          </div>
          <button className="btn ghost sm" type="button" onClick={onClose}>关闭</button>
        </div>

        <div className="drawer-body">
          {loading && <Spinner text="加载会话链路…" />}
          {err && <Alert type="error">{err}</Alert>}

          {!loading && !err && (
            <>
              {/* 会话链路时间线：消息 + memory/RAG/GraphRAG/MCP 调用按时间合并 */}
              <div className="section-title" style={{ marginBottom: 8, display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap' }}>
                <span>
                  会话链路时间线（{messages.length} 条消息
                  {histories.length > 0 && ` · ${histories.length} 条能力调用`}）
                </span>
                <span style={{ display: 'inline-flex', gap: 6, flexWrap: 'wrap' }}>
                  {storeTags.filter((s) => s.on).map((s) => (
                    <span key={s.name} className="store-tag" title={s.desc}>
                      <b>{s.name}</b> {s.desc}
                    </span>
                  ))}
                </span>
              </div>
              {merged.length === 0 ? (
                <div className="muted">该会话暂无消息记录。</div>
              ) : (
                <div className="conv-trace-msgs trace-line">
                  {merged.map((item) =>
                    item.kind === 'hist' ? (
                      // 能力调用节点：记忆/RAG/GraphRAG/MCP 召回（带存储触达标注）
                      (() => {
                        const h = item.h;
                        const meta = HISTORY_KIND_META[h.kind] || { label: h.kind, tone: 'default', store: '—' };
                        return (
                          <div className={`conv-trace-msg is-op trace-node${h.ok ? '' : ' is-error'}`} key={`h-${h.id}`}>
                            <div className="conv-trace-msg-head">
                              <Badge tone={meta.tone}>{meta.label}</Badge>
                              <Badge tone="default" style={{ fontSize: 11 }}>📡 {meta.store}</Badge>
                              <span className="muted mono" style={{ fontSize: 12 }}>{formatTime(h.created_at)}</span>
                              <span className="muted mono" style={{ fontSize: 12 }}>{h.latency_ms ?? 0}ms</span>
                              {!h.ok && <Badge tone="error">失败</Badge>}
                              {(h.task_id ?? 0) > 0 && (
                                <button className="btn ghost sm" type="button" style={{ padding: '0 6px', fontSize: 12 }} onClick={() => openTaskTrace(h.task_id)}>
                                  #{h.task_id} 链路 →
                                </button>
                              )}
                            </div>
                            <div style={{ marginTop: 4, wordBreak: 'break-word' }}>
                              <span className="mono cell-sub">{h.ref || '—'}</span>
                              {h.query && <div className="cell-sub ellipsis" title={h.query}>{h.query}</div>}
                              <div className="cell-sub ellipsis" title={h.summary}>{h.summary || '—'}</div>
                            </div>
                            {h.detail && (
                              <details style={{ marginTop: 4 }}>
                                <summary className="muted" style={{ cursor: 'pointer', fontSize: 12 }}>命中明细</summary>
                                <pre className="block mono" style={{ marginTop: 4, whiteSpace: 'pre-wrap', fontSize: 12 }}>{prettyPayload(h.detail)}</pre>
                              </details>
                            )}
                          </div>
                        );
                      })()
                    ) : (
                      // 消息节点：用户 / 助手（meta 召回明细默认全部展开）
                      (() => {
                        const m = item.m;
                        const tids = msgTaskIds(m.meta);
                        const isUserMsg = m.role === 'user';
                        const secs = isUserMsg ? null : msgRecallSections(m.meta);
                        return (
                          <div key={`m-${m.id}`} className={`conv-trace-msg${isUserMsg ? ' is-user' : ''}`}>
                            <div className="conv-trace-msg-head">
                              <Badge tone={isUserMsg ? 'default' : 'indigo'}>{m.speaker || ROLE_LABELS[m.role] || m.role}</Badge>
                              <span className="muted mono" style={{ fontSize: 12 }}>{formatTime(m.created_at)}</span>
                              {m.task_id > 0 && (
                                <button className="btn ghost sm" type="button" style={{ padding: '0 6px', fontSize: 12 }} onClick={() => openTaskTrace(m.task_id)}>
                                  #{m.task_id} 链路 →
                                </button>
                              )}
                              {tids.filter((tid) => tid !== m.task_id).map((tid) => (
                                <button key={tid} className="btn ghost sm" type="button" style={{ padding: '0 6px', fontSize: 12 }} onClick={() => openTaskTrace(tid)}>
                                  #{tid} 链路 →
                                </button>
                              ))}
                            </div>
                            <div className="conv-trace-msg-body">{m.content}</div>
                            {secs && (
                              <div className="recall-detail">
                                {secs.map((s) => (
                                  <div key={s.key} className={`recall-sec${s.error ? ' is-error' : ''}`}>
                                    <div className="recall-sec-title">
                                      {s.title}
                                      {s.store && <span className="recall-store">📡 {s.store}</span>}
                                    </div>
                                    {s.error && <div className="recall-err">{s.error}</div>}
                                    {/* 记忆明细：逐条内容行 */}
                                    {s.lines && (
                                      <ul className="recall-lines">
                                        {s.lines.map((l, i) => <li key={i}>{String(l).replace(/^- /, '')}</li>)}
                                      </ul>
                                    )}
                                    {/* RAG 明细：得分 + 来源 + 片段 */}
                                    {s.hits && (
                                      <ul className="recall-lines">
                                        {s.hits.map((h, i) => (
                                          <li key={i}>
                                            <span className="mono recall-score">[{typeof h.score === 'number' ? h.score.toFixed(3) : h.score}]</span>
                                            {h.source && <span className="recall-src"> {h.source}</span>}
                                            {h.doc_id && <span className="muted mono" style={{ fontSize: 11 }}> ({h.doc_id})</span>}
                                            <div className="recall-text">{h.text}</div>
                                          </li>
                                        ))}
                                      </ul>
                                    )}
                                    {/* 图谱明细：实体 chips + 关系行 */}
                                    {s.entities && s.entities.length > 0 && (
                                      <div className="recall-entities">
                                        {s.entities.map((e, i) => (
                                          <span key={i} className="recall-entity" title={e.desc || e.name}>
                                            {e.name}{e.type ? <em>{e.type}</em> : null}
                                          </span>
                                        ))}
                                      </div>
                                    )}
                                    {s.relations && s.relations.length > 0 && (
                                      <ul className="recall-lines recall-rels">
                                        {s.relations.map((r, i) => <li key={i} className="mono">{r}</li>)}
                                      </ul>
                                    )}
                                    {!s.lines && !s.hits && !s.entities && !s.relations && !s.error && (
                                      <div className="muted" style={{ fontSize: 12 }}>本轮记录早于明细落库，仅显示计数。</div>
                                    )}
                                  </div>
                                ))}
                              </div>
                            )}
                          </div>
                        );
                      })()
                    )
                  )}
                </div>
              )}

              {/* 该会话编排产生的 Agent 任务 */}
              <div className="section-title" style={{ margin: '16px 0 8px' }}>
                会话编排任务（{tasks.length} 个）
              </div>
              {tasks.length === 0 ? (
                <div className="muted">该会话未调用 Agent（纯对话 / 检索会话），上方时间线即完整链路。</div>
              ) : (
                <div className="table-wrap">
                  <table className="table">
                    <thead>
                      <tr>
                        <th>任务</th>
                        <th>Agent</th>
                        <th>状态</th>
                        <th className="num">进度</th>
                        <th>时间</th>
                        <th className="num">操作</th>
                      </tr>
                    </thead>
                    <tbody>
                      {tasks.map((t) => (
                        <tr key={t.id}>
                          <td><div className="cell-main ellipsis" style={{ maxWidth: 260 }} title={t.title}>{t.title}</div></td>
                          <td className="mono">{t.agent_key}</td>
                          <td><Badge tone={STATUS_TONES[t.status] || 'default'}>{STATUS_LABELS[t.status] || t.status}</Badge></td>
                          <td className="num mono">{t.progress ?? 0}%</td>
                          <td className="cell-sub">{formatTime(t.created_at)}</td>
                          <td className="num">
                            <button className="btn ghost sm" type="button" onClick={() => openTaskTrace(t.id)}>链路</button>
                          </td>
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

      {/* 嵌套任务链路详情（叠在会话链路之上） */}
      {taskDetail && (
        <TaskDetailDrawer
          detail={taskDetail}
          events={taskEvents}
          calls={taskCalls}
          histories={taskHistories}
          onClose={() => setTaskDetail(null)}
          onOpenTask={openTaskTrace}
        />
      )}
    </div>
  );
}
