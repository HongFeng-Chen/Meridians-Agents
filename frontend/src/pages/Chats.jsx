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
  failed: { text: '失败', tone: 'error' },
  canceled: { text: '已取消', tone: 'warn' },
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

  // 入口条自身轻量拉一次任务状态做徽标
  const [status, setStatus] = useState(null);
  useEffect(() => {
    let alive = true;
    agentfactory.getTask({ id: taskId })
      .then((t) => { if (alive) setStatus(t?.status || null); })
      .catch(() => { /* 徽标失败不打扰 */ });
    return () => { alive = false; };
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
        {rich && (
          <div className="chat-view-toggle io-view-toggle">
            <button className={rendered ? 'active' : ''} onClick={() => setView('render')}>渲染</button>
            <button className={!rendered ? 'active' : ''} onClick={() => setView('raw')}>原文</button>
          </div>
        )}
        {rendered ? (
          <Markdown text={msg.content} className="chat-md" />
        ) : (
          <div className="chat-content">{msg.content}</div>
        )}
        {chips && (
          <div className="chat-meta">
            {chips.map((c, i) => (
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
  const [model, setModel] = useState('');
  const [busy, setBusy] = useState(false);
  const [pendingText, setPendingText] = useState('');
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
  }, [messages.length, pendingText]);

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

  /** 普通对话：无会话先建（标题取首条消息），随后交给后端 LLM 编排 */
  const send = async () => {
    const text = input.trim();
    if (!text || busy) return;
    setBusy(true);
    setError('');
    setPendingText('思考中…（LLM 决策是否调用 Agent / MCP 工具）');
    const tmpID = `tmp-${Date.now()}`;
    setMessages((prev) => [...prev, { id: tmpID, role: 'user', content: text, created_at: new Date().toISOString(), meta: '' }]);
    try {
      let convID = active?.id;
      if (!convID) {
        const res = await conversation.createConversation({ title: truncate(text, 24) || '新对话' });
        const c = res.conversation;
        setActive(c);
        notifyChanged();
        convID = c.id;
      }
      const r = await conversation.conversationChat({ id: convID, content: text });
      setInput('');
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

      {/* 会话主体 */}
      <section className="chat-main panel">
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
              {messages.map((m) => <MessageBubble key={m.id} msg={m} roles={roles} />)}
              {(pendingText || (inDebate && isLive(debate))) && (
                <div className="chat-msg assistant">
                  <div className="chat-bubble pending">
                    <Spinner text={pendingText || '下一个角色正在组织语言…'} />
                  </div>
                </div>
              )}
              <div ref={bottomRef} />
            </div>

            {error && <div style={{ padding: '0 16px' }}><Alert type="error">{error}</Alert></div>}

            <footer className="chat-composer">
              <div className="chat-input-row">
                <textarea
                  className="input"
                  rows={2}
                  placeholder={
                    inDebate
                      ? '监督引导：写下你的补充要求或追问，角色下一轮会读到（不会强迫它们改立场）'
                      : '发消息…（Enter 发送，Shift+Enter 换行）LLM 会自动决定是否调用 Agent / MCP 工具'
                  }
                  value={input}
                  disabled={busy}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && !e.shiftKey) {
                      e.preventDefault();
                      if (inDebate) supervise();
                      else send();
                    }
                  }}
                />
                <button
                  className="btn primary"
                  type="button"
                  disabled={busy || !input.trim() || (inDebate && !active)}
                  onClick={inDebate ? supervise : send}
                >
                  {busy ? '执行中…' : inDebate ? '提交引导' : '发送'}
                </button>
              </div>
            </footer>
          </>
        )}
      </section>
    </div>
  );
}
