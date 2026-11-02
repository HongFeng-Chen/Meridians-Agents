import { useEffect, useMemo, useRef, useState } from 'react';
import { Badge, GRAPH_GROUPS, GRAPH_HINTS } from './UI.jsx';

// AgentFlow DAG 编排画布（Dify 风格）：
//   · 节点：Agent 节点 / 条件节点 / 输出节点，位置可拖动
//   · 连线：从节点右侧圆点拖到目标节点；一条出边 = 串行，多条出边 = 并行扇出
//   · 多条入边 = 汇聚（等全部上游完成）；条件节点按「是 / 否」分支放行
//   · 点击连线删除；点击条件分支标签可互换 是/否
// 数据：{ nodes:[{id,kind,agent_key,label,x,y,cond}], edges:[{from,to,when}] }

const NODE_W = 152;
const NODE_H = 62;
const COND_H = 56;
const OUT_W = 92;
const OUT_H = 40;
const PAD = 10;
const CANVAS_H = 330;

let seq = 0;
const nextID = (kind) => {
  seq += 1;
  return `${kind}-${Date.now().toString(36).slice(-4)}${seq}`;
};

const sizeOf = (n) => {
  if (n.kind === 'output') return { w: OUT_W, h: OUT_H };
  if (n.kind === 'condition') return { w: NODE_W, h: COND_H };
  return { w: NODE_W, h: NODE_H };
};

const condSummary = (c) => {
  if (!c) return '未配置';
  if (c.mode === 'llm') return c.question ? `LLM：${c.question}` : 'LLM 判断';
  const ops = { contains: '包含', not_contains: '不包含', not_empty: '非空', is_empty: '为空', gt: '大于', lt: '小于' };
  return `${ops[c.op] || '非空'}${c.value ? `「${c.value}」` : ''}`;
};

