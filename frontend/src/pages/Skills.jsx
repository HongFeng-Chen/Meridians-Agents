import { useCallback, useEffect, useMemo, useState } from 'react';
import { skill } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import SkillDetailDrawer from '../components/SkillDetailDrawer.jsx';
import SkillExecutionDrawer from '../components/SkillExecutionDrawer.jsx';
import SkillRunDrawer from '../components/SkillRunDrawer.jsx';
import {
  EXEC_LABELS,
  EXEC_STATUS_OPTIONS,
  MODE_LABELS,
  MODE_TONES,
  execTone,
  fmtLatency,
  isExecutableMode,
  shortTime,
} from '../components/skillRuntime.js';
import { IconBox, IconRefresh, IconSearch, IconTrash } from '../components/Icons.jsx';

const TABS = [
  { key: 'installed', label: '已安装' },
  { key: 'executions', label: '执行记录' },
  { key: 'market', label: '技能市场' },
  { key: 'hubs', label: '市场配置' },
];

const MODE_FILTERS = [
  { key: '', label: '全部' },
  { key: 'local', label: '本地' },
  { key: 'remote', label: '远程' },
  { key: 'async', label: '异步' },
];

const EMPTY_EXEC_FILTER = { skill: '', status: '', task_id: '' };

// 技能页面 = 技能清单 + 运行时操作台。
//
// 技能不再只是「一段 SKILL.md」：远程/异步技能有执行配置、Worker 实例和执行账本，
// 所以这里除了增删启停，还要能改执行配置、盯实例状态、手动试跑和查执行历史。
export default function Skills() {
  const [tab, setTab] = useState('installed');
  const [stats, setStats] = useState(null);
  const [skills, setSkills] = useState([]);
  const [hubs, setHubs] = useState([]);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  // 详情抽屉
  const [detail, setDetail] = useState(null);
  const [files, setFiles] = useState(null);

  // 试跑 / 执行详情
  const [runTarget, setRunTarget] = useState(null);
  const [runInput, setRunInput] = useState('');
  const [execId, setExecId] = useState('');
  const [execInitial, setExecInitial] = useState(null);

  // 已安装筛选
  const [modeFilter, setModeFilter] = useState('');
  const [keyword, setKeyword] = useState('');

  // 执行记录
  const [execFilters, setExecFilters] = useState(EMPTY_EXEC_FILTER);
  const [execRows, setExecRows] = useState([]);
  const [execLoading, setExecLoading] = useState(false);
  const [execError, setExecError] = useState('');

  // 安装表单
  const [form, setForm] = useState({ name: '', version: '1.0.0', content: '', files: '' });
  const [showForm, setShowForm] = useState(false);
  const [showUrlForm, setShowUrlForm] = useState(false);
  const [urlForm, setUrlForm] = useState({ url: '', name: '', version: '', token: '', enable: true });

  // 市场
  const [hubName, setHubName] = useState('');
  const [marketKeyword, setMarketKeyword] = useState('');
  const [market, setMarket] = useState([]);
  const [searchSeq, setSearchSeq] = useState(0);

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

  const loadExecutions = useCallback(async () => {
    setExecLoading(true);
    setExecError('');
    try {
      const res = await skill.listSkillExecutions({
        skill: execFilters.skill,
        status: execFilters.status,
        task_id: execFilters.task_id.trim(),
        limit: 200,
      });
      setExecRows(res.executions || []);
    } catch (err) {
      setExecError(err.message);
    } finally {
      setExecLoading(false);
    }
  }, [execFilters]);

  useEffect(() => {
    if (tab === 'executions') loadExecutions();
  }, [tab, loadExecutions]);

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

  const filteredSkills = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    return skills.filter((s) => {
      const mode = s.execution?.mode || 'local';
      if (modeFilter && mode !== modeFilter) return false;
      if (!kw) return true;
      return [s.name, s.title, s.description, s.category, ...(s.tags || [])]
        .filter(Boolean)
        .some((v) => String(v).toLowerCase().includes(kw));
    });
  }, [skills, modeFilter, keyword]);

  const instPg = usePagedList(filteredSkills, { size: 10, resetKey: `${modeFilter}|${keyword}` });
  const execPg = usePagedList(execRows, { size: 20, resetKey: JSON.stringify(execFilters) });
  const mktPg = usePagedList(market, { size: 10, resetKey: searchSeq });

  const modeCount = useMemo(() => {
    let local = 0;
    let executable = 0;
    for (const s of skills) {
      if (isExecutableMode(s.execution?.mode)) executable += 1;
      else local += 1;
    }
    return { local, executable };
  }, [skills]);

  const execSummary = useMemo(() => {
    const pending = execRows.filter((e) => e.status === 'queued' || e.status === 'running').length;
    const failed = execRows.filter((e) => e.status === 'failed' || e.status === 'timeout').length;
    return { pending, failed };
  }, [execRows]);

  const openDetail = async (name) => {
    setFiles(null);
    setError('');
    try {
      const [sk, fl] = await Promise.all([skill.getSkill({ name }), skill.listSkillFiles({ name })]);
      setDetail(sk);
      setFiles(fl);
    } catch (err) {
      setError(err.message);
    }
  };

  const openRun = (target, input = '') => {
    setRunInput(input);
    setRunTarget(target);
  };

  // 从执行记录重跑：把原来的 input 原样带回试跑抽屉
  const rerun = (exec) => {
    const target = skills.find((s) => s.name === exec.skill_name) || {
      name: exec.skill_name,
      title: exec.skill_name,
      execution: { mode: exec.mode },
    };
    let text = exec.input || '{}';
    try {
      text = JSON.stringify(JSON.parse(text), null, 2);
    } catch {
      // 保留原始文本
    }
    setExecId('');
    setExecInitial(null);
    openRun(target, text);
  };

  const openExecutionsFor = (skillName) => {
    setDetail(null);
    setExecFilters({ ...EMPTY_EXEC_FILTER, skill: skillName || '' });
    setTab('executions');
  };

  const search = async () => {
    setBusy('search');
    setError('');
    try {
      const res = await skill.searchHubSkills({ q: marketKeyword, hub: hubName, limit: 50 });
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
        <StatCard label="技能总数" value={skills.length || stats?.skills || 0} hint="MySQL skills" tone="blue" />
        <StatCard label="已启用" value={stats?.enabled ?? 0} hint="可被 Agent 调用" tone="teal" />
        <StatCard label="远程 / 异步" value={modeCount.executable} hint="经过 Worker 分布式执行" tone="indigo" />
        <StatCard label="本地指令包" value={modeCount.local} hint="只注入 SKILL.md" tone="violet" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <div className="tabs">
        {TABS.map((t) => (
          <button key={t.key} type="button" className={`tab${tab === t.key ? ' active' : ''}`} onClick={() => setTab(t.key)}>
            {t.label}
            {t.key === 'executions' && execRows.length > 0 && <span className="tab-count">{execRows.length}</span>}
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
          <button className="btn ghost sm" type="button" onClick={() => (tab === 'executions' ? loadExecutions() : load())}>
            <IconRefresh size={15} /> 刷新
          </button>
        </div>
      </div>

      {tab === 'installed' && (
        <>
          <Panel
            title="技能列表"
            desc="同名视为升级；启停即时生效；远程/异步技能可试跑并查看落点 Worker"
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
            <div className="query-row" style={{ marginBottom: 12 }}>
              <div className="chips">
                {MODE_FILTERS.map((f) => (
                  <button
                    key={f.key || 'all'}
                    type="button"
                    className={`chip${modeFilter === f.key ? ' active' : ''}`}
                    onClick={() => setModeFilter(f.key)}
                  >
                    {f.label}
                  </button>
                ))}
              </div>
              <label className="field inline" style={{ maxWidth: 280 }}>
                <input
                  className="input sm"
                  placeholder="按名称 / 标题 / 标签过滤"
                  value={keyword}
                  onChange={(e) => setKeyword(e.target.value)}
                />
              </label>
              <span className="muted" style={{ fontSize: 12.5, marginBottom: 6 }}>
                共 {filteredSkills.length} 个
              </span>
            </div>

            {filteredSkills.length === 0 ? (
              <EmptyState icon={IconBox} title="没有匹配的技能" desc="换个筛选条件，或从技能市场安装 / 手动粘贴 SKILL.md" />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>名称</th>
                      <th>版本</th>
                      <th>来源</th>
                      <th>模式</th>
                      <th className="num">文件</th>
                      <th>状态</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {instPg.paged.map((s) => {
                      const mode = s.execution?.mode || 'local';
                      return (
                        <tr key={s.name} className={s.name === detail?.name ? 'row-active' : ''}>
                          <td>
                            <div className="cell-main mono">{s.name}</div>
                            <div className="cell-sub">
                              {s.title || '—'}
                              {s.category ? ` · ${s.category}` : ''}
                            </div>
                          </td>
                          <td className="mono">{s.version || '—'}</td>
                          <td>{s.source}</td>
                          <td>
                            <span className={`badge ${MODE_TONES[mode]}`}>{MODE_LABELS[mode]}</span>
                            {isExecutableMode(mode) && s.execution?.handler && (
                              <div className="cell-sub mono">handler={s.execution.handler}</div>
                            )}
                          </td>
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
                              title={isExecutableMode(mode) ? '按当前执行配置真实调用一次' : '本地模式：返回 SKILL.md 内容'}
                              onClick={() => openRun(s)}
                            >
                              试跑
                            </button>
                            <button
                              className="btn ghost sm"
                              type="button"
                              disabled={!s.enabled}
                              title="返回技能内容并落一条使用历史（可在「技能使用历史」查看）"
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
                      );
                    })}
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
                    <span className="muted" style={{ fontSize: 12.5 }}>
                      安装后到技能详情里可以把执行模式改成远程或异步
                    </span>
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

          <HistoryPanel
            kind="skill_use"
            title="技能使用历史"
            desc="每次技能使用的用途说明、耗时与关联任务"
            emptyTitle="暂无使用记录"
            emptyDesc="点技能行上的「使用」按钮后，这里会留下记录；Agent 任务内的使用会带上任务号"
          />
        </>
      )}

      {tab === 'executions' && (
        <Panel
          title="执行记录"
          desc="远程/异步技能的执行账本：排队、落点、尝试次数与耗时都在这；本地模式不记录"
          actions={
            <>
              <select
                className="input sm"
                value={execFilters.skill}
                onChange={(e) => setExecFilters({ ...execFilters, skill: e.target.value })}
              >
                <option value="">全部技能</option>
                {skills
                  .filter((s) => isExecutableMode(s.execution?.mode))
                  .map((s) => (
                    <option key={s.name} value={s.name}>
                      {s.name}
                    </option>
                  ))}
              </select>
              <input
                className="input sm"
                style={{ maxWidth: 140 }}
                placeholder="任务号"
                value={execFilters.task_id}
                onChange={(e) => setExecFilters({ ...execFilters, task_id: e.target.value })}
                onKeyDown={(e) => e.key === 'Enter' && loadExecutions()}
              />
              <button className="btn ghost sm" type="button" onClick={loadExecutions} disabled={execLoading}>
                {execLoading ? <Spinner text="查询" /> : '查询'}
              </button>
            </>
          }
        >
          <div className="query-row" style={{ marginBottom: 12 }}>
            <div className="chips">
              <button
                type="button"
                className={`chip${execFilters.status === '' ? ' active' : ''}`}
                onClick={() => setExecFilters({ ...execFilters, status: '' })}
              >
                全部状态
              </button>
              {EXEC_STATUS_OPTIONS.map((s) => (
                <button
                  key={s}
                  type="button"
                  className={`chip${execFilters.status === s ? ' active' : ''}`}
                  onClick={() => setExecFilters({ ...execFilters, status: s })}
                >
                  {EXEC_LABELS[s]}
                </button>
              ))}
            </div>
            <span className="muted" style={{ fontSize: 12.5, marginBottom: 6 }}>
              最近 {execRows.length} 条 · 排队/执行中 {execSummary.pending} · 失败/超时 {execSummary.failed}
            </span>
          </div>

          {execError && <Alert type="error">{execError}</Alert>}

          {!execLoading && execRows.length === 0 ? (
            <EmptyState
              icon={IconBox}
              title="没有执行记录"
              desc="远程/异步技能被 Agent 调用或手动试跑后，会在这里留下账本；本地技能只注入 SKILL.md，不记录"
            />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>执行 ID</th>
                    <th>技能</th>
                    <th>模式</th>
                    <th>状态</th>
                    <th>任务</th>
                    <th>落点</th>
                    <th className="num">尝试</th>
                    <th className="num">耗时</th>
                    <th>时间</th>
                  </tr>
                </thead>
                <tbody>
                  {execPg.paged.map((e) => (
                    <tr
                      key={e.id}
                      className="row-click"
                      onClick={() => {
                        setExecInitial(e);
                        setExecId(e.id);
                      }}
                    >
                      <td className="mono" style={{ fontSize: 12 }}>{e.id}</td>
                      <td className="mono">{e.skill_name}</td>
                      <td>
                        <span className={`badge ${MODE_TONES[e.mode] || 'default'}`}>{MODE_LABELS[e.mode] || e.mode}</span>
                      </td>
                      <td>
                        <span className={`badge ${execTone(e.status)}`}>{EXEC_LABELS[e.status] || e.status}</span>
                        {e.error && <div className="cell-sub ellipsis" title={e.error}>{e.error}</div>}
                      </td>
                      <td className="mono">{e.task_id || '—'}</td>
                      <td className="mono" style={{ fontSize: 12 }}>{e.worker_id || '—'}</td>
                      <td className="num">{e.attempts || 1}</td>
                      <td className="num">{fmtLatency(e.latency_ms)}</td>
                      <td className="mono" style={{ fontSize: 12 }}>{shortTime(e.created_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          <Pagination page={execPg.page} pages={execPg.pages} total={execPg.total} size={execPg.size} onPage={execPg.setPage} onSize={execPg.setSize} />
          {execRows.length >= 200 && (
            <div className="muted" style={{ fontSize: 12.5, marginTop: 8 }}>
              只展示最近 200 条；用技能或任务号筛选可以缩小范围。
            </div>
          )}
        </Panel>
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
                value={marketKeyword}
                onChange={(e) => setMarketKeyword(e.target.value)}
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
          actions={<button className="btn ghost sm" type="button" onClick={load}>刷新</button>}
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

      {detail && (
        <SkillDetailDrawer
          detail={detail}
          files={files}
          onClose={() => setDetail(null)}
          onChanged={load}
          onRun={openRun}
          onOpenExecution={(id) => {
            setExecInitial(null);
            setExecId(id);
          }}
          onOpenAllExecutions={openExecutionsFor}
        />
      )}

      {runTarget && (
        <SkillRunDrawer
          target={runTarget}
          initialInput={runInput}
          onClose={() => {
            setRunTarget(null);
            setRunInput('');
            if (tab === 'executions') loadExecutions();
          }}
          onOpenExecution={(id) => {
            setExecInitial(null);
            setExecId(id);
          }}
        />
      )}

      {execId && (
        <SkillExecutionDrawer
          executionId={execId}
          initial={execInitial && execInitial.id === execId ? execInitial : null}
          onClose={() => {
            setExecId('');
            setExecInitial(null);
          }}
          onRerun={rerun}
          onChanged={() => {
            if (tab === 'executions') loadExecutions();
          }}
        />
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
