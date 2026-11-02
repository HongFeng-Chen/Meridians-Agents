import { useCallback, useEffect, useState } from 'react';
import { mcp } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import { IconLink, IconRefresh, IconSearch, IconTrash } from '../components/Icons.jsx';

// MCP 管理面：
//   服务配置（MySQL）→ 连接（进程内实例，http.Client 运行时绑定）→ 工具同步
//   （MySQL → Embedding → Milvus 向量 + Redis 缓存）→ 语义召回 → 调用。
const TABS = [
  { key: 'servers', label: '服务' },
  { key: 'tools', label: '工具' },
  { key: 'recall', label: '语义召回' },
];

const TRANSPORTS = [
  { value: 'http', label: 'http · Streamable HTTP' },
  { value: 'stdio', label: 'stdio · 本地子进程' },
];

// 服务上的连接类动作：同一个发请求、同一套刷新逻辑
const SERVER_ACTIONS = {
  connect: { label: '连接成功', call: (id) => mcp.connectMCPServer({ id }) },
  disconnect: { label: '已断开', call: (id) => mcp.disconnectMCPServer({ id }) },
  ping: { label: '探活成功', call: (id) => mcp.pingMCPServer({ id }) },
  sync: { label: '同步完成', call: (id) => mcp.syncMCPServer({ id }) },
};

function emptyForm() {
  return {
    id: '',
    name: '',
    namespace: '',
    transport: 'http',
    description: '',
    endpoint: '',
    command: '',
    args: '',
    headers: '',
    env: '',
    health_check_interval: '30',
    enabled: true,
  };
}

function formFrom(server) {
  return {
    id: server.id,
    name: server.name || '',
    namespace: server.namespace || '',
    transport: server.transport || 'http',
    description: server.description || '',
    endpoint: server.endpoint || '',
    command: server.command || '',
    args: (server.args || []).join('\n'),
    headers: server.headers && Object.keys(server.headers).length ? JSON.stringify(server.headers, null, 2) : '',
    env: server.env && Object.keys(server.env).length ? JSON.stringify(server.env, null, 2) : '',
    health_check_interval: server.health_check_interval ? String(server.health_check_interval) : '30',
    enabled: !!server.enabled,
  };
}

function formatTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return v;
  return d.toLocaleString('zh-CN', { hour12: false });
}

// 表单里的 JSON 对象字段：空串视为 {}，非法 JSON 直接抛错由页面展示
function parseMap(text, label) {
  const raw = text.trim();
  if (!raw) return {};
  let data;
  try {
    data = JSON.parse(raw);
  } catch {
    throw new Error(`${label} 需为 JSON 对象，例如 {"Authorization":"Bearer xxx"}`);
  }
  if (data === null || typeof data !== 'object' || Array.isArray(data)) {
    throw new Error(`${label} 需为 JSON 对象（键值对），不能是数组或字面量`);
  }
  for (const [k, v] of Object.entries(data)) {
    if (typeof v !== 'string') throw new Error(`${label}.${k} 的值必须是字符串`);
  }
  return data;
}