export default function AgentFlow({ agents, flow, onChange, selfKey, disabled, nodeSettings, onCreateAgent, onSaveAgent }) {
  const canvasRef = useRef(null);
  const dragRef = useRef(null);
  const linkRef = useRef(null);
  const [cw, setCw] = useState(700);
  const [ch, setCh] = useState(CANVAS_H);
  const [full, setFull] = useState(false);
  const [selected, setSelected] = useState('');
  const [linkFrom, setLinkFrom] = useState('');
  const [cursor, setCursor] = useState({ x: 0, y: 0 });
  const [hint, setHint] = useState('');
  // 画布内快捷新建 Agent：创建后进入全局 Agent 列表，可被外层任意 Agent 引用（层级构造）
  const [createOpen, setCreateOpen] = useState(false);
  const [createDraft, setCreateDraft] = useState({ name: '', agent_key: '', description: '', graph_type: 'react' });
  const [createBusy, setCreateBusy] = useState(false);
  // 节点 Agent 基础数据面板：点击 Agent 节点加载该 Agent 数据，可编辑并保存
  const [draft, setDraft] = useState(null);
  const [saveBusy, setSaveBusy] = useState(false);

  const nodes = flow?.nodes || [];
  const edges = flow?.edges || [];

  useEffect(() => {
    const el = canvasRef.current;
    if (!el) return;
    const ro = new ResizeObserver((en) => {
      const w = en[0]?.contentRect?.width;
      const h = en[0]?.contentRect?.height;
      if (w) setCw(w);
      if (h) setCh(h);
    });
    ro.observe(el);
    setCw(el.clientWidth);
    setCh(el.clientHeight);
    return () => ro.disconnect();
  }, []);

  // 全屏时：ESC 退出全屏（capture 阶段拦截，避免同时触发弹层的 ESC 关闭）
  useEffect(() => {
    if (!full) return;
    const onKey = (e) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        e.preventDefault();
        setFull(false);
      }
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [full]);

  useEffect(() => {
    if (!hint) return;
    const t = setTimeout(() => setHint(''), 2600);
    return () => clearTimeout(t);
  }, [hint]);

  const clamp = (x, y, n) => {
    const { w, h } = sizeOf(n);
    return {
      x: Math.max(PAD, Math.min(cw - w - PAD, x)),
      y: Math.max(PAD, Math.min(ch - h - PAD, y)),
    };
  };

  // 渲染视图：把保存坐标夹取到当前画布尺寸内（画布变窄 / 外部数据超界时防止裁切）
  const view = nodes.map((n) => ({ ...n, ...clamp(n.x, n.y, n) }));

  const patch = (next) => onChange({ ...flow, ...next });
  const moveNode = (id, x, y) => patch({ nodes: nodes.map((n) => (n.id === id ? { ...n, x, y } : n)) });
  const updateNode = (id, data) => patch({ nodes: nodes.map((n) => (n.id === id ? { ...n, ...data } : n)) });

  const hitTest = (x, y, exceptID) => {
    for (let i = view.length - 1; i >= 0; i--) {
      const n = view[i];
      if (n.id === exceptID) continue;
      const { w, h } = sizeOf(n);
      if (x >= n.x && x <= n.x + w && y >= n.y && y <= n.y + h) return n;
    }
    return null;
  };

  // 拖动节点 / 拖出连线
  const onNodeDown = (e, n) => {
    if (disabled) return;
    e.stopPropagation();
    e.currentTarget.setPointerCapture(e.pointerId);
    const rect = canvasRef.current.getBoundingClientRect();
    dragRef.current = { id: n.id, dx: e.clientX - rect.left - n.x, dy: e.clientY - rect.top - n.y };
    setSelected(n.id);
  };

  const onCanvasMove = (e) => {
    const rect = canvasRef.current.getBoundingClientRect();
    const px = e.clientX - rect.left;
    const py = e.clientY - rect.top;
    if (dragRef.current) {
      const { id, dx, dy } = dragRef.current;
      const n = view.find((x) => x.id === id);
      if (!n) return;
      const pos = clamp(px - dx, py - dy, n);
      moveNode(id, Math.round(pos.x), Math.round(pos.y));
      return;
    }
    if (linkRef.current) setCursor({ x: px, y: py });
  };

  const onCanvasUp = () => {
    dragRef.current = null;
    if (!linkRef.current) return;
    const from = linkRef.current;
    linkRef.current = null;
    setLinkFrom('');
    const target = hitTest(cursor.x, cursor.y, from);
    if (target) addEdge(from, target.id);
  };

  const startLink = (e, n) => {
    if (disabled) return;
    e.stopPropagation();
    e.currentTarget.setPointerCapture(e.pointerId);
    const rect = canvasRef.current.getBoundingClientRect();
    linkRef.current = n.id;
    setLinkFrom(n.id);
    setCursor({ x: e.clientX - rect.left, y: e.clientY - rect.top });
    setSelected(n.id);
  };

  // fromID 是否已能沿现有边走到 toID（用于阻止成环）
  const reaches = (fromID, toID) => {
    const stack = [fromID];
    const seen = new Set();
    while (stack.length) {
      const cur = stack.pop();
      if (cur === toID) return true;
      if (seen.has(cur)) continue;
      seen.add(cur);
      edges.filter((x) => x.from === cur).forEach((x) => stack.push(x.to));
    }
    return false;
  };

  const addEdge = (from, to) => {
    if (from === to) return;
    if (edges.some((e) => e.from === from && e.to === to)) {
      setHint('这两点之间已经有连线了');
      return;
    }
    if (reaches(to, from)) {
      setHint('该连线会形成环，已阻止');
      return;
    }
    const src = nodes.find((n) => n.id === from);
    let when;
    if (src?.kind === 'condition') {
      const hasTrue = edges.some((e) => e.from === from && (e.when === 'true' || !e.when));
      when = hasTrue ? 'false' : 'true';
    }
    patch({ edges: [...edges, { from, to, ...(when ? { when } : {}) }] });
  };

  const removeEdge = (idx) => {
    if (disabled) return;
    patch({ edges: edges.filter((_, i) => i !== idx) });
  };

  const toggleWhen = (idx) => {
    if (disabled) return;
    patch({ edges: edges.map((e, i) => (i === idx ? { ...e, when: e.when === 'false' ? 'true' : 'false' } : e)) });
  };

  const addNode = (kind, agentKey) => {
    if (disabled) return;
    if (kind === 'output' && nodes.some((n) => n.kind === 'output')) {
      setHint('输出节点只能有一个');
      return;
    }
    const anchor = nodes.find((n) => n.id === selected);
    const idx = nodes.length;
    const draft = {
      id: nextID(kind),
      kind,
      label: kind === 'output' ? '输出' : kind === 'condition' ? '条件判断' : '',
      agent_key: agentKey || '',
      x: Math.max(PAD, Math.min(cw - 160, anchor ? anchor.x + 190 : 40 + (idx % 4) * 40)),
      y: Math.max(PAD, Math.min(ch - 80, anchor ? anchor.y : 40 + (idx % 3) * 80)),
      ...(kind === 'condition' ? { cond: { mode: 'rule', op: 'contains', value: '', question: '' } } : {}),
    };
    let nextEdges = edges;
    if (anchor) {
      let when;
      if (anchor.kind === 'condition') {
        const hasTrue = edges.some((e) => e.from === anchor.id && (e.when === 'true' || !e.when));
        when = hasTrue ? 'false' : 'true';
      }
      nextEdges = [...edges, { from: anchor.id, to: draft.id, ...(when ? { when } : {}) }];
    }
    onChange({ ...flow, nodes: [...nodes, draft], edges: nextEdges });
    setSelected(draft.id);
  };

  const removeNode = (id) => {
    if (disabled) return;
    onChange({
      ...flow,
      nodes: nodes.filter((n) => n.id !== id),
      edges: edges.filter((e) => e.from !== id && e.to !== id),
    });
    setSelected('');
  };

  // 画布内新建 Agent：走全局 createAgentDef（role=sub），成功后自动加为画布节点
  const submitCreate = async (e) => {
    e.preventDefault();
    if (!onCreateAgent || createBusy) return;
    const name = createDraft.name.trim();
    const key = createDraft.agent_key.trim().toLowerCase();
    if (!name) {
      setHint('请填写 Agent 名称');
      return;
    }
    if (!/^[a-z][a-z0-9_-]{1,63}$/.test(key)) {
      setHint('标识需小写字母开头，2~64 位，仅含小写字母 / 数字 / - / _');
      return;
    }
    setCreateBusy(true);
    try {
      const res = await onCreateAgent({ ...createDraft, name, agent_key: key });
      if (!res) return; // 失败时错误已由外层提示
      setCreateOpen(false);
      setCreateDraft({ name: '', agent_key: '', description: '', graph_type: 'react' });
      setHint(`已创建 Agent「${res.name}」，可被其他 Agent 引用`);
      addNode('agent', res.agent_key);
    } finally {
      setCreateBusy(false);
    }
  };

  // 按拓扑层级自动排布：列宽 186，行高 82
  const autoLayout = () => {
    const indeg = new Map(nodes.map((n) => [n.id, 0]));
    edges.forEach((e) => indeg.set(e.to, (indeg.get(e.to) || 0) + 1));
    const level = new Map();
    let queue = nodes.filter((n) => (indeg.get(n.id) || 0) === 0).map((n) => n.id);
    queue.forEach((id) => level.set(id, 0));
    let guard = 0;
    while (queue.length && guard++ < 200) {
      const next = [];
      queue.forEach((id) => {
        edges.filter((e) => e.from === id).forEach((e) => {
          level.set(e.to, Math.max(level.get(e.to) ?? 0, (level.get(id) || 0) + 1));
          next.push(e.to);
        });
      });
      queue = [...new Set(next)];
    }
    const rows = new Map();
    patch({
      nodes: nodes.map((n) => {
        const lv = level.get(n.id) || 0;
        const row = rows.get(lv) || 0;
        rows.set(lv, row + 1);
        return { ...n, x: PAD + lv * 186, y: 24 + row * 82 };
      }),
    });
  };

  const anchors = (n) => {
    const { w, h } = sizeOf(n);
    return { out: { x: n.x + w, y: n.y + h / 2 }, in: { x: n.x, y: n.y + h / 2 } };
  };

  const bezier = (a, b) => {
    const dx = Math.max(40, Math.abs(b.x - a.x) / 2);
    return `M ${a.x} ${a.y} C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`;
  };

  const midPoint = (a, b) => {
    const dx = Math.max(40, Math.abs(b.x - a.x) / 2);
    const p1 = { x: a.x + dx, y: a.y };
    const p2 = { x: b.x - dx, y: b.y };
    return {
      x: (a.x + 3 * p1.x + 3 * p2.x + b.x) / 8,
      y: (a.y + 3 * p1.y + 3 * p2.y + b.y) / 8,
    };
  };

  const linkPath = useMemo(() => {
    if (!linkFrom) return '';
    const src = view.find((n) => n.id === linkFrom);
    return src ? bezier(anchors(src).out, cursor) : '';
  }, [linkFrom, cursor, view]);

  const outputCount = nodes.filter((n) => n.kind === 'output').length;
  const agentNodes = nodes.filter((n) => n.kind === 'agent');
  const pool = agents.filter((a) => a.agent_key !== selfKey && !agentNodes.some((n) => n.agent_key === a.agent_key));
  const selNode = nodes.find((n) => n.id === selected);
  const selDef = selNode?.kind === 'agent' ? agents.find((a) => a.agent_key === selNode.agent_key) : null;
  const selDefKey = selDef?.agent_key || '';
  const selDefVersion = selDef?.version;

  // 选中节点变化 / 该 Agent 保存成功（version 变化）后，同步面板草稿
  useEffect(() => {
    if (!selDef) {
      setDraft(null);
      return;
    }
    setDraft({
      name: selDef.name || '',
      role: selDef.role || 'sub',
      graph_type: selDef.graph_type || 'react',
      description: selDef.description || '',
      system: selDef.system || '',
      llm_model: selDef.llm_model || '',
      max_turns: selDef.max_turns ? String(selDef.max_turns) : '12',
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selDefKey, selDefVersion]);

  const saveSel = async () => {
    if (!onSaveAgent || !selDef || !draft || saveBusy) return;
    if (!draft.name.trim()) {
      setHint('名称必填');
      return;
    }
    setSaveBusy(true);
    try {
      const res = await onSaveAgent(selDef, draft);
      if (res) setHint(`已保存「${res.name}」（v${res.version}）`);
    } finally {
      setSaveBusy(false);
    }
  };

  return (
    <div className={`flow-wrap${full ? ' flow-full' : ''}`}>
      {/* 节点数据管理：监察 / 风评 / 定时，随编排组件一起编辑 */}
      {nodeSettings && (
        <div className="flow-settings">
          <div className="flow-settings-head">
            <span className="field-label" style={{ margin: 0 }}>节点设置</span>
            <span className="cell-sub">监察、风险评估与定时调度属于该节点的编排数据</span>
          </div>
          <div className="flow-settings-grid">
            <label className="field inline">
              <span className="field-label">节点模型数据源（与基本信息同步）</span>
              <select
                className="input"
                value={nodeSettings.llmSource || ''}
                onChange={(e) => nodeSettings.onUpdate({ llm_source: e.target.value })}
              >
                <option value="">平台默认（default 数据源优先）</option>
                {(nodeSettings.llmSourceOptions || []).map((p) => (
                  <option key={p.name} value={p.name}>{p.name}{p.default_model ? ` · ${p.default_model}` : ''}</option>
                ))}
              </select>
              <span className="cell-sub">该源调用失败自动回退 default，任务不中断</span>
            </label>
            <label className="field inline">
              <span className="field-label">监察 Agent（可空）</span>
              <select
                className="input"
                value={nodeSettings.supervisorKey || ''}
                onChange={(e) => nodeSettings.onUpdate({ supervisor_key: e.target.value })}
              >
                <option value="">不监察</option>
                {(nodeSettings.supervisorOptions || []).map((a) => (
                  <option key={a.id} value={a.agent_key}>{a.name}（{a.agent_key}）</option>
                ))}
              </select>
            </label>
            <label className="field inline">
              <span className="field-label">风险评估 Agent（仅 MCP 调用前）</span>
              <select
                className="input"
                value={nodeSettings.riskKey || ''}
                onChange={(e) => nodeSettings.onUpdate({ risk_agent_key: e.target.value })}
              >
                <option value="">不评估</option>
                {(nodeSettings.riskOptions || []).map((a) => (
                  <option key={a.id} value={a.agent_key}>{a.name}（{a.agent_key}）</option>
                ))}
              </select>
            </label>
          </div>
          <label className="flow-check">
            <input
              type="checkbox"
              checked={!!nodeSettings.autoRun}
              onChange={(e) => nodeSettings.onUpdate({ auto_run: e.target.checked })}
            />
            <span>后台自动运行（按 cron 周期自动提交任务）</span>
          </label>
          {!!nodeSettings.autoRun && (
            <>
              <div className="flow-settings-grid">
                <label className="field inline">
                  <span className="field-label">cron 表达式（分 时 日 月 周）</span>
                  <input
                    className="input mono"
                    value={nodeSettings.cronExpr || ''}
                    placeholder="0 9 * * *"
                    onChange={(e) => nodeSettings.onUpdate({ cron_expr: e.target.value })}
                  />
                </label>
                <div className="field inline">
                  <span className="field-label">常用模板</span>
                  <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginTop: 6 }}>
                    {[
                      ['每小时', '0 * * * *'],
                      ['每天 9 点', '0 9 * * *'],
                      ['每 10 分钟', '*/10 * * * *'],
                      ['工作日 9 点', '0 9 * * 1-5'],
                    ].map(([label, expr]) => (
                      <button
                        key={expr}
                        type="button"
                        className={`btn ghost sm${nodeSettings.cronExpr === expr ? ' active' : ''}`}
                        onClick={() => nodeSettings.onUpdate({ cron_expr: expr })}
                      >
                        {label}
                      </button>
                    ))}
                  </div>
                </div>
              </div>
              <label className="field inline" style={{ display: 'block' }}>
                <span className="field-label">定时运行的输入内容</span>
                <textarea
                  className="input"
                  rows={2}
                  value={nodeSettings.cronInput || ''}
                  placeholder="例如：汇总今天的工单并给出优先级建议"
                  onChange={(e) => nodeSettings.onUpdate({ cron_input: e.target.value })}
                />
              </label>
              {nodeSettings.nextRunAt && (
                <div className="cell-sub">下次运行：{nodeSettings.nextRunAt}</div>
              )}
            </>
          )}
        </div>
      )}

      <div className="flow-toolbar">
        <span className="cell-sub">
          {nodes.length === 0
            ? '从下方添加 Agent / 条件 / 输出节点，拖动节点右侧圆点连线'
            : `一个节点多条出边 = 并行，多条入边 = 汇聚 · ${edges.length} 条连线`}
        </span>
        <div style={{ flex: 1 }} />
        {hint && <span className="flow-hint">{hint}</span>}
        {onCreateAgent && (
          <button
            className="btn ghost sm"
            type="button"
            disabled={disabled}
            title="快速新建子 Agent：创建后进入全局列表，可被外层任意 Agent 编排引用"
            onClick={() => setCreateOpen((v) => !v)}
          >
            ＋ 新增 Agent
          </button>
        )}
        <button className="btn ghost sm" type="button" disabled={disabled || nodes.length === 0} onClick={autoLayout}>
          自动排列
        </button>
        <button
          className="btn ghost sm"
          type="button"
          title={full ? '退出全屏（ESC）' : '全屏编排，画布铺满整个窗口'}
          onClick={() => setFull((v) => !v)}
        >
          {full ? '⤡ 退出全屏' : '⤢ 全屏编排'}
        </button>
      </div>

      {createOpen && onCreateAgent && (
        <form className="flow-create" onSubmit={submitCreate}>
          <div className="flow-inspector-head">
            <span className="field-label" style={{ margin: 0 }}>新建子 Agent</span>
            <span className="cell-sub">创建后保存到全局 Agent 列表，可被外层任意 Agent 编排引用，构造层级</span>
          </div>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10, marginTop: 8 }}>
            <label className="field inline">
              <span className="field-label">名称</span>
              <input
                className="input"
                autoFocus
                placeholder="例如：资料研究员"
                value={createDraft.name}
                onChange={(e) => setCreateDraft({ ...createDraft, name: e.target.value })}
              />
            </label>
            <label className="field inline">
              <span className="field-label">标识（全局唯一）</span>
              <input
                className="input mono"
                placeholder="issue-researcher"
                value={createDraft.agent_key}
                onChange={(e) => setCreateDraft({ ...createDraft, agent_key: e.target.value })}
              />
            </label>
          </div>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 150px', gap: 10, marginTop: 10 }}>
            <label className="field inline">
              <span className="field-label">职责描述（同时作为角色提示词，可后续编辑完善）</span>
              <input
                className="input"
                placeholder="擅长什么、输出什么"
                value={createDraft.description}
                onChange={(e) => setCreateDraft({ ...createDraft, description: e.target.value })}
              />
            </label>
            <label className="field inline">
              <span className="field-label">执行图</span>
              <select
                className="input"
                value={createDraft.graph_type}
                onChange={(e) => setCreateDraft({ ...createDraft, graph_type: e.target.value })}
              >
                <option value="react">react · 循环</option>
                <option value="simple">simple · 直答</option>
              </select>
            </label>
          </div>
          <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 8, marginTop: 10 }}>
            <button className="btn ghost sm" type="button" disabled={createBusy} onClick={() => setCreateOpen(false)}>
              取消
            </button>
            <button className="btn primary sm" type="submit" disabled={createBusy}>
              {createBusy ? '创建中…' : '创建并加入画布'}
            </button>
          </div>
        </form>
      )}

      <div className="flow-body">
        <div className="flow-main">
          <div
            ref={canvasRef}
            className="flow-canvas"
            style={{ height: full ? undefined : CANVAS_H, flex: full ? '1 1 auto' : undefined, minHeight: full ? 220 : undefined }}
            onPointerMove={onCanvasMove}
            onPointerUp={onCanvasUp}
            onPointerLeave={onCanvasUp}
            onClick={() => setSelected('')}
          >
        <svg className="flow-edges" width={cw} height={ch} aria-hidden="true">
          {edges.map((e, i) => {
            const from = view.find((n) => n.id === e.from);
            const to = view.find((n) => n.id === e.to);
            if (!from || !to) return null;
            const a = anchors(from).out;
            const b = anchors(to).in;
            const d = bezier(a, b);
            const mid = midPoint(a, b);
            const label = from.kind === 'condition' ? (e.when === 'false' ? '否' : '是') : '';
            return (
              <g key={`${e.from}-${e.to}-${i}`}>
                <path d={d} className="flow-edge-hit" onClick={(ev) => { ev.stopPropagation(); removeEdge(i); }} />
                <path d={d} className="flow-edge" markerEnd="url(#flow-arrow)" />
                {label && (
                  <g className="flow-edge-label" onClick={(ev) => { ev.stopPropagation(); toggleWhen(i); }}>
                    <rect x={mid.x - 11} y={mid.y - 9} width={22} height={18} rx={9} />
                    <text x={mid.x} y={mid.y + 4} textAnchor="middle">{label}</text>
                  </g>
                )}
              </g>
            );
          })}
          {linkPath && <path d={linkPath} className="flow-edge temp" />}
          <defs>
            <marker id="flow-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto">
              <path d="M 0 0 L 10 5 L 0 10 z" fill="#b9bfd0" />
            </marker>
          </defs>
        </svg>

        {view.map((n) => (
          <div
            key={n.id}
            className={`flow-node kind-${n.kind}${selected === n.id ? ' selected' : ''}`}
            style={{ left: n.x, top: n.y, width: sizeOf(n).w, height: sizeOf(n).h }}
            onPointerDown={(e) => onNodeDown(e, n)}
            onClick={(e) => { e.stopPropagation(); setSelected(n.id); }}
          >
            {n.kind === 'output' ? (
              <div className="flow-node-out">输出</div>
            ) : (
              <>
                <div className="flow-node-head">
                  <span className="flow-node-name" title={n.kind === 'condition' ? condSummary(n.cond) : n.agent_key}>
                    {n.kind === 'condition' ? '条件判断' : n.label || n.agent_key}
                  </span>
                  {!disabled && (
                    <button
                      className="flow-node-del"
                      type="button"
                      title="删除节点"
                      onPointerDown={(e) => e.stopPropagation()}
                      onClick={(e) => { e.stopPropagation(); removeNode(n.id); }}
                    >
                      ✕
                    </button>
                  )}
                </div>
                <div className="flow-node-sub">{n.kind === 'condition' ? condSummary(n.cond) : n.agent_key}</div>
                <div className="flow-node-foot">
                  {n.kind === 'condition' ? <Badge tone="violet">条件</Badge> : <Badge tone="default">Agent</Badge>}
                  <span className="cell-sub">
                    {edges.filter((e) => e.to === n.id).length} 入 · {edges.filter((e) => e.from === n.id).length} 出
                  </span>
                </div>
              </>
            )}
            <span className="flow-port in" />
            {n.kind !== 'output' && (
              <span
                className={`flow-port out${linkFrom === n.id ? ' active' : ''}`}
                title="拖动连线到目标节点"
                onPointerDown={(e) => startLink(e, n)}
              />
            )}
          </div>
        ))}

        {nodes.length === 0 && <div className="flow-empty">画布还是空的：从下面添加第一个节点</div>}
          </div>

          {selNode && selNode.kind === 'condition' && (
        <div className="flow-inspector">
          <div className="flow-inspector-head">
            <span className="field-label" style={{ margin: 0 }}>条件判断配置</span>
            <div className="flow-seg">
              {[['rule', '规则'], ['llm', 'LLM 判断']].map(([m, label]) => (
                <button
                  key={m}
                  type="button"
                  className={`flow-seg-btn${(selNode.cond?.mode || 'rule') === m ? ' active' : ''}`}
                  onClick={() => updateNode(selNode.id, { cond: { ...selNode.cond, mode: m } })}
                >
                  {label}
                </button>
              ))}
            </div>
          </div>
          {(selNode.cond?.mode || 'rule') === 'rule' ? (
            <div style={{ display: 'grid', gridTemplateColumns: '150px 1fr', gap: 10, marginTop: 8 }}>
              <select
                className="input"
                value={selNode.cond?.op || 'contains'}
                onChange={(e) => updateNode(selNode.id, { cond: { ...selNode.cond, op: e.target.value } })}
              >
                <option value="contains">包含关键字</option>
                <option value="not_contains">不包含关键字</option>
                <option value="not_empty">内容非空</option>
                <option value="is_empty">内容为空</option>
                <option value="gt">数值大于</option>
                <option value="lt">数值小于</option>
              </select>
              <input
                className="input"
                placeholder="比较值（包含 / 数值类规则填写）"
                value={selNode.cond?.value || ''}
                disabled={['not_empty', 'is_empty'].includes(selNode.cond?.op)}
                onChange={(e) => updateNode(selNode.id, { cond: { ...selNode.cond, value: e.target.value } })}
              />
            </div>
          ) : (
            <input
              className="input"
              style={{ marginTop: 8 }}
              placeholder="交给模型判断的问题，例如：上游内容是否已包含明确结论？"
              value={selNode.cond?.question || ''}
              onChange={(e) => updateNode(selNode.id, { cond: { ...selNode.cond, question: e.target.value } })}
            />
          )}
          <div className="cell-sub" style={{ marginTop: 6 }}>
            判断对象是汇聚上来的上游产出；两条出边分别标记「是 / 否」，点击连线上的标签可互换。
          </div>
        </div>
      )}

          {/* 节点 Agent 基础数据面板：点选画布 Agent 节点后加载，可编辑并保存 */}
          {selDef && draft && onSaveAgent && (
            <div className="flow-agent-pane">
              <div className="flow-agent-pane-head">
                <span className="field-label" style={{ margin: 0 }}>Agent 基础数据</span>
                <span className="cell-sub mono">{selDef.agent_key}</span>
              </div>
              <div className="flow-agent-pane-body">
                <label className="field inline">
                  <span className="field-label">标识</span>
                  <input className="input mono" value={selDef.agent_key} disabled />
                </label>
                <label className="field inline">
                  <span className="field-label">名称</span>
                  <input className="input" value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
                </label>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8 }}>
                  <label className="field inline">
                    <span className="field-label">角色</span>
                    <select className="input" value={draft.role} onChange={(e) => setDraft({ ...draft, role: e.target.value })}>
                      <option value="root">根 Agent</option>
                      <option value="sub">子 Agent</option>
                      <option value="supervisor">监察 Agent</option>
                      <option value="risk">风险评估</option>
                    </select>
                  </label>
                  <label className="field inline">
                    <span className="field-label">最大轮数（1~40）</span>
                    <input className="input" type="number" min="1" max="40" value={draft.max_turns} onChange={(e) => setDraft({ ...draft, max_turns: e.target.value })} />
                  </label>
                </div>
                <label className="field inline">
                  <span className="field-label">执行图（架构模式）</span>
                  <select className="input" value={draft.graph_type} onChange={(e) => setDraft({ ...draft, graph_type: e.target.value })}>
                    {GRAPH_GROUPS.map((g) => (
                      <optgroup key={g.label} label={g.label}>
                        {g.options.map(([v, l]) => (
                          <option key={v} value={v}>{l}</option>
                        ))}
                      </optgroup>
                    ))}
                  </select>
                </label>
                {GRAPH_HINTS[draft.graph_type] && <div className="cell-sub" style={{ marginTop: -4 }}>{GRAPH_HINTS[draft.graph_type]}</div>}
                <label className="field inline">
                  <span className="field-label">职责描述（语义召回来源）</span>
                  <textarea className="input" rows={2} value={draft.description} onChange={(e) => setDraft({ ...draft, description: e.target.value })} />
                </label>
                <label className="field inline">
                  <span className="field-label">角色提示词（人设 / 工作方法 / 边界）</span>
                  <textarea className="input" rows={5} value={draft.system} onChange={(e) => setDraft({ ...draft, system: e.target.value })} />
                </label>
                <label className="field inline">
                  <span className="field-label">指定模型（留空 = 平台默认）</span>
                  <input className="input mono" placeholder="留空 = 平台默认" value={draft.llm_model} onChange={(e) => setDraft({ ...draft, llm_model: e.target.value })} />
                </label>
              </div>
              <div className="flow-agent-pane-foot">
                <span className="cell-sub">能力白名单等完整配置请到列表「编辑」</span>
                <button className="btn primary sm" type="button" disabled={saveBusy} onClick={saveSel}>
                  {saveBusy ? '保存中…' : '保存'}
                </button>
              </div>
            </div>
          )}
        </div>
      </div>

      <div className="flow-pool">
        <span className="field-label" style={{ marginBottom: 6, display: 'block' }}>
          添加节点（先选中某个节点，添加时会自动接线）
        </span>
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, maxHeight: 104, overflow: 'auto' }}>
          {pool.map((a) => (
            <button
              key={a.id}
              type="button"
              disabled={disabled}
              className="btn ghost sm"
              title={a.description}
              onClick={() => addNode('agent', a.agent_key)}
            >
              + {a.name}
              <span className="cell-sub mono" style={{ marginLeft: 6 }}>{a.agent_key}</span>
            </button>
          ))}
          <button className="btn ghost sm" type="button" disabled={disabled} onClick={() => addNode('condition')}>
            + 条件判断
          </button>
          <button
            className="btn ghost sm"
            type="button"
            disabled={disabled || outputCount > 0}
            title={outputCount > 0 ? '输出节点只能有一个' : '汇总输出'}
            onClick={() => addNode('output')}
          >
            + 输出
          </button>
        </div>
      </div>
    </div>
  );
}
