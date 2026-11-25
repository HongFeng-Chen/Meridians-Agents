import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { agentfactory, conversation, history, llm } from '../api.js';
import { Alert, Badge, Spinner } from '../components/UI.jsx';
import { TaskDetailDrawer, STATUS_TONES } from '../components/TaskDetailDrawer.jsx';
import { Markdown } from '../components/Markdown.jsx';
import DebateSetup from '../components/DebateSetup.jsx';

// 会话（Chats）：
//   1) 普通对话 —— 用户与 LLM 直接对话，LLM 自主决定是否调用 Agent / MCP 工具，
//      每轮自动召回长期记忆 / RAG / GraphRAG 作为上下文，链路落库。
//   2) 辩论会话 —— 用户只给议题 + 若干临时角色（人设/立场/风格），
//      各角色按人设自主发言、互相反驳，用户仅监督：可叫停、补充引导、增减角色；
//      结束后由中立主持人输出综合结论。进度靠轮询拉取，消息渐进出现在同一条流里。

const truncate = (s, n) => (s && s.length > n ? `${s.slice(0, n)}…` : s || '');

const hueOf = (name) => {
  let h = 0;
  for (let i = 0; i < (name || '').length; i += 1) h = (h * 31 + name.charCodeAt(i)) % 360;
  return h;
};

const DEBATE_STATUS = {
  running: { text: '推演中', tone: 'indigo' },
  paused: { text: '已暂停', tone: 'warn' },
  stopping: { text: '收尾中', tone: 'warn' },
  finished: { text: '已结束', tone: 'default' },
  stopped: { text: '已叫停', tone: 'warn' },
};

// paused 也算活跃：暂停期间保持轮询（状态可被别处恢复 / 消息仍可能在途）
const isLive = (d) => d && ['running', 'paused', 'stopping'].includes(d.status);

// ApprovalCard 会话内人工审批卡（参考 WorkBuddy 提问卡的交互样式）：
// 左侧强调条 + 「人工审批」徽标 + 任务上下文 + 选项按钮。
// 批准直接生效；驳回先展开备注输入，确认后以失败终态结束任务。
function ApprovalCard({ task, busy, done, rejecting, rejectNote, onRejectOpen, onRejectCancel, onRejectNote, onApprove, onReject }) {
  return (
    <div className="approval-card">
      <div className="approval-head">
        <span className="approval-badge">🛡️ 人工审批</span>
        {task.risk_pause_level === 'high' && <span className="approval-risk">高危操作需人工确认</span>}
        <span className="approval-task-no">任务 #{task.id}</span>
      </div>
      <div className="approval-title">{task.title || `${task.agent_key} 请求执行`}</div>
      {task.input && <div className="approval-desc">{truncate(task.input, 260)}</div>}
      {done ? (
        <div className={`approval-done ${done.decision === 'reject' ? 'bad' : ''}`}>
          {done.decision === 'approve'
            ? `已批准任务 #${done.id}，Agent 正在从断点续跑…`
            : `已驳回任务 #${done.id}${done.note ? `：${done.note}` : ''}`}
        </div>
      ) : task.status && task.status !== 'paused' ? (
        <div className="approval-done">该任务已不在审批挂起状态（当前 {task.status}），无需审批</div>
      ) : (
        <div className="approval-actions">
          {rejecting ? (
            <>
              <input
                className="approval-note-input"
                placeholder="驳回原因（可选）"
                value={rejectNote}
                disabled={busy}
                onChange={(e) => onRejectNote(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') onReject();
                }}
              />
              <button className="approval-btn ghost" type="button" disabled={busy} onClick={onRejectCancel}>
                取消
              </button>
              <button className="approval-btn danger" type="button" disabled={busy} onClick={onReject}>
                确认驳回
              </button>
            </>
          ) : (
            <>
              <button className="approval-btn primary" type="button" disabled={busy} onClick={onApprove}>
                {busy ? '提交中…' : '批准'}
              </button>
              <button className="approval-btn ghost" type="button" disabled={busy} onClick={onRejectOpen}>
                驳回
              </button>
            </>
          )}
        </div>
      )}
    </div>
  );
}

function metaChips(metaStr) {
  if (!metaStr) return null;
  let m;
  try {
    m = JSON.parse(metaStr);
  } catch {
    return null;
  }
  const chips = [];
  if (m.op === 'chat' && m.turns != null) chips.push({ text: `LLM 编排 ${m.turns} 轮`, tone: 'indigo' });
  if (m.op === 'debate' && m.round != null) chips.push({ text: `第 ${m.round} 轮`, tone: 'indigo' });
  if (m.op === 'debate_summary') chips.push({ text: '中立汇总', tone: 'teal' });
  if (Array.isArray(m.tools)) {
    for (const t of m.tools) chips.push({ text: t, tone: 'violet' });
  }
  if (m.memories != null) chips.push({ text: `记忆 ${m.memories} 条`, tone: 'teal' });
  if (m.rag_hits != null) chips.push({ text: `知识库命中 ${m.rag_hits}`, tone: 'teal' });
  if (m.graph_entities != null) chips.push({ text: `图谱 实体 ${m.graph_entities} · 关系 ${m.graph_relations ?? 0}`, tone: 'teal' });
  if (m.task_id) chips.push({ text: `任务 #${m.task_id}`, tone: 'indigo' });
  if (m.agent_key) chips.push({ text: m.agent_key, tone: 'default' });
  if (m.hits != null) chips.push({ text: `命中 ${m.hits} 条`, tone: 'teal' });
  if (m.entities != null) chips.push({ text: `实体 ${m.entities} · 关系 ${m.relations ?? 0}`, tone: 'teal' });
  if (m.tool) chips.push({ text: m.tool, tone: 'violet' });
  if (m.elapsed_ms != null) chips.push({ text: `${m.elapsed_ms} ms`, tone: 'default' });
  if (m.cached) chips.push({ text: '缓存命中', tone: 'warn' });
  if (m.ok === false || m.is_error) chips.push({ text: m.error ? `失败：${truncate(m.error, 60)}` : '失败', tone: 'error' });
  return chips;
}

// 从消息 meta.tools（如 "agent:issue-researcher(#12)"）提取会话编排调用的任务号
const agentTaskIds = (metaStr) => {
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
};

const TRACE_STATUS = {
  pending: { text: '排队中', tone: 'default' },
  running: { text: '执行中', tone: 'indigo' },
  succeeded: { text: '成功', tone: 'ok' },
  retry_wait: { text: '等待重试', tone: 'warn' },
  failed: { text: '失败', tone: 'error' },
  canceled: { text: '已取消', tone: 'warn' },
  timeout: { text: '执行超时', tone: 'error' },
};

