import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { agentfactory, conversation, history, llm } from '../api.js';
import { Alert, Badge, EmptyState, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import {
  TaskDetailDrawer,
  STATUS_TONES,
  STATUS_LABELS,
  formatTime,
  formatDuration,
  openConversation,
} from '../components/TaskDetailDrawer.jsx';
import ConvTraceDrawer from '../components/ConvTraceDrawer.jsx';
import { IconRefresh, IconSearch, IconSpark, IconTrash } from '../components/Icons.jsx';

// 任务中心：
//   任务 = 异步 worker 的执行单位；parent_task_id 形成「多 Agent 协作」链路树；
//   trace 事件流（thought / tool_call / tool_result / delegate / final / error）支撑链路检索；
//   运行中任务每 2s 轮询详情 + 事件，实现实时状态；
//   详情面板同时展示该任务的 LLM 调用记录（token 用量 / 模型 / 耗时）
//   与关联调用历史（MCP 调用 / 技能使用 / 记忆召回 / RAG / GraphRAG 召回，op_histories）。
//   会话列表合并进任务列表（💬 会话行）：点「链路」弹出会话链路（消息时间线 + 编排任务）。


export default function Tasks() {
  const [stats, setStats] = useState(null);
  const [tasks, setTasks] = useState([]);
  const [convs, setConvs] = useState([]);
  const [convTotal, setConvTotal] = useState(0);
  // kind: '' 全部 | task 仅任务 | conv 仅会话
  const [filter, setFilter] = useState({ status: '', keyword: '', agent_key: '', kind: '' });
  const [agents, setAgents] = useState([]);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  // 详情 + trace + LLM 调用记录 + 关联调用历史
  const [detail, setDetail] = useState(null);
  const [events, setEvents] = useState([]);
  const [calls, setCalls] = useState([]);
  const [histories, setHistories] = useState([]);
  // 会话链路弹层（💬 会话行的「链路」）
  const [convTrace, setConvTrace] = useState(null);
  const pollRef = useRef(null);

  // 秒级时钟：列表里有未终态任务时每秒走一次，
  // 让「耗时」列对运行中任务实时跳动（终态任务用 finished_at 定格，不依赖时钟）。
  const [now, setNow] = useState(() => Date.now());
  const hasRunning = tasks.some((t) => ['pending', 'running'].includes(t.status));
  useEffect(() => {
    if (!hasRunning) return undefined;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [hasRunning]);

  // 合并视图：会话行（💬）与任务行按时间倒序混排。
  // 状态 / Agent 筛选只对任务有意义 → 选中时隐藏会话行；关键词同时匹配会话标题与最近消息。
  const mergedRows = useMemo(() => {
    const kw = filter.keyword.trim().toLowerCase();
    const showConv = filter.kind !== 'task' && !filter.status && !filter.agent_key;
    const rows = [];
    if (filter.kind !== 'conv') {
      for (const t of tasks) rows.push({ kind: 'task', key: `t-${t.id}`, sort: new Date(t.created_at).getTime() || 0, t });
    }
    if (showConv) {
      for (const c of convs) {
        if (kw && !`${c.title || ''} ${c.last_message || ''}`.toLowerCase().includes(kw)) continue;
        rows.push({ kind: 'conv', key: `c-${c.id}`, sort: new Date(c.updated_at).getTime() || 0, c });
      }
    }
    return rows.sort((a, b) => b.sort - a.sort);
  }, [tasks, convs, filter]);

  // 列表分页（对合并后的行）：筛选条件变化时回到第 1 页
  const taskPg = usePagedList(mergedRows, { size: 10, resetKey: `${filter.status}|${filter.agent_key}|${filter.keyword}|${filter.kind}` });

  const act = async (tag, fn) => {
    setBusy(tag);
    setError('');
    setNotice('');
    try {
      return await fn();
    } catch (err) {
      setError(err.message);
      return null;
    } finally {
      setBusy('');
    }
  };

  const loadList = useCallback(async () => {
    try {
      const [st, list, cl] = await Promise.all([
        agentfactory.getTaskStats(),
        // 列表只展示根任务：子任务通过链路弹层中的「查看子任务」进入
        agentfactory.listTasks({ ...filter, root_only: true, limit: 100 }),
        // 会话列表合并进任务中心（💬 会话行，支持链路查询）
        conversation.listConversations({ limit: 100 }).catch(() => ({ conversations: [], total: 0 })),
      ]);
      setStats(st);
      setTasks(list.tasks || []);
      setConvs(cl.conversations || []);
      setConvTotal(cl.total ?? (cl.conversations || []).length);
    } catch (err) {
      setError(err.message);
    }
  }, [filter]);

  const loadDetail = useCallback(async (id) => {
    try {
      const [t, ev, cl, hs] = await Promise.all([
        agentfactory.getTask({ id }),
        agentfactory.listTaskEvents({ id, limit: 300 }),
        llm.listLLMCalls({ task_id: id, limit: 50 }).catch(() => ({ calls: [] })),
        history.listHistories({ task_id: id, limit: 100 }).catch(() => ({ histories: [] })),
      ]);
      setDetail(t);
      setEvents(ev.events || []);
      setCalls(cl.calls || []);
      setHistories(hs.histories || []);
      return t;
    } catch (err) {
      setError(err.message);
      return null;
    }
  }, []);

  useEffect(() => {
    loadList();
    agentfactory.listAgentDefs({ limit: 200 }).then((res) => setAgents(res.agents || [])).catch(() => {});
  }, [loadList]);

  // 实时轮询：详情打开且任务未到终态时，每 2s 刷新详情 + 列表
  useEffect(() => {
    if (!detail) return undefined;
    const tick = async () => {
      const t = await loadDetail(detail.id);
      loadList();
      if (!t || ['succeeded', 'failed', 'canceled'].includes(t.status)) {
        clearInterval(pollRef.current);
        pollRef.current = null;
      }
    };
    if (pollRef.current) clearInterval(pollRef.current);
    if (['succeeded', 'failed', 'canceled'].includes(detail.status)) return undefined;
    pollRef.current = setInterval(tick, 2000);
    return () => {
      if (pollRef.current) {
        clearInterval(pollRef.current);
        pollRef.current = null;
      }
    };
  }, [detail?.id, detail?.status, loadDetail, loadList]);

  const openDetail = async (id) => {
    await loadDetail(id);
  };

  const cancelTask = async (t) => {
    const res = await act(`cancel-${t.id}`, () => agentfactory.cancelTask({ id: t.id }));
    if (!res) return;
    setNotice(`任务 #${t.id} 已${res.status === 'canceled' ? '取消' : '标记取消中'}`);
    loadList();
    if (detail?.id === t.id) loadDetail(t.id);
  };

  const retryTask = async (t) => {
    const res = await act(`retry-${t.id}`, () => agentfactory.retryTask({ id: t.id }));
    if (!res) return;
    setNotice(`任务 #${t.id} 已重新入队`);
    loadList();
    if (detail?.id === t.id) loadDetail(t.id);
  };

  const removeNotice = () => setNotice('');

  return (
    <div className="page">
      <div className="stat-grid">
        <StatCard label="任务总数" value={stats?.tasks ?? 0} hint="含子 Agent 任务" tone="indigo" />
        <StatCard label="执行中" value={stats?.running ?? 0} hint={`排队 ${stats?.pending ?? 0} 个`} tone="teal" />
        <StatCard label="成功" value={stats?.succeeded ?? 0} hint={`失败 ${stats?.failed ?? 0} · 取消 ${stats?.canceled ?? 0}`} tone="teal" />
        <StatCard label="完成率" value={stats?.tasks ? `${Math.round((((stats.succeeded ?? 0) + (stats.failed ?? 0) + (stats.canceled ?? 0)) / stats.tasks) * 100)}%` : '—'} hint="终态任务占比" tone="violet" />
        <StatCard label="会话" value={convTotal} hint="💬 点会话行的「链路」查询会话链路" tone="indigo" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && (
        <Alert type="success">
          {notice}
          <button className="btn ghost sm" type="button" style={{ marginLeft: 12 }} onClick={removeNotice}>知道了</button>
        </Alert>
      )}

      <Panel
        title="任务列表"
        desc="任务与会话混排：💬 会话行支持链路查询；仅展示根任务，多 Agent 协作的子任务在链路弹层中通过「查看子任务」进入"
        actions={
          <button className="btn ghost sm" type="button" onClick={loadList}>
            <IconRefresh size={15} /> 刷新
          </button>
        }
      >
        <div style={{ display: 'flex', gap: 8, marginBottom: 14, flexWrap: 'wrap' }}>
          <select className="input" style={{ width: 120 }} value={filter.kind} onChange={(e) => setFilter({ ...filter, kind: e.target.value })}>
            <option value="">全部类型</option>
            <option value="task">仅任务</option>
            <option value="conv">仅会话</option>
          </select>
          <select className="input" style={{ width: 140 }} value={filter.status} onChange={(e) => setFilter({ ...filter, status: e.target.value })}>
            <option value="">全部状态</option>
            {Object.entries(STATUS_LABELS).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </select>
          <select className="input" style={{ width: 200 }} value={filter.agent_key} onChange={(e) => setFilter({ ...filter, agent_key: e.target.value })}>
            <option value="">全部 Agent</option>
            {agents.map((a) => <option key={a.id} value={a.agent_key}>{a.name}</option>)}
          </select>
          <input className="input" style={{ width: 240 }} placeholder="搜索输入内容…" value={filter.keyword} onChange={(e) => setFilter({ ...filter, keyword: e.target.value })} onKeyDown={(e) => e.key === 'Enter' && loadList()} />
          <button className="btn ghost sm" type="button" onClick={loadList}>
            <IconSearch size={14} /> 查询
          </button>
        </div>

        {mergedRows.length === 0 ? (
          <EmptyState icon={IconSpark} title="还没有任务或会话" desc="到「会话」页发起对话，或到「Agent 工厂」页对任意 Agent 试运行" />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>任务 / 会话</th>
                  <th>Agent</th>
                  <th>状态</th>
                  <th className="num">进度</th>
                  <th className="num">耗时</th>
                  <th>时间</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {taskPg.paged.map((row) =>
                  row.kind === 'conv' ? (
                    // 💬 会话行：链路查询弹出会话链路（消息时间线 + 编排任务）
                    <tr key={row.key}>
                      <td>
                        <div className="cell-main">
                          <span title={row.c.title}>💬 {row.c.title || row.c.id}</span>
                        </div>
                        <div className="cell-sub ellipsis" style={{ maxWidth: 420 }} title={row.c.last_message}>
                          {row.c.last_message || '（暂无消息）'}
                        </div>
                      </td>
                      <td>
                        <div className="cell-main mono" style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                          <Badge tone="blue">会话</Badge>
                          <span className="muted" style={{ fontSize: 12 }}>{row.c.agent_key || '纯对话'}</span>
                        </div>
                        <div className="cell-sub mono ellipsis" style={{ maxWidth: 180 }} title={row.c.id}>{row.c.id}</div>
                      </td>
                      <td>
                        <Badge tone="default">{row.c.message_count ?? 0} 条消息</Badge>
                        {row.c.has_debate && <Badge tone="violet" style={{ marginLeft: 6 }}>辩论</Badge>}
                      </td>
                      <td className="num muted">—</td>
                      <td className="num muted">—</td>
                      <td>
                        <div className="cell-sub">创建 {formatTime(row.c.created_at)}</div>
                        <div className="cell-sub">活跃 {formatTime(row.c.updated_at)}</div>
                      </td>
                      <td className="num">
                        <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
                          <button className="btn ghost sm" type="button" onClick={() => setConvTrace(row.c)}>链路</button>
                          <button className="btn ghost sm" type="button" title="打开会话窗口" onClick={() => openConversation(row.c.id)}>💬</button>
                        </div>
                      </td>
                    </tr>
                  ) : (
                    <tr key={row.key} className={row.t.id === detail?.id ? 'row-active' : ''}>
                      <td>
                        <div className="cell-main">{row.t.title}</div>
                      </td>
                      <td>
                        {/* 任务编号与 Agent 同时排列；会话编排产生的任务带 💬 来源会话徽标（点击直达会话链路） */}
                        <div className="cell-main mono" style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                          <span style={{ color: 'var(--accent)', fontWeight: 600 }}>#{row.t.id}</span>
                          <span>{row.t.agent_key}</span>
                          {row.t.conv_id && (
                            <button
                              className="btn ghost sm"
                              type="button"
                              style={{ padding: '0 6px', fontSize: 12 }}
                              title={`查看来源会话链路 ${row.t.conv_id}`}
                              onClick={() => openConversation(row.t.conv_id)}
                            >
                              💬 会话
                            </button>
                          )}
                        </div>
                        {(row.t.depth ?? 0) > 0 && <div className="cell-sub">深度 {row.t.depth ?? 0}</div>}
                      </td>
                      <td>
                        <Badge tone={STATUS_TONES[row.t.status] || 'default'}>{STATUS_LABELS[row.t.status] || row.t.status}</Badge>
                        {row.t.archived && <Badge tone="default" style={{ marginLeft: 6 }}>历史</Badge>}
                        {row.t.error_msg && <div className="cell-sub" style={{ color: '#b91c1c' }}>{row.t.error_msg}</div>}
                      </td>
                      <td className="num" style={{ minWidth: 90 }}>
                        <div style={{ background: '#eef0f4', borderRadius: 4, height: 6, width: 70, display: 'inline-block', verticalAlign: 'middle', marginRight: 6 }}>
                          <div style={{ width: `${row.t.progress ?? 0}%`, background: 'var(--accent)', height: '100%', borderRadius: 4 }} />
                        </div>
                        <span className="mono">{row.t.progress ?? 0}%</span>
                      </td>
                      <td className="num mono" style={{ minWidth: 78, whiteSpace: 'nowrap' }}>
                        {formatDuration(row.t.started_at, row.t.finished_at, now)}
                      </td>
                      <td>
                        <div className="cell-sub">创建 {formatTime(row.t.created_at)}</div>
                        {row.t.finished_at && <div className="cell-sub">结束 {formatTime(row.t.finished_at)}</div>}
                      </td>
                      <td className="num">
                        <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
                          <button className="btn ghost sm" type="button" onClick={() => openDetail(row.t.id)}>链路</button>
                          {['pending', 'running'].includes(row.t.status) && (
                            <button className="btn ghost sm" type="button" disabled={busy === `cancel-${row.t.id}`} onClick={() => cancelTask(row.t)}>取消</button>
                          )}
                          {['succeeded', 'failed', 'canceled'].includes(row.t.status) && (
                            <button className="btn ghost sm" type="button" disabled={busy === `retry-${row.t.id}`} onClick={() => retryTask(row.t)}>重试</button>
                          )}
                        </div>
                      </td>
                    </tr>
                  )
                )}
              </tbody>
            </table>
          </div>
        )}
        <Pagination page={taskPg.page} pages={taskPg.pages} total={taskPg.total} size={taskPg.size} onPage={taskPg.setPage} onSize={taskPg.setSize} />
      </Panel>

      {/* 会话链路弹层：💬 会话行的「链路」入口 */}
      {convTrace && (
        <ConvTraceDrawer convId={convTrace.id} onClose={() => setConvTrace(null)} />
      )}

      {detail && (
        <TaskDetailDrawer
          detail={detail}
          events={events}
          calls={calls}
          histories={histories}
          busy={busy}
          onClose={() => setDetail(null)}
          onOpenTask={openDetail}
          onCancel={cancelTask}
          onRetry={retryTask}
        />
      )}
    </div>
  );
}
