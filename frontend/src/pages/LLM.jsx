import { useCallback, useEffect, useState } from 'react';
import { llm } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import { LLMCallDetail, LLMCallsTable, fmtTokens } from '../components/LLMCalls.jsx';
import { IconPlug, IconRefresh, IconSpark, IconTrash } from '../components/Icons.jsx';

const KINDS = ['openai', 'deepseek', 'ollama', 'qwen', 'siliconflow', 'moonshot', 'custom'];

// 各平台 OpenAI 兼容根路径提示：base_url 必须指向「会拼上 /chat/completions 就可用」的根
const BASE_HINTS = {
  qwen: { placeholder: 'https://dashscope.aliyuncs.com/compatible-mode/v1', hint: '通义/百炼：用 compatible-mode/v1 兼容入口（不是 /api/v1）' },
  deepseek: { placeholder: 'https://api.deepseek.com', hint: 'DeepSeek：https://api.deepseek.com（带不带 /v1 均可）' },
  openai: { placeholder: 'https://api.openai.com/v1', hint: 'OpenAI：https://api.openai.com/v1' },
  ollama: { placeholder: 'http://127.0.0.1:11434/v1', hint: 'Ollama：http://127.0.0.1:11434/v1' },
  siliconflow: { placeholder: 'https://api.siliconflow.cn/v1', hint: 'SiliconFlow：https://api.siliconflow.cn/v1' },
  moonshot: { placeholder: 'https://api.moonshot.cn/v1', hint: 'Moonshot：https://api.moonshot.cn/v1' },
};

const EMPTY = {
  name: '',
  kind: 'openai',
  base_url: '',
  api_key: '',
  default_model: '',
  models: '',
  scenes: [],
  priority: 0,
  is_default: false,
};

