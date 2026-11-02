import { useCallback, useEffect, useMemo, useState } from 'react';
import { aiplatform } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Panel, Spinner, StatCard } from '../components/UI.jsx';
import { IconCloud, IconLink, IconRefresh, IconSearch, IconTrash } from '../components/Icons.jsx';

const TABS = [
  { key: 'platforms', label: '平台' },
  { key: 'market', label: 'MCP 广场' },
];

// 千问 / 阿里云百炼的接入形态（官方文档：MCP 服务地址为 /api/v1/mcps/{code}/mcp）
const QWEN_PRESET = {
  kind: 'qwen',
  base_url: 'https://dashscope.aliyuncs.com',
  auth_scheme: 'bearer',
  catalog_path: '/api/v1/mcps',
  streamable_path: '/api/v1/mcps/{code}/mcp',
  sse_path: '/api/v1/mcps/{code}/sse',
  description: '千问 / 阿里云百炼 MCP 广场（MCP 服务地址 /api/v1/mcps/{code}/mcp）',
};

const EMPTY_FORM = {
  name: '',
  kind: 'qwen',
  base_url: '',
  api_key: '',
  auth_scheme: 'bearer',
  auth_header: '',
  catalog_path: '',
  streamable_path: '',
  sse_path: '',
  skill_base_url: '',
  description: '',
  enabled: true,
  is_default: false,
};

