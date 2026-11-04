import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { agentfactory, llm } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, GRAPH_GROUPS, GRAPH_HINTS, GRAPH_LABELS, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import AgentFlow from '../components/AgentFlow.jsx';
import { IconBrain, IconRefresh, IconSearch, IconSpark, IconTrash } from '../components/Icons.jsx';

// Agent 工厂管理页：
//   Agent = 一份可配置定义（MySQL）+ 向量注册表（Milvus 语义召回）+ Redis 缓存。
//   两条入口：人工低代码表单 / LLM 元编程（create-by-llm，默认沙箱灰度）。
//   护栏展示：能力白名单（权限边界）、防自举（LLM 不能造带 create_agent 的 Agent）、配额、沙箱。
const TABS = [
  { key: 'agents', label: 'Agent 定义' },
  { key: 'recall', label: '语义召回' },
];

const MULTI_AGENT_GRAPHS = ['orchestrator', 'hierarchical', 'debate', 'blackboard'];

function emptyForm() {
  return {
    id: '',
    agent_key: '',
    name: '',
    description: '',
    system: '',
    llm_model: '',
    llm_source: '',
    graph_type: 'react',
    auto_compose: false,
    capabilities: [],
    max_turns: '12',
    sandbox: false,
    role: 'root',
    orchestration: 'auto',
    members: [],
    flow: emptyGraph(),
    supervisor_key: '',
    supervision_scope: [],
    risk_agent_key: '',
    auto_run: false,
    cron_expr: '',
    cron_input: '',
  };
}

function formFrom(def) {
  return {
    id: def.id,
    agent_key: def.agent_key || '',
    name: def.name || '',
    description: def.description || '',
    system: def.system || '',
    llm_model: def.llm_model || '',
    llm_source: def.llm_source || '',
    graph_type: def.graph_type || 'react',
    auto_compose: !!def.auto_compose,
    capabilities: def.capabilities || [],
    max_turns: def.max_turns ? String(def.max_turns) : '12',
    sandbox: !!def.sandbox,
    role: def.role || 'root',
    orchestration: def.orchestration || 'auto',
    members: def.members || [],
    flow: graphFromDef(def),
    supervisor_key: def.supervisor_key || '',
    supervision_scope: def.supervision_scope || [],
    risk_agent_key: def.risk_agent_key || '',
    auto_run: !!def.auto_run,
    cron_expr: def.cron_expr || '',
    cron_input: def.cron_input || '',
    next_run_at: def.next_run_at || '',
    version: def.version,
  };
}

function formatTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return v;
  return d.toLocaleString('zh-CN', { hour12: false });
}