// TaskTraceInline 会话链路内嵌任务链路入口：
// 会话编排调用了 Agent 时，消息下方显示任务链路入口（任务编号与来源会话共用），
// 点击在当前页右侧弹出 50% 屏宽弹层——复用任务中心的链路详情
// （概要 / 输入答案 / Trace 时间线 / LLM 调用记录 / 关联调用历史）。
function TaskTraceInline({ taskId }) {
  const [open, setOpen] = useState(false);
  const [detail, setDetail] = useState(null);
  const [events, setEvents] = useState([]);
  const [calls, setCalls] = useState([]);
  const [histories, setHistories] = useState([]);
  const [loaded, setLoaded] = useState(false);
  const [err, setErr] = useState('');

  // 打开弹层：一次性拉取任务详情 + 事件 + LLM 调用 + 操作历史（与任务中心详情同源）
  const openDrawer = async () => {
    setOpen(true);
    if (loaded) return;
    try {
      const [t, ev, cl, hs] = await Promise.all([
        agentfactory.getTask({ id: taskId }),
        agentfactory.listTaskEvents({ id: taskId, limit: 300 }).catch(() => ({ events: [] })),
        llm.listLLMCalls({ task_id: taskId, limit: 50 }).catch(() => ({ calls: [] })),
        history.listHistories({ task_id: taskId, limit: 100 }).catch(() => ({ histories: [] })),
      ]);
      setDetail(t);
      setEvents(ev.events || []);
      setCalls(cl.calls || []);
      setHistories(hs.histories || []);
      setLoaded(true);
    } catch (e) {
      setErr(e.message || String(e));
    }
  };

  // 入口条徽标：任务在 pending / running / retry_wait 时每 3s 轮询刷新，
  // 保证重新进入会话也能看到「最新执行状态」（排队中 → 执行中 → 成功/失败）
  const [status, setStatus] = useState(null);
  useEffect(() => {
    let alive = true;
    let timer = null;
    const load = () => {
      agentfactory.getTask({ id: taskId })
        .then((t) => {
          if (!alive) return;
          const s = t?.status || null;
          setStatus(s);
          const liveStatus = s && ['pending', 'running', 'retry_wait'].includes(s);
          if (liveStatus && !timer) timer = setInterval(load, 3000);
          if (!liveStatus && timer) { clearInterval(timer); timer = null; }
        })
        .catch(() => { /* 徽标失败不打扰 */ });
    };
    load();
    return () => { alive = false; if (timer) clearInterval(timer); };
  }, [taskId]);

  const st = status ? TRACE_STATUS[status] || { text: status, tone: 'default' } : null;
  return (
    <div className="chat-task-trace">
      <button className="chat-task-toggle" type="button" onClick={openDrawer}>
        <span className="mono" style={{ color: 'var(--accent)', fontWeight: 600 }}>#{taskId}</span>
        <span>任务链路（会话编排调用 Agent · 点击查看详情）</span>
        {st && <Badge tone={st.tone}>{st.text}</Badge>}
        <span className="chat-task-caret">▸</span>
      </button>
      {open && (
        <TaskDetailDrawer
          detail={detail || { id: taskId, agent_key: '…', status: 'pending', progress: 0, input: '', created_at: '' }}
          events={events}
          calls={calls}
          histories={histories}
          onClose={() => setOpen(false)}
          width="50vw"
        />
      )}
      {open && err && <Alert type="error">{err}</Alert>}
    </div>
  );
}

