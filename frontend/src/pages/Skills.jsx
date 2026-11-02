import { useCallback, useEffect, useState } from 'react';
import { skill } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import { IconBox, IconRefresh, IconSearch, IconTrash } from '../components/Icons.jsx';

const TABS = [
  { key: 'installed', label: '已安装' },
  { key: 'market', label: '技能市场' },
  { key: 'hubs', label: '市场配置' },
];

export default function Skills() {
  const [tab, setTab] = useState('installed');
  const [stats, setStats] = useState(null);
  const [skills, setSkills] = useState([]);
  const [hubs, setHubs] = useState([]);
  const [detail, setDetail] = useState(null);
  const [files, setFiles] = useState(null);
  const [fileBody, setFileBody] = useState(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');
  const [form, setForm] = useState({ name: '', version: '1.0.0', content: '', files: '' });
  const [showForm, setShowForm] = useState(false);
  // 从 URL 安装：没有市场 API、只给下载链接的分发方式（千问技能包 / 内部归档）
  const [showUrlForm, setShowUrlForm] = useState(false);
  const [urlForm, setUrlForm] = useState({ url: '', name: '', version: '', token: '', enable: true });

  // 市场
  const [keyword, setKeyword] = useState('');
  const [hubName, setHubName] = useState('');
  const [market, setMarket] = useState([]);

  // 分页：已安装技能 / 市场搜索结果（searchSeq 变化时回到第 1 页）
  const [searchSeq, setSearchSeq] = useState(0);
  const instPg = usePagedList(skills, { size: 10, resetKey: tab });
  const mktPg = usePagedList(market, { size: 10, resetKey: searchSeq });

  const load = useCallback(async () => {
    setError('');
    try {
      const [st, list, hb] = await Promise.all([
        skill.getSkillStats(),
        skill.listSkills({ limit: 200 }),
        skill.listHubs(),
      ]);
      setStats(st);
      setSkills(list.skills || []);
      setHubs(hb.hubs || []);
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

  const openDetail = async (name) => {
    setDetail(null);
    setFiles(null);
    setFileBody(null);
    try {
      const [sk, fl] = await Promise.all([skill.getSkill({ name }), skill.listSkillFiles({ name })]);
      setDetail(sk);
      setFiles(fl);
    } catch (err) {
      setError(err.message);
    }
  };

  const readFile = async (name, path) => {
    setFileBody(null);
    try {
      setFileBody(await skill.readSkillFile({ name, path }));
    } catch (err) {
      setError(err.message);
    }
  };

  const search = async () => {
    setBusy('search');
    setError('');
    try {
      const res = await skill.searchHubSkills({ q: keyword, hub: hubName, limit: 50 });
      setMarket(res.skills || []);
      setSearchSeq((v) => v + 1);
      if (!res.skills?.length) setNotice('没有搜索到技能，确认市场地址与网络');
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const install = async () => {
    let parsed = [];
    if (form.files.trim()) {
      try {
        parsed = JSON.parse(form.files);
      } catch {
        setError('files 需为 JSON 数组，形如 [{"path":"scripts/run.py","content":"..."}]');
        return;
      }
    }
    await act(
      'install',
      async () => {
        await skill.installSkill({ name: form.name, version: form.version, content: form.content, files: parsed });
        setForm({ name: '', version: '1.0.0', content: '', files: '' });
        setShowForm(false);
      },
      '技能已安装/升级',
    );
  };

  // 从 URL 安装：后端按响应形态分流——zip 走包内结构、单文件当 SKILL.md、JSON 按字段解析
  const installFromURL = async () => {
    await act(
      'url-install',
      async () => {
        await skill.importSkillFromURL({
          url: urlForm.url.trim(),
          name: urlForm.name.trim(),
          version: urlForm.version.trim(),
          token: urlForm.token.trim(),
          enable: urlForm.enable,
        });
        setUrlForm({ url: '', name: '', version: '', token: '', enable: true });
        setShowUrlForm(false);
      },
      '已从 URL 安装技能',
    );
  };

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="技能总数" value={stats?.skills ?? 0} hint="MySQL skills" tone="blue" />
        <StatCard label="已启用" value={stats?.enabled ?? 0} hint="可落盘给运行时读取" tone="teal" />
        <StatCard label="技能市场" value={stats?.hubs ?? 0} hint="skill_hubs" tone="indigo" />
        <StatCard label="已安装" value={instPg.total} hint={`每页 ${instPg.size} 条 · 共 ${instPg.pages} 页`} tone="violet" />
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
          <button
            className="btn ghost sm"
            type="button"
            onClick={async () => {
              try {
                const r = await skill.syncSkills();
                setNotice(`已落盘到 ${r.dir}：同步 ${r.synced?.length || 0} 个，清理 ${r.removed?.length || 0} 个`);
              } catch (err) {
                setError(err.message);
              }
            }}
          >
            落盘同步
          </button>
          <button className="btn ghost sm" type="button" onClick={load}>
            <IconRefresh size={15} /> 刷新
          </button>
        </div>
      </div>

      {tab === 'installed' && (
        <>
          <Panel
            title="技能列表"
            desc="同名视为升级；启停即时生效"
            actions={
              <>
                <button className="btn ghost sm" type="button" onClick={() => setShowUrlForm((v) => !v)}>
                  {showUrlForm ? '收起' : '从 URL 安装'}
                </button>
                <button className="btn ghost sm" type="button" onClick={() => setShowForm((v) => !v)}>
                  {showForm ? '收起' : '手动安装'}
                </button>
              </>
            }
          >
            {skills.length === 0 ? (
              <EmptyState icon={IconBox} title="暂无技能" desc="可从技能市场安装，或手动粘贴 SKILL.md" />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>名称</th>
                      <th>版本</th>
                      <th>来源</th>
                      <th className="num">文件</th>
                      <th>状态</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {instPg.paged.map((s) => (
                      <tr key={s.name} className={s.name === detail?.name ? 'row-active' : ''}>
                        <td className="mono">
                          {s.name}
                          {s.category && <Badge>{s.category}</Badge>}
                        </td>
                        <td className="mono">{s.version || '—'}</td>
                        <td>{s.source}</td>
                        <td className="num">{s.file_count}</td>
                        <td>{s.enabled ? <Badge tone="ok">启用</Badge> : <Badge tone="warn">停用</Badge>}</td>
                        <td className="num">
                          <button className="btn ghost sm" type="button" onClick={() => openDetail(s.name)}>
                            查看
                          </button>
                          <button
                            className="btn ghost sm"
                            type="button"
                            disabled={!s.enabled}
                            title="返回技能内容并落一条使用历史（可在「调用历史」页查看）"
                            onClick={() =>
                              act(
                                `u-${s.name}`,
                                () => skill.useSkill({ name: s.name, purpose: 'console 手动使用' }),
                                `${s.name} 已记录一次使用`,
                              )
                            }
                          >
                            使用
                          </button>
                          <button
                            className="btn ghost sm"
                            type="button"
                            onClick={() =>
                              act(
                                `t-${s.name}`,
                                () => (s.enabled ? skill.disableSkill({ name: s.name }) : skill.enableSkill({ name: s.name })),
                                `${s.name} 已${s.enabled ? '停用' : '启用'}`,
                              )
                            }
                          >
                            {s.enabled ? '停用' : '启用'}
                          </button>
                          <button
                            className="btn danger sm"
                            type="button"
                            disabled={busy === `d-${s.name}`}
                            onClick={() => act(`d-${s.name}`, () => skill.uninstallSkill({ name: s.name }), `${s.name} 已卸载`)}
                          >
                            {busy === `d-${s.name}` ? <Spinner text="卸载" /> : <IconTrash size={14} />}
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            <Pagination page={instPg.page} pages={instPg.pages} total={instPg.total} size={instPg.size} onPage={instPg.setPage} onSize={instPg.setSize} />

            {showForm && (
              <FormDrawer title="手动安装技能" desc="粘贴 SKILL.md 内容；同名视为升级" onClose={() => setShowForm(false)}>
              <div className="form">
                <div className="query-row">
                  <label className="field inline">
                    <span className="field-label">名称（小写字母/数字开头）</span>
                    <input
                      className="input mono"
                      value={form.name}
                      onChange={(e) => setForm({ ...form, name: e.target.value })}
                    />
                  </label>
                  <label className="field inline">
                    <span className="field-label">版本</span>
                    <input
                      className="input mono"
                      value={form.version}
                      onChange={(e) => setForm({ ...form, version: e.target.value })}
                    />
                  </label>
                </div>
                <label className="field">
                  <span className="field-label">SKILL.md 内容</span>
                  <textarea
                    className="textarea mono"
                    rows={6}
                    value={form.content}
                    onChange={(e) => setForm({ ...form, content: e.target.value })}
                  />
                </label>
                <label className="field">
                  <span className="field-label">附加文件（JSON 数组，可选）</span>
                  <textarea
                    className="textarea mono"
                    rows={3}
                    placeholder='[{"path":"scripts/run.py","content":"print(1)"}]'
                    value={form.files}
                    onChange={(e) => setForm({ ...form, files: e.target.value })}
                  />
                </label>
                <div className="form-actions">
                  <button
                    className="btn primary sm"
                    type="button"
                    onClick={install}
                    disabled={!form.name || (!form.content && !form.files) || busy === 'install'}
                  >
                    {busy === 'install' ? <Spinner text="安装中" /> : '安装'}
                  </button>
                </div>
              </div>
              </FormDrawer>
            )}

            {showUrlForm && (
              <FormDrawer
                title="从 URL 安装技能"
                desc="支持 zip 技能包 / 单个 SKILL.md / JSON 包；千问这类只给下载链接的来源走这里"
                onClose={() => setShowUrlForm(false)}
              >
                <div className="form">
                  <label className="field">
                    <span className="field-label">技能包地址（http/https）</span>
                    <input
                      className="input mono"
                      placeholder="https://example.com/skills/xxx.zip"
                      value={urlForm.url}
                      onChange={(e) => setUrlForm({ ...urlForm, url: e.target.value })}
                    />
                    <span className="cell-sub" style={{ marginTop: 4 }}>
                      zip 按包内 SKILL.md + 附件入库；单文件按 SKILL.md 正文处理（此时必须填名称）
                    </span>
                  </label>
                  <div className="query-row">
                    <label className="field inline">
                      <span className="field-label">名称（包内没有时必填）</span>
                      <input
                        className="input mono"
                        placeholder="留空则取包内名称"
                        value={urlForm.name}
                        onChange={(e) => setUrlForm({ ...urlForm, name: e.target.value })}
                      />
                    </label>
                    <label className="field inline">
                      <span className="field-label">版本（可空）</span>
                      <input
                        className="input mono"
                        placeholder="1.0.0"
                        value={urlForm.version}
                        onChange={(e) => setUrlForm({ ...urlForm, version: e.target.value })}
                      />
                    </label>
                  </div>
                  <label className="field">
                    <span className="field-label">访问令牌（可空，私有链接用）</span>
                    <input
                      className="input mono"
                      type="password"
                      placeholder="Authorization: Bearer …"
                      value={urlForm.token}
                      onChange={(e) => setUrlForm({ ...urlForm, token: e.target.value })}
                    />
                  </label>
                  <div className="form-actions">
                    <label className="chip" style={{ cursor: 'pointer' }}>
                      <input
                        type="checkbox"
                        checked={urlForm.enable}
                        onChange={(e) => setUrlForm({ ...urlForm, enable: e.target.checked })}
                      />{' '}
                      安装后立即启用
                    </label>
                    <button
                      className="btn primary sm"
                      type="button"
                      onClick={installFromURL}
                      disabled={!urlForm.url.trim() || busy === 'url-install'}
                    >
                      {busy === 'url-install' ? <Spinner text="拉取中" /> : '拉取并安装'}
                    </button>
                  </div>
                </div>
              </FormDrawer>
            )}
          </Panel>

          {detail && (
            <Panel
              title={`${detail.title || detail.name}`}
              desc={detail.description || '无描述'}
              actions={<Badge tone="indigo">{detail.source}</Badge>}
            >
              <div className="grid-2 align-start">
                <div>
                  <div className="field-label">文件清单</div>
                  <ul className="list-reset">
                    <li>
                      <button className="btn ghost sm" type="button" onClick={() => readFile(detail.name, 'SKILL.md')}>
                        SKILL.md
                      </button>
                      <span className="muted mono" style={{ marginLeft: 'auto' }}>
                        {files?.size ?? '—'} B
                      </span>
                    </li>
                    {(files?.files || []).map((f) => (
                      <li key={f.path}>
                        <button className="btn ghost sm" type="button" onClick={() => readFile(detail.name, f.path)}>
                          {f.path}
                        </button>
                        <span className="muted mono" style={{ marginLeft: 'auto' }}>
                          {f.size} B
                        </span>
                      </li>
                    ))}
                  </ul>
                </div>
                <div>
                  <div className="field-label">{fileBody ? fileBody.path : '文件内容'}</div>
                  <pre className="block mono">{fileBody ? fileBody.content : '点击左侧文件查看内容'}</pre>
                </div>
              </div>
            </Panel>
          )}

          <HistoryPanel
            kind="skill_use"
            title="技能使用历史"
            desc="每次技能使用的用途说明、耗时与关联任务"
            emptyTitle="暂无使用记录"
            emptyDesc="点技能行上的「使用」按钮后，这里会留下记录；Agent 任务内的使用会带上任务号"
          />
        </>
      )}

      {tab === 'market' && (
        <Panel
          title="技能市场"
          desc="按关键字搜索市场条目，直接安装（支持 JSON / ZIP 包）"
          actions={
            <>
              <select className="input sm" value={hubName} onChange={(e) => setHubName(e.target.value)}>
                <option value="">默认市场</option>
                {hubs.map((h) => (
                  <option key={h.name} value={h.name}>
                    {h.name}
                  </option>
                ))}
              </select>
              <input
                className="input sm"
                placeholder="关键字"
                value={keyword}
                onChange={(e) => setKeyword(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && search()}
              />
              <button className="btn ghost sm" type="button" onClick={search} disabled={busy === 'search'}>
                {busy === 'search' ? <Spinner text="搜索" /> : <><IconSearch size={15} /> 搜索</>}
              </button>
            </>
          }
        >
          {market.length === 0 ? (
            <EmptyState icon={IconSearch} title="还没有搜索结果" desc="输入关键字后回车，或直接用一个技能名搜索" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>名称</th>
                    <th>版本</th>
                    <th>描述</th>
                    <th className="num">下载</th>
                    <th className="num">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {mktPg.paged.map((m) => (
                    <tr key={`${m.hub}/${m.name}`}>
                      <td>
                        <div className="cell-main">{m.title || m.name}</div>
                        <div className="cell-sub mono">{m.name}</div>
                      </td>
                      <td className="mono">{m.version || '—'}</td>
                      <td className="ellipsis" title={m.description}>
                        {m.description || m.title || '—'}
                      </td>
                      <td className="num">{m.downloads ?? 0}</td>
                      <td className="num">
                        {m.installed ? (
                          <Badge tone="ok">已安装</Badge>
                        ) : (
                          <button
                            className="btn primary sm"
                            type="button"
                            disabled={busy === `i-${m.name}`}
                            onClick={() =>
                              act(
                                `i-${m.name}`,
                                () => skill.installFromHub({ name: m.name, version: m.version, hub: m.hub, enable: true }),
                                `${m.name} 已安装`,
                              )
                            }
                          >
                            {busy === `i-${m.name}` ? <Spinner text="安装" /> : '安装'}
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <Pagination page={mktPg.page} pages={mktPg.pages} total={mktPg.total} size={mktPg.size} onPage={mktPg.setPage} onSize={mktPg.setSize} />
        </Panel>
      )}

      {tab === 'hubs' && (
        <Panel
          title="技能市场配置"
          desc="地址存 MySQL，conf 只做兜底；默认市场用于未指定 hub 的调用"
          actions={<button className="btn ghost sm" type="button" onClick={() => setTab('hubs')}>刷新</button>}
        >
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>名称</th>
                  <th>地址</th>
                  <th>状态</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {hubs.map((h) => (
                  <tr key={h.name}>
                    <td className="mono">
                      {h.name}
                      {h.has_token && <Badge tone="ok">Token</Badge>}
                    </td>
                    <td className="mono ellipsis" title={h.base_url}>
                      {h.base_url}
                    </td>
                    <td>
                      {h.is_default && <Badge tone="indigo">默认</Badge>}{' '}
                      {h.enabled ? <Badge tone="ok">启用</Badge> : <Badge tone="warn">停用</Badge>}
                    </td>
                    <td className="num">
                      <button
                        className="btn ghost sm"
                        type="button"
                        disabled={h.is_default}
                        onClick={() => act(`dh-${h.name}`, () => skill.setDefaultHub({ name: h.name }), `${h.name} 设为默认市场`)}
                      >
                        设为默认
                      </button>
                      <button
                        className="btn danger sm"
                        type="button"
                        onClick={() => act(`rh-${h.name}`, () => skill.deleteHub({ name: h.name }), `${h.name} 已删除`)}
                      >
                        <IconTrash size={14} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <HubForm onCreate={(payload) => act('hub', () => skill.createHub(payload), '市场已新增')} busy={busy === 'hub'} />
        </Panel>
      )}
    </div>
  );
}

function HubForm({ onCreate, busy }) {
  const [form, setForm] = useState({ name: '', base_url: '', token: '', description: '' });
  return (
    <div className="form" style={{ marginTop: 16 }}>
      <div className="query-row">
        <label className="field inline">
          <span className="field-label">市场名称</span>
          <input className="input mono" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
        </label>
        <label className="field inline">
          <span className="field-label">Base URL</span>
          <input
            className="input mono"
            placeholder="https://skillhub.example.com"
            value={form.base_url}
            onChange={(e) => setForm({ ...form, base_url: e.target.value })}
          />
        </label>
        <label className="field inline">
          <span className="field-label">Token（可选）</span>
          <input
            className="input mono"
            type="password"
            value={form.token}
            onChange={(e) => setForm({ ...form, token: e.target.value })}
          />
        </label>
      </div>
      <div className="form-actions">
        <button
          className="btn primary sm"
          type="button"
          disabled={!form.name || !form.base_url || busy}
          onClick={() => onCreate({ ...form, enabled: true })}
        >
          {busy ? <Spinner text="保存中" /> : '新增市场'}
        </button>
      </div>
    </div>
  );
}