// ModelCombo 可输入下拉框：既能手动敲模型名，也能从大模型数据源（provider）里选
function ModelCombo({ value, onChange, options, placeholder }) {
  const [open, setOpen] = useState(false);
  const boxRef = useRef(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e) => {
      if (boxRef.current && !boxRef.current.contains(e.target)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  const kw = value.trim().toLowerCase();
  const filtered = kw
    ? options.filter((o) => o.model.toLowerCase().includes(kw) || o.provider.toLowerCase().includes(kw))
    : options;

  return (
    <div ref={boxRef} style={{ position: 'relative' }}>
      <input
        className="input"
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        onFocus={() => setOpen(true)}
      />
      {open && filtered.length > 0 && (
        <div className="combo-menu" role="listbox">
          {filtered.slice(0, 60).map((o) => (
            <button
              key={`${o.provider}/${o.model}`}
              type="button"
              className={`combo-item${o.model === value ? ' active' : ''}`}
              onClick={() => { onChange(o.model); setOpen(false); }}
            >
              <span className="mono">{o.model}</span>
              <span className="cell-sub">{o.provider}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

// —— 编排图工具 ——

const emptyGraph = () => ({ nodes: [], edges: [] });

// 旧版 flow 只存节点坐标 {"positions":{key:{x,y}}}
function legacyPositions(raw) {
  if (!raw) return {};
  try {
    const obj = typeof raw === 'string' ? JSON.parse(raw) : raw;
    return obj && typeof obj.positions === 'object' && obj.positions ? obj.positions : {};
  } catch {
    return {};
  }
}

// 定义 → 画布图结构；旧定义（members + 串行/并行）自动转换成等价的图
function graphFromDef(def) {
  const raw = def?.flow;
  if (raw) {
    try {
      const obj = typeof raw === 'string' ? JSON.parse(raw) : raw;
      if (obj && Array.isArray(obj.nodes) && obj.nodes.length > 0) {
        return { nodes: obj.nodes, edges: Array.isArray(obj.edges) ? obj.edges : [] };
      }
    } catch {
      /* 坏数据降级 */
    }
  }
  const members = def?.members || [];
  const mode = def?.orchestration || 'auto';
  if (members.length === 0 || mode === 'auto') return emptyGraph();

  const positions = legacyPositions(raw);
  const nodes = members.map((key, i) => ({
    id: `n${i + 1}`,
    kind: 'agent',
    agent_key: key,
    x: Number.isFinite(positions[key]?.x) ? positions[key].x : 40 + i * 186,
    y: Number.isFinite(positions[key]?.y) ? positions[key].y : 40,
  }));
  const out = { id: 'out', kind: 'output', label: '输出', x: 40 + members.length * 186, y: 50 };
  const edges = [];
  if (mode === 'serial') {
    for (let i = 0; i < nodes.length - 1; i++) edges.push({ from: nodes[i].id, to: nodes[i + 1].id });
  } else {
    nodes.forEach((n) => edges.push({ from: n.id, to: out.id }));
  }
  return { nodes: [...nodes, out], edges };
}

// 图的节点 / 连线数（列表展示用），非图结构返回 null
function graphStats(raw) {
  if (!raw) return null;
  try {
    const obj = typeof raw === 'string' ? JSON.parse(raw) : raw;
    if (!obj || !Array.isArray(obj.nodes) || obj.nodes.length === 0) return null;
    return { nodes: obj.nodes.length, edges: (obj.edges || []).length };
  } catch {
    return null;
  }
}

// 从图里取出参与执行的 Agent（写回 members，列表/旧逻辑仍可读）
function graphMembers(graph) {
  return (graph?.nodes || []).filter((n) => n.kind === 'agent' && n.agent_key).map((n) => n.agent_key);
}

const CAP_TONES = { factory: 'violet', agent: 'teal', mcp: 'indigo', skill: 'warn' };

// 能力白名单 Tab：按类型分页浏览 + 检索；「已选」跨分类汇总，方便翻页后取消勾选
const CAP_TABS = [
  { key: 'mcp', label: 'MCP 工具' },
  { key: 'agent', label: 'Agent 委派' },
  { key: 'skill', label: '技能' },
  { key: 'factory', label: '工厂' },
  { key: 'selected', label: '已选' },
];
const CAP_PAGE_SIZE = 12;

// 角色 / 编排方式展示
const ROLE_LABELS = { root: '根 Agent', sub: '子 Agent', supervisor: '监察 Agent', risk: '风险评估' };
const ROLE_TONES = { root: 'indigo', sub: 'default', supervisor: 'warn', risk: 'violet' };
const ORCH_LABELS = { auto: '自动（LLM 决定）', serial: '串行', parallel: '并行' };
const STATUS_TONES = {
  succeeded: 'ok',
  running: 'indigo',
  pending: 'warn',
  failed: 'error',
  canceled: 'default',
};

export default function Agents() {
  const [tab, setTab] = useState('agents');
  const [stats, setStats] = useState(null);
  const [agents, setAgents] = useState([]);
  const [caps, setCaps] = useState([]);
  const [providers, setProviders] = useState([]);
  const [capTab, setCapTab] = useState('mcp');
  const [capKw, setCapKw] = useState('');
  const [capPage, setCapPage] = useState(1);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState(emptyForm());

  // LLM 元编程
  const [llmReq, setLlmReq] = useState('');
  const [llmOut, setLlmOut] = useState(null);

  // 试运行（提交异步任务）
  const [runTarget, setRunTarget] = useState(null);
  const [runInput, setRunInput] = useState('');
  const [runTask, setRunTask] = useState(null);

  // 语义召回
  const [query, setQuery] = useState('');
  const [hits, setHits] = useState([]);

  // 列表筛选：关键词 + 角色 + 执行图 + 状态
  const [listFilter, setListFilter] = useState({ kw: '', role: '', graph: '', state: '' });
  const filteredAgents = agents.filter((d) => {
    if (listFilter.role && (d.role || 'root') !== listFilter.role) return false;
    if (listFilter.graph && (d.graph_type || 'react') !== listFilter.graph) return false;
    if (listFilter.state === 'enabled' && !d.enabled) return false;
    if (listFilter.state === 'disabled' && d.enabled) return false;
    if (listFilter.kw) {
      const kw = listFilter.kw.trim().toLowerCase();
      if (kw && !`${d.name || ''}${d.agent_key || ''}${d.description || ''}`.toLowerCase().includes(kw)) return false;
    }
    return true;
  });
  const filterActive = listFilter.kw || listFilter.role || listFilter.graph || listFilter.state;

  // 父子层级：members 引用关系 → 树。外层只显示父级（未被任何 Agent 引用为成员的），
  // 子级默认收起，点展开逐层下钻；有筛选时退回平铺（保证按关键词/角色能搜到子级）。
  const childKeySet = useMemo(() => {
    const s = new Set();
    for (const a of agents) for (const k of a.members || []) if (k) s.add(k);
    return s;
  }, [agents]);
  const childMap = useMemo(() => {
    const map = new Map();
    for (const a of agents) {
      const kids = [];
      const seen = new Set();
      for (const k of a.members || []) {
        if (!k || seen.has(k)) continue;
        seen.add(k);
        const def = agents.find((x) => x.agent_key === k);
        if (def) kids.push(def);
      }
      if (kids.length) map.set(a.id, kids);
    }
    return map;
  }, [agents]);
  const treeMode = !filterActive;
  const rootAgents = useMemo(
    () => filteredAgents.filter((d) => !childKeySet.has(d.agent_key)),
    [filteredAgents, childKeySet],
  );
  const listAgents = treeMode ? rootAgents : filteredAgents;
  const [expanded, setExpanded] = useState({});

  // Agent 定义列表分页（筛选变化时回到第 1 页）
  const agentPg = usePagedList(listAgents, {
    size: 10,
    resetKey: `${tab}|${listFilter.kw}|${listFilter.role}|${listFilter.graph}|${listFilter.state}`,
  });

  const loadBase = useCallback(async () => {
    setError('');
    try {
      const [st, list, capabilities, provs] = await Promise.all([
        agentfactory.getAgentFactoryStats(),
        agentfactory.listAgentDefs({ limit: 200 }),
        agentfactory.listCapabilities(),
        llm.listProviders({ all: true }),
      ]);
      setStats(st);
      setAgents(list.agents || []);
      setCaps(capabilities.capabilities || []);
      setProviders(provs.providers || []);
    } catch (err) {
      setError(err.message);
    }
  }, []);

  useEffect(() => {
    loadBase();
  }, [loadBase]);

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

  const saveAgent = async (e) => {
    e.preventDefault();
    // 前置校验：沙箱与委派能力互斥（后端同一规则，这里提前给出可执行提示）
    const deleg = form.capabilities.filter((c) => c.startsWith('agent:'));
    if (form.sandbox && deleg.length > 0) {
      setError(`沙箱 Agent 不能包含委派能力 ${deleg.join('、')}：取消勾选「沙箱灰度」后可委派，或移除这些能力`);
      return;
    }
    const payload = {
      agent_key: form.agent_key,
      name: form.name,
      description: form.description,
      system: form.system,
      llm_model: form.llm_model,
      llm_source: form.llm_source,
      graph_type: form.graph_type,
      // auto 图恒自动装配；其他图由开关决定
      auto_compose: form.graph_type === 'auto' || !!form.auto_compose,
      capabilities: form.capabilities,
      middlewares: ['audit'],
      max_turns: Number(form.max_turns) || 0,
      sandbox: form.sandbox,
      role: form.role,
      orchestration: form.orchestration,
      members: graphMembers(form.flow),
      flow: JSON.stringify(form.flow || emptyGraph()),
      supervisor_key: form.supervisor_key,
      supervision_scope: form.supervision_scope,
      risk_agent_key: form.risk_agent_key,
      auto_run: form.auto_run,
      cron_expr: form.cron_expr,
      cron_input: form.cron_input,
    };
    const res = await act('save', () =>
      form.id ? agentfactory.updateAgentDef({ id: form.id, input: payload }) : agentfactory.createAgentDef(payload),
    );
    if (!res) return;
    setNotice(`${res.name} 已保存（v${res.version}）`);
    setShowForm(false);
    setForm(emptyForm());
    await loadBase();
  };

  const removeAgent = async (d) => {
    if (!window.confirm(`删除 Agent「${d.name}」？向量与缓存会一并清理，任务历史保留。`)) return;
    const res = await act(`del-${d.id}`, () => agentfactory.deleteAgentDef({ id: d.id }));
    if (!res) return;
    setNotice(`已删除 ${d.name}`);
    await loadBase();
  };

  const toggleAgent = async (d) => {
    const res = await act(`tg-${d.id}`, () => agentfactory.toggleAgentDef({ id: d.id, enabled: !d.enabled }));
    if (!res) return;
    setNotice(`${res.name} 已${res.enabled ? '启用' : '停用'}`);
    await loadBase();
  };

  const createByLLM = async () => {
    const res = await act('llm-create', () => agentfactory.createAgentByLLM({ requirement: llmReq }));
    if (!res) return;
    setLlmOut(res);
    setLlmReq('');
    setNotice(`LLM 已创建 Agent「${res.agent?.name}」（沙箱灰度中，验证后可编辑关闭沙箱）；角色提示词已自动沉淀到「提示词工程」`);
    await loadBase();
  };

  const submitRun = async (d) => {
    const res = await act('run', () => agentfactory.submitTask({ agent_key: d.agent_key, input: runInput }));
    if (!res) return;
    setRunTask(res);
    setNotice(`任务 #${res.id} 已提交，worker 正在执行——到「任务中心」查看实时链路`);
    setRunTarget(null);
    setRunInput('');
  };

  const searchAgents = async () => {
    const res = await act('recall', () => agentfactory.searchAgentDefs({ query, limit: 8 }));
    if (!res) return;
    setHits(res.hits || []);
  };

  // 层级行渲染：父级带展开/收起开关，子级缩进展示（递归支持嵌套编排）
  const renderAgentRow = (d, depth, kids) => {
    const hasKids = kids.length > 0;
    const open = !!expanded[d.id];
    return (
      <tr key={d.id} className={depth > 0 ? 'child-row' : ''}>
        <td style={depth > 0 ? { paddingLeft: 18 + depth * 26 } : undefined}>
          <div className="cell-main" style={{ display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap' }}>
            {treeMode && (hasKids ? (
              <button
                className="btn ghost sm tree-toggle"
                type="button"
                title={open ? '收起子 Agent' : `展开 ${kids.length} 个子 Agent`}
                onClick={() => setExpanded((m) => ({ ...m, [d.id]: !m[d.id] }))}
              >
                {open ? '▾' : '▸'} {kids.length}
              </button>
            ) : (
              <span className="tree-toggle tree-leaf" aria-hidden="true" />
            ))}
            {d.name}
            {d.sandbox && <Badge tone="warn">沙箱</Badge>}
            {!d.enabled && <Badge tone="default">停用</Badge>}
          </div>
          <div className="cell-sub mono">{d.agent_key} · v{d.version}</div>
        </td>
        <td>
          <div className="cell-desc desc-clamp" title={d.description}>{d.description || '—'}</div>
        </td>
        <td>
          <Badge tone={ROLE_TONES[d.role || 'root'] || 'default'}>{ROLE_LABELS[d.role || 'root']}</Badge>
          {(() => {
            const st = graphStats(d.flow);
            return st ? (
              <div className="cell-sub">编排图 · {st.nodes} 节点 / {st.edges} 连线</div>
            ) : (
              <div className="cell-sub">
                {d.graph_type} · {ORCH_LABELS[d.orchestration || 'auto']}
                {(d.members || []).length > 0 ? ` · ${d.members.length} 成员` : ''}
              </div>
            );
          })()}
          {d.supervisor_key && <div className="cell-sub mono">监察 {d.supervisor_key}</div>}
          {d.risk_agent_key && <div className="cell-sub mono">风评 {d.risk_agent_key}</div>}
          {d.auto_run && (
            <div className="cell-sub">
              <Badge tone="teal">cron</Badge> <span className="mono">{d.cron_expr}</span>
              {d.next_run_at && <div className="cell-sub">下次 {formatTime(d.next_run_at)}</div>}
            </div>
          )}
        </td>
        <td>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4 }}>
            {(d.capabilities || []).length === 0 ? <span className="muted">无（纯对话）</span> :
              (d.capabilities || []).map((c) => <Badge key={c} tone="default">{c}</Badge>)}
          </div>
        </td>
        <td><Badge tone={d.source === 'llm' ? 'violet' : 'default'}>{d.source === 'llm' ? 'LLM 自造' : '人工'}</Badge></td>
        <td className="num">{d.max_turns}</td>
        <td className="num">
          <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
            <button className="btn ghost sm" type="button" onClick={() => { setRunTarget(d); setRunTask(null); }}>
              手动执行
            </button>
            <button className="btn ghost sm" type="button" onClick={() => { setForm(formFrom(d)); setShowForm(true); }}>
              编辑
            </button>
            <button className="btn ghost sm" type="button" disabled={busy === `tg-${d.id}`} onClick={() => toggleAgent(d)}>
              {d.enabled ? '停用' : '启用'}
            </button>
            <button className="btn danger sm" type="button" disabled={busy === `del-${d.id}`} onClick={() => removeAgent(d)}>
              {busy === `del-${d.id}` ? <Spinner text="删除" /> : <IconTrash size={14} />}
            </button>
          </div>
        </td>
      </tr>
    );
  };
  const renderTreeRows = (d, depth) => {
    const kids = treeMode ? (childMap.get(d.id) || []) : [];
    const rows = [renderAgentRow(d, depth, kids)];
    if (treeMode && expanded[d.id]) {
      for (const k of kids) rows.push(...renderTreeRows(k, depth + 1));
    }
    return rows;
  };

  const toggleCap = (name) => {
    if (form.sandbox && name.startsWith('agent:')) {
      setError(`沙箱 Agent 不能包含委派能力 ${name}：取消勾选「沙箱灰度」后可委派，或改用其他能力`);
      return;
    }
    setError('');
    setForm((f) => ({
      ...f,
      capabilities: f.capabilities.includes(name)
        ? f.capabilities.filter((c) => c !== name)
        : [...f.capabilities, name],
    }));
  };

  // 委派能力（agent:<key>）与沙箱互斥：勾选沙箱时清掉已选委派项并提示
  const toggleSandbox = (checked) => {
    setError('');
    setForm((f) => {
      const stripped = f.capabilities.filter((c) => !c.startsWith('agent:'));
      if (checked && stripped.length !== f.capabilities.length) {
        setNotice('沙箱灰度已开启：已自动移除委派能力（沙箱内不允许委派其他 Agent）');
      }
      return { ...f, sandbox: checked, capabilities: stripped };
    });
  };

  // 监察范围多选
  const toggleScope = (key) =>
    setForm((f) => ({
      ...f,
      supervision_scope: f.supervision_scope.includes(key)
        ? f.supervision_scope.filter((k) => k !== key)
        : [...f.supervision_scope, key],
    }));

  // 角色候选：可编排的其他 Agent / 监察 Agent / 风险评估 Agent
  const otherAgents = agents.filter((a) => a.agent_key !== form.agent_key);

  // 大模型数据源里配置的全部模型（去重），供「指定模型」下拉选择
  const modelOptions = (() => {
    const seen = new Set();
    const out = [];
    providers.forEach((prov) => {
      const names = [...(prov.models || [])];
      if (prov.default_model && !names.includes(prov.default_model)) names.unshift(prov.default_model);
      names.forEach((m) => {
        if (!m || seen.has(m)) return;
        seen.add(m);
        out.push({ model: m, provider: prov.name });
      });
    });
    return out;
  })();
  const supervisorAgents = agents.filter((a) => a.role === 'supervisor' && a.enabled);
  const riskAgents = agents.filter((a) => a.role === 'risk' && a.enabled);

  // 能力白名单：当前 Tab + 检索 + 分页
  const capKwTrim = capKw.trim().toLowerCase();
  const capTabCaps = caps.filter((c) => c.type === capTab);
  const capSearched = capKwTrim
    ? capTabCaps.filter((c) => c.name.toLowerCase().includes(capKwTrim) || (c.description || '').toLowerCase().includes(capKwTrim))
    : capTabCaps;
  const capPages = Math.max(1, Math.ceil(capSearched.length / CAP_PAGE_SIZE));
  const capPageCur = Math.min(capPage, capPages);
  const capPageItems = capSearched.slice((capPageCur - 1) * CAP_PAGE_SIZE, capPageCur * CAP_PAGE_SIZE);
  const capSelected = caps.filter((c) => form.capabilities.includes(c.name));
  const capTypeCount = (t) => caps.filter((c) => c.type === t).length;

  return (
    <div className="page">
      <div className="stat-grid">
        <StatCard label="Agent 总数" value={stats?.agents ?? 0} hint={`启用 ${stats?.enabled_agents ?? 0} 个`} tone="indigo" />
        <StatCard label="LLM 自造" value={stats?.llm_born ?? 0} hint="元编程创建的 Agent" tone="violet" />
        <StatCard label="沙箱灰度" value={stats?.sandboxed ?? 0} hint="验证通过后可关闭沙箱" tone="blue" />
        <StatCard label="可用能力" value={stats?.capabilities ?? 0} hint="MCP 工具 + Agent 委派 + 工厂" tone="teal" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <div className="tabs">
        {TABS.map((t) => (
          <button key={t.key} type="button" className={`tab${tab === t.key ? ' active' : ''}`} onClick={() => setTab(t.key)}>
            {t.label}
          </button>
        ))}
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          <button className="btn ghost sm" type="button" onClick={loadBase}>
            <IconRefresh size={15} /> 刷新
          </button>
        </div>
      </div>

      {tab === 'agents' && (
        <>
          <Panel
            title="LLM 元编程 · 造一个新 Agent"
            desc="描述需求，LLM 自行决定能力白名单与执行图；护栏：白名单授权 / 防自举 / 配额 / 沙箱灰度"
          >
            <div style={{ display: 'flex', gap: 8 }}>
              <input
                className="input"
                style={{ flex: 1 }}
                placeholder="例：我需要一个审查合同条款风险的合规 Agent，能检索法条 MCP 工具"
                value={llmReq}
                onChange={(e) => setLlmReq(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && createByLLM()}
              />
              <button className="btn primary sm" type="button" disabled={busy === 'llm-create' || !llmReq.trim()} onClick={createByLLM}>
                {busy === 'llm-create' ? <Spinner text="生成中" /> : <><IconSpark size={14} /> 生成 Agent</>}
              </button>
            </div>
            {llmOut && (
              <pre className="block mono" style={{ marginTop: 12, whiteSpace: 'pre-wrap' }}>
                {llmOut.raw_output}
              </pre>
            )}
          </Panel>

          <Panel
            title="Agent 定义"
            desc="Agent 是数据不是代码：新增 Agent = 加一条配置；能力白名单即权限边界"
            actions={
              <button
                className="btn ghost sm"
                type="button"
                onClick={() => {
                  setForm(emptyForm());
                  setShowForm((v) => !(showForm && !form.id));
                }}
              >
                {showForm && !form.id ? '收起' : '新建 Agent'}
              </button>
            }
          >
            {showForm && (
              <FormDrawer
                width="min(1000px, 96vw)"
                title={form.id ? `编辑 Agent · ${form.name || form.agent_key}` : '新建 Agent'}
                desc={form.id ? `修改会生成新版本（当前 v${form.version || '—'}）；标识不可改` : 'Agent 是数据不是代码：新增 Agent = 加一条配置'}
                onClose={() => { setShowForm(false); setForm(emptyForm()); }}
              >
              <form className="form" onSubmit={saveAgent}>
                <section className="form-section">
                  <div className="form-section-title">基本信息</div>
                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
                    <label className="field inline">
                      <span className="field-label">标识（全局唯一，委派能力名 agent:{'{key}'}）</span>
                      <input className="input" value={form.agent_key} onChange={(e) => setForm({ ...form, agent_key: e.target.value })} placeholder="research-agent" disabled={!!form.id} />
                    </label>
                    <label className="field inline">
                      <span className="field-label">名称</span>
                      <input className="input" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="调研助手" />
                    </label>
                    <label className="field inline">
                      <span className="field-label">角色</span>
                      <select className="input" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
                        <option value="root">根 Agent（手动 / 定时触发）</option>
                        <option value="sub">子 Agent（被委派或编排执行）</option>
                        <option value="supervisor">监察 Agent（审查成员产出）</option>
                        <option value="risk">风险评估 Agent（MCP 调用前评估）</option>
                      </select>
                    </label>
                    <label className="field inline">
                      <span className="field-label">执行图（架构模式）</span>
                      <select className="input" value={form.graph_type} onChange={(e) => setForm({ ...form, graph_type: e.target.value })}>
                        {GRAPH_GROUPS.map((g) => (
                          <optgroup key={g.label} label={g.label}>
                            {g.options.map(([v, label]) => (
                              <option key={v} value={v}>{label}</option>
                            ))}
                          </optgroup>
                        ))}
                      </select>
                      <span className="cell-sub" style={{ marginTop: 4 }}>
                        {GRAPH_HINTS[form.graph_type] || ''}
                        {MULTI_AGENT_GRAPHS.includes(form.graph_type) && form.flow?.nodes?.length === 0
                          ? form.auto_compose
                            ? ' 未连成员也没关系：执行前会自动创建成员 Agent。'
                            : ' 该模式需要在下方编排画布上连入成员 Agent，或勾选「自动装配成员」。'
                          : ''}
                      </span>
                    </label>
                  </div>
                  <label className="field inline" style={{ display: 'block', marginTop: 12 }}>
                    <span className="field-label">职责描述（语义召回来源，写清擅长什么）</span>
                    <textarea className="input" rows={2} value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} />
                  </label>
                  <div className="field inline" style={{ display: 'block', marginTop: 12 }}>
                    <span className="field-label">角色提示词（人设 / 工作方法 / 边界）</span>
                    <textarea className="input" rows={6} value={form.system} onChange={(e) => setForm({ ...form, system: e.target.value })} placeholder="可手写；AI 打分 / 优化在「提示词工程」页进行" />
                    <div className="cell-sub" style={{ marginTop: 6 }}>
                      AI 打分 / AI 优化已移至「提示词工程」页：
                      {form.id
                        ? <a href={`#/prompts?agent=${encodeURIComponent(form.agent_key)}`}>去打分 / 优化「{form.name || form.agent_key}」的角色提示词 →</a>
                        : <span>Agent 保存后可关联优化；LLM 创建的 Agent 会自动把提示词沉淀到提示词工程</span>}
                    </div>
                  </div>
                  <div style={{ display: 'grid', gridTemplateColumns: '2fr 1fr', gap: 12, marginTop: 12 }}>
                    <label className="field inline">
                      <span className="field-label">指定模型（空 = 平台默认，可输入或从数据源选择）</span>
                      <ModelCombo
                        value={form.llm_model}
                        onChange={(v) => setForm({ ...form, llm_model: v })}
                        options={modelOptions}
                        placeholder="留空 = 平台默认"
                      />
                    </label>
                    <label className="field inline">
                      <span className="field-label">最大轮数（1~40）</span>
                      <input className="input" type="number" min="1" max="40" value={form.max_turns} onChange={(e) => setForm({ ...form, max_turns: e.target.value })} />
                    </label>
                  </div>
                  <label className="field inline" style={{ display: 'block', marginTop: 12 }}>
                    <span className="field-label">默认模型数据源（空 = 平台默认；该源调用失败自动回退 default）</span>
                    <select className="input" value={form.llm_source} onChange={(e) => setForm({ ...form, llm_source: e.target.value })}>
                      <option value="">平台默认（default 数据源优先）</option>
                      {providers.filter((p) => p.enabled).map((p) => (
                        <option key={p.name} value={p.name}>{p.name}{p.default_model ? ` · ${p.default_model}` : ''}{p.is_default ? '（当前默认）' : ''}</option>
                      ))}
                    </select>
                    <span className="cell-sub" style={{ marginTop: 4 }}>
                      该 Agent 所有轮次（含 ReAct / 委派 / 监察路径）优先走此数据源；不可用或调用失败时自动回落 default 链路，任务不中断。
                    </span>
                  </label>
                </section>

                {form.role === 'root' && (
                  <section className="form-section">
                    <div className="form-section-title">编排（节点数据在编排组件内管理）</div>
                    <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 10 }}>
                      <input
                        type="checkbox"
                        checked={form.graph_type === 'auto' || form.auto_compose}
                        disabled={form.graph_type === 'auto'}
                        onChange={(e) => setForm({ ...form, auto_compose: e.target.checked })}
                      />
                      <span className="field-label" style={{ margin: 0 }}>自动装配成员（执行前由 LLM 判定架构与分工，缺角色时自动创建子 Agent）</span>
                    </label>
                    <div className="cell-sub" style={{ marginBottom: 12 }}>
                      {form.graph_type === 'auto'
                        ? '当前为「自动编排」：每次任务先由 LLM 选择执行图（含是否需要多 Agent），再按结果执行，规划记录在任务链路的 orchestrate 事件里。'
                        : (form.flow?.nodes?.length || 0) > 1
                          ? '画布上已有编排节点：手绘 DAG 优先执行，自动装配不会改动这张图（取消全部连线后才会接管）。'
                          : form.auto_compose
                            ? '每次任务都会重新评估架构与成员；成员集合有变化时回写本定义（控制台可见）。'
                          : '勾选后无需手工连成员：首次执行会自动创建 role=sub、sandbox=true 的子 Agent，之后复用同名成员。'}
                    </div>
                    <AgentFlow
                      agents={otherAgents}
                      flow={form.flow}
                      selfKey={form.agent_key}
                      onChange={(next) => setForm((f) => ({ ...f, flow: { ...f.flow, ...next } }))}
                      onCreateAgent={async (draft) => {
                        // 画布内快捷新建子 Agent：保存到全局列表（其他 Agent 可引用），并刷新节点池
                        const res = await act('flow-create', () =>
                          agentfactory.createAgentDef({
                            agent_key: draft.agent_key,
                            name: draft.name,
                            description: draft.description,
                            system: draft.description || `你是 ${draft.name}。`,
                            graph_type: draft.graph_type || 'react',
                            capabilities: [],
                            middlewares: ['audit'],
                            max_turns: 12,
                            role: 'sub',
                            orchestration: 'auto',
                            members: [],
                            flow: JSON.stringify(emptyGraph()),
                            supervisor_key: '',
                            supervision_scope: [],
                            risk_agent_key: '',
                            auto_run: false,
                            cron_expr: '',
                            cron_input: '',
                          }),
                        );
                        if (res) {
                          setNotice(`画布内已创建子 Agent「${res.name}」，可在其他 Agent 中引用`);
                          await loadBase();
                        }
                        return res;
                      }}
                      onSaveAgent={async (def, patch) => {
                        // 画布右侧面板保存节点 Agent 基础数据：仅覆盖基础字段，其余保持原值
                        const res = await act('flow-save', () =>
                          agentfactory.updateAgentDef({
                            id: def.id,
                            input: {
                              agent_key: def.agent_key,
                              name: patch.name,
                              description: patch.description,
                              system: patch.system,
                              llm_model: patch.llm_model,
                              llm_source: patch.llm_source ?? (def.llm_source || ''),
                              graph_type: patch.graph_type,
                              capabilities: def.capabilities || [],
                              middlewares: ['audit'],
                              max_turns: Number(patch.max_turns) || 0,
                              sandbox: !!def.sandbox,
                              role: patch.role,
                              orchestration: def.orchestration || 'auto',
                              members: def.members || [],
                              flow: def.flow || JSON.stringify(graphFromDef(def)),
                              supervisor_key: def.supervisor_key || '',
                              supervision_scope: def.supervision_scope || [],
                              risk_agent_key: def.risk_agent_key || '',
                              auto_run: !!def.auto_run,
                              cron_expr: def.cron_expr || '',
                              cron_input: def.cron_input || '',
                            },
                          }),
                        );
                        if (res) {
                          setNotice(`节点 Agent「${res.name}」基础数据已保存（v${res.version}）`);
                          await loadBase();
                        }
                        return res;
                      }}
                      nodeSettings={{
                        llmSource: form.llm_source,
                        llmSourceOptions: providers.filter((p) => p.enabled),
                        supervisorKey: form.supervisor_key,
                        supervisorOptions: supervisorAgents,
                        riskKey: form.risk_agent_key,
                        riskOptions: riskAgents,
                        autoRun: form.auto_run,
                        cronExpr: form.cron_expr,
                        cronInput: form.cron_input,
                        nextRunAt: form.next_run_at ? formatTime(form.next_run_at) : '',
                        onUpdate: (patch) => setForm((f) => ({ ...f, ...patch })),
                      }}
                    />
                  </section>
                )}

                {form.role === 'supervisor' && (
                  <section className="form-section">
                    <div className="form-section-title">监察范围</div>
                    <div className="field">
                      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                        {otherAgents.map((a) => (
                          <button
                            key={a.id}
                            type="button"
                            className={`btn ghost sm${form.supervision_scope.includes(a.agent_key) ? ' active' : ''}`}
                            onClick={() => toggleScope(a.agent_key)}
                          >
                            {form.supervision_scope.includes(a.agent_key) ? '✓ ' : ''}{a.name}
                          </button>
                        ))}
                      </div>
                      <div className="cell-sub" style={{ marginTop: 6 }}>勾选要审查的 Agent；一个都不选 = 审查全部成员</div>
                    </div>
                  </section>
                )}

                {form.role === 'risk' && (
                  <div className="cell-sub" style={{ marginBottom: 14 }}>
                    风险评估 Agent 只会在其他 Agent 调用 MCP 工具前被触发：输入为工具名 + 参数，输出风险等级（low / medium / high），high 会被直接拦截。
                  </div>
                )}

                <section className="form-section">
                  <div className="form-section-title">能力白名单（权限边界；LLM 创建的 Agent 不能带 create_agent）</div>

                  {/* Tab 行 + 检索框 */}
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', marginBottom: 10 }}>
                    <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                      {CAP_TABS.map((t) => (
                        <button
                          key={t.key}
                          type="button"
                          className={`btn ghost sm${capTab === t.key ? ' active' : ''}`}
                          onClick={() => { setCapTab(t.key); setCapPage(1); }}
                        >
                          {t.label}
                          <span className="cell-sub" style={{ marginLeft: 4 }}>
                            {t.key === 'selected' ? form.capabilities.length : capTypeCount(t.key)}
                          </span>
                        </button>
                      ))}
                    </div>
                    <input
                      className="input"
                      style={{ width: 200, marginLeft: 'auto' }}
                      placeholder="检索能力名 / 描述"
                      value={capKw}
                      onChange={(e) => { setCapKw(e.target.value); setCapPage(1); }}
                    />
                  </div>

                  {/* 已选能力：跨 Tab 常驻，翻页后也能取消勾选 */}
                  {capSelected.length > 0 && (
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 8 }}>
                      {capSelected.map((c) => (
                        <button
                          key={c.name}
                          type="button"
                          className="btn ghost sm active"
                          title="点击移出白名单"
                          onClick={() => toggleCap(c.name)}
                        >
                          <Badge tone={CAP_TONES[c.type] || 'default'}>{c.type}</Badge> {c.name} ✕
                        </button>
                      ))}
                    </div>
                  )}

                  {/* 当前页能力 */}
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, minHeight: 76, maxHeight: 230, overflow: 'auto', alignContent: 'flex-start' }}>
                    {capTab === 'selected'
                      ? capSelected.map((c) => {
                          const capDenied = form.sandbox && c.name.startsWith('agent:');
                          return (
                            <button
                              key={c.name}
                              type="button"
                              className="btn ghost sm active"
                              title={capDenied ? '沙箱 Agent 不允许委派其他 Agent：先取消勾选「沙箱灰度」' : c.description}
                              onClick={() => toggleCap(c.name)}
                            >
                              <Badge tone={CAP_TONES[c.type] || 'default'}>{c.type}</Badge> {c.name} ✓
                            </button>
                          );
                        })
                      : capPageItems.map((c) => {
                          const capDenied = form.sandbox && c.name.startsWith('agent:');
                          return (
                            <button
                              key={c.name}
                              type="button"
                              disabled={capDenied}
                              className={`btn ghost sm${form.capabilities.includes(c.name) ? ' active' : ''}`}
                              title={capDenied ? '沙箱 Agent 不允许委派其他 Agent：先取消勾选「沙箱灰度」' : c.description}
                              style={capDenied ? { opacity: 0.45, cursor: 'not-allowed' } : undefined}
                              onClick={() => toggleCap(c.name)}
                            >
                              <Badge tone={CAP_TONES[c.type] || 'default'}>{c.type}</Badge> {c.name}
                            </button>
                          );
                        })}
                    {(capTab === 'selected' ? capSelected : capPageItems).length === 0 && (
                      <div className="cell-sub" style={{ padding: '18px 0' }}>
                        {capKwTrim ? '没有匹配的能力，换个关键词试试' : capTab === 'selected' ? '还未勾选任何能力' : '该分类下暂无可用能力'}
                      </div>
                    )}
                  </div>

                  {/* Tab 内翻页 */}
                  {capTab !== 'selected' && capPages > 1 && (
                    <Pagination page={capPageCur} pages={capPages} total={capSearched.length} onPage={setCapPage} className="sm" />
                  )}

                  <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 12 }}>
                    <input type="checkbox" checked={form.sandbox} onChange={(e) => toggleSandbox(e.target.checked)} />
                    <span className="field-label" style={{ margin: 0 }}>沙箱灰度（禁止委派其他 Agent，验证通过后关闭）</span>
                  </label>
                  {form.sandbox && <div className="cell-sub" style={{ marginTop: 6 }}>委派能力（agent:*）不可选；需要委派请先关闭沙箱。</div>}
                </section>

                <div className="modal-footer">
                  <button className="btn ghost sm" type="button" onClick={() => { setShowForm(false); setForm(emptyForm()); }}>
                    取消
                  </button>
                  <button className="btn primary sm" type="submit" disabled={busy === 'save'}>
                    {busy === 'save' ? <Spinner text="保存" /> : form.id ? '保存修改' : '创建 Agent'}
                  </button>
                </div>
              </form>
              </FormDrawer>
            )}

            {agents.length === 0 ? (
              <EmptyState icon={IconBrain} title="还没有 Agent" desc="点右上「新建 Agent」或用 LLM 元编程创建第一个 Agent" />
            ) : (
              <>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center', marginBottom: 10 }}>
                <input
                  className="input"
                  style={{ width: 210 }}
                  placeholder="搜索名称 / 标识 / 描述"
                  value={listFilter.kw}
                  onChange={(e) => setListFilter({ ...listFilter, kw: e.target.value })}
                />
                <select
                  className="input"
                  style={{ width: 130 }}
                  value={listFilter.role}
                  onChange={(e) => setListFilter({ ...listFilter, role: e.target.value })}
                >
                  <option value="">全部角色</option>
                  {Object.entries(ROLE_LABELS).map(([v, label]) => (
                    <option key={v} value={v}>{label}</option>
                  ))}
                </select>
                <select
                  className="input"
                  style={{ width: 170 }}
                  value={listFilter.graph}
                  onChange={(e) => setListFilter({ ...listFilter, graph: e.target.value })}
                >
                  <option value="">全部执行图</option>
                  {GRAPH_GROUPS.map((g) => (
                    <optgroup key={g.label} label={g.label}>
                      {g.options.map(([v, label]) => (
                        <option key={v} value={v}>{label.split(' · ')[0]}</option>
                      ))}
                    </optgroup>
                  ))}
                </select>
                <select
                  className="input"
                  style={{ width: 110 }}
                  value={listFilter.state}
                  onChange={(e) => setListFilter({ ...listFilter, state: e.target.value })}
                >
                  <option value="">全部状态</option>
                  <option value="enabled">启用中</option>
                  <option value="disabled">已停用</option>
                </select>
                {filterActive && (
                  <button
                    className="btn ghost sm"
                    type="button"
                    onClick={() => setListFilter({ kw: '', role: '', graph: '', state: '' })}
                  >
                    清除筛选
                  </button>
                )}
                <span className="cell-sub" style={{ marginLeft: 'auto' }}>
                  {filterActive
                    ? `${filteredAgents.length} / ${agents.length} 个 Agent`
                    : `${rootAgents.length} 个父级 · 共 ${agents.length} 个 Agent`}
                </span>
              </div>
              {listAgents.length === 0 ? (
                <EmptyState icon={IconSearch} title="没有符合条件的 Agent" desc="调整筛选条件，或点「清除筛选」查看全部" />
              ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Agent</th>
                      <th>职责描述</th>
                      <th>角色 / 编排</th>
                      <th>能力白名单</th>
                      <th>来源</th>
                      <th className="num">轮数</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {agentPg.paged.map((d) => renderTreeRows(d, 0)).flat()}
                  </tbody>
                </table>
              </div>
              )}
              </>
            )}
            <Pagination page={agentPg.page} pages={agentPg.pages} total={agentPg.total} size={agentPg.size} onPage={agentPg.setPage} onSize={agentPg.setSize} />
          </Panel>

          {runTarget && (
            <Panel title={`试运行 · ${runTarget.name}`} desc="提交异步任务，worker 执行过程会产生完整 trace 链路（思考/工具调用/委派）" actions={<button className="btn ghost sm" type="button" onClick={() => setRunTarget(null)}>关闭</button>}>
              <div style={{ display: 'flex', gap: 8 }}>
                <input className="input" style={{ flex: 1 }} value={runInput} onChange={(e) => setRunInput(e.target.value)} placeholder="给这个 Agent 的任务描述…" />
                <button className="btn primary sm" type="button" disabled={busy === 'run' || !runInput.trim()} onClick={() => submitRun(runTarget)}>
                  {busy === 'run' ? <Spinner text="提交" /> : '提交任务'}
                </button>
              </div>
              {runTask && (
                <Alert type="success">
                  任务 #{runTask.id} 已提交（状态：{runTask.status}）。前往侧边栏「任务中心」查看实时状态与链路。
                </Alert>
              )}
            </Panel>
          )}
        </>
      )}

      {tab === 'recall' && (
        <Panel title="语义召回" desc="需求描述 → Milvus 向量注册表 → 最匹配的 Agent（多 Agent 协作时由 LLM 据此选人）">
          <div style={{ display: 'flex', gap: 8 }}>
            <input
              className="input"
              style={{ flex: 1 }}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && searchAgents()}
              placeholder="例：帮我审查一份采购合同的风险条款"
            />
            <button className="btn primary sm" type="button" disabled={busy === 'recall' || !query.trim()} onClick={searchAgents}>
              {busy === 'recall' ? <Spinner text="检索" /> : <><IconSearch size={14} /> 检索</>}
            </button>
          </div>
          {hits.length > 0 && (
            <div className="table-wrap" style={{ marginTop: 16 }}>
              <table className="table">
                <thead>
                  <tr>
                    <th className="num">分数</th>
                    <th>Agent</th>
                    <th>职责</th>
                    <th className="num">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {hits.map((h) => (
                    <tr key={h.agent.id}>
                      <td className="num mono">{h.score.toFixed(3)}</td>
                      <td>
                        <div className="cell-main">{h.agent.name}</div>
                        <div className="cell-sub mono">{h.agent.agent_key}</div>
                      </td>
                      <td>{h.agent.description}</td>
                      <td className="num">
                        <button className="btn ghost sm" type="button" onClick={() => { setRunTarget(h.agent); setTab('agents'); }}>
                          试运行
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {query && hits.length === 0 && !busy && <EmptyState icon={IconBrain} title="没有匹配的 Agent" desc="换个说法，或先创建能干这活的 Agent" />}
        </Panel>
      )}
    </div>
  );
}
