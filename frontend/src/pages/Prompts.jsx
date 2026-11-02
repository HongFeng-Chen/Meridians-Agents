import { useCallback, useEffect, useMemo, useState } from 'react';
import { agentfactory, prompt } from '../api.js';
import { Alert, Badge, EmptyState, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import { IconPrompt, IconRefresh, IconSpark } from '../components/Icons.jsx';

const SCENES = ['agent', 'rag', 'graph', 'extract', 'memory', 'general'];

const PAGE_TABS = [
  { key: 'tpl', label: '模板管理' },
  { key: 'agent', label: 'Agent 提示词' },
];

// ===== Agent 角色提示词工程面板（LLM 打分 / 优化，结果可直接保存回 Agent）=====

const STUDIO_GRADE_TONE = { A: 'ok', B: 'ok', C: 'warn', D: 'error' };

function StudioScoreBar({ score }) {
  const color = score >= 80 ? '#16a34a' : score >= 60 ? '#d97706' : '#dc2626';
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
      <span className="mono" style={{ width: 34, textAlign: 'right', fontSize: 12 }}>{score}</span>
      <div style={{ flex: 1, height: 6, borderRadius: 3, background: 'rgba(127,127,127,.18)', overflow: 'hidden' }}>
        <div style={{ width: `${Math.max(0, Math.min(100, score))}%`, height: '100%', borderRadius: 3, background: color }} />
      </div>
    </div>
  );
}