export default function AIPlatforms() {
  const [tab, setTab] = useState('platforms');
  const [platforms, setPlatforms] = useState([]);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [form, setForm] = useState(null); // null=关闭；否则为编辑中的表单
  const [tests, setTests] = useState({});

  // MCP 广场
  const [platformSel, setPlatformSel] = useState('');
  const [keyword, setKeyword] = useState('');
  const [items, setItems] = useState([]);
  const [hint, setHint] = useState('');
  const [searched, setSearched] = useState(false);
  const [direct, setDirect] = useState({ code: '', transport: 'http', namespace: '' });

  const load = useCallback(async () => {
    setError('');
    try {
      const res = await aiplatform.listPlatforms({ limit: 100 });
      const list = res.platforms || [];
      setPlatforms(list);
      setPlatformSel((cur) => cur || list.find((p) => p.is_default)?.name || list.find((p) => p.enabled)?.name || '');
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
      const res = await fn();
      if (okText) setNotice(okText);
      await load();
      return res;
    } catch (err) {
      setError(err.message);
      return null;
    } finally {
      setBusy('');
    }
  };

  const openCreate = () => setForm({ ...EMPTY_FORM, ...pickPreset('qwen') });
  const openEdit = (p) =>
    setForm({
      name: p.name,
      kind: p.kind,
      base_url: p.base_url || '',
      api_key: '',
      auth_scheme: p.auth_scheme || 'bearer',
      auth_header: p.auth_header || '',
      catalog_path: p.catalog_path || '',
      streamable_path: p.streamable_path || '',
      sse_path: p.sse_path || '',
      skill_base_url: p.skill_base_url || '',
      description: p.description || '',
      enabled: p.enabled,
      is_default: p.is_default,
      editing: true,
    });

  const save = async () => {
    if (!form) return;
    const payload = {
      name: form.name.trim(),
      kind: form.kind,
      base_url: form.base_url.trim(),
      auth_scheme: form.auth_scheme,
      auth_header: form.auth_header.trim(),
      catalog_path: form.catalog_path.trim(),
      streamable_path: form.streamable_path.trim(),
      sse_path: form.sse_path.trim(),
      skill_base_url: form.skill_base_url.trim(),
      description: form.description.trim(),
      enabled: form.enabled,
      is_default: form.is_default,
    };
    if (form.api_key.trim()) payload.api_key = form.api_key.trim();

    const res = await act(
      'save',
      () => (form.editing ? aiplatform.updatePlatform({ ...payload, name: form.name }) : aiplatform.createPlatform(payload)),
      form.editing ? '平台已更新' : '平台已添加',
    );
    if (!res) return;
    if (res.warning) setNotice(`${form.editing ? '平台已更新' : '平台已添加'}，但技能市场登记失败：${res.warning}`);
    setForm(null);
  };

  const search = async () => {
    setBusy('search');
    setError('');
    setNotice('');
    setHint('');
    try {
      const res = await aiplatform.searchMCP({ platform: platformSel, keyword, limit: 30 });
      setItems(res.items || []);
      setSearched(true);
      if (!res.items?.length) setNotice('没有搜到服务：确认平台目录地址与 API Key，或直接用「按编码接入」');
    } catch (err) {
      setItems([]);
      setSearched(true);
      setError(err.message);
      setHint('目录接口不可用时，用左侧「按编码接入」直接拉取：千问服务的 code 形如 WebSearch、amap-maps');
    } finally {
      setBusy('');
    }
  };

  const doImport = async (code, transport, namespace) => {
    const res = await act(
      `i-${code}`,
      () =>
        aiplatform.importMCP({
          platform: platformSel,
          code,
          transport: transport || 'http',
          namespace: namespace || '',
          sync_tools: true,
        }),
      null,
    );
    if (!res) return;
    const bits = [`${res.server_name} 已接入（工具 ${res.tool_count} 个）`];
    if (res.reused) bits.unshift('已存在同名服务，已重新连接并同步');
    if (res.message) bits.push(res.message);
    setNotice(bits.join('；'));
    if (searched) {
      try {
        const again = await aiplatform.searchMCP({ platform: platformSel, keyword, limit: 30 });
        setItems(again.items || []);
      } catch {
        /* 刷新失败不影响接入结果 */
      }
    }
  };

  const testPlatform = async (name) => {
    setBusy(`t-${name}`);
    setError('');
    try {
      const res = await aiplatform.testPlatform({ name });
      setTests((m) => ({ ...m, [name]: res }));
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const stats = useMemo(() => {
    const enabled = platforms.filter((p) => p.enabled).length;
    const withKey = platforms.filter((p) => p.has_key).length;
    const hubs = platforms.filter((p) => p.skill_hub).length;
    const def = platforms.find((p) => p.is_default)?.name || '—';
    return { enabled, withKey, hubs, def };
  }, [platforms]);

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="平台" value={platforms.length} hint="千问 / 百炼 / 自建" tone="blue" />
        <StatCard label="已启用" value={stats.enabled} hint="启用后才参与搜索" tone="teal" />
        <StatCard label="已配 Key" value={stats.withKey} hint="Bearer / 自定义头" tone="indigo" />
        <StatCard label="默认平台" value={stats.def} hint={`关联技能市场 ${stats.hubs} 个`} tone="violet" />
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
          <button className="btn ghost sm" type="button" onClick={openCreate}>
            <IconCloud size={15} /> 添加平台
          </button>
          <button className="btn ghost sm" type="button" onClick={load}>
            <IconRefresh size={15} /> 刷新
          </button>
        </div>
      </div>

      {tab === 'platforms' && (
        <Panel
          title="AI 平台"
          desc="登记千问 / 百炼这类 MCP 广场：填网关地址与 API Key，即可搜索并一键接入 MCP 服务；填技能源地址会同时登记为技能市场"
        >
          {platforms.length === 0 ? (
            <EmptyState
              icon={IconCloud}
              title="还没有平台"
              desc="点右上「添加平台」，选千问预设，填 DASHSCOPE API Key 即可"
            />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>名称</th>
                    <th>类型</th>
                    <th>网关地址</th>
                    <th>鉴权</th>
                    <th>技能市场</th>
                    <th>状态</th>
                    <th className="num">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {platforms.map((p) => (
                    <tr key={p.name}>
                      <td>
                        <div className="cell-main mono">
                          {p.name}
                          {p.is_default && <Badge tone="ok">默认</Badge>}
                        </div>
                        <div className="cell-sub">{p.description || '—'}</div>
                      </td>
                      <td>
                        <Badge>{p.kind}</Badge>
                      </td>
                      <td className="mono ellipsis" title={p.base_url}>
                        {p.base_url || '—'}
                        <div className="cell-sub mono">{p.catalog_path || '（无目录接口）'}</div>
                      </td>
                      <td>
                        {p.has_key ? <Badge tone="ok">{p.auth_scheme}</Badge> : <Badge tone="warn">未配置</Badge>}
                      </td>
                      <td className="mono">{p.skill_hub || '—'}</td>
                      <td>
                        {p.enabled ? <Badge tone="ok">启用</Badge> : <Badge>停用</Badge>}
                        {tests[p.name] && (
                          <div className="cell-sub" title={tests[p.name].message}>
                            {tests[p.name].ok ? `可用 · ${tests[p.name].latency_ms}ms` : '不可用'}
                          </div>
                        )}
                      </td>
                      <td className="num">
                        <div style={{ display: 'flex', gap: 4, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
                          <button
                            className="btn ghost sm"
                            type="button"
                            disabled={busy === `t-${p.name}`}
                            onClick={() => testPlatform(p.name)}
                          >
                            {busy === `t-${p.name}` ? <Spinner text="测" /> : '测试'}
                          </button>
                          {!p.is_default && (
                            <button
                              className="btn ghost sm"
                              type="button"
                              onClick={() => act(`d-${p.name}`, () => aiplatform.setDefaultPlatform({ name: p.name }), `${p.name} 设为默认`)}
                            >
                              设默认
                            </button>
                          )}
                          <button className="btn ghost sm" type="button" onClick={() => openEdit(p)}>
                            编辑
                          </button>
                          <button
                            className="btn danger sm"
                            type="button"
                            onClick={() => {
                              if (!window.confirm(`删除平台 ${p.name}？已接入的 MCP 服务不会被删除。`)) return;
                              act(`x-${p.name}`, () => aiplatform.deletePlatform({ name: p.name }), `${p.name} 已删除`);
                            }}
                          >
                            <IconTrash size={14} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {tests && Object.keys(tests).length > 0 && (
            <div className="muted" style={{ marginTop: 10 }}>
              测试结果：
              {Object.entries(tests).map(([name, r]) => (
                <div key={name} className="mono" style={{ fontSize: 12 }}>
                  {name}: {r.ok ? '✓' : '✗'} {r.message}（目录 {r.catalog_ok ? '可用' : '不可用'} ·{' '}
                  {r.auth_configured ? '已配 Key' : '无 Key'}）
                </div>
              ))}
            </div>
          )}
        </Panel>
      )}

      {tab === 'market' && (
        <>
          <Panel
            title="MCP 广场"
            desc="从平台目录搜索 MCP 服务，接入后落成本地 MCP 服务（连接 + tools/list + 向量化走既有链路）"
            actions={
              <>
                <select className="input sm" value={platformSel} onChange={(e) => setPlatformSel(e.target.value)}>
                  {platforms.map((p) => (
                    <option key={p.name} value={p.name}>
                      {p.name}
                    </option>
                  ))}
                </select>
                <input
                  className="input sm"
                  placeholder="关键字（如 搜索 / 地图）"
                  value={keyword}
                  onChange={(e) => setKeyword(e.target.value)}
                  onKeyDown={(e) => e.key === 'Enter' && search()}
                />
                <button className="btn ghost sm" type="button" onClick={search} disabled={busy === 'search' || !platformSel}>
                  {busy === 'search' ? <Spinner text="搜索" /> : <><IconSearch size={15} /> 搜索</>}
                </button>
              </>
            }
          >
            {hint && <Alert type="info">{hint}</Alert>}
            {!searched ? (
              <EmptyState icon={IconSearch} title="输入关键字搜索平台目录" desc="也可以展开下方「按编码接入」，直接用 mcpCode 拉取" />
            ) : items.length === 0 ? (
              <EmptyState icon={IconLink} title="没有结果" desc="换个关键字，或改用下方按编码接入" />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>服务</th>
                      <th>传输</th>
                      <th>接入地址</th>
                      <th className="num">工具</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {items.map((m) => (
                      <tr key={m.code}>
                        <td>
                          <div className="cell-main">{m.name || m.code}</div>
                          <div className="cell-sub mono">{m.code}</div>
                          {m.description && <div className="cell-sub ellipsis">{m.description}</div>}
                        </td>
                        <td className="mono">{m.transport || 'http'}</td>
                        <td className="mono ellipsis" title={m.endpoint}>
                          {m.endpoint || '—'}
                        </td>
                        <td className="num">{m.tool_count || '—'}</td>
                        <td className="num">
                          {m.installed ? (
                            <Badge tone="ok">已接入</Badge>
                          ) : (
                            <button
                              className="btn primary sm"
                              type="button"
                              disabled={busy === `i-${m.code}`}
                              onClick={() => doImport(m.code, 'http', '')}
                            >
                              {busy === `i-${m.code}` ? <Spinner text="接入" /> : '接入'}
                            </button>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          <Panel
            title="按编码接入"
            desc="目录接口不可用时用这条路：只依赖路径模板（千问：/api/v1/mcps/{code}/mcp），不需要平台开放列表 API"
          >
            <div className="form">
              <div className="query-row">
                <label className="field inline">
                  <span className="field-label">mcpCode</span>
                  <input
                    className="input mono"
                    placeholder="WebSearch / amap-maps / market-cmapi011033"
                    value={direct.code}
                    onChange={(e) => setDirect({ ...direct, code: e.target.value })}
                  />
                  <span className="cell-sub" style={{ marginTop: 4 }}>
                    接入后本地服务名为 &lt;平台&gt;-&lt;code&gt;，工具唯一名 &lt;命名空间&gt;__&lt;工具名&gt;
                  </span>
                </label>
                <label className="field inline">
                  <span className="field-label">传输</span>
                  <select
                    className="input"
                    value={direct.transport}
                    onChange={(e) => setDirect({ ...direct, transport: e.target.value })}
                  >
                    <option value="http">Streamable HTTP（推荐）</option>
                    <option value="sse">SSE（本地会降级为 HTTP）</option>
                  </select>
                </label>
                <label className="field inline">
                  <span className="field-label">命名空间（可空，默认平台名）</span>
                  <input
                    className="input mono"
                    placeholder="qwen"
                    value={direct.namespace}
                    onChange={(e) => setDirect({ ...direct, namespace: e.target.value })}
                  />
                </label>
              </div>
              <div className="form-actions">
                <button
                  className="btn primary"
                  type="button"
                  disabled={!platformSel || !direct.code.trim() || busy === `i-${direct.code}`}
                  onClick={() => doImport(direct.code.trim(), direct.transport, direct.namespace.trim())}
                >
                  {busy === `i-${direct.code}` ? <Spinner text="接入中" /> : '接入为本地 MCP 服务'}
                </button>
                <span className="muted">接入后立刻连接并拉取 tools/list，再做工具向量化</span>
              </div>
            </div>
          </Panel>
        </>
      )}

      {form && (
        <FormDrawer
          title={form.editing ? `编辑平台 · ${form.name}` : '添加 AI 平台'}
          desc="千问预设已填好官方地址与路径模板；只需补 name 与 API Key"
          onClose={() => setForm(null)}
          width={720}
        >
          <div className="form">
            <div className="form-actions">
              <button className="btn ghost sm" type="button" onClick={() => setForm({ ...form, ...pickPreset('qwen') })}>
                千问 / 百炼预设
              </button>
              <button className="btn ghost sm" type="button" onClick={() => setForm({ ...form, ...pickPreset('generic') })}>
                通用（自建广场）
              </button>
              <span className="muted">预设只填地址与路径模板，不会覆盖名称与 API Key</span>
            </div>

            <div className="query-row">
              <label className="field inline">
                <span className="field-label">名称（唯一，小写）</span>
                <input
                  className="input mono"
                  placeholder="qwen"
                  value={form.name}
                  disabled={form.editing}
                  title={form.editing ? '平台名是主键，不可修改' : undefined}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                />
              </label>
              <label className="field inline">
                <span className="field-label">类型</span>
                <select className="input" value={form.kind} onChange={(e) => setForm({ ...form, kind: e.target.value })}>
                  <option value="qwen">qwen（千问）</option>
                  <option value="bailian">bailian（百炼）</option>
                  <option value="generic">generic（自建 / 第三方）</option>
                </select>
              </label>
            </div>

            <div className="query-row">
              <label className="field inline">
                <span className="field-label">API Key{form.editing ? '（留空不改）' : ''}</span>
                <input
                  className="input mono"
                  type="password"
                  placeholder={form.editing ? '已配置则留空保持不变' : 'sk-…'}
                  value={form.api_key}
                  onChange={(e) => setForm({ ...form, api_key: e.target.value })}
                />
                <span className="cell-sub" style={{ marginTop: 4 }}>
                  千问填 DASHSCOPE_API_KEY；只进不出，读取接口仅返回是否已配
                </span>
              </label>
              <label className="field inline">
                <span className="field-label">鉴权方式</span>
                <select
                  className="input"
                  value={form.auth_scheme}
                  onChange={(e) => setForm({ ...form, auth_scheme: e.target.value })}
                >
                  <option value="bearer">bearer（Authorization: Bearer）</option>
                  <option value="header">header（自定义头）</option>
                  <option value="none">none（不需要）</option>
                </select>
              </label>
              {form.auth_scheme === 'header' && (
                <label className="field inline">
                  <span className="field-label">鉴权头名称</span>
                  <input
                    className="input mono"
                    placeholder="X-Api-Key"
                    value={form.auth_header}
                    onChange={(e) => setForm({ ...form, auth_header: e.target.value })}
                  />
                </label>
              )}
            </div>

            <div className="query-row">
              <label className="field inline">
                <span className="field-label">网关地址（base_url）</span>
                <input
                  className="input mono"
                  placeholder="https://dashscope.aliyuncs.com"
                  value={form.base_url}
                  onChange={(e) => setForm({ ...form, base_url: e.target.value })}
                />
              </label>
              <label className="field inline">
                <span className="field-label">目录接口（可空＝不搜索）</span>
                <input
                  className="input mono"
                  placeholder="/api/v1/mcps"
                  value={form.catalog_path}
                  onChange={(e) => setForm({ ...form, catalog_path: e.target.value })}
                />
              </label>
            </div>

            <div className="query-row">
              <label className="field inline">
                <span className="field-label">接入路径模板（必须含 {'{code}'}）</span>
                <input
                  className="input mono"
                  placeholder="/api/v1/mcps/{code}/mcp"
                  value={form.streamable_path}
                  onChange={(e) => setForm({ ...form, streamable_path: e.target.value })}
                />
              </label>
              <label className="field inline">
                <span className="field-label">SSE 路径模板（可空）</span>
                <input
                  className="input mono"
                  placeholder="/api/v1/mcps/{code}/sse"
                  value={form.sse_path}
                  onChange={(e) => setForm({ ...form, sse_path: e.target.value })}
                />
              </label>
            </div>

            <div className="query-row">
              <label className="field inline">
                <span className="field-label">技能源地址（可空，填了就登记技能市场）</span>
                <input
                  className="input mono"
                  placeholder="https://…/api/v1/skills"
                  value={form.skill_base_url}
                  onChange={(e) => setForm({ ...form, skill_base_url: e.target.value })}
                />
              </label>
              <label className="field inline">
                <span className="field-label">说明</span>
                <input
                  className="input"
                  placeholder="千问 / 阿里云百炼 MCP 广场"
                  value={form.description}
                  onChange={(e) => setForm({ ...form, description: e.target.value })}
                />
              </label>
            </div>

            <div className="form-actions">
              <label className="chip" style={{ cursor: 'pointer' }}>
                <input
                  type="checkbox"
                  checked={form.enabled}
                  onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
                />{' '}
                启用（停用的平台不参与默认选择，但保留配置）
              </label>
              <label className="chip" style={{ cursor: 'pointer' }}>
                <input
                  type="checkbox"
                  checked={form.is_default}
                  onChange={(e) => setForm({ ...form, is_default: e.target.checked })}
                />{' '}
                设为默认平台
              </label>
            </div>

            <div className="form-actions">
              <button className="btn primary" type="button" disabled={busy === 'save'} onClick={save}>
                {busy === 'save' ? <Spinner text="保存" /> : '保存'}
              </button>
              <button className="btn ghost" type="button" onClick={() => setForm(null)}>
                取消
              </button>
            </div>
          </div>
        </FormDrawer>
      )}
    </div>
  );
}

function pickPreset(kind) {
  if (kind === 'generic') {
    return {
      kind: 'generic',
      base_url: '',
      catalog_path: '/api/v1/mcps',
      streamable_path: '/api/v1/mcps/{code}/mcp',
      sse_path: '',
      description: '自建 / 第三方 MCP 广场',
    };
  }
  return QWEN_PRESET;
}