export default function LLM() {
  const [stats, setStats] = useState(null);
  const [current, setCurrent] = useState(null);
  const [scenes, setScenes] = useState([]);
  const [providers, setProviders] = useState([]);
  const [form, setForm] = useState(EMPTY);
  const [showForm, setShowForm] = useState(false);
  // 编辑中的数据源名称；null = 新增模式
  const [editing, setEditing] = useState(null);
  // 调试对话数据源；'' = 按场景路由
  const [chatProvider, setChatProvider] = useState('');
  const [testResult, setTestResult] = useState(null);
  const [remote, setRemote] = useState(null);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  // 数据源列表分页
  const provPg = usePagedList(providers, { size: 10, resetKey: '' });

  const load = useCallback(async () => {
    setError('');
    try {
      const [st, cur, sc, list] = await Promise.all([
        llm.getLLMStats(),
        llm.getCurrent(),
        llm.listScenes(),
        llm.listProviders({ all: true }),
      ]);
      setStats(st);
      setCurrent(cur);
      setScenes(sc.scenes || []);
      setProviders(list.providers || []);
    } catch (err) {
      setError(err.message);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const act = async (tag, fn, okText) => {
    setBusy(tag);
    setError('');
    setNotice('');
    try {
      await fn();
      if (okText) setNotice(okText);
      await load();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const formPayload = () => ({
    kind: form.kind,
    base_url: form.base_url,
    // 编辑时留空 = 不改动已存密钥；undefined 在 JSON 序列化时被省略
    ...(form.api_key ? { api_key: form.api_key } : {}),
    default_model: form.default_model,
    models: form.models ? form.models.split(/[,，\s]+/).filter(Boolean) : [],
    scenes: form.scenes,
    priority: Number(form.priority) || 0,
    is_default: form.is_default,
  });

  const create = () =>
    act(
      'create',
      async () => {
        await llm.createProvider({
          name: form.name,
          ...formPayload(),
          temperature: 0.3,
          max_tokens: 2048,
          timeout_seconds: 120,
          description: '',
        });
        setForm(EMPTY);
        setEditing(null);
        setShowForm(false);
      },
      '数据源已创建',
    );

  const update = () =>
    act(
      'update',
      async () => {
        await llm.updateProvider({ name: editing, ...formPayload() });
        setForm(EMPTY);
        setEditing(null);
        setShowForm(false);
      },
      `${editing} 已更新`,
    );

  // 打开编辑表单：用列表数据回填（密钥不回显，留空保持不变）
  const openEdit = (p) => {
    setEditing(p.name);
    setForm({
      name: p.name,
      kind: p.kind || 'custom',
      base_url: p.base_url || '',
      api_key: '',
      default_model: p.default_model || '',
      models: (p.models || []).join(', '),
      scenes: p.scenes || [],
      priority: p.priority ?? 0,
      is_default: !!p.is_default,
    });
    setShowForm(true);
    setTestResult(null);
    setRemote(null);
    window.scrollTo({ top: 0, behavior: 'smooth' });
  };

  const closeForm = () => {
    setShowForm(false);
    setEditing(null);
    setForm(EMPTY);
  };

  const toggleScene = (scene) =>
    setForm((f) => ({
      ...f,
      scenes: f.scenes.includes(scene) ? f.scenes.filter((s) => s !== scene) : [...f.scenes, scene],
    }));

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="数据源" value={stats?.providers ?? 0} hint="MySQL llm_providers" tone="blue" />
        <StatCard label="已启用" value={stats?.enabled ?? 0} hint="参与场景路由" tone="teal" />
        <StatCard label="默认数据源" value={stats?.default || '—'} hint="场景未命中时的兜底" tone="indigo" />
        <StatCard label="当前模型" value={current?.model || '—'} hint="RAG / GraphRAG 实际调用" tone="violet" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}
      {testResult && (
        <Alert type={testResult.ok ? 'success' : 'error'}>
          {testResult.ok
            ? `${testResult.provider} / ${testResult.model} 连通正常，${testResult.latency_ms}ms，回复：${testResult.reply}`
            : `${testResult.provider} 测试失败：${testResult.error}`}
        </Alert>
      )}
      {remote && (
        <Alert type="info">
          {remote.name} 远端共 {remote.total} 个模型：{remote.models.slice(0, 20).join(', ')}
          {remote.total > 20 ? ' …' : ''}
        </Alert>
      )}

      <Panel
        title="数据源列表"
        desc="按场景命中 → 默认数据源 → 优先级排序；调用失败自动切下一个"
        actions={
          <>
            <button
              className="btn ghost sm"
              type="button"
              onClick={() => (showForm && !editing ? closeForm() : (setEditing(null), setForm(EMPTY), setShowForm(true)))}
            >
              {showForm && !editing ? '收起' : '新增数据源'}
            </button>
            <button className="btn ghost sm" type="button" onClick={load}>
              <IconRefresh size={15} /> 刷新
            </button>
          </>
        }
      >
        {providers.length === 0 ? (
          <EmptyState icon={IconPlug} title="暂无数据源" desc="新增一个 OpenAI 兼容的数据源即可启用问答" />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>名称</th>
                  <th>类型</th>
                  <th>模型</th>
                  <th>场景</th>
                  <th>状态</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {provPg.paged.map((p) => (
                  <tr key={p.name}>
                    <td className="mono">
                      {p.name}
                      {p.has_key && <Badge tone="ok">密钥</Badge>}
                    </td>
                    <td>{p.kind}</td>
                    <td className="mono ellipsis" title={p.default_model}>
                      {p.default_model || '—'}
                    </td>
                    <td className="muted">{(p.scenes || []).join(', ') || '通用'}</td>
                    <td>
                      {p.is_default && <Badge tone="indigo">默认</Badge>}{' '}
                      {p.enabled ? <Badge tone="ok">启用</Badge> : <Badge tone="warn">停用</Badge>}
                    </td>
                    <td className="num">
                      <button
                        className="btn ghost sm"
                        type="button"
                        onClick={() => openEdit(p)}
                      >
                        编辑
                      </button>
                      <button
                        className="btn ghost sm"
                        type="button"
                        disabled={busy === `test-${p.name}`}
                        onClick={async () => {
                          setBusy(`test-${p.name}`);
                          setTestResult(null);
                          try {
                            setTestResult({ ...(await llm.testProvider({ name: p.name })), provider: p.name });
                          } catch (err) {
                            setError(err.message);
                          } finally {
                            setBusy('');
                          }
                        }}
                      >
                        {busy === `test-${p.name}` ? <Spinner text="测试" /> : '测试'}
                      </button>
                      <button
                        className="btn ghost sm"
                        type="button"
                        onClick={async () => {
                          try {
                            const r = await llm.listRemoteModels({ name: p.name });
                            setRemote({ name: p.name, ...r });
                          } catch (err) {
                            setError(err.message);
                          }
                        }}
                      >
                        模型
                      </button>
                      <button
                        className="btn ghost sm"
                        type="button"
                        disabled={p.enabled}
                        onClick={() => act(`en-${p.name}`, () => llm.enableProvider({ name: p.name }), `${p.name} 已启用`)}
                      >
                        启用
                      </button>
                      <button
                        className="btn ghost sm"
                        type="button"
                        disabled={!p.enabled}
                        onClick={() => act(`dis-${p.name}`, () => llm.disableProvider({ name: p.name }), `${p.name} 已停用`)}
                      >
                        停用
                      </button>
                      <button
                        className="btn ghost sm"
                        type="button"
                        disabled={p.is_default}
                        onClick={() => act(`def-${p.name}`, () => llm.setDefaultProvider({ name: p.name }), `${p.name} 设为默认`)}
                      >
                        设为默认
                      </button>
                      <button
                        className="btn danger sm"
                        type="button"
                        disabled={busy === `del-${p.name}`}
                        onClick={() => act(`del-${p.name}`, () => llm.deleteProvider({ name: p.name }), `${p.name} 已删除`)}
                      >
                        {busy === `del-${p.name}` ? <Spinner text="删除" /> : <IconTrash size={14} />}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <Pagination page={provPg.page} pages={provPg.pages} total={provPg.total} size={provPg.size} onPage={provPg.setPage} onSize={provPg.setSize} />
      </Panel>

      {showForm && (
        <FormDrawer
          title={editing ? `编辑数据源 · ${editing}` : '新增数据源'}
          desc={editing ? '留空 API Key 表示保持已存密钥不变' : '仅支持 OpenAI 兼容协议（/chat/completions 与 /models）'}
          onClose={closeForm}
        >
          <div className="form">
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">名称（唯一）</span>
                <input
                  className="input mono"
                  placeholder="deepseek-main"
                  value={form.name}
                  disabled={!!editing}
                  title={editing ? '数据源名称是主键，不可修改' : undefined}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                />
              </label>
              <label className="field inline">
                <span className="field-label">类型</span>
                <select className="input" value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value })}>
                  {KINDS.map((k) => (
                    <option key={k} value={k}>
                      {k}
                    </option>
                  ))}
                </select>
              </label>
            </div>
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">Base URL</span>
                <input
                  className="input mono"
                  placeholder={BASE_HINTS[form.kind]?.placeholder || 'https://api.deepseek.com/v1'}
                  value={form.base_url}
                  onChange={(e) => setForm({ ...form, base_url: e.target.value })}
                />
                <span className="cell-sub" style={{ marginTop: 4 }}>
                  {BASE_HINTS[form.kind]?.hint || '填 OpenAI 兼容根路径：会拼上 /chat/completions 直接可用'}
                </span>
              </label>
              <label className="field inline">
                <span className="field-label">API Key</span>
                <input
                  className="input mono"
                  type="password"
                  placeholder={editing && providers.find((p) => p.name === editing)?.has_key ? '已配置（留空保持不变）' : 'sk-…'}
                  value={form.api_key}
                  onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                />
              </label>
            </div>
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">默认模型</span>
                <input
                  className="input mono"
                  placeholder="deepseek-chat"
                  value={form.default_model}
                  onChange={(e) => setForm({ ...form, default_model: e.target.value })}
                />
              </label>
              <label className="field inline">
                <span className="field-label">可选模型（逗号分隔）</span>
                <input
                  className="input mono"
                  value={form.models}
                  onChange={(e) => setForm({ ...form, models: e.target.value })}
                />
              </label>
            </div>
            <div className="field">
              <span className="field-label">服务场景（不选=通用）</span>
              <div className="chips">
                {scenes.map((s) => (
                  <label
                    key={s}
                    className="chip"
                    style={{ cursor: 'pointer', background: form.scenes.includes(s) ? 'var(--accent-soft)' : undefined }}
                  >
                    <input
                      type="checkbox"
                      checked={form.scenes.includes(s)}
                      onChange={() => toggleScene(s)}
                    />{' '}
                    {s}
                  </label>
                ))}
              </div>
            </div>
            <div className="form-actions">
              <label className="chip" style={{ cursor: 'pointer' }}>
                <input
                  type="checkbox"
                  checked={form.is_default}
                  onChange={(e) => setForm({ ...form, is_default: e.target.checked })}
                />{' '}
                设为默认
              </label>
              <button
                className="btn primary sm"
                type="button"
                onClick={editing ? update : create}
                disabled={!form.name || !form.base_url || !form.default_model || busy === 'create' || busy === 'update'}
              >
                {busy === 'create' || busy === 'update' ? (
                  <Spinner text={editing ? '保存中' : '创建中'} />
                ) : editing ? (
                  '保存修改'
                ) : (
                  '创建'
                )}
              </button>
              {editing && (
                <button className="btn ghost sm" type="button" onClick={closeForm}>
                  取消
                </button>
              )}
            </div>
          </div>
        </FormDrawer>
      )}

      <Panel
        title="调试对话"
        desc="直接走 Manager，等价于业务侧的 Chatter 调用；默认按场景路由，可指定数据源"
        actions={
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <select
              className="input"
              style={{ width: 180 }}
              value={chatProvider}
              title="指定本次调试对话使用的数据源"
              onChange={(e) => setChatProvider(e.target.value)}
            >
              <option value="">按场景路由（默认）</option>
              {providers.filter((p) => p.enabled).map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name} · {p.default_model}
                </option>
              ))}
            </select>
            {chatProvider && <Badge tone="indigo">指定 {chatProvider}</Badge>}
          </div>
        }
      >
        <ChatBox provider={chatProvider} />
      </Panel>

      <CallHistory />
    </div>
  );
}

// CallHistory LLM 调用历史：每次数据源尝试一条，含 token 用量与任务关联
function CallHistory() {
  const [stats, setStats] = useState(null);
  const [calls, setCalls] = useState([]);
  const [total, setTotal] = useState(0);
  const [status, setStatus] = useState('');
  const [model, setModel] = useState('');
  const [error, setError] = useState('');
  const [detail, setDetail] = useState(null); // 详情弹层：当前查看的调用记录
  // token 下钻：按 agent/scene/purpose/model 维度聚合排行
  const [bdDim, setBdDim] = useState('agent_key');
  const [bdRows, setBdRows] = useState([]);

  // 调用记录分页：筛选条件变化时回到第 1 页
  const callsPg = usePagedList(calls, { size: 10, resetKey: `${status}|${model}` });

  const loadBreakdown = useCallback(async (dim) => {
    try {
      const res = await llm.breakdownTokens({ dim: dim || 'agent_key', days: 0 });
      setBdRows(res.rows || []);
    } catch {
      setBdRows([]);
    }
  }, []);

  useEffect(() => {
    loadBreakdown(bdDim);
  }, [bdDim, loadBreakdown]);

  const load = useCallback(async () => {
    setError('');
    try {
      const f = {};
      if (status) f.status = status;
      if (model.trim()) f.model = model.trim();
      const [st, list] = await Promise.all([llm.getLLMCallStats({}), llm.listLLMCalls({ ...f, limit: 200 })]);
      setStats(st);
      setCalls(list.calls || []);
      setTotal(list.total ?? 0);
    } catch (err) {
      setError(err.message);
    }
  }, [status, model]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <Panel
      title="调用历史"
      desc="每次 LLM 调用一条记录：token 用量、耗时、模型与任务关联（agentfactory 任务可在任务中心查看）"
      actions={
        <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <select className="input" style={{ width: 110 }} value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="">全部状态</option>
            <option value="ok">成功</option>
            <option value="fail">失败</option>
          </select>
          <input
            className="input"
            style={{ width: 180 }}
            placeholder="按模型过滤，如 deepseek-chat"
            value={model}
            onChange={(e) => setModel(e.target.value)}
          />
          <button className="btn ghost sm" type="button" onClick={load}>刷新</button>
        </div>
      }
    >
      {error && <Alert type="error">{error}</Alert>}
      {stats && (
        <div className="kv" style={{ marginBottom: 12 }}>
          <div><span className="muted">调用次数</span> <span className="mono">{stats.calls ?? 0}</span>{(stats.failures ?? 0) > 0 && <span className="muted">（失败 {stats.failures}）</span>}</div>
          <div><span className="muted">输入 tokens</span> <span className="mono">{fmtTokens(stats.prompt_tokens)}</span></div>
          <div><span className="muted">输出 tokens</span> <span className="mono">{fmtTokens(stats.completion_tokens)}</span></div>
          <div><span className="muted">总计</span> <span className="mono" style={{ fontWeight: 600 }}>{fmtTokens(stats.total_tokens)}</span></div>
        </div>
      )}
      {stats?.by_model?.length > 0 && (
        <div style={{ marginBottom: 12, display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          {stats.by_model.map((m) => (
            <Badge key={m.model} tone="violet">
              {m.model} · {m.calls} 次 · {fmtTokens(m.total_tokens ?? (m.prompt_tokens ?? 0) + (m.completion_tokens ?? 0))} tok
            </Badge>
          ))}
        </div>
      )}
      {/* token 用量下钻：找 token 大户 */}
      <div style={{ marginBottom: 12, border: '1px solid rgba(127,127,127,.28)', borderRadius: 10, padding: '10px 12px', background: 'rgba(127,127,127,.05)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 8 }}>
          <strong style={{ fontSize: 13 }}>Token 用量下钻</strong>
          <span className="cell-sub">按维度聚合排行，定位 token 大户</span>
          <span style={{ flex: 1 }} />
          <select className="input sm" style={{ width: 140 }} value={bdDim} onChange={(e) => setBdDim(e.target.value)}>
            <option value="agent_key">按 Agent</option>
            <option value="scene">按场景</option>
            <option value="purpose">按用途</option>
            <option value="model">按模型</option>
          </select>
        </div>
        {bdRows.length === 0 ? (
          <div className="cell-sub">暂无数据</div>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>{bdDim === 'agent_key' ? 'Agent' : bdDim === 'scene' ? '场景' : bdDim === 'purpose' ? '用途' : '模型'}</th>
                  <th className="num">调用</th>
                  <th className="num">输入 tok</th>
                  <th className="num">输出 tok</th>
                  <th className="num">总计</th>
                  <th className="num">失败</th>
                </tr>
              </thead>
              <tbody>
                {bdRows.map((r) => (
                  <tr key={r.key}>
                    <td className="ellipsis mono" title={r.key}>{r.key}</td>
                    <td className="num">{r.calls}</td>
                    <td className="num mono">{fmtTokens(r.prompt_tokens)}</td>
                    <td className="num mono">{fmtTokens(r.completion_tokens)}</td>
                    <td className="num mono" style={{ fontWeight: 600 }}>{fmtTokens(r.total_tokens)}</td>
                    <td className="num">{r.failures > 0 ? <Badge tone="error">{r.failures}</Badge> : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {/* token 用量下钻：找 token 大户 */}
      <div style={{ marginBottom: 12, border: '1px solid rgba(127,127,127,.28)', borderRadius: 10, padding: '10px 12px', background: 'rgba(127,127,127,.05)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 8 }}>
          <strong style={{ fontSize: 13 }}>Token 用量下钻</strong>
          <span className="cell-sub">按维度聚合排行，定位 token 大户</span>
          <span style={{ flex: 1 }} />
          <select className="input sm" style={{ width: 140 }} value={bdDim} onChange={(e) => setBdDim(e.target.value)}>
            <option value="agent_key">按 Agent</option>
            <option value="scene">按场景</option>
            <option value="purpose">按用途</option>
            <option value="model">按模型</option>
          </select>
        </div>
        {bdRows.length === 0 ? (
          <div className="cell-sub">暂无数据</div>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>{bdDim === 'agent_key' ? 'Agent' : bdDim === 'scene' ? '场景' : bdDim === 'purpose' ? '用途' : '模型'}</th>
                  <th className="num">调用</th>
                  <th className="num">输入 tok</th>
                  <th className="num">输出 tok</th>
                  <th className="num">总计</th>
                  <th className="num">失败</th>
                </tr>
              </thead>
              <tbody>
                {bdRows.map((r) => (
                  <tr key={r.key}>
                    <td className="ellipsis mono" title={r.key}>{r.key}</td>
                    <td className="num">{r.calls}</td>
                    <td className="num mono">{fmtTokens(r.prompt_tokens)}</td>
                    <td className="num mono">{fmtTokens(r.completion_tokens)}</td>
                    <td className="num mono" style={{ fontWeight: 600 }}>{fmtTokens(r.total_tokens)}</td>
                    <td className="num">{r.failures > 0 ? <Badge tone="error">{r.failures}</Badge> : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {calls.length === 0 ? (
        <EmptyState title="暂无调用记录" desc="发起一次对话或提交一个 Agent 任务后，这里会记录每次 LLM 调用的 token 用量" />
      ) : (
        <>
          <div className="table-wrap">
            <LLMCallsTable calls={callsPg.paged} showTask onDetail={setDetail} />
          </div>
          <Pagination
            page={callsPg.page}
            pages={callsPg.pages}
            total={callsPg.total}
            size={callsPg.size}
            onPage={callsPg.setPage}
            onSize={callsPg.setSize}
          />
          {total > calls.length && <div className="muted" style={{ marginTop: 8 }}>服务端共 {total} 条，当前已加载最近 {calls.length} 条</div>}
        </>
      )}
      {detail && <LLMCallDetail call={detail} onClose={() => setDetail(null)} />}
    </Panel>
  );
}

function ChatBox({ provider = '' }) {
  const [user, setUser] = useState('用一句话说明 RRF 融合排序的作用');
  const [reply, setReply] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const send = async () => {
    if (!user.trim()) return;
    setBusy(true);
    setError('');
    setReply('');
    try {
      const res = await llm.llmChat({
        user,
        system: '你是简洁的技术助手，用中文回答。',
        scene: 'general',
        ...(provider ? { provider } : {}),
      });
      setReply(res.reply);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="form">
      <label className="field">
        <span className="field-label">消息</span>
        <textarea className="textarea" rows={3} value={user} onChange={(e) => setUser(e.target.value)} />
      </label>
      <div className="form-actions">
        <button className="btn primary sm" type="button" onClick={send} disabled={busy}>
          {busy ? <Spinner text="生成中" /> : <><IconSpark size={15} /> 发送</>}
        </button>
      </div>
      {error && <Alert type="error">{error}</Alert>}
      {reply && (
        <div className="answer">
          <div className="answer-icon">
            <IconSpark size={16} />
          </div>
          <div className="answer-text">{reply}</div>
        </div>
      )}
    </div>
  );
}