// 内容是否含 Markdown / Mermaid（决定是否展示「渲染/原文」切换）：
// 围栏代码、标题、表格、加粗、列表任一命中即算富文本
const RICH_RE = /```|(^|\n)\s*#{1,6}\s+\S|\|.+\||\*\*[^*\n]+\*\*|(^|\n)\s*[-*+]\s+\S/;
const looksRich = (t) => !!t && RICH_RE.test(t);

function MessageBubble({ msg, roles = [] }) {
  const chips = metaChips(msg.meta);
  const isUser = msg.role === 'user';
  const speaker = msg.speaker || '';
  const isHost = speaker === '主持人';
  const role = roles.find((r) => r.name === speaker);
  const hue = hueOf(speaker);
  const rich = looksRich(msg.content);
  // 含 md / uml 的消息默认渲染展示，可一键切回原文；纯文本消息无切换
  const [view, setView] = useState('render');
  const rendered = rich && view === 'render';
  return (
    <div className={`chat-msg ${isUser ? 'user' : 'assistant'}`}>
      {!isUser && speaker && (
        <div className="chat-avatar" style={{ background: `hsl(${hue} 58% 46%)` }} title={speaker}>
          {speaker.slice(0, 1)}
        </div>
      )}
      <div className={`chat-bubble${isHost ? ' host' : ''}${isUser ? ' supervise' : ''}`}>
        {!isUser && speaker && (
          <div className="chat-speaker">
            <span className="chat-speaker-name" style={{ color: `hsl(${hue} 55% 38%)` }}>{speaker}</span>
            {isHost && <span className="chat-speaker-role">中立汇总</span>}
            {role?.stance && <span className="chat-speaker-role">{truncate(role.stance, 24)}</span>}
          </div>
        )}
        {isUser && <div className="chat-speaker"><span className="chat-speaker-name">监督者</span></div>}
        {/* 附件：图片网格 / 视频播放器（url 为 /api/files/ 静态地址） */}
        {Array.isArray(msg.attachments) && msg.attachments.length > 0 && (
          <div className="chat-atts">
            {msg.attachments.map((a, i) => (
              a.mime?.startsWith('video/')
                ? <video key={`${a.url}-${i}`} src={a.url} controls preload="metadata" className="chat-att-video" />
                : (
                  <a key={`${a.url}-${i}`} href={a.url} target="_blank" rel="noopener noreferrer" className="chat-att-img-link">
                    <img src={a.url} alt={a.name || '附件'} loading="lazy" className="chat-att-img" />
                  </a>
                )
            ))}
          </div>
        )}
        {rich && (
          <div className="chat-view-toggle io-view-toggle">
            <button className={rendered ? 'active' : ''} onClick={() => setView('render')}>渲染</button>
            <button className={!rendered ? 'active' : ''} onClick={() => setView('raw')}>原文</button>
          </div>
        )}
        {msg.content ? (
          rendered ? (
            <Markdown text={msg.content} className="chat-md" />
          ) : (
            <div className="chat-content">{msg.content}</div>
          )
        ) : null}
        {(chips || msg.attachments?.length > 0) && (
          <div className="chat-meta">
            {(chips || []).map((c, i) => (
              <Badge key={`${c.text}-${i}`} tone={c.tone}>{c.text}</Badge>
            ))}
            <span className="chat-time">{new Date(msg.created_at).toLocaleString('zh-CN', { hour12: false })}</span>
          </div>
        )}
        {/* 会话编排调用了 Agent：任务链路合并到会话链路（内嵌展开，任务编号与会话共用） */}
        {!isUser && agentTaskIds(msg.meta).map((tid) => <TaskTraceInline key={tid} taskId={tid} />)}
      </div>
    </div>
  );
}

// 辩论过程实时面板：配置页下方区域，开局后实时滚动展示各角色推演，
// 支持暂停 / 叫停（操作按钮在 DebateSetup 的 actions 行）。
function DebateLivePanel({ debate, messages }) {
  const bodyRef = useRef(null);
  const live = isLive(debate);
  const paused = debate.status === 'paused';
  const st = DEBATE_STATUS[debate.status] || { text: debate.status, tone: 'default' };
  // round 可能为 undefined / 0（开局尚未进入第 1 轮），兜底避免 NaN
  const curRound = Math.max(Number(debate.round) || 0, 1);

  // 新消息自动贴底
  useEffect(() => {
    const el = bodyRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages.length]);

  return (
    <div className="debate-live">
      <div className="debate-live-head">
        <span className="debate-live-title">辩论过程</span>
        <Badge tone={st.tone}>{st.text}</Badge>
        <span className="cell-sub">
          第 {curRound}/{debate.max_rounds} 轮 · {debate.end_rule === 'consensus' ? '共识判定' : '按轮次'}
        </span>
        {live && <Spinner text={paused ? '已暂停' : '推演中…'} />}
      </div>
      <div className="debate-live-body" ref={bodyRef}>
        {messages.length === 0 ? (
          <div className="debate-live-empty">正在开场，角色即将依次发言…</div>
        ) : (
          messages.map((m) => <MessageBubble key={m.id} msg={m} roles={debate.roles || []} />)
        )}
      </div>
    </div>
  );
}

export default function Chats({ debateEntry = false, onNewChat, onNewDebate, onNavigate }) {
  const [convs, setConvs] = useState([]);
  const [kw, setKw] = useState('');
  const [tab, setTab] = useState('all'); // all = 对话历史 / debate = 辩论历史
  const [active, setActive] = useState(null); // 当前会话
  const [messages, setMessages] = useState([]);
  const [debate, setDebate] = useState(null); // 当前会话的辩论（有 = 辩论视图）
  const [setup, setSetup] = useState(false); // 是否在辩论创建面板
  const [input, setInput] = useState('');
  // 待发送附件：{file, name, mime, size, url(objectURL 预览)}；发送时先上传再随消息提交
  const [pendingFiles, setPendingFiles] = useState([]);
  const fileInputRef = useRef(null);
  const [uploading, setUploading] = useState(false);
  const [model, setModel] = useState('');
  const [busy, setBusy] = useState(false);
  const [pendingText, setPendingText] = useState('');
  // 新会话启动召回：只含长期记忆（不含历史会话消息），来自 createConversation 的 bootstrap
  const [boot, setBoot] = useState(null);
  const [bootOpen, setBootOpen] = useState(false);
  const [addRole, setAddRole] = useState(null); // 辩论中加角色的内联表单
  const [error, setError] = useState('');
  const [hasMore, setHasMore] = useState(false); // 是否还有更早的消息（向上翻页）
  const [loadingOlder, setLoadingOlder] = useState(false); // 正在加载更早消息
  const bottomRef = useRef(null);
  const streamRef = useRef(null);
  const loadingOlderRef = useRef(false); // 滚动事件防抖：同一时刻只发一个翻页请求
  const jumpBottomRef = useRef(false); // 刚打开会话：首屏消息渲染后即时贴底（微信式）
  const stickBottomRef = useRef(false); // 用户是否贴底（异步渲染撑高内容时决定是否跟随）
  const scrollAnchorRef = useRef(null); // 向上翻页时的滚动锚点（保持视口不跳动）
  const activeIdRef = useRef(null); // 当前会话 id（供轮询/滚动回调读取最新值）

  const notifyChanged = () => window.dispatchEvent(new Event('wb:conv-changed'));

  // 滚动/轮询回调里读最新值，避免闭包旧快照
  const messagesRef = useRef([]);
  useEffect(() => {
    messagesRef.current = messages;
  }, [messages]);
  const hasMoreRef = useRef(false);
  useEffect(() => {
    hasMoreRef.current = hasMore;
  }, [hasMore]);

  // 会话消息分页（微信式）：默认最新 20 条，滚动到顶向上翻页
  const PAGE_SIZE = 20;

  const loadMessages = useCallback(async (convID) => {
    const res = await conversation.listMessages({ id: convID, limit: PAGE_SIZE });
    const list = res.messages || [];
    setHasMore(!!res.has_more);
    setMessages(list);
    return list;
  }, []);

  // 向上翻页：取当前最早一条之前的更早一页，prepend 后用滚动锚点保持视口不跳
  const loadOlder = useCallback(async () => {
    const el = streamRef.current;
    const convID = activeIdRef.current;
    const first = messagesRef.current[0];
    if (!el || !convID || !first || loadingOlderRef.current || !hasMoreRef.current) return;
    loadingOlderRef.current = true;
    setLoadingOlder(true);
    try {
      const res = await conversation.listMessages({ id: convID, before_id: Number(first.id) || first.id, limit: PAGE_SIZE });
      const older = res.messages || [];
      if (older.length) {
        scrollAnchorRef.current = { height: el.scrollHeight, top: el.scrollTop };
        setMessages((prev) => [...older, ...prev]);
      }
      setHasMore(!!res.has_more);
    } catch {
      /* 翻页失败不打扰用户，滚动可重试 */
    } finally {
      loadingOlderRef.current = false;
      setLoadingOlder(false);
    }
  }, []);

  // prepend 消息后按锚点恢复滚动位置：新增内容高度 = 新 scrollHeight - 记录的旧 scrollHeight
  useEffect(() => {
    const anchor = scrollAnchorRef.current;
    const el = streamRef.current;
    if (!anchor || !el) return;
    scrollAnchorRef.current = null;
    el.scrollTop = el.scrollHeight - anchor.height + anchor.top;
  }, [messages.length]);

  const onStreamScroll = useCallback(() => {
    const el = streamRef.current;
    if (!el) return;
    // 记录用户是否贴底：Mermaid 等异步渲染撑高内容时据此决定是否跟随
    stickBottomRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    if (el.scrollTop > 60) return;
    loadOlder();
  }, [loadOlder]);

  const loadConvs = useCallback(async () => {
    try {
      const res = await conversation.listConversations({ kw, limit: 50 });
      setConvs(res.conversations || []);
    } catch (err) {
      setError(err.message);
    }
  }, [kw]);

  useEffect(() => {
    loadConvs();
  }, [loadConvs]);

  useEffect(() => {
    window.addEventListener('wb:conv-changed', loadConvs);
    return () => window.removeEventListener('wb:conv-changed', loadConvs);
  }, [loadConvs]);

  // 当前生效模型（页头展示）
  useEffect(() => {
    (async () => {
      try {
        const cur = await llm.getCurrent();
        setModel(cur?.provider?.model || cur?.model || '');
      } catch {
        /* 展示性信息，失败忽略 */
      }
    })();
  }, []);

  const openConv = useCallback(async (c) => {
    setActive(c);
    activeIdRef.current = c.id;
    setDebate(null);
    setSetup(false);
    setAddRole(null);
    setError('');
    jumpBottomRef.current = true; // 首屏消息渲染后即时贴底
    stickBottomRef.current = true; // 打开会话默认跟随贴底
    try {
      await loadMessages(c.id);
      const d = await conversation.getCurrentDebate({ id: c.id });
      if (d?.debate) setDebate(d.debate);
    } catch (err) {
      setError(err.message);
    }
  }, [loadMessages]);

  // 从「新辩论」入口进来：直接展开辩论配置面板
  useEffect(() => {
    if (debateEntry) newDebateSetup();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debateEntry]);

  // 任务中心「💬 会话」直达：按 conv_id 拉取并打开会话（链路双向互通）
  useEffect(() => {
    const handler = async (e) => {
      const convId = e.detail;
      if (!convId) return;
      try {
        const res = await conversation.getConversation({ id: convId });
        const conv = res.conversation || { id: convId };
        await openConv({ ...conv, id: convId });
      } catch (err) {
        setError(err.message);
      }
    };
    window.addEventListener('wb:open-conv', handler);
    return () => window.removeEventListener('wb:open-conv', handler);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [openConv]);

  // 进入辩论模式那一刻，目录自动切到「辩论历史」分组（仅切一次，不打扰用户手动切换）
  const wasInDebate = useRef(false);
  useEffect(() => {
    const nowIn = setup || !!debate;
    if (nowIn && !wasInDebate.current) setTab('debate');
    wasInDebate.current = nowIn;
  }, [setup, debate]);

  // 辩论进度轮询：进行中每 1.6s 拉一次辩论状态 + 消息，实现渐进展示
  useEffect(() => {
    if (!active?.id || !isLive(debate)) return undefined;
    let alive = true;
    const tick = async () => {
      try {
        const [d, r] = await Promise.all([
          conversation.getCurrentDebate({ id: active.id }),
          conversation.getConversation({ id: active.id }),
        ]);
        if (!alive) return;
        if (d?.debate) setDebate(d.debate);
        if (r?.messages) {
          // 合并而非整体替换：保留用户向上翻页加载的更早消息，只更新/追加最新一页
          setMessages((prev) => {
            const newest = r.messages;
            if (!prev.length || !newest.length) return newest;
            const firstId = Number(newest[0].id);
            if (!Number.isFinite(firstId)) return newest;
            const older = prev.filter((m) => Number(m.id) < firstId);
            return older.length ? [...older, ...newest] : newest;
          });
        }
        if (d?.debate && !isLive(d.debate)) notifyChanged();
        if (d?.debate && !isLive(d.debate)) notifyChanged();
      } catch {
        /* 轮询失败不打扰用户，下次再试 */
      }
    };
    const timer = setInterval(tick, 1600);
    tick();
    return () => {
      alive = false;
      clearInterval(timer);
    };
    // 只在「会话变了 / 状态变了」时重建定时器，避免每帧刷新数据重置节奏
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active?.id, debate?.status]);

  // —— 重新进入会话时的「执行中」状态补齐 ——
  // 后端 Chat 先落 user 消息、编排完成才落 assistant 回复（Agent 任务最长等待 5 分钟）。
  // 若重新进入会话时最后一轮仍在执行（最后一条是 user 且非本地乐观消息），
  // 展示与发送时一致的「思考中…」气泡，并每 2s 轮询最新一页，结果落库后自动补上。
  const [convPending, setConvPending] = useState(false);
  const [, setPendingTick] = useState(0); // 轮询节拍：即使无新消息也驱动「等待超时」文案刷新
  const [stopNote, setStopNote] = useState('');
  // 已被用户主动「停止等待」的死轮次（conv → 末条 user 消息 id）：
  // 后端编排被重启打断时 assistant 回复永远不会再落库，标记后不再轮询、
  // 展示「已停止等待」提示；用户再次发消息（更新的消息出现）自动失效。
  const [abandonedMap, setAbandonedMap] = useState(() => {
    try {
      return JSON.parse(localStorage.getItem('chats:abandoned') || '{}');
    } catch {
      return {};
    }
  });
  const persistAbandoned = (convId, msgId) => {
    setAbandonedMap((prev) => {
      const next = { ...prev, [convId]: String(msgId) };
      try {
        localStorage.setItem('chats:abandoned', JSON.stringify(next));
      } catch { /* 存储失败只影响跨会话记忆，不影响本次 */ }
      return next;
    });
  };
  // 停止等待：轮询立即停 + 终止本会话仍在执行的任务（pending/running/paused）
  const stopWaiting = async () => {
    const convId = active?.id;
    const last = messagesRef.current[messagesRef.current.length - 1];
    setConvPending(false);
    if (convId && last && last.role === 'user') persistAbandoned(convId, last.id);
    const canceled = [];
    try {
      const r = await agentfactory.listTasks({ conv_id: convId, limit: 20 });
      const live = (r?.tasks || []).filter((t) => ['pending', 'running', 'paused'].includes(t.status));
      for (const t of live) {
        try {
          await agentfactory.cancelTask({ id: t.id });
          canceled.push(`#${t.id}`);
        } catch { /* 单个终止失败不阻断其余 */ }
      }
    } catch { /* 列表失败不阻断，仅停止等待 */ }
    setStopNote(canceled.length
      ? `已终止任务 ${canceled.join('、')}，本轮不再等待结果。可直接重新发送消息继续对话。`
      : '已停止等待本轮结果（本轮可能已被后端重启中断）。可直接重新发送消息继续对话。');
  };
  useEffect(() => {
    const inDebateNow = !!debate && !setup;
    if (!active?.id || inDebateNow || busy) {
      setConvPending(false);
      return undefined;
    }
    const list = messagesRef.current;
    const last = list.length ? list[list.length - 1] : null;
    if (!last || last.role !== 'user' || String(last.id).startsWith('tmp-')) {
      setConvPending(false);
      return undefined;
    }
    // 用户已对这轮「停止等待」：不再轮询，渲染停止提示
    if (String(abandonedMap[active.id] ?? '') === String(last.id)) {
      setConvPending(false);
      return undefined;
    }
    setConvPending(true);
    let alive = true;
    const tick = async () => {
      try {
        const res = await conversation.listMessages({ id: active.id, limit: PAGE_SIZE });
        if (!alive) return;
        const newest = res.messages || [];
        setMessages((prev) => {
          if (!prev.length || !newest.length) return prev;
          const firstId = Number(newest[0].id);
          if (!Number.isFinite(firstId)) return prev;
          // 保留向上翻页加载的更早消息，并剔除本地乐观插入的 tmp 消息（与辩论轮询同策略）
          const older = prev.filter((m) => Number(m.id) < firstId && !String(m.id).startsWith('tmp-'));
          return [...older, ...newest];
        });
        setPendingTick((v) => v + 1);
      } catch {
        /* 轮询失败不打扰用户，下次再试 */
      }
    };
    const timer = setInterval(tick, 2000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
    // messages.length 变化（assistant 回复落库）即重评条件，结果到达后自动停轮询
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active?.id, debate?.status, setup, busy, messages.length, abandonedMap]);

  // —— 会话内人工审批卡 ——
  // Agent 任务因高危操作触发四眼审批时挂起（status=paused）。参考 WorkBuddy 的
  // 提问卡样式，把「批准 / 驳回」内联到会话流里：不用跳任务中心，点卡片按钮即可，
  // 批准后任务从断点续跑，本轮对话继续。每 2.5s 轮询该会话的挂起任务（best-effort）。
  const [approvalTask, setApprovalTask] = useState(null);
  const [approvalBusy, setApprovalBusy] = useState(false);
  const approvalBusyRef = useRef(false); // 轮询闭包里读的实时标记（state 有渲染延迟）
  const [rejectOpen, setRejectOpen] = useState(false);
  const [rejectNote, setRejectNote] = useState('');
  const [approvalDone, setApprovalDone] = useState(null); // { id, decision, note } 审批结果内联展示
  useEffect(() => {
    const inDebateNow = !!debate && !setup;
    if (!active?.id || inDebateNow) {
      setApprovalTask(null);
      return undefined;
    }
    let alive = true;
    const tick = () => {
      // 审批提交中不覆盖卡片状态，避免轮询的旧快照把刚清掉的卡又插回来
      if (approvalBusyRef.current) return;
      agentfactory.listTasks({ conv_id: active.id, status: 'paused', limit: 10 })
        .then((r) => {
          if (!alive) return;
          // 双保险：只认真正处于 paused 的任务（后端 status 过滤修复前的兜底）
          const t = (r?.tasks || []).find((x) => x.status === 'paused') || null;
          setApprovalTask(t);
        })
        .catch(() => { /* best-effort：审批卡拉不到就下轮再试 */ });
    };
    tick();
    const timer = setInterval(tick, 2500);
    return () => {
      alive = false;
      clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active?.id, debate?.status, setup]);

  const decideApproval = async (decision, note = '') => {
    if (!approvalTask) return;
    approvalBusyRef.current = true;
    setApprovalBusy(true);
    try {
      // 提交前先核对实时状态：轮询快照可能已过期（任务被并发续跑/取消）
      const cur = await agentfactory.getTask({ id: approvalTask.id }).catch(() => null);
      if (!cur || cur.status !== 'paused') {
        setApprovalTask(null);
        setApprovalDone(null);
        setError(`任务 #${approvalTask.id} 已不在审批挂起状态${cur?.status ? `（当前 ${cur.status}）` : ''}，无需审批`);
        return;
      }
      await agentfactory.approveTask({ id: approvalTask.id, decision, note });
      setApprovalDone({ id: approvalTask.id, decision, note });
      setApprovalTask(null);
      setRejectOpen(false);
      setRejectNote('');
    } catch (err) {
      setError(err.message);
    } finally {
      approvalBusyRef.current = false;
      setApprovalBusy(false);
    }
  };

  // 流内容变化时贴底：
  //   刚打开会话 → 即时贴底（微信式从最新一条看起）；
  //   其余情况 → 只有接近底部才自动滚，避免轮询/翻页把视野顶走。
  useEffect(() => {
    const el = streamRef.current;
    if (!el) return;
    if (jumpBottomRef.current) {
      jumpBottomRef.current = false;
      bottomRef.current?.scrollIntoView(); // 即时，无动画
      return;
    }
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 160;
    if (nearBottom || messages.length <= 2) bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages.length, pendingText, convPending]);

  // Mermaid 等异步渲染撑高气泡后：仅当用户此前贴底（或刚进会话）时跟随贴底，
  // 用户已向上翻阅时绝不打扰其位置。
  useEffect(() => {
    const onResized = () => {
      if (stickBottomRef.current || jumpBottomRef.current) {
        jumpBottomRef.current = false;
        bottomRef.current?.scrollIntoView();
      }
    };
    window.addEventListener('wb:md-resized', onResized);
    return () => window.removeEventListener('wb:md-resized', onResized);
  }, []);

  function newChat() {
    setActive(null);
    activeIdRef.current = null;
    setMessages([]);
    setHasMore(false);
    setDebate(null);
    setSetup(false);
    setInput('');
    setError('');
  }

  function newDebateSetup() {
    setActive(null);
    setMessages([]);
    setDebate(null);
    setAddRole(null);
    setSetup(true);
    setInput('');
    setError('');
  }

  const renameConv = async (c) => {
    const title = window.prompt('重命名会话', c.title || '');
    if (title == null || !title.trim()) return;
    try {
      await conversation.updateConversation({ id: c.id, title: title.trim() });
      notifyChanged();
      if (active?.id === c.id) setActive({ ...c, title: title.trim() });
    } catch (err) {
      setError(err.message);
    }
  };

  const removeConv = async (c) => {
    if (!window.confirm(`删除会话「${c.title || c.id}」？消息一并删除，任务链路保留。`)) return;
    try {
      await conversation.deleteConversation({ id: c.id });
      notifyChanged();
      if (active?.id === c.id) (onNewChat ? onNewChat() : newChat());
    } catch (err) {
      setError(err.message);
    }
  };

  /** 普通对话：无会话先建（标题取首条消息），随后交给后端 LLM 编排。
   *  附件先逐一上传（/api/conversations/media），再把返回的 attachment 列表随消息提交；
   *  图片会以视觉方式交给 LLM（模型不支持时后端自动降级纯文本），视频仅作文字说明。 */
  const send = async () => {
    const text = input.trim();
    if ((!text && pendingFiles.length === 0) || busy || uploading) return;
    setBusy(true);
    setError('');
    setPendingText('思考中…（LLM 决策是否调用 Agent / MCP 工具）');
    const tmpID = `tmp-${Date.now()}`;
    // 乐观插入：本地先用 objectURL 预览，发送成功后由后端消息替换
    const optimisticAtts = pendingFiles.map((f) => ({ name: f.name, url: f.url, mime: f.mime, size: f.size }));
    setMessages((prev) => [...prev, { id: tmpID, role: 'user', content: text, created_at: new Date().toISOString(), meta: '', attachments: optimisticAtts }]);
    try {
      // 1) 上传附件
      const atts = [];
      for (const f of pendingFiles) {
        const res = await conversation.uploadMedia({ file: f.file, name: f.name });
        if (res?.attachment) atts.push(res.attachment);
      }
      let convID = active?.id;
      if (!convID) {
        const res = await conversation.createConversation({ title: truncate(text, 24) || '新对话' });
        const c = res.conversation;
        // 启动召回：只有长期记忆；skipped 表示记忆系统未装配/召回失败，不打扰用户
        const b = res.bootstrap;
        setBoot(b && !b.skipped && (b.memories?.length || 0) > 0
          ? { id: c.id, count: b.memories.length, block: b.context_block, source: b.source }
          : null);
        setActive(c);
        notifyChanged();
        convID = c.id;
      }
      const r = await conversation.conversationChat({ id: convID, content: text, attachments: atts });
      setInput('');
      clearPendingFiles();
      setMessages((prev) => [
        ...prev.filter((m) => m.id !== tmpID),
        ...(r.user_message ? [r.user_message] : []),
        ...(r.assistant_message ? [r.assistant_message] : []),
      ]);
      notifyChanged();
    } catch (err) {
      setMessages((prev) => prev.filter((m) => m.id !== tmpID));
      setError(err.message);
    } finally {
      setBusy(false);
      setPendingText('');
    }
  };

  /** 选择附件：只收图片 / 视频，客户端先做类型与数量校验（大小由后端二次校验） */
  const addFiles = (fileList) => {
    const files = [...(fileList || [])];
    if (!files.length) return;
    setError('');
    setPendingFiles((prev) => {
      const next = [...prev];
      for (const f of files) {
        if (next.length >= 9) {
          setError('附件最多 9 个');
          break;
        }
        const isImage = f.type.startsWith('image/');
        const isVideo = f.type.startsWith('video/');
        if (!isImage && !isVideo) {
          setError(`不支持的附件类型：${f.name}（仅支持图片 / 视频）`);
          continue;
        }
        const limit = isImage ? 10 * 1024 * 1024 : 50 * 1024 * 1024;
        if (f.size > limit) {
          setError(`附件过大：${f.name}（${isImage ? '图片' : '视频'}上限 ${limit / 1024 / 1024}MB）`);
          continue;
        }
        next.push({ file: f, name: f.name, mime: f.type, size: f.size, url: URL.createObjectURL(f) });
      }
      return next;
    });
  };

  const removePendingFile = (idx) => {
    setPendingFiles((prev) => {
      const dropped = prev[idx];
      if (dropped) URL.revokeObjectURL(dropped.url);
      return prev.filter((_, i) => i !== idx);
    });
  };

  const clearPendingFiles = () => {
    setPendingFiles((prev) => {
      for (const f of prev) URL.revokeObjectURL(f.url);
      return [];
    });
  };

  // ---- Ctrl+V 粘贴与拖拽添加附件 ----
  // 只拦截「剪贴板里是图片/视频文件」的情况（截图工具粘贴即命中），
  // 纯文本粘贴照常走 textarea 默认行为；辩论模式不收附件，不做拦截。
  const onPasteFiles = (e) => {
    if (inDebate) return;
    const files = [...(e.clipboardData?.files || [])].filter(
      (f) => f.type.startsWith('image/') || f.type.startsWith('video/'),
    );
    if (!files.length) return;
    e.preventDefault();
    addFiles(files);
  };

  // 拖拽：挂在聊天主体整块（消息区 + 输入区都可作为落点），
  // 用 dragenter/dragleave 计数解决子元素来回触发导致的闪烁。
  const dragDepth = useRef(0);
  const [dragActive, setDragActive] = useState(false);
  const onDragEnter = (e) => {
    if (inDebate || !e.dataTransfer) return;
    e.preventDefault();
    dragDepth.current += 1;
    setDragActive(true);
  };
  const onDragOver = (e) => {
    if (inDebate) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
  };
  const onDragLeave = (e) => {
    e.preventDefault();
    dragDepth.current -= 1;
    if (dragDepth.current <= 0) {
      dragDepth.current = 0;
      setDragActive(false);
    }
  };
  const onDropFiles = (e) => {
    e.preventDefault();
    dragDepth.current = 0;
    setDragActive(false);
    if (inDebate) return;
    const files = [...(e.dataTransfer?.files || [])].filter(
      (f) => f.type.startsWith('image/') || f.type.startsWith('video/'),
    );
    if (files.length) addFiles(files);
  };

  // 组件卸载时回收预览用的 objectURL（经 ref 读最新列表，避免卸载后 setState）
  const pendingRef = useRef([]);
  useEffect(() => {
    pendingRef.current = pendingFiles;
  }, [pendingFiles]);
  useEffect(() => () => {
    for (const f of pendingRef.current) URL.revokeObjectURL(f.url);
  }, []);

  /** 辩论监督：把用户发言写进会话，角色下一轮会读到并回应 */
  const supervise = async () => {
    const text = input.trim();
    if (!text || busy || !active?.id) return;
    setBusy(true);
    setError('');
    try {
      await conversation.appendMessage({ id: active.id, role: 'user', content: text });
      setInput('');
      await loadMessages(active.id);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  /** 创建辩论：会话不存在时先建（标题取议题），再开局 */
  const startDebate = async ({ topic, roles, maxRounds, endRule }) => {
    setBusy(true);
    setError('');
    try {
      let convID = active?.id;
      if (!convID) {
        const res = await conversation.createConversation({ title: truncate(topic, 24) || '辩论会话' });
        setActive(res.conversation);
        notifyChanged();
        convID = res.conversation.id;
      } else {
        await conversation.updateConversation({ id: convID, title: truncate(topic, 24) || '辩论会话' });
        setActive((prev) => (prev ? { ...prev, title: truncate(topic, 24) } : prev));
      }
      const r = await conversation.createDebate({
        id: convID,
        topic,
        roles,
        max_rounds: maxRounds,
        end_rule: endRule,
      });
      setDebate(r.debate);
      setMessages([]);
      // 留在配置面板视图：下方「辩论过程」区实时展示推演（可暂停 / 叫停 / 进入会话）
      notifyChanged();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const stopDebate = async () => {
    if (!active?.id) return;
    if (!window.confirm('叫停后当前发言结束即收尾并输出中立汇总，确定？')) return;
    setBusy(true);
    try {
      const r = await conversation.stopDebate({ id: active.id });
      if (r?.debate) setDebate(r.debate);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  /** 暂停辩论：当前发言结束后挂起，可恢复 */
  const pauseDebate = async () => {
    if (!active?.id) return;
    setBusy(true);
    try {
      const r = await conversation.pauseDebate({ id: active.id });
      if (r?.debate) setDebate(r.debate);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  /** 恢复已暂停的辩论 */
  const resumeDebate = async () => {
    if (!active?.id) return;
    setBusy(true);
    try {
      const r = await conversation.resumeDebate({ id: active.id });
      if (r?.debate) setDebate(r.debate);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const submitRole = async (role) => {
    if (!active?.id || !role.name.trim()) return;
    setBusy(true);
    setError('');
    try {
      const r = await conversation.addDebateRole({ id: active.id, role });
      if (r?.debate) setDebate(r.debate);
      setAddRole(null);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const dropRole = async (name) => {
    if (!active?.id) return;
    if (!window.confirm(`移除角色「${name}」？下一轮起不再发言。`)) return;
    setBusy(true);
    try {
      const r = await conversation.removeDebateRole({ id: active.id, name });
      if (r?.debate) setDebate(r.debate);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const statusMeta = debate ? DEBATE_STATUS[debate.status] || { text: debate.status, tone: 'default' } : null;
  const roles = useMemo(() => debate?.roles || [], [debate]);
  const inDebate = !!debate && !setup;
  // round 可能为 undefined / 0（开局尚未进入第 1 轮），兜底避免 NaN
  const curRound = Math.max(Number(debate?.round) || 0, 1);
  const shownConvs = useMemo(
    () => (tab === 'debate' ? convs.filter((c) => c.has_debate) : convs),
    [convs, tab],
  );

  // 上一轮等待是否已超时（Agent 任务最长等待 5 分钟 + 余量）：超时改提示可能已中断
  const pendingOverdue = (() => {
    if (!convPending) return false;
    const last = messages[messages.length - 1];
    if (!last) return false;
    return Date.now() - new Date(last.created_at).getTime() > 6 * 60 * 1000;
  })();

  // 当前会话末条 user 消息是否已被「停止等待」（死轮次标记命中）
  const lastMsgForStop = messages.length ? messages[messages.length - 1] : null;
  const convStoppedShown = !!lastMsgForStop
    && lastMsgForStop.role === 'user'
    && !String(lastMsgForStop.id).startsWith('tmp-')
    && String(abandonedMap[active?.id] ?? '') === String(lastMsgForStop.id);

  return (
    <div className="chats-layout">
      {/* 会话目录 */}
      <aside className="chat-dir panel">
        <div className="chat-dir-head">
          <button className="btn primary sm" type="button" onClick={() => (onNewChat ? onNewChat() : newChat())}>
            ＋ 新对话
          </button>
          <button
            className="btn sm"
            type="button"
            onClick={() => (onNewDebate ? onNewDebate() : newDebateSetup())}
            title="多角色自主辩论，你只做监督"
          >
            ⚖ 新辩论
          </button>
        </div>
        <div className="chat-dir-tabs">
          <button
            className={`chat-dir-tab${tab === 'all' ? ' active' : ''}`}
            type="button"
            onClick={() => setTab('all')}
          >
            对话历史
          </button>
          <button
            className={`chat-dir-tab${tab === 'debate' ? ' active' : ''}`}
            type="button"
            onClick={() => setTab('debate')}
          >
            辩论历史
          </button>
        </div>
        <div className="chat-dir-head" style={{ borderTop: 0, paddingTop: 0 }}>
          <input className="input" placeholder="搜索会话" value={kw} onChange={(e) => setKw(e.target.value)} />
        </div>
        <div className="chat-dir-list">
          {shownConvs.length === 0 && (
            <div className="cell-sub" style={{ padding: '12px' }}>
              {tab === 'debate' ? '还没有辩论会话，点上方「⚖ 新辩论」开一场' : '还没有会话，点上方「新对话」开始'}
            </div>
          )}
          {shownConvs.map((c) => (
            <div
              key={c.id}
              className={`chat-dir-item${active?.id === c.id ? ' active' : ''}`}
              onClick={() => openConv(c)}
              role="button"
              tabIndex={0}
              onKeyDown={(e) => e.key === 'Enter' && openConv(c)}
            >
              <div className="chat-dir-title">
                <span className="chat-dir-name">
                  {c.has_debate && <span className="chat-dir-flag" title="辩论会话">⚖</span>}
                  {c.title || '未命名会话'}
                </span>
                <span className="chat-dir-actions">
                  <button
                    className="chat-dir-btn"
                    type="button"
                    title="重命名"
                    onClick={(e) => { e.stopPropagation(); renameConv(c); }}
                  >
                    ✎
                  </button>
                  <button
                    className="chat-dir-btn"
                    type="button"
                    title="删除"
                    onClick={(e) => { e.stopPropagation(); removeConv(c); }}
                  >
                    ✕
                  </button>
                </span>
              </div>
              <div className="cell-sub">{truncate(c.last_message, 40) || '（暂无消息）'}</div>
              <div className="chat-dir-sub">
                <span className="mono">#{c.id}</span>
                <span>{c.message_count} 条 · {new Date(c.updated_at).toLocaleString('zh-CN', { hour12: false })}</span>
              </div>
            </div>
          ))}
        </div>
      </aside>

      {/* 会话主体：整块接受拖拽（图片/视频文件拖进来即加入待发送附件） */}
      <section
        className={`chat-main panel${dragActive ? ' chat-drag-active' : ''}`}
        onDragEnter={onDragEnter}
        onDragOver={onDragOver}
        onDragLeave={onDragLeave}
        onDrop={onDropFiles}
      >
        {dragActive && (
          <div className="chat-drop-hint">
            <div className="chat-drop-hint-card">🖼 松开鼠标，把图片 / 视频加入待发送附件</div>
          </div>
        )}
        {setup ? (
          <>
            {/* 开局失败的报错必须在这里可见：否则「开始辩论」失败时表现为没反应 */}
            {error && (
              <div style={{ padding: '0 16px', marginBottom: 12 }}>
                <Alert type="error">
                  {error}
                  <span className="muted" style={{ display: 'block', marginTop: 4 }}>
                    可直接重新点击「开始辩论」重试（会话已创建时会复用，不会重复建）。
                  </span>
                </Alert>
              </div>
            )}
            <DebateSetup
              onCancel={() => {
                setSetup(false);
                setError('');
                if (!active) (onNewChat ? onNewChat() : newChat());
              }}
              onSubmit={startDebate}
              busy={busy}
              live={setup && debate ? debate : null}
              onPause={pauseDebate}
              onResume={resumeDebate}
              onStopDebate={stopDebate}
              onEnterChat={() => {
                setSetup(false);
                setError('');
              }}
            >
              {setup && debate && <DebateLivePanel debate={debate} messages={messages} busy={busy} />}
            </DebateSetup>
          </>
        ) : (
          <>
            {active && (
              <header className="chat-head">
                <div>
                  <strong>{active.title || '未命名会话'}</strong>
                  <div className="cell-sub mono">
                    会话 ID：{active.id}
                    {model ? ` · 模型 ${model}` : ''}
                  </div>
                </div>
                <div className="chat-head-actions">
                  {statusMeta && <Badge tone={statusMeta.tone}>{statusMeta.text}</Badge>}
                  {debate && (
                    <span className="cell-sub">
                      第 {curRound}/{debate.max_rounds} 轮 ·{' '}
                      {debate.end_rule === 'consensus' ? '共识判定' : '按轮次'}
                    </span>
                  )}
                  <button className="btn ghost sm" type="button" onClick={() => onNavigate('tasks')}>
                    查看任务链路
                  </button>
                </div>
              </header>
            )}

            {/* 辩论监督台：角色名单 + 增删 + 叫停 */}
            {inDebate && (
              <div className="debate-rail">
                <div className="debate-rail-roles">
                  {roles.map((r) => (
                    <span className="debate-chip" key={r.name} title={[r.persona, r.stance, r.style].filter(Boolean).join(' · ')}>
                      <span className="debate-chip-dot" style={{ background: `hsl(${hueOf(r.name)} 58% 46%)` }} />
                      <span className="debate-chip-name">{r.name}</span>
                      {isLive(debate) && (
                        <button className="debate-chip-x" type="button" title="移除该角色" onClick={() => dropRole(r.name)}>
                          ✕
                        </button>
                      )}
                    </span>
                  ))}
                  {isLive(debate) && !addRole && (
                    <button className="btn ghost sm" type="button" onClick={() => setAddRole({ name: '', persona: '', stance: '', style: '' })}>
                      ＋ 角色
                    </button>
                  )}
                  {isLive(debate) && <Spinner text={debate.status === 'paused' ? '已暂停，等待恢复…' : '角色自主推演中…'} />}
                  {debate?.status === 'paused' && (
                    <button className="btn primary sm" type="button" disabled={busy} onClick={resumeDebate}>
                      继续推演
                    </button>
                  )}
                  {debate?.status === 'running' && (
                    <button className="btn ghost sm" type="button" disabled={busy} onClick={pauseDebate}>
                      暂停
                    </button>
                  )}
                  {isLive(debate) && (
                    <button className="btn danger sm debate-stop" type="button" disabled={busy} onClick={stopDebate}>
                      叫停并汇总
                    </button>
                  )}
                </div>
                {addRole && (
                  <div className="debate-addrole">
                    <input className="input" placeholder="角色名" value={addRole.name} onChange={(e) => setAddRole({ ...addRole, name: e.target.value })} />
                    <input className="input" placeholder="人设" value={addRole.persona} onChange={(e) => setAddRole({ ...addRole, persona: e.target.value })} />
                    <input className="input" placeholder="立场" value={addRole.stance} onChange={(e) => setAddRole({ ...addRole, stance: e.target.value })} />
                    <input className="input" placeholder="风格" value={addRole.style} onChange={(e) => setAddRole({ ...addRole, style: e.target.value })} />
                    <button className="btn primary sm" type="button" disabled={busy || !addRole.name.trim()} onClick={() => submitRole(addRole)}>
                      加入（下一轮生效）
                    </button>
                    <button className="btn ghost sm" type="button" onClick={() => setAddRole(null)}>
                      取消
                    </button>
                  </div>
                )}
              </div>
            )}

            <div className="chat-stream" ref={streamRef} onScroll={onStreamScroll}>
              {active && loadingOlder && <div className="chat-stream-top">正在加载更早的消息…</div>}
              {active && !hasMore && messages.length > 0 && !loadingOlder && (
                <div className="chat-stream-top muted">已经到最早的消息了</div>
              )}
              {!active && !inDebate && (
                <div className="chat-empty">
                  <h2>开始一个新对话</h2>
                  <p>
                    直接和 LLM 对话即可：它会自动召回长期记忆、RAG 知识库与 GraphRAG 图谱作为上下文，
                    并自主决定是否调用 Agent 或 MCP 工具；每轮交互都会记录到会话链路，
                    Agent 委派生成的任务可在任务中心查看。
                  </p>
                  <p style={{ marginTop: 12 }}>
                    想听多方争论而不是一个答案？点左上角「⚖ 新辩论」，给议题和几个角色，让它们自己吵。
                  </p>
                </div>
              )}
              {active && messages.length === 0 && inDebate && (
                <div className="chat-empty">
                  <h2>{debate.topic}</h2>
                  <p>辩论已开局，第 1 轮发言马上开始。你可以随时在下方补充引导，或直接叫停。</p>
                </div>
              )}
              {active && boot && boot.id === active.id && (
                <div className="chat-boot">
                  <div className="chat-boot-row">
                    <button className="link-btn" type="button" onClick={() => setBootOpen((v) => !v)}>
                      {bootOpen ? '收起召回内容' : `新会话已召回 ${boot.count} 条长期记忆`}
                    </button>
                    <span className="muted">仅长期记忆，未召回历史会话</span>
                  </div>
                  {bootOpen && <pre className="chat-boot-body">{boot.block}</pre>}
                </div>
              )}
              {messages.map((m) => <MessageBubble key={m.id} msg={m} roles={roles} />)}
              {/* 人工审批卡：本会话编排的任务因高危操作挂起时，内联提供批准 / 驳回 */}
              {approvalTask && (
                <ApprovalCard
                  task={approvalTask}
                  busy={approvalBusy}
                  done={approvalDone}
                  rejecting={rejectOpen}
                  rejectNote={rejectNote}
                  onRejectOpen={() => setRejectOpen(true)}
                  onRejectCancel={() => { setRejectOpen(false); setRejectNote(''); }}
                  onRejectNote={setRejectNote}
                  onApprove={() => decideApproval('approve')}
                  onReject={() => decideApproval('reject', rejectNote.trim() || '人工驳回')}
                />
              )}
              {/* 执行中气泡：本地发送等待（pendingText）与重新进入会话时的在途轮次（convPending）
                  复用同一套样式，保证「重进会话看到的最新执行状态」与发送后一致 */}
              {(pendingText || convPending || (inDebate && isLive(debate))) && (
                <div className="chat-msg assistant">
                  <div className="chat-bubble pending">
                    <Spinner
                      text={
                        pendingText
                          || (convPending && pendingOverdue
                            ? '上一轮执行等待超时（Agent 任务最长等待 5 分钟，可能已中断），仍在自动刷新…'
                            : undefined)
                          || (convPending ? '思考中…（LLM 决策是否调用 Agent / MCP 工具）' : undefined)
                          || '下一个角色正在组织语言…'
                      }
                    />
                    {/* WorkBuddy 式停止入口：重进会话的在途轮次可主动停止等待，
                        并顺带终止本会话仍在执行的后端任务 */}
                    {convPending && (
                      <div className="chat-pending-stop">
                        <button className="chat-stop-btn" type="button" onClick={stopWaiting}>
                          ⏹ 停止等待
                        </button>
                      </div>
                    )}
                  </div>
                </div>
              )}
              {/* 用户已停止等待的死轮次：muted 提示替代气泡，不再轮询 */}
              {convStoppedShown && !pendingText && (
                <div className="chat-msg assistant">
                  <div className="chat-bubble stopped">⏹ {stopNote || '已停止等待本轮结果（本轮可能已被后端重启中断）。'}</div>
                </div>
              )}
              <div ref={bottomRef} />
            </div>

            {error && <div style={{ padding: '0 16px' }}><Alert type="error">{error}</Alert></div>}

            <footer className="chat-composer">
              {/* 附件预览条：objectURL 缩略图，可单个移除 */}
              {pendingFiles.length > 0 && (
                <div className="chat-attach-strip">
                  {pendingFiles.map((f, i) => (
                    <div className="chat-attach-item" key={`${f.name}-${i}`} title={`${f.name}（${(f.size / 1024 / 1024).toFixed(1)}MB）`}>
                      {f.mime.startsWith('image/') ? (
                        <img src={f.url} alt={f.name} className="chat-attach-thumb" />
                      ) : (
                        <video src={f.url} className="chat-attach-thumb" muted />
                      )}
                      <span className="chat-attach-name">{truncate(f.name, 14)}</span>
                      <button
                        className="chat-attach-remove"
                        type="button"
                        aria-label="移除附件"
                        onClick={() => removePendingFile(i)}
                      >
                        ✕
                      </button>
                    </div>
                  ))}
                </div>
              )}
              {/* 输入区（WorkBuddy 式卡片）：无边框输入面 + 底部工具条（左提示/附件，右发送） */}
              <div className="composer-card">
                <textarea
                  className="composer-textarea"
                  rows={2}
                  placeholder={
                    inDebate
                      ? '监督引导：写下你的补充要求或追问…'
                      : '发消息…（可点 📎、Ctrl+V 粘贴或拖入图片 / 视频）'
                  }
                  value={input}
                  disabled={busy}
                  onChange={(e) => setInput(e.target.value)}
                  onPaste={onPasteFiles}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && !e.shiftKey) {
                      e.preventDefault();
                      if (inDebate) supervise();
                      else send();
                    }
                  }}
                />
                <div className="composer-toolbar">
                  <div className="composer-hint">
                    {inDebate
                      ? 'Enter 提交引导 · 角色下一轮会读到（不会强迫它们改立场）'
                      : 'Enter 发送，Shift+Enter 换行 · LLM 会自动决定是否调用 Agent / MCP 工具'}
                  </div>
                  <div className="composer-actions">
                    <input
                      ref={fileInputRef}
                      type="file"
                      accept="image/*,video/mp4,video/webm,video/quicktime"
                      multiple
                      hidden
                      onChange={(e) => {
                        addFiles(e.target.files);
                        e.target.value = '';
                      }}
                    />
                    <button
                      className="composer-icon-btn"
                      type="button"
                      title="添加图片 / 视频附件"
                      aria-label="添加图片 / 视频附件"
                      disabled={busy || uploading}
                      onClick={() => fileInputRef.current?.click()}
                    >
                      📎
                    </button>
                    <button
                      className="composer-send-btn"
                      type="button"
                      disabled={busy || uploading || (inDebate ? !input.trim() || !active : (!input.trim() && pendingFiles.length === 0))}
                      onClick={inDebate ? supervise : send}
                    >
                      {busy ? '执行中…' : inDebate ? '提交引导' : '发送'}
                    </button>
                  </div>
                </div>
              </div>
            </footer>
          </>
        )}
      </section>
    </div>
  );
}