function safeParse(text) {
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

// 按 input_schema 生成参数骨架：有 required 就只列必填，否则列出全部属性
function argsSkeleton(tool) {
  const schema = safeParse(tool?.input_schema);
  const props = schema?.properties;
  if (!props || typeof props !== 'object') return '{}';
  const required = new Set(Array.isArray(schema.required) ? schema.required : []);
  const out = {};
  for (const [key, def] of Object.entries(props)) {
    if (required.size > 0 && !required.has(key)) continue;
    out[key] = sampleValue(def);
  }
  return JSON.stringify(out, null, 2);
}

function sampleValue(def) {
  if (def?.default !== undefined) return def.default;
  if (Array.isArray(def?.enum) && def.enum.length) return def.enum[0];
  switch (def?.type) {
    case 'integer':
    case 'number':
      return 0;
    case 'boolean':
      return false;
    case 'array':
      return [];
    case 'object':
      return {};
    default:
      return '';
  }
}

function syncLine(r) {
  if (!r) return '';
  return `拉取 ${r.fetched ?? 0} · 新增 ${r.added ?? 0} · 更新 ${r.updated ?? 0} · 移除 ${r.removed ?? 0} · 向量化 ${r.vectorized ?? 0}`;
}

export default function Mcp() {
  const [tab, setTab] = useState('servers');
  const [stats, setStats] = useState(null);
  const [servers, setServers] = useState([]);
  const [namespaces, setNamespaces] = useState([]);
  const [detail, setDetail] = useState(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  // 新建 / 编辑表单
  const [showForm, setShowForm] = useState(false);
  const [form, setForm] = useState(emptyForm());

  // 运行时 http.Client（不落库，只影响当前进程）
  const [client, setClient] = useState({ timeout_seconds: '', proxy_url: '', insecure_tls: false });

  // 工具页
  const [tools, setTools] = useState([]);
  const [filter, setFilter] = useState({ mcp_server_id: '', namespace: '', q: '', vectorized: '' });

  // 工具调用
  const [callTarget, setCallTarget] = useState(null);
  const [callArgs, setCallArgs] = useState('{}');
  const [callOut, setCallOut] = useState(null);

  // 语义召回
  const [recallQuery, setRecallQuery] = useState('');
  const [recallNs, setRecallNs] = useState('');
  const [hits, setHits] = useState([]);

  // 列表分页：服务 / 工具（筛选变化时回到第 1 页）
  const serverPg = usePagedList(servers, { size: 10, resetKey: '' });
  const toolPg = usePagedList(tools, { size: 10, resetKey: `${filter.mcp_server_id}|${filter.namespace}|${filter.vectorized}|${filter.q}` });

  const loadServers = useCallback(async () => {
    setError('');
    try {
      const [st, list, ns] = await Promise.all([
        mcp.getMCPStats(),
        mcp.listMCPServers({ limit: 200 }),
        mcp.listMCPNamespaces(),
      ]);
      setStats(st);
      setServers(list.servers || []);
      setNamespaces(ns.namespaces || []);
    } catch (err) {
      setError(err.message);
    }
  }, []);

  useEffect(() => {
    loadServers();
  }, [loadServers]);

  const loadTools = useCallback(async () => {
    try {
      const res = await mcp.listMCPTools({ ...filter, limit: 200 });
      setTools(res.tools || []);
    } catch (err) {
      setError(err.message);
    }
  }, [filter]);

  useEffect(() => {
    if (tab === 'tools') loadTools();
  }, [tab, loadTools]);

  // act：统一 loading / 错误 / 提示，返回结果便于调用方决定是否刷新
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

  const refreshDetail = async (id) => {
    const sv = await act(`detail-${id}`, () => mcp.getMCPServer({ id }));
    if (sv) setDetail(sv);
  };

  const runServerAction = async (s, kind) => {
    const def = SERVER_ACTIONS[kind];
    const res = await act(`${kind}-${s.id}`, () => def.call(s.id));
    if (!res) return;
    let msg = `${s.name} ${def.label}`;
    if (kind === 'sync') {
      msg += `：${syncLine(res.result)}`;
      if (res.result?.warning) msg += `（${res.result.warning}）`;
    }
    setNotice(msg);
    await loadServers();
    if (detail?.id === s.id) await refreshDetail(s.id);
    if (tab === 'tools') await loadTools();
  };

  const syncAll = async () => {
    const res = await act('sync-all', () => mcp.syncAllMCPServers());
    if (!res) return;
    const results = res.results || [];
    const total = results.reduce(
      (acc, r) => ({
        added: acc.added + (r.added || 0),
        updated: acc.updated + (r.updated || 0),
        removed: acc.removed + (r.removed || 0),
        vectorized: acc.vectorized + (r.vectorized || 0),
        warn: acc.warn + (r.warning ? 1 : 0),
      }),
      { added: 0, updated: 0, removed: 0, vectorized: 0, warn: 0 },
    );
    setNotice(
      `已同步 ${results.length} 个服务：新增 ${total.added} · 更新 ${total.updated} · 移除 ${total.removed} · 向量化 ${total.vectorized}` +
        (total.warn ? `（${total.warn} 个服务有告警）` : ''),
    );
    await loadServers();
    if (tab === 'tools') await loadTools();
  };

  const submitServer = async () => {
    let headers;
    let env;
    try {
      headers = parseMap(form.headers, 'headers');
      env = parseMap(form.env, 'env');
    } catch (err) {
      setError(err.message);
      return;
    }
    const isHTTP = form.transport === 'http';
    const payload = {
      name: form.name.trim(),
      namespace: form.namespace.trim(),
      transport: form.transport,
      description: form.description.trim(),
      health_check_interval: Number(form.health_check_interval) || 30,
      enabled: form.enabled ? 'true' : 'false',
      endpoint: isHTTP ? form.endpoint.trim() : '',
      headers: isHTTP ? headers : {},
      command: isHTTP ? '' : form.command.trim(),
      args: isHTTP
        ? []
        : form.args
            .split('\n')
            .map((v) => v.trim())
            .filter(Boolean),
      env: isHTTP ? {} : env,
    };
    const editing = !!form.id;
    const res = await act('save', () =>
      editing ? mcp.updateMCPServer({ id: form.id, ...payload }) : mcp.createMCPServer(payload),
    );
    if (!res) return;
    setNotice(editing ? `${res.name} 已保存` : `${res.name} 已创建，可点击「连接」拉取工具`);
    setShowForm(false);
    setForm(emptyForm());
    await loadServers();
    if (editing && detail?.id === res.id) setDetail(res);
  };

  const deleteServer = async (s) => {
    const res = await act(`del-${s.id}`, () => mcp.deleteMCPServer({ id: s.id }));
    if (!res) return;
    setNotice(`${s.name} 已删除（连带工具、向量与缓存）`);
    if (detail?.id === s.id) setDetail(null);
    await loadServers();
    if (tab === 'tools') await loadTools();
  };

  const toggleServer = async (s) => {
    const res = await act(`tg-${s.id}`, () => mcp.toggleMCPServer({ id: s.id, enabled: !s.enabled }));
    if (!res) return;
    // protojson 会省略 false 字段，所以按操作意图提示而不是读响应值
    setNotice(`${s.name} 已${!s.enabled ? '启用' : '停用'}`);
    await loadServers();
  };

  const applyClient = async () => {
    if (!detail) return;
    const res = await act('client', () =>
      mcp.setMCPClient({
        id: detail.id,
        client: {
          timeout_seconds: Number(client.timeout_seconds) || 0,
          proxy_url: client.proxy_url.trim(),
          insecure_tls: !!client.insecure_tls,
        },
      }),
    );
    if (!res) return;
    setNotice(`${detail.name} 的 http.Client 已更新${res.running ? '，已重连' : ''}`);
    await refreshDetail(detail.id);
    await loadServers();
  };

  const toggleTool = async (t) => {
    const res = await act(`tt-${t.id}`, () => mcp.toggleMCPTool({ id: t.id, enabled: !t.enabled }));
    if (!res) return;
    setNotice(`${res.unique_name} 已${!t.enabled ? '启用' : '停用'}`);
    await loadTools();
    await loadServers();
  };

  const deleteTool = async (t) => {
    const res = await act(`dt-${t.id}`, () => mcp.deleteMCPTool({ id: t.id }));
    if (!res) return;
    setNotice(`${t.unique_name} 已删除`);
    if (callTarget?.id === t.id) setCallTarget(null);
    await loadTools();
    await loadServers();
  };

  const openCall = (tool) => {
    setCallTarget(tool);
    setCallArgs(argsSkeleton(tool));
    setCallOut(null);
    setError('');
  };

  const runCall = async () => {
    if (!callTarget) return;
    if (callArgs.trim()) {
      const parsed = safeParse(callArgs);
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        setError('参数需为 JSON 对象，例如 {"owner":"octocat"}');
        return;
      }
    }
    const res = await act('call', () => mcp.callMCPTool({ id: callTarget.id, arguments: callArgs.trim() || '{}' }));
    if (res) setCallOut(res);
  };

  const runRecall = async () => {
    if (!recallQuery.trim()) return;
    const res = await act('recall', () =>
      mcp.searchMCPTools({ q: recallQuery.trim(), namespace: recallNs, limit: 10 }),
    );
    if (!res) return;
    setHits(res.hits || []);
    if (!res.hits?.length) setNotice('没有召回结果：确认工具已同步且 vectorized 为 true');
  };

  const openToolsOf = (server) => {
    setFilter({ ...filter, mcp_server_id: server.id });
    setTab('tools');
  };

  const runningCount = servers.filter((s) => s.status?.running).length;

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="服务总数" value={stats?.servers ?? 0} hint={`启用 ${stats?.enabled_servers ?? 0} 个`} tone="blue" />
        <StatCard label="运行中" value={stats?.running ?? runningCount} hint="进程内持有实例数" tone="teal" />
        <StatCard label="工具总数" value={stats?.tools ?? 0} hint={`启用 ${stats?.enabled_tools ?? 0} 个`} tone="indigo" />
        <StatCard label="已向量化" value={stats?.vectorized ?? 0} hint={`${stats?.namespaces ?? 0} 个命名空间`} tone="violet" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <div className="tabs">
        {TABS.map((t) => (
          <button
            key={t.key}
            type="button"
            className={`tab${tab === t.key ? ' active' : ''}`}
            onClick={() => setTab(t.key)}
          >
            {t.label}
          </button>
        ))}
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8 }}>
          <button className="btn ghost sm" type="button" onClick={syncAll} disabled={busy === 'sync-all'}>
            {busy === 'sync-all' ? <Spinner text="同步中" /> : '全部同步'}
          </button>
          <button className="btn ghost sm" type="button" onClick={loadServers}>
            <IconRefresh size={15} /> 刷新
          </button>
        </div>
      </div>

      {tab === 'servers' && (
        <>
          <Panel
            title="MCP 服务"
            desc="配置存 MySQL；连接由进程内 Manager 持有，http.Client 运行时绑定、不落库"
            actions={
              <button
                className="btn ghost sm"
                type="button"
                onClick={() => {
                  setForm(emptyForm());
                  setShowForm((v) => !(showForm && !form.id));
                }}
              >
                {showForm && !form.id ? '收起' : '新建服务'}
              </button>
            }
          >
            {servers.length === 0 ? (
              <EmptyState
                icon={IconLink}
                title="还没有 MCP 服务"
                desc="新建一个 http 或 stdio 服务，连接后同步工具清单"
              />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>服务</th>
                      <th>传输</th>
                      <th>状态</th>
                      <th className="num">工具</th>
                      <th>最近同步</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {serverPg.paged.map((s) => {
                      const running = !!s.status?.running;
                      return (
                        <tr key={s.id} className={s.id === detail?.id ? 'row-active' : ''}>
                          <td>
                            <div className="cell-main">
                              {s.name}
                              {!s.enabled && <Badge tone="warn">停用</Badge>}
                            </div>
                            <div className="cell-sub mono">
                              {s.namespace} · {s.transport === 'http' ? s.endpoint : [s.command, ...(s.args || [])].join(' ')}
                            </div>
                          </td>
                          <td>
                            <Badge tone={s.transport === 'http' ? 'indigo' : 'teal'}>{s.transport}</Badge>
                          </td>
                          <td>
                            {running ? <Badge tone="ok">运行中</Badge> : <Badge>未连接</Badge>}
                            {s.status?.error_msg && (
                              <span title={s.status.error_msg}>
                                <Badge tone="warn">有错误</Badge>
                              </span>
                            )}
                          </td>
                          <td className="num">{s.tool_count ?? 0}</td>
                          <td className="mono">{formatTime(s.status?.last_sync_at)}</td>
                          <td className="num">
                            {running ? (
                              <button
                                className="btn ghost sm"
                                type="button"
                                disabled={busy === `disconnect-${s.id}`}
                                onClick={() => runServerAction(s, 'disconnect')}
                              >
                                断开
                              </button>
                            ) : (
                              <button
                                className="btn ghost sm"
                                type="button"
                                disabled={busy === `connect-${s.id}`}
                                onClick={() => runServerAction(s, 'connect')}
                              >
                                {busy === `connect-${s.id}` ? <Spinner text="连接" /> : '连接'}
                              </button>
                            )}
                            <button
                              className="btn ghost sm"
                              type="button"
                              disabled={busy === `ping-${s.id}`}
                              onClick={() => runServerAction(s, 'ping')}
                            >
                              探活
                            </button>
                            <button
                              className="btn ghost sm"
                              type="button"
                              disabled={busy === `sync-${s.id}`}
                              onClick={() => runServerAction(s, 'sync')}
                            >
                              {busy === `sync-${s.id}` ? <Spinner text="同步" /> : '同步'}
                            </button>
                            <button className="btn ghost sm" type="button" onClick={() => refreshDetail(s.id)}>
                              详情
                            </button>
                            <button
                              className="btn ghost sm"
                              type="button"
                              onClick={() => {
                                setForm(formFrom(s));
                                setShowForm(true);
                              }}
                            >
                              编辑
                            </button>
                            <button
                              className="btn ghost sm"
                              type="button"
                              onClick={() => toggleServer(s)}
                              disabled={busy === `tg-${s.id}`}
                            >
                              {s.enabled ? '停用' : '启用'}
                            </button>
                            <button
                              className="btn danger sm"
                              type="button"
                              disabled={busy === `del-${s.id}`}
                              onClick={() => deleteServer(s)}
                            >
                              {busy === `del-${s.id}` ? <Spinner text="删除" /> : <IconTrash size={14} />}
                            </button>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}

            <Pagination page={serverPg.page} pages={serverPg.pages} total={serverPg.total} size={serverPg.size} onPage={serverPg.setPage} onSize={serverPg.setSize} />

            {showForm && (
              <FormDrawer
                title={form.id ? `编辑服务 · ${form.name}` : '新建服务'}
                desc={form.id ? '名称是主键不可改；修改配置后需重新连接生效' : '配置存 MySQL；连接由进程内 Manager 持有'}
                onClose={() => {
                  setForm(emptyForm());
                  setShowForm(false);
                }}
              >
              <div className="form">
                <div className="query-row">
                  <label className="field inline">
                    <span className="field-label">服务名称（全局唯一）</span>
                    <input
                      className="input mono"
                      placeholder="github-mcp"
                      value={form.name}
                      onChange={(e) => setForm({ ...form, name: e.target.value })}
                    />
                  </label>
                  <label className="field inline">
                    <span className="field-label">命名空间（工具唯一名前缀）</span>
                    <input
                      className="input mono"
                      placeholder="github"
                      value={form.namespace}
                      onChange={(e) => setForm({ ...form, namespace: e.target.value })}
                    />
                  </label>
                  <label className="field inline">
                    <span className="field-label">传输协议</span>
                    <select
                      className="input"
                      value={form.transport}
                      onChange={(e) => setForm({ ...form, transport: e.target.value })}
                    >
                      {TRANSPORTS.map((t) => (
                        <option key={t.value} value={t.value}>
                          {t.label}
                        </option>
                      ))}
                    </select>
                  </label>
                  <label className="field inline">
                    <span className="field-label">探活间隔（秒）</span>
                    <input
                      className="input mono"
                      value={form.health_check_interval}
                      onChange={(e) => setForm({ ...form, health_check_interval: e.target.value })}
                    />
                  </label>
                </div>

                {form.transport === 'http' ? (
                  <>
                    <label className="field">
                      <span className="field-label">Endpoint（Streamable HTTP）</span>
                      <input
                        className="input mono"
                        placeholder="https://api.githubcopilot.com/mcp/"
                        value={form.endpoint}
                        onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
                      />
                    </label>
                    <label className="field">
                      <span className="field-label">请求头模板（JSON 对象，支持 ${'{ENV}'} 占位）</span>
                      <textarea
                        className="textarea mono"
                        rows={3}
                        placeholder={'{\n  "Authorization": "Bearer ${GITHUB_TOKEN}"\n}'}
                        value={form.headers}
                        onChange={(e) => setForm({ ...form, headers: e.target.value })}
                      />
                    </label>
                  </>
                ) : (
                  <>
                    <div className="query-row">
                      <label className="field inline">
                        <span className="field-label">命令（可执行文件）</span>
                        <input
                          className="input mono"
                          placeholder="npx"
                          value={form.command}
                          onChange={(e) => setForm({ ...form, command: e.target.value })}
                        />
                      </label>
                    </div>
                    <label className="field">
                      <span className="field-label">参数（一行一个）</span>
                      <textarea
                        className="textarea mono"
                        rows={3}
                        placeholder={'-y\n@modelcontextprotocol/server-filesystem\n/tmp'}
                        value={form.args}
                        onChange={(e) => setForm({ ...form, args: e.target.value })}
                      />
                    </label>
                    <label className="field">
                      <span className="field-label">环境变量（JSON 对象，支持 ${'{ENV}'} 展开）</span>
                      <textarea
                        className="textarea mono"
                        rows={3}
                        placeholder={'{\n  "GITHUB_TOKEN": "${GITHUB_TOKEN}"\n}'}
                        value={form.env}
                        onChange={(e) => setForm({ ...form, env: e.target.value })}
                      />
                    </label>
                  </>
                )}

                <label className="field">
                  <span className="field-label">描述（参与向量化，建议写清能力边界）</span>
                  <textarea
                    className="textarea"
                    rows={2}
                    value={form.description}
                    onChange={(e) => setForm({ ...form, description: e.target.value })}
                  />
                </label>

                <div className="form-actions">
                  <button
                    className="btn primary sm"
                    type="button"
                    onClick={submitServer}
                    disabled={!form.name || busy === 'save'}
                  >
                    {busy === 'save' ? <Spinner text="保存中" /> : form.id ? '保存修改' : '创建服务'}
                  </button>
                  <label className="field inline" style={{ flexDirection: 'row', alignItems: 'center', gap: 6 }}>
                    <input
                      type="checkbox"
                      checked={form.enabled}
                      onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
                    />
                    <span className="field-label">启用（启用后每次重启会按配置重连）</span>
                  </label>
                  {form.id && (
                    <button
                      className="btn ghost sm"
                      type="button"
                      onClick={() => {
                        setForm(emptyForm());
                        setShowForm(false);
                      }}
                    >
                      取消编辑
                    </button>
                  )}
                </div>
              </div>
              </FormDrawer>
            )}
          </Panel>

          {detail && (
            <Panel
              title={`服务详情 · ${detail.name}`}
              desc={detail.description || '无描述'}
              actions={
                <>
                  <Badge tone={detail.transport === 'http' ? 'indigo' : 'teal'}>{detail.transport}</Badge>
                  <button className="btn ghost sm" type="button" onClick={() => openToolsOf(detail)}>
                    查看工具 ({detail.tool_count ?? 0})
                  </button>
                </>
              }
            >
              <div className="grid-2 align-start">
                <div className="kv">
                  <div>
                    <span>ID</span>
                    <b className="mono">{detail.id}</b>
                  </div>
                  <div>
                    <span>命名空间</span>
                    <b className="mono">{detail.namespace}</b>
                  </div>
                  <div>
                    <span>连接状态</span>
                    <b>{detail.status?.running ? <Badge tone="ok">运行中</Badge> : <Badge>未连接</Badge>}</b>
                  </div>
                  <div>
                    <span>协议版本</span>
                    <b className="mono">{detail.status?.protocol_version || '—'}</b>
                  </div>
                  <div>
                    <span>服务版本</span>
                    <b className="mono">{detail.status?.server_version || '—'}</b>
                  </div>
                  <div>
                    <span>当前 http.Client</span>
                    <b className="mono">{detail.status?.http_client || '默认（60s，走环境代理）'}</b>
                  </div>
                  <div>
                    <span>最近探活</span>
                    <b className="mono">{formatTime(detail.status?.last_ping_at)}</b>
                  </div>
                  <div>
                    <span>最近同步</span>
                    <b className="mono">{formatTime(detail.status?.last_sync_at)}</b>
                  </div>
                  <div>
                    <span>更新时间</span>
                    <b className="mono">{formatTime(detail.updated_at)}</b>
                  </div>
                </div>

                <div className="form">
                  <div className="field-label">运行时 http.Client（覆盖参数后立即重连，不写库）</div>
                  <div className="query-row">
                    <label className="field inline">
                      <span className="field-label">超时（秒，0 用默认）</span>
                      <input
                        className="input mono"
                        value={client.timeout_seconds}
                        onChange={(e) => setClient({ ...client, timeout_seconds: e.target.value })}
                      />
                    </label>
                    <label className="field inline">
                      <span className="field-label">代理地址（空走环境变量）</span>
                      <input
                        className="input mono"
                        placeholder="http://127.0.0.1:7890"
                        value={client.proxy_url}
                        onChange={(e) => setClient({ ...client, proxy_url: e.target.value })}
                      />
                    </label>
                  </div>
                  <label className="field inline" style={{ flexDirection: 'row', alignItems: 'center', gap: 6 }}>
                    <input
                      type="checkbox"
                      checked={client.insecure_tls}
                      onChange={(e) => setClient({ ...client, insecure_tls: e.target.checked })}
                    />
                    <span className="field-label">跳过 TLS 校验（自签证书的内网服务）</span>
                  </label>
                  <div className="form-actions">
                    <button
                      className="btn primary sm"
                      type="button"
                      onClick={applyClient}
                      disabled={busy === 'client'}
                    >
                      {busy === 'client' ? <Spinner text="应用" /> : '应用并重连'}
                    </button>
                    <span className="muted">只影响当前进程内存中的实例</span>
                  </div>
                  {detail.status?.error_msg && (
                    <Alert type="error">最近一次失败：{detail.status.error_msg}</Alert>
                  )}
                </div>
              </div>
            </Panel>
          )}
        </>
      )}

      {tab === 'tools' && (
        <>
          <Panel
            title="工具清单"
            desc="元数据存 MySQL，向量存 Milvus，热点对象缓存 Redis；停用后语义召回不再命中"
            actions={
              <>
                <select
                  className="input sm"
                  value={filter.mcp_server_id}
                  onChange={(e) => setFilter({ ...filter, mcp_server_id: e.target.value })}
                >
                  <option value="">全部服务</option>
                  {servers.map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.name}
                    </option>
                  ))}
                </select>
                <select
                  className="input sm"
                  value={filter.namespace}
                  onChange={(e) => setFilter({ ...filter, namespace: e.target.value })}
                >
                  <option value="">全部命名空间</option>
                  {namespaces.map((n) => (
                    <option key={n} value={n}>
                      {n}
                    </option>
                  ))}
                </select>
                <select
                  className="input sm"
                  value={filter.vectorized}
                  onChange={(e) => setFilter({ ...filter, vectorized: e.target.value })}
                >
                  <option value="">向量化不限</option>
                  <option value="true">已向量化</option>
                  <option value="false">待向量化</option>
                </select>
                <input
                  className="input sm"
                  placeholder="工具名 / 描述"
                  value={filter.q}
                  onChange={(e) => setFilter({ ...filter, q: e.target.value })}
                  onKeyDown={(e) => e.key === 'Enter' && loadTools()}
                />
                <button className="btn ghost sm" type="button" onClick={loadTools}>
                  <IconSearch size={15} /> 查询
                </button>
              </>
            }
          >
            {tools.length === 0 ? (
              <EmptyState
                icon={IconSearch}
                title="暂无工具"
                desc="先连接服务并点击「同步」，从 tools/list 拉取工具清单"
              />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>工具</th>
                      <th>服务 / 命名空间</th>
                      <th>标签</th>
                      <th>状态</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {toolPg.paged.map((t) => (
                      <tr key={t.id}>
                        <td>
                          <div className="cell-main">{t.display_name || t.tool_name}</div>
                          <div className="cell-sub mono">{t.unique_name}</div>
                          {t.description && (
                            <div className="cell-sub ellipsis" title={t.description}>
                              {t.description}
                            </div>
                          )}
                        </td>
                        <td>
                          <div className="cell-main">{t.mcp_server_name || '—'}</div>
                          <div className="cell-sub mono">{t.namespace}</div>
                        </td>
                        <td>
                          {(t.tags || []).length === 0 ? (
                            <span className="muted">—</span>
                          ) : (
                            <div className="badges-row inline">
                              {(t.tags || []).slice(0, 3).map((tag) => (
                                <Badge key={tag}>{tag}</Badge>
                              ))}
                            </div>
                          )}
                        </td>
                        <td>
                          {t.enabled ? <Badge tone="ok">启用</Badge> : <Badge tone="warn">停用</Badge>}{' '}
                          {t.vectorized ? <Badge tone="indigo">已向量化</Badge> : <Badge>待向量化</Badge>}
                        </td>
                        <td className="num">
                          <button className="btn ghost sm" type="button" onClick={() => openCall(t)}>
                            调用
                          </button>
                          <button
                            className="btn ghost sm"
                            type="button"
                            disabled={busy === `tt-${t.id}`}
                            onClick={() => toggleTool(t)}
                          >
                            {t.enabled ? '停用' : '启用'}
                          </button>
                          <button
                            className="btn danger sm"
                            type="button"
                            disabled={busy === `dt-${t.id}`}
                            onClick={() => deleteTool(t)}
                          >
                            <IconTrash size={14} />
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <Pagination page={toolPg.page} pages={toolPg.pages} total={toolPg.total} size={toolPg.size} onPage={toolPg.setPage} onSize={toolPg.setSize} />
          </Panel>

          {callTarget && (
            <Panel
              title={`调用 · ${callTarget.unique_name}`}
              desc={callTarget.description || '参数已按 input_schema 生成骨架，可自行修改'}
              actions={
                <>
                  <Badge tone="indigo">{callTarget.namespace}</Badge>
                  <button className="btn ghost sm" type="button" onClick={() => setCallTarget(null)}>
                    关闭
                  </button>
                </>
              }
            >
              <div className="grid-2 align-start">
                <div className="form">
                  <label className="field">
                    <span className="field-label">arguments（JSON 对象）</span>
                    <textarea
                      className="textarea mono"
                      rows={8}
                      value={callArgs}
                      onChange={(e) => setCallArgs(e.target.value)}
                    />
                  </label>
                  <div className="form-actions">
                    <button className="btn primary sm" type="button" onClick={runCall} disabled={busy === 'call'}>
                      {busy === 'call' ? <Spinner text="调用中" /> : '执行调用'}
                    </button>
                    <button
                      className="btn ghost sm"
                      type="button"
                      onClick={() => {
                        setCallArgs(argsSkeleton(callTarget));
                        setCallOut(null);
                      }}
                    >
                      重置参数
                    </button>
                  </div>
                </div>
                <div>
                  <div className="field-label">
                    返回结果
                    {callOut && (
                      <>
                        {' '}
                        {callOut.is_error ? <Badge tone="warn">isError</Badge> : <Badge tone="ok">成功</Badge>}
                        <span className="muted mono"> {callOut.elapsed_ms} ms</span>
                      </>
                    )}
                  </div>
                  {!callOut && <pre className="block mono">尚未调用</pre>}
                  {(callOut?.content || []).map((c, i) => (
                    <pre className="block mono" key={i}>
                      {c.type === 'text' ? c.text : c.data || JSON.stringify(c, null, 2)}
                    </pre>
                  ))}
                  {callOut?.structured && (
                    <pre className="block mono">structuredContent: {callOut.structured}</pre>
                  )}
                  {callOut && (callOut.content || []).length === 0 && !callOut.structured && (
                    <pre className="block mono">（该工具没有返回内容）</pre>
                  )}
                </div>
              </div>
              <details style={{ marginTop: 12 }}>
                <summary className="field-label" style={{ cursor: 'pointer' }}>
                  input_schema
                </summary>
                <pre className="block mono">{prettyJSON(callTarget.input_schema)}</pre>
              </details>
            </Panel>
          )}

          <HistoryPanel
            kind="mcp_call"
            title="MCP 调用历史"
            desc="每次工具调用的参数、结果摘要、耗时与关联任务；失败会记录错误原因"
            emptyTitle="暂无调用记录"
            emptyDesc="调用一次工具后，这里会留下记录；Agent 任务内的调用会带上任务号"
          />
        </>
      )}

      {tab === 'recall' && (
        <Panel
          title="工具语义召回"
          desc="问题向量化 → Milvus 检索 tool_id → Redis / MySQL 补齐详情，等价于 Agent 选工具的第一步"
          actions={
            <>
              <select className="input sm" value={recallNs} onChange={(e) => setRecallNs(e.target.value)}>
                <option value="">全部命名空间</option>
                {namespaces.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
              <input
                className="input sm"
                placeholder="自然语言，如：帮我看下这个仓库有哪些未关闭的 issue"
                style={{ minWidth: 320 }}
                value={recallQuery}
                onChange={(e) => setRecallQuery(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && runRecall()}
              />
              <button
                className="btn primary sm"
                type="button"
                onClick={runRecall}
                disabled={!recallQuery.trim() || busy === 'recall'}
              >
                {busy === 'recall' ? <Spinner text="召回" /> : '召回'}
              </button>
            </>
          }
        >
          {hits.length === 0 ? (
            <EmptyState icon={IconSearch} title="还没有召回结果" desc="用一句自然语言描述你想做的事" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th className="num">分数</th>
                    <th>工具</th>
                    <th>服务 / 命名空间</th>
                    <th>命中来源</th>
                    <th className="num">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {hits.map((h) => (
                    <tr key={h.tool?.id || h.tool?.unique_name}>
                      <td className="num mono">{(h.score ?? 0).toFixed(3)}</td>
                      <td>
                        <div className="cell-main">{h.tool?.display_name || h.tool?.tool_name}</div>
                        <div className="cell-sub mono">{h.tool?.unique_name}</div>
                        <div className="cell-sub ellipsis" title={h.tool?.description}>
                          {h.tool?.description}
                        </div>
                      </td>
                      <td>
                        <div className="cell-main">{h.tool?.mcp_server_name || '—'}</div>
                        <div className="cell-sub mono">{h.tool?.namespace}</div>
                      </td>
                      <td>
                        <Badge tone={h.source === 'redis' ? 'teal' : 'indigo'}>{h.source || 'mysql'}</Badge>
                      </td>
                      <td className="num">
                        <button
                          className="btn ghost sm"
                          type="button"
                          disabled={!h.tool?.id}
                          onClick={() => {
                            openCall(h.tool);
                            setTab('tools');
                          }}
                        >
                          调用
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      )}
    </div>
  );
}

function prettyJSON(raw) {
  if (!raw) return '（无）';
  const data = safeParse(raw);
  return data ? JSON.stringify(data, null, 2) : raw;
}