function StudioPanel({ studio, onApply, onClose, onSuggestion, onApplySave, applySaving }) {
  if (!studio) return null;
  const isScore = studio.kind === 'score';
  const isOpt = studio.kind === 'optimize';
  const title = isScore ? 'AI 打分报告' : 'AI 优化结果';
  return (
    <div style={{ marginTop: 10, border: '1px solid rgba(127,127,127,.28)', borderRadius: 10, padding: 14, background: 'rgba(127,127,127,.06)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <strong style={{ fontSize: 13 }}>{title}</strong>
        {studio.model && <span className="cell-sub mono">{studio.model}</span>}
        <span style={{ flex: 1 }} />
        {isOpt && onApplySave && (
          <button className="btn primary sm" type="button" disabled={applySaving} onClick={onApplySave}>
            {applySaving ? <Spinner text="保存中" /> : '采用并保存回 Agent'}
          </button>
        )}
        <button className="btn ghost sm" type="button" onClick={onApply}>采用到编辑区</button>
        <button className="btn ghost sm" type="button" onClick={onClose}>关闭</button>
      </div>

      {isScore && (
        <>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 10, margin: '10px 0 6px' }}>
            <span style={{ fontSize: 30, fontWeight: 700 }}>{studio.total}</span>
            <Badge tone={STUDIO_GRADE_TONE[studio.grade] || 'info'}>等级 {studio.grade}</Badge>
            {studio.summary && <span className="cell-sub">{studio.summary}</span>}
          </div>
          {(studio.dimensions || []).map((d) => (
            <div key={d.name} style={{ display: 'grid', gridTemplateColumns: '80px 1fr', gap: 10, alignItems: 'center', marginBottom: 4 }}>
              <span className="cell-sub" style={{ fontSize: 12 }}>{d.name}</span>
              <div>
                <StudioScoreBar score={d.score} />
                {d.comment && <div className="cell-sub" style={{ fontSize: 12, marginTop: 2 }}>{d.comment}</div>}
              </div>
            </div>
          ))}
          {(studio.issues || []).length > 0 && (
            <div style={{ marginTop: 10 }}>
              <div className="field-label" style={{ marginBottom: 4 }}>主要问题</div>
              {studio.issues.map((s, i) => <div key={i} className="cell-sub" style={{ marginBottom: 2 }}>• {s}</div>)}
            </div>
          )}
          {(studio.suggestions || []).length > 0 && (
            <div style={{ marginTop: 10 }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
                <div className="field-label">改进建议</div>
                {onSuggestion && (
                  <button
                    className="btn ghost sm"
                    type="button"
                    onClick={() => onSuggestion([...(studio.suggestions || []), ...(studio.issues || [])].join('；'))}
                  >
                    全部应用为优化目标
                  </button>
                )}
              </div>
              {studio.suggestions.map((s, i) => (
                <div key={i} style={{ display: 'flex', alignItems: 'flex-start', gap: 6, marginBottom: 2 }}>
                  <span className="cell-sub" style={{ flex: 1 }}>• {s}</span>
                  {onSuggestion && (
                    <button className="btn ghost sm" type="button" onClick={() => onSuggestion(s)} title="填入优化目标">
                      → 优化目标
                    </button>
                  )}
                </div>
              ))}
            </div>
          )}
        </>
      )}

      {isOpt && (
        <>
          <div style={{ margin: '8px 0 4px' }} className="cell-sub">
            评分：<strong style={{ color: '#d97706' }}>{studio.score_before}</strong> → <strong style={{ color: '#16a34a' }}>{studio.score_after}</strong>
            {onApplySave && <span>　·　点右上「采用并保存回 Agent」可直接生效（Agent 生成新版本）</span>}
          </div>
          {(studio.changes || []).length > 0 && (
            <div style={{ marginTop: 4 }}>
              <div className="field-label" style={{ marginBottom: 4 }}>改动说明</div>
              {studio.changes.map((s, i) => <div key={i} className="cell-sub" style={{ marginBottom: 2 }}>• {s}</div>)}
            </div>
          )}
          <details style={{ marginTop: 10 }} open>
            <summary className="cell-sub" style={{ cursor: 'pointer' }}>优化后提示词全文</summary>
            <pre className="mono" style={{ whiteSpace: 'pre-wrap', fontSize: 12, margin: '8px 0 0', padding: 10, borderRadius: 8, background: 'rgba(127,127,127,.1)', maxHeight: 260, overflow: 'auto' }}>{studio.prompt}</pre>
          </details>
        </>
      )}
    </div>
  );
}

export default function Prompts() {
  const [stats, setStats] = useState(null);
  const [list, setList] = useState([]);
  const [scene, setScene] = useState('');
  const [keyword, setKeyword] = useState('');
  const [selected, setSelected] = useState('');
  const [detail, setDetail] = useState(null);
  const [versions, setVersions] = useState([]);
  const [draft, setDraft] = useState('');
  const [note, setNote] = useState('');
  const [vScores, setVScores] = useState({}); // version → 最新评测得分（harness 回写）
  const [vars, setVars] = useState({});
  const [preview, setPreview] = useState(null);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState({ key: '', name: '', scene: 'rag', content: '' });

  // Agent 角色提示词工程：选 Agent → AI 打分 / 优化 → 保存回 Agent
  const [agents, setAgents] = useState([]);
  const [agentKey, setAgentKey] = useState('');
  const [agentSystem, setAgentSystem] = useState('');
  const [agentGoal, setAgentGoal] = useState('');
  const [studio, setStudio] = useState(null); // { kind: 'score'|'optimize', ... }
  const [tab, setTab] = useState('tpl'); // 页签：tpl=模板管理（列表+版本历史）/ agent=Agent 提示词

  const selAgent = useMemo(() => agents.find((a) => a.agent_key === agentKey) || null, [agents, agentKey]);

  // 模板列表分页：筛选条件变化时回到第 1 页
  const listPg = usePagedList(list, { size: 10, resetKey: `${scene}|${keyword}` });

  const loadList = useCallback(async () => {
    setError('');
    try {
      const [st, data] = await Promise.all([
        prompt.getPromptStats(),
        prompt.listPrompts({ scene, q: keyword, limit: 200 }),
      ]);
      setStats(st);
      const items = data.prompts || [];
      setList(items);
      setSelected((cur) => (items.some((p) => p.key === cur) ? cur : items[0]?.key || ''));
    } catch (err) {
      setError(err.message);
    }
  }, [scene, keyword]);

  useEffect(() => {
    loadList();
  }, [loadList]);

  const loadDetail = useCallback(async (key) => {
    if (!key) {
      setDetail(null);
      setVersions([]);
      return;
    }
    try {
      const [d, vs] = await Promise.all([
        prompt.getPrompt({ key }),
        prompt.listVersions({ key, limit: 50 }),
      ]);
      setDetail(d);
      setVersions(vs.versions || []);
      setDraft(d.active?.content ?? '');
      setNote('');
      setPreview(null);
      setVars({});
      // 版本评测得分（每个版本取最新一条）
      try {
        const sc = await prompt.listPromptScores({ key, limit: 100 });
        const map = {};
        (sc.scores || []).forEach((s) => {
          if (map[s.version] === undefined) map[s.version] = s; // 列表时间倒序，首个即最新
        });
        setVScores(map);
      } catch {
        setVScores({});
      }
      // 版本评测得分（每个版本取最新一条）
      try {
        const sc = await prompt.listPromptScores({ key, limit: 100 });
        const map = {};
        (sc.scores || []).forEach((s) => {
          if (map[s.version] === undefined) map[s.version] = s; // 列表时间倒序，首个即最新
        });
        setVScores(map);
      } catch {
        setVScores({});
      }
    } catch (err) {
      setError(err.message);
    }
  }, []);

  useEffect(() => {
    loadDetail(selected);
  }, [selected, loadDetail]);

  // —— Agent 角色提示词工程 ——

  const loadAgents = useCallback(async () => {
    try {
      const res = await agentfactory.listAgentDefs({ limit: 200 });
      setAgents(res.agents || []);
      setAgentKey((cur) => cur || res.agents?.[0]?.agent_key || '');
    } catch (err) {
      setError(err.message);
    }
  }, []);

  useEffect(() => {
    loadAgents();
  }, [loadAgents]);

  // 支持 #/prompts?agent=xxx 深链：Agent 管理页跳转过来直接切到 Agent 页签并选中对应 Agent
  useEffect(() => {
    const q = new URLSearchParams(window.location.hash.split('?')[1] || '');
    const k = q.get('agent');
    if (k) {
      setAgentKey(k);
      setTab('agent');
    }
  }, []);

  // 选中 Agent 变化 → 加载其角色提示词到编辑区
  useEffect(() => {
    setAgentSystem(selAgent?.system || '');
    setStudio(null);
  }, [selAgent]);

  const agentScore = () =>
    run('agent-score', async () => {
      const r = await prompt.scorePrompt({
        prompt: agentSystem,
        task: selAgent?.description || '',
        llm_source: selAgent?.llm_source || '',
      });
      setStudio({ kind: 'score', ...r });
    });

  const agentOptimize = () =>
    run('agent-optimize', async () => {
      const r = await prompt.optimizePrompt({
        prompt: agentSystem,
        task: selAgent?.description || '',
        goal: agentGoal,
        llm_source: selAgent?.llm_source || '',
      });
      setStudio({ kind: 'optimize', ...r });
    });

  const adoptStudio = () => {
    if (!studio?.prompt) return;
    setAgentSystem(studio.prompt);
    setNotice('已采用到编辑区，点「保存回 Agent」生效');
  };

  // 打分报告的改进建议 → 一键填入优化目标（可逐条或全部）
  const applySuggestion = (text) => {
    setAgentGoal(text);
    setStudio((cur) => (cur?.kind === 'score' ? null : cur));
    setNotice('已填入优化目标，点「AI 优化」执行');
    window.scrollTo({ top: 0, behavior: 'smooth' });
  };

  const saveAgentSystem = (content) =>
    run('agent-save', async () => {
      const ag = selAgent;
      if (!ag) return;
      const flow = typeof ag.flow === 'string' ? ag.flow : JSON.stringify(ag.flow || { nodes: [], edges: [] });
      await agentfactory.updateAgentDef({
        id: ag.id,
        input: {
          agent_key: ag.agent_key,
          name: ag.name,
          description: ag.description || '',
          system: content ?? agentSystem,
          llm_model: ag.llm_model || '',
          llm_source: ag.llm_source || '',
          graph_type: ag.graph_type || 'react',
          capabilities: ag.capabilities || [],
          middlewares: ag.middlewares || ['audit'],
          max_turns: ag.max_turns || 12,
          sandbox: !!ag.sandbox,
          role: ag.role || 'root',
          orchestration: ag.orchestration || 'auto',
          members: ag.members || [],
          flow,
          supervisor_key: ag.supervisor_key || '',
          supervision_scope: ag.supervision_scope || [],
          risk_agent_key: ag.risk_agent_key || '',
          auto_run: !!ag.auto_run,
          cron_expr: ag.cron_expr || '',
          cron_input: ag.cron_input || '',
        },
      });
      setNotice(`已保存回 Agent「${ag.name}」（生成新版本）`);
      await loadAgents();
    });

  // 优化结果一步到位：采用 + 保存回 Agent
  const adoptAndSave = () => {
    if (!studio?.prompt || !selAgent) return;
    setAgentSystem(studio.prompt);
    saveAgentSystem(studio.prompt);
    setStudio(null);
  };

  const run = async (tag, fn, okText) => {
    setBusy(tag);
    setError('');
    setNotice('');
    try {
      await fn();
      if (okText) setNotice(okText);
      await loadList();
      if (selected) await loadDetail(selected);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const publish = (activate) =>
    run(
      activate ? 'publish' : 'draft',
      () => prompt.publishVersion({ key: selected, content: draft, note, activate }),
      activate ? '已发布并生效' : '已保存为草稿版本',
    );

  const create = () =>
    run('create', async () => {
      await prompt.createPrompt({ ...form, tags: [], temperature: 0.3, author: 'console' });
      setCreating(false);
      setForm({ key: '', name: '', scene: 'rag', content: '' });
    }, '模板已创建');

  const variables = detail?.active?.variables ?? [];
  const activeVersion = useMemo(() => detail?.active?.version ?? 0, [detail]);

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="提示词模板" value={stats?.prompts ?? 0} hint="含内置与自建" tone="blue" />
        <StatCard label="版本总数" value={stats?.versions ?? 0} hint="只增不改，可回滚" tone="indigo" />
        <StatCard label="当前模板" value={detail?.active_version || '—'} hint={detail?.key || '未选择'} tone="teal" />
        <StatCard label="变量数" value={variables.length} hint="{{var}} 占位符" tone="violet" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <div className="tabs" role="tablist">
        {PAGE_TABS.map((t) => (
          <button key={t.key} type="button" className={`tab${tab === t.key ? ' active' : ''}`} onClick={() => setTab(t.key)}>
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'agent' && (
      <Panel
        title="Agent 角色提示词"
        desc="LLM 创建 Agent 时会把角色提示词自动沉淀到提示词工程；此处可对任意 Agent 的角色提示词 AI 打分 / AI 优化，并保存回 Agent"
        actions={selAgent ? <Badge tone="indigo">{selAgent.llm_source || 'default 数据源'}</Badge> : null}
      >
        {agents.length === 0 ? (
          <EmptyState icon={IconPrompt} title="还没有 Agent" desc="到「Agent 工厂」新建，或用 LLM 按需求生成一个 Agent" />
        ) : (
          <div className="form">
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              <label className="field">
                <span className="field-label">选择 Agent</span>
                <select className="input" value={agentKey} onChange={(e) => setAgentKey(e.target.value)}>
                  {agents.map((a) => (
                    <option key={a.agent_key} value={a.agent_key}>
                      {a.name}（{a.agent_key}）
                    </option>
                  ))}
                </select>
              </label>
              <label className="field">
                <span className="field-label">优化目标（可选）</span>
                <input
                  className="input"
                  value={agentGoal}
                  onChange={(e) => setAgentGoal(e.target.value)}
                  placeholder="如：输出更稳定 / 减少幻觉 / 更精简"
                />
              </label>
            </div>
            <label className="field">
              <span className="field-label">
                角色提示词（编辑后点「保存回 Agent」生成新版本；打分 / 优化使用该 Agent 的模型数据源）
              </span>
              <textarea
                className="textarea mono"
                rows={8}
                value={agentSystem}
                onChange={(e) => setAgentSystem(e.target.value)}
                placeholder={selAgent?.system ? '' : '该 Agent 还没有角色提示词'}
              />
            </label>
            <div className="form-actions">
              <button className="btn ghost sm" type="button" disabled={busy === 'agent-score'} onClick={agentScore}>
                {busy === 'agent-score' ? <Spinner text="评审中" /> : <><IconSpark size={14} /> AI 打分</>}
              </button>
              <button className="btn ghost sm" type="button" disabled={busy === 'agent-optimize'} onClick={agentOptimize}>
                {busy === 'agent-optimize' ? <Spinner text="优化中" /> : <><IconSpark size={14} /> AI 优化</>}
              </button>
              <button className="btn primary sm" type="button" disabled={busy === 'agent-save' || !selAgent} onClick={() => saveAgentSystem()}>
                {busy === 'agent-save' ? <Spinner text="保存中" /> : '保存回 Agent'}
              </button>
            </div>
            <StudioPanel
              studio={studio}
              onApply={adoptStudio}
              onClose={() => setStudio(null)}
              onSuggestion={applySuggestion}
              onApplySave={adoptAndSave}
              applySaving={busy === 'agent-save'}
            />
          </div>
        )}
      </Panel>
      )}

      {tab === 'tpl' && (
      <>
      <div className="grid-2 align-start">
        <Panel
          title="模板列表"
          desc="按场景筛选，点击行加载详情"
          actions={
            <>
              <select className="input sm" value={scene} onChange={(e) => setScene(e.target.value)}>
                <option value="">全部场景</option>
                {SCENES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
              <input
                className="input sm"
                placeholder="搜索 key / 名称"
                value={keyword}
                onChange={(e) => setKeyword(e.target.value)}
              />
              <button className="btn ghost sm" type="button" onClick={loadList}>
                <IconRefresh size={15} /> 刷新
              </button>
            </>
          }
        >
          {list.length === 0 ? (
            <EmptyState icon={IconPrompt} title="暂无模板" desc="服务首次启动会自动写入内置模板" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>Key</th>
                    <th>场景</th>
                    <th className="num">版本</th>
                  </tr>
                </thead>
                <tbody>
                  {listPg.paged.map((p) => (
                    <tr
                      key={p.key}
                      onClick={() => setSelected(p.key)}
                      style={{ cursor: 'pointer' }}
                      className={p.key === selected ? 'row-active' : ''}
                    >
                      <td className="ellipsis" title={p.key}>
                        {p.key}
                        {p.builtin && <Badge tone="teal">内置</Badge>}
                      </td>
                      <td>{p.scene || '—'}</td>
                      <td className="num">
                        v{p.active_version || 0} / {p.version_count}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <Pagination page={listPg.page} pages={listPg.pages} total={listPg.total} size={listPg.size} onPage={listPg.setPage} onSize={listPg.setSize} />
          <div className="form-actions">
            <button className="btn ghost sm" type="button" onClick={() => setCreating((v) => !v)}>
              {creating ? '收起新建' : '新建模板'}
            </button>
          </div>
          {creating && (
            <div className="form">
              <label className="field">
                <span className="field-label">Key</span>
                <input
                  className="input mono"
                  placeholder="rag.qa.system"
                  value={form.key}
                  onChange={(e) => setForm({ ...form, key: e.target.value })}
                />
              </label>
              <label className="field">
                <span className="field-label">名称</span>
                <input
                  className="input"
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                />
              </label>
              <label className="field">
                <span className="field-label">场景</span>
                <select
                  className="input"
                  value={form.scene}
                  onChange={(e) => setForm({ ...form, scene: e.target.value })}
                >
                  {SCENES.map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              </label>
              <label className="field">
                <span className="field-label">内容</span>
                <textarea
                  className="textarea"
                  rows={4}
                  value={form.content}
                  onChange={(e) => setForm({ ...form, content: e.target.value })}
                />
              </label>
              <div className="form-actions">
                <button
                  className="btn primary sm"
                  type="button"
                  onClick={create}
                  disabled={!form.key || !form.content || busy === 'create'}
                >
                  {busy === 'create' ? <Spinner text="创建中" /> : '创建'}
                </button>
              </div>
            </div>
          )}
        </Panel>

        <Panel
          title={detail ? `${detail.name || detail.key}` : '模板详情'}
          desc={detail ? `${detail.key} · 当前生效 v${detail.active_version} · ${detail.description || '无描述'}` : '在左侧选择模板'}
          actions={
            detail && (
              <Badge tone={detail.builtin ? 'teal' : 'indigo'}>{detail.builtin ? '内置' : '自定义'}</Badge>
            )
          }
        >
          {!detail ? (
            <EmptyState icon={IconPrompt} title="未选择模板" desc="点击左侧任意行查看内容与历史版本" />
          ) : (
            <div className="form">
              <label className="field">
                <span className="field-label">
                  内容（编辑后发布为新版本，v{activeVersion} → v{activeVersion + 1}）
                </span>
                <textarea
                  className="textarea mono"
                  rows={10}
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                />
              </label>
              <label className="field">
                <span className="field-label">变更说明（可选）</span>
                <input className="input" value={note} onChange={(e) => setNote(e.target.value)} />
              </label>
              <div className="form-actions">
                <button
                  className="btn primary sm"
                  type="button"
                  onClick={() => publish(true)}
                  disabled={!draft.trim() || busy === 'publish'}
                >
                  {busy === 'publish' ? <Spinner text="发布中" /> : '发布并生效'}
                </button>
                <button className="btn ghost sm" type="button" onClick={() => publish(false)} disabled={busy === 'draft'}>
                  只存草稿
                </button>
                <button
                  className="btn ghost sm"
                  type="button"
                  onClick={() => run('rollback', () => prompt.rollbackPrompt({ key: selected }), '已回滚到上一版本')}
                  disabled={busy === 'rollback' || (detail.version_count || 0) < 2}
                >
                  回滚
                </button>
                <button
                  className="btn ghost sm"
                  type="button"
                  onClick={() =>
                    run('reset', () => prompt.resetPrompt({ key: selected, author: 'console' }), '已恢复内置默认内容')
                  }
                  disabled={busy === 'reset' || !detail.builtin}
                >
                  恢复默认
                </button>
              </div>

              {variables.length > 0 && (
                <div className="form">
                  <span className="field-label">渲染预览（填充变量，不调用大模型）</span>
                  {variables.map((v) => (
                    <label className="field inline" key={v}>
                      <span className="field-label mono">{v}</span>
                      <input
                        className="input"
                        value={vars[v] ?? ''}
                        onChange={(e) => setVars({ ...vars, [v]: e.target.value })}
                      />
                    </label>
                  ))}
                  <div className="form-actions">
                    <button
                      className="btn ghost sm"
                      type="button"
                      onClick={async () => {
                        setBusy('render');
                        setError('');
                        try {
                          setPreview(await prompt.renderPrompt({ key: selected, vars }));
                        } catch (err) {
                          setError(err.message);
                        } finally {
                          setBusy('');
                        }
                      }}
                      disabled={busy === 'render'}
                    >
                      渲染
                    </button>
                  </div>
                  {preview && (
                    <>
                      <pre className="mono ellipsis" style={{ whiteSpace: 'pre-wrap' }}>{preview.rendered}</pre>
                      {preview.missing?.length > 0 && (
                        <Alert type="info">缺少变量：{preview.missing.join(', ')}</Alert>
                      )}
                    </>
                  )}
                </div>
              )}
            </div>
          )}
        </Panel>
      </div>

      {detail && (
        <Panel title="版本历史" desc="版本只增不改；激活即把线下版本指回线上">
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th className="num">版本</th>
                  <th>说明</th>
                  <th>作者</th>
                  <th>状态</th>
                  <th>评测得分</th>
                  <th>时间</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {versions.map((v) => (
                  <tr key={v.version}>
                    <td className="num mono">v{v.version}</td>
                    <td className="ellipsis" title={v.note}>{v.note || '—'}</td>
                    <td>{v.author || '—'}</td>
                    <td>{v.active ? <Badge tone="ok">生效中</Badge> : <span className="muted">历史</span>}</td>
                    <td>
                      {vScores[v.version] ? (
                        <a
                          href={`#/eval?prompt=${encodeURIComponent(selected)}&version=${v.version}`}
                          title={`运行 #${vScores[v.version].run_id} · ${vScores[v.version].cases} 个用例`}
                        >
                          <Badge tone={vScores[v.version].score >= 80 ? 'ok' : vScores[v.version].score >= 60 ? 'warn' : 'error'}>
                            {Math.round(vScores[v.version].score)} 分
                          </Badge>
                        </a>
                      ) : <span className="muted">未评测</span>}
                    </td>
                    <td className="muted mono">{(v.created_at || '').replace('T', ' ').slice(0, 19)}</td>
                    <td className="num">
                      <a href={`#/eval?prompt=${encodeURIComponent(selected)}&version=${v.version}`}>
                        <button className="btn ghost sm" type="button">评测</button>
                      </a>
                      <button
                        className="btn ghost sm"
                        type="button"
                        disabled={v.active || busy === `act-${v.version}`}
                        onClick={() =>
                          run(
                            `act-${v.version}`,
                            () => prompt.activatePrompt({ key: selected, version: v.version }),
                            `已激活 v${v.version}`,
                          )
                        }
                      >
                        激活
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Panel>
      )}
      </>
      )}
    </div>
  );
}