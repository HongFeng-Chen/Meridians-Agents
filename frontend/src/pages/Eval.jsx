import { useCallback, useEffect, useRef, useState } from 'react';
import { harness } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Pagination, Panel, Spinner, StatCard, usePagedList } from '../components/UI.jsx';
import { IconFlask, IconRefresh, IconTrash } from '../components/Icons.jsx';

// 评测 Harness 管理页：
//   harness 是「考官」不是「考生」——用例集（被测对象 + 用例）→ 启动异步运行 →
//   规则断言 + LLM Judge 判分 → 报告聚合。进度实时落库，运行中 3s 轮询。
//   被测对象两种形态：agent:<agent_key>（走执行图）/ model:<模型名>（直连对话）。

const TABS = [
  { key: 'sets', label: '用例集' },
  { key: 'runs', label: '运行记录' },
];

// 断言规则七类（与 biz/harness types.go 一致）
const CHECK_KINDS = [
  { v: 'contains', label: '包含（忽略大小写）', ph: '期望包含的文本' },
  { v: 'not_contains', label: '不包含', ph: '禁止出现的文本' },
  { v: 'regex', label: '正则匹配', ph: '正则表达式' },
  { v: 'json_valid', label: '可解析 JSON', ph: '' },
  { v: 'min_length', label: '长度下限', ph: '如 50' },
  { v: 'max_length', label: '长度上限', ph: '如 2000' },
  { v: 'llm_judge', label: 'LLM 评审', ph: '评分要点，如：答案需涵盖退货时限与运费承担方' },
];

const RUN_STATUS_TONES = {
  pending: 'default',
  running: 'indigo',
  succeeded: 'ok',
  failed: 'error',
  canceled: 'warn',
};

const EMPTY_SET_FORM = { id: 0, name: '', description: '', target: '', tags: '', enabled: true };
const EMPTY_CASE_FORM = {
  id: 0, set_id: 0, name: '', input: '', context: '', expect: '',
  checks: [], tags: '', enabled: true,
};
const EMPTY_RUN_FORM = { set_id: 0, target: '', judge_model: '', concurrency: 2, per_case_timeout_sec: 120, prompt_key: '', prompt_version: 0 };

function fmtTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return v;
  return d.toLocaleString('zh-CN', { hour12: false });
}

function fmtScore(v) {
  if (v === undefined || v === null || v < 0) return '—';
  return `${Number(v).toFixed(1)}`;
}

export default function Eval() {
  const [tab, setTab] = useState('sets');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  // —— 用例集 ——
  const [sets, setSets] = useState([]);
  const [setKw, setSetKw] = useState('');
  const [setForm, setSetForm] = useState(null);
  // 用例管理抽屉：null=关闭；{ set, cases }
  const [caseBook, setCaseBook] = useState(null);
  const [caseForm, setCaseForm] = useState(null);

  // —— 运行 ——
  const [runs, setRuns] = useState([]);
  const [runSetFilter, setRunSetFilter] = useState('');
  const [runStatusFilter, setRunStatusFilter] = useState('');
  const [runForm, setRunForm] = useState(null); // 启动运行
  // 报告抽屉：null=关闭；{ run, items, loading }
  const [report, setReport] = useState(null);
  const [reportItem, setReportItem] = useState(null);
  const pollRef = useRef(null);

  const loadSets = useCallback(async (kw = setKw) => {
    try {
      const res = await harness.listSets({ q: kw, limit: 100 });
      setSets(res.sets || []);
      setError('');
    } catch (err) {
      setError(err.message);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const loadRuns = useCallback(async () => {
    try {
      const res = await harness.listRuns({
        set_id: runSetFilter || 0,
        status: runStatusFilter,
        limit: 100,
      });
      setRuns(res.runs || []);
      setError('');
    } catch (err) {
      setError(err.message);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runSetFilter, runStatusFilter]);

  useEffect(() => { loadSets(); }, [loadSets]);
  useEffect(() => { loadRuns(); }, [loadRuns]);

  // 深链：提示词工程页「评测此版本」跳转过来，预填提示词版本评测标注
  // （agent.<agent_key>.system 模板可同时推出被测对象 agent:<agent_key>）
  useEffect(() => {
    const q = new URLSearchParams(window.location.hash.split('?')[1] || '');
    const pk = q.get('prompt');
    if (!pk) return;
    const pv = Number(q.get('version')) || 0;
    let target = '';
    const m = pk.match(/^agent\.([^.]+)\.system$/);
    if (m) target = `agent:${m[1]}`;
    setRunForm({ ...EMPTY_RUN_FORM, target, prompt_key: pk, prompt_version: pv });
  }, []);

  // 运行中自动轮询（3s），全部到终态后停止
  useEffect(() => {
    const active = runs.some((r) => r.status === 'pending' || r.status === 'running');
    if (!active || tab !== 'runs') {
      if (pollRef.current) { clearInterval(pollRef.current); pollRef.current = null; }
      return;
    }
    if (pollRef.current) return;
    pollRef.current = setInterval(loadRuns, 3000);
    return () => {
      if (pollRef.current) { clearInterval(pollRef.current); pollRef.current = null; }
    };
  }, [runs, tab, loadRuns]);

  const act = async (key, fn) => {
    setBusy(key);
    setError('');
    try {
      return await fn();
    } catch (err) {
      setError(err.message);
      return null;
    } finally {
      setBusy('');
    }
  };

  // —— 用例集操作 ——

  const saveSet = async () => {
    if (!setForm.name.trim()) { setError('用例集名称不能为空'); return; }
    const res = await act('saveSet', () => harness.saveSet({
      id: setForm.id,
      name: setForm.name.trim(),
      description: setForm.description,
      target: setForm.target.trim(),
      tags: setForm.tags.split(/[,，]/).map((t) => t.trim()).filter(Boolean),
      enabled: setForm.enabled,
    }));
    if (res) {
      setNotice(setForm.id ? '用例集已更新' : '用例集已创建');
      setSetForm(null);
      loadSets();
    }
  };

  const removeSet = async (s) => {
    if (!window.confirm(`删除用例集「${s.name}」？其下全部用例会一并删除。`)) return;
    const res = await act(`delSet-${s.id}`, () => harness.deleteSet({ id: s.id }));
    if (res !== null) { setNotice('用例集已删除'); loadSets(); }
  };

  const openCases = async (s) => {
    const res = await act(`cases-${s.id}`, () => harness.getSet({ id: s.id }));
    if (res) setCaseBook({ set: res.set, cases: res.cases || [] });
  };

  // —— 用例操作 ——

  const saveCase = async () => {
    if (!caseForm.input.trim()) { setError('用例输入不能为空'); return; }
    const res = await act('saveCase', () => harness.saveCase({
      id: caseForm.id,
      set_id: caseForm.set_id,
      name: caseForm.name.trim(),
      input: caseForm.input,
      context: caseForm.context,
      expect: caseForm.expect,
      checks: caseForm.checks.map((c) => ({ kind: c.kind, pattern: c.pattern, weight: Number(c.weight) || 0 })),
      tags: caseForm.tags.split(/[,，]/).map((t) => t.trim()).filter(Boolean),
      enabled: caseForm.enabled,
    }));
    if (res) {
      setNotice(caseForm.id ? '用例已更新' : '用例已添加');
      const fresh = await harness.getSet({ id: caseForm.set_id });
      setCaseBook({ set: fresh.set, cases: fresh.cases || [] });
      setCaseForm(null);
      loadSets();
    }
  };

  const removeCase = async (c) => {
    if (!window.confirm(`删除用例「${c.name || c.input.slice(0, 20)}」？`)) return;
    const res = await act(`delCase-${c.id}`, () => harness.deleteCase({ id: c.id }));
    if (res !== null) {
      setCaseBook((b) => ({ ...b, cases: b.cases.filter((x) => x.id !== c.id) }));
      loadSets();
    }
  };

  // —— 运行操作 ——

  const startRun = async () => {
    const res = await act('startRun', () => harness.startRun({
      set_id: runForm.set_id,
      target: runForm.target.trim(),
      judge_model: runForm.judge_model.trim(),
      concurrency: Number(runForm.concurrency) || 2,
      per_case_timeout_sec: Number(runForm.per_case_timeout_sec) || 120,
      // 提示词版本评测标注（从提示词工程页深链带过来）：运行完成后得分自动回写该版本
      prompt_key: runForm.prompt_key || '',
      prompt_version: Number(runForm.prompt_version) || 0,
    }));
    if (res?.run) {
      setNotice(`评测运行 #${res.run.id} 已启动${runForm.prompt_key ? '（完成后得分回写提示词版本）' : ''}`);
      setRunForm(null);
      setTab('runs');
      loadRuns();
    }
  };

  const stopRun = async (r) => {
    const res = await act(`stop-${r.id}`, () => harness.stopRun({ id: r.id }));
    if (res !== null) { setNotice(`运行 #${r.id} 停止信号已发出（当前用例完成后生效）`); loadRuns(); }
  };

  const removeRun = async (r) => {
    if (!window.confirm(`删除运行 #${r.id} 及其全部逐例结果？`)) return;
    const res = await act(`delRun-${r.id}`, () => harness.deleteRun({ id: r.id }));
    if (res !== null) { setNotice('运行已删除'); loadRuns(); }
  };

  const openReport = async (r) => {
    setReportItem(null);
    setReport({ run: r, items: [], loading: true });
    try {
      const res = await harness.getRun({ id: r.id });
      setReport({ run: res.run, items: res.items || [], loading: false });
    } catch (err) {
      setError(err.message);
      setReport(null);
    }
  };

  // —— 列表分页 ——
  const setsList = usePagedList(sets, { size: 8, resetKey: setKw });
  const runsList = usePagedList(runs, { size: 10, resetKey: `${runSetFilter}-${runStatusFilter}` });

  const activeRuns = runs.filter((r) => r.status === 'running' || r.status === 'pending');
  const lastRun = runs[0];

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h2>评测中心</h2>
          <p className="cell-sub">评测 Harness：用例集 → 异步运行 → 规则断言 + LLM 评审判分 → 报告聚合</p>
        </div>
        <div className="row-actions">
          <button className="btn ghost sm" type="button" onClick={() => (tab === 'sets' ? loadSets() : loadRuns())}>
            <IconRefresh /> 刷新
          </button>
          {tab === 'sets' && (
            <button className="btn primary sm" type="button" onClick={() => setSetForm({ ...EMPTY_SET_FORM })}>
              新建用例集
            </button>
          )}
        </div>
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="ok">{notice}</Alert>}

      <div className="tabs" role="tablist">
        {TABS.map((t) => (
          <button
            key={t.key}
            type="button"
            role="tab"
            aria-selected={tab === t.key}
            className={`tab${tab === t.key ? ' active' : ''}`}
            onClick={() => setTab(t.key)}
          >
            {t.label}
          </button>
        ))}
      </div>

      {tab === 'sets' && (
        <Panel
          title="用例集"
          desc="一个用例集 = 一个被测对象的一组用例；被测对象填 agent:<agent_key>（走执行图）或 model:<模型名>（模型直连）"
        >
          <div style={{ display: 'flex', gap: 8, marginBottom: 10 }}>
            <input
              className="input"
              style={{ width: 240 }}
              placeholder="搜索名称 / 描述 / 被测对象"
              value={setKw}
              onChange={(e) => { setSetKw(e.target.value); loadSets(e.target.value); }}
            />
          </div>
          {sets.length === 0 ? (
            <EmptyState icon={IconFlask} title="还没有用例集" desc="点右上「新建用例集」，先定义被测对象，再添加用例与断言" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>名称</th>
                    <th>被测对象</th>
                    <th className="num">启用用例</th>
                    <th>状态</th>
                    <th>更新时间</th>
                    <th className="num">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {setsList.paged.map((s) => (
                    <tr key={s.id}>
                      <td>
                        <div className="cell-main">{s.name}</div>
                        {s.description && <div className="cell-sub ellipsis" style={{ maxWidth: 260 }}>{s.description}</div>}
                      </td>
                      <td><span className="mono">{s.target || <span className="cell-sub">未设置</span>}</span></td>
                      <td className="num">{s.case_count}</td>
                      <td><Badge tone={s.enabled ? 'ok' : 'default'}>{s.enabled ? '启用' : '停用'}</Badge></td>
                      <td className="cell-sub">{fmtTime(s.updated_at)}</td>
                      <td className="num">
                        <button className="btn ghost sm" type="button" onClick={() => openCases(s)}>用例</button>
                        <button
                          className="btn ghost sm"
                          type="button"
                          disabled={busy === `run-${s.id}`}
                          onClick={() => setRunForm({ ...EMPTY_RUN_FORM, set_id: s.id, target: s.target })}
                        >
                          {busy === `run-${s.id}` ? <Spinner text="" /> : '运行'}
                        </button>
                        <button className="btn ghost sm" type="button" onClick={() => setSetForm({
                          id: s.id, name: s.name, description: s.description || '', target: s.target || '',
                          tags: (s.tags || []).join(', '), enabled: !!s.enabled,
                        })}
                        >
                          编辑
                        </button>
                        <button className="btn ghost sm" type="button" onClick={() => removeSet(s)}>
                          <IconTrash />
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {sets.length > setsList.size && (
            <Pagination page={setsList.page} pages={setsList.pages} total={setsList.total} size={setsList.size} onPage={setsList.setPage} onSize={setsList.setSize} />
          )}
        </Panel>
      )}

      {tab === 'runs' && (
        <Panel
          title="运行记录"
          desc={activeRuns.length > 0 ? `有 ${activeRuns.length} 次运行进行中，3 秒自动刷新` : '评测运行历史；运行进行中会自动轮询进度'}
        >
          <div style={{ display: 'flex', gap: 8, marginBottom: 10, flexWrap: 'wrap' }}>
            <select className="input" style={{ width: 180 }} value={runSetFilter} onChange={(e) => setRunSetFilter(e.target.value)}>
              <option value="">全部用例集</option>
              {sets.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
            </select>
            <select className="input" style={{ width: 140 }} value={runStatusFilter} onChange={(e) => setRunStatusFilter(e.target.value)}>
              <option value="">全部状态</option>
              <option value="pending">等待中</option>
              <option value="running">运行中</option>
              <option value="succeeded">已完成</option>
              <option value="failed">失败</option>
              <option value="canceled">已取消</option>
            </select>
          </div>
          {runs.length === 0 ? (
            <EmptyState icon={IconFlask} title="还没有评测运行" desc="到「用例集」Tab 点「运行」启动一次评测" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>运行</th>
                    <th>用例集</th>
                    <th>被测对象</th>
                    <th>状态</th>
                    <th className="num">进度</th>
                    <th className="num">通过 / 失败 / 异常</th>
                    <th className="num">得分</th>
                    <th className="num">耗时</th>
                    <th className="num">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {runsList.paged.map((r) => (
                    <tr key={r.id}>
                      <td className="mono">#{r.id}</td>
                      <td>{r.set_name}</td>
                      <td><span className="mono ellipsis" style={{ maxWidth: 150, display: 'inline-block' }}>{r.target}</span></td>
                      <td>
                        <Badge tone={RUN_STATUS_TONES[r.status] || 'default'}>
                          {r.status === 'running' ? '运行中' : r.status === 'pending' ? '等待中' : r.status === 'succeeded' ? '已完成' : r.status === 'failed' ? '失败' : r.status === 'canceled' ? '已取消' : r.status}
                        </Badge>
                      </td>
                      <td className="num">{r.done}/{r.total}</td>
                      <td className="num">{r.passed} / {r.failed} / {r.errored}</td>
                      <td className="num"><strong>{fmtScore(r.score)}</strong></td>
                      <td className="num">{r.cost_ms ? `${(r.cost_ms / 1000).toFixed(1)}s` : '—'}</td>
                      <td className="num">
                        <button className="btn ghost sm" type="button" onClick={() => openReport(r)}>报告</button>
                        {(r.status === 'running' || r.status === 'pending') && (
                          <button className="btn ghost sm" type="button" disabled={busy === `stop-${r.id}`} onClick={() => stopRun(r)}>停止</button>
                        )}
                        <button className="btn ghost sm" type="button" onClick={() => removeRun(r)}><IconTrash /></button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {runs.length > runsList.size && (
            <Pagination page={runsList.page} pages={runsList.pages} total={runsList.total} size={runsList.size} onPage={runsList.setPage} onSize={runsList.setSize} />
          )}
        </Panel>
      )}

      {/* —— 新建 / 编辑用例集 —— */}
      {setForm && (
        <FormDrawer title={setForm.id ? '编辑用例集' : '新建用例集'} desc="被测对象 agent:<agent_key> 走 Agent 执行图；model:<模型名> 直接对话模型" onClose={() => setSetForm(null)}>
          <div className="form">
            <label className="field">
              <span className="field-label">名称 *</span>
              <input className="input" value={setForm.name} onChange={(e) => setSetForm({ ...setForm, name: e.target.value })} placeholder="如：客服 Agent 回归评测" />
            </label>
            <label className="field">
              <span className="field-label">说明</span>
              <input className="input" value={setForm.description} onChange={(e) => setSetForm({ ...setForm, description: e.target.value })} />
            </label>
            <label className="field">
              <span className="field-label">被测对象（启动运行时可覆盖）</span>
              <input className="input" value={setForm.target} onChange={(e) => setSetForm({ ...setForm, target: e.target.value })} placeholder="agent:customer-support 或 model:qwen-max" />
            </label>
            <label className="field">
              <span className="field-label">标签（逗号分隔）</span>
              <input className="input" value={setForm.tags} onChange={(e) => setSetForm({ ...setForm, tags: e.target.value })} placeholder="回归, 客服" />
            </label>
            <label className="field inline">
              <span className="field-label">启用</span>
              <input type="checkbox" checked={setForm.enabled} onChange={(e) => setSetForm({ ...setForm, enabled: e.target.checked })} />
            </label>
            <div className="form-actions">
              <button className="btn primary" type="button" disabled={busy === 'saveSet'} onClick={saveSet}>
                {busy === 'saveSet' ? <Spinner text="保存" /> : '保存'}
              </button>
              <button className="btn ghost" type="button" onClick={() => setSetForm(null)}>取消</button>
            </div>
          </div>
        </FormDrawer>
      )}

      {/* —— 用例管理抽屉 —— */}
      {caseBook && (
        <FormDrawer
          title={`用例管理 · ${caseBook.set.name}`}
          desc={`${caseBook.cases.filter((c) => c.enabled).length} / ${caseBook.cases.length} 条启用`}
          onClose={() => setCaseBook(null)}
          width={860}
        >
          <div className="form">
            <div className="row-actions" style={{ justifyContent: 'flex-end' }}>
              <button
                className="btn primary sm"
                type="button"
                onClick={() => setCaseForm({ ...EMPTY_CASE_FORM, set_id: caseBook.set.id })}
              >
                新建用例
              </button>
            </div>
            {caseForm && (
              <div className="form" style={{ border: '1px solid var(--border)', borderRadius: 10, padding: 14 }}>
                <div className="query-row">
                  <label className="field inline">
                    <span className="field-label">用例名</span>
                    <input className="input" value={caseForm.name} onChange={(e) => setCaseForm({ ...caseForm, name: e.target.value })} placeholder="如：退货政策问答" />
                  </label>
                  <label className="field inline">
                    <span className="field-label">标签（逗号分隔）</span>
                    <input className="input" value={caseForm.tags} onChange={(e) => setCaseForm({ ...caseForm, tags: e.target.value })} />
                  </label>
                  <label className="field inline" style={{ maxWidth: 70 }}>
                    <span className="field-label">启用</span>
                    <input type="checkbox" checked={caseForm.enabled} onChange={(e) => setCaseForm({ ...caseForm, enabled: e.target.checked })} />
                  </label>
                </div>
                <label className="field">
                  <span className="field-label">输入 *（喂给被测对象）</span>
                  <textarea className="input" rows={2} value={caseForm.input} onChange={(e) => setCaseForm({ ...caseForm, input: e.target.value })} />
                </label>
                <div className="query-row">
                  <label className="field inline">
                    <span className="field-label">前置上下文（可选，拼接在输入前）</span>
                    <input className="input" value={caseForm.context} onChange={(e) => setCaseForm({ ...caseForm, context: e.target.value })} />
                  </label>
                  <label className="field inline">
                    <span className="field-label">参考答案（LLM 评审 / 人工比对用）</span>
                    <input className="input" value={caseForm.expect} onChange={(e) => setCaseForm({ ...caseForm, expect: e.target.value })} />
                  </label>
                </div>
                <div>
                  <div className="field-label" style={{ marginBottom: 6 }}>断言规则（空 = 只采集输出不判分）</div>
                  {caseForm.checks.map((c, i) => {
                    const kind = CHECK_KINDS.find((k) => k.v === c.kind) || CHECK_KINDS[0];
                    return (
                      <div key={i} className="query-row" style={{ marginBottom: 6 }}>
                        <select
                          className="input"
                          style={{ width: 150 }}
                          value={c.kind}
                          onChange={(e) => {
                            const checks = [...caseForm.checks];
                            checks[i] = { ...c, kind: e.target.value };
                            setCaseForm({ ...caseForm, checks });
                          }}
                        >
                          {CHECK_KINDS.map((k) => <option key={k.v} value={k.v}>{k.label}</option>)}
                        </select>
                        <input
                          className="input"
                          style={{ flex: 1 }}
                          placeholder={kind.ph}
                          disabled={c.kind === 'json_valid'}
                          value={c.pattern}
                          onChange={(e) => {
                            const checks = [...caseForm.checks];
                            checks[i] = { ...c, pattern: e.target.value };
                            setCaseForm({ ...caseForm, checks });
                          }}
                        />
                        <input
                          className="input"
                          style={{ width: 76 }}
                          type="number"
                          step="0.5"
                          min="0"
                          title="权重（0 按 1 计）"
                          value={c.weight || ''}
                          onChange={(e) => {
                            const checks = [...caseForm.checks];
                            checks[i] = { ...c, weight: e.target.value };
                            setCaseForm({ ...caseForm, checks });
                          }}
                        />
                        <button
                          className="btn ghost sm"
                          type="button"
                          onClick={() => setCaseForm({ ...caseForm, checks: caseForm.checks.filter((_, j) => j !== i) })}
                        >
                          ✕
                        </button>
                      </div>
                    );
                  })}
                  <button
                    className="btn ghost sm"
                    type="button"
                    onClick={() => setCaseForm({ ...caseForm, checks: [...caseForm.checks, { kind: 'contains', pattern: '', weight: 1 }] })}
                  >
                    + 断言
                  </button>
                </div>
                <div className="form-actions">
                  <button className="btn primary sm" type="button" disabled={busy === 'saveCase'} onClick={saveCase}>
                    {busy === 'saveCase' ? <Spinner text="保存" /> : caseForm.id ? '保存用例' : '添加用例'}
                  </button>
                  <button className="btn ghost sm" type="button" onClick={() => setCaseForm(null)}>取消</button>
                </div>
              </div>
            )}
            {caseBook.cases.length === 0 ? (
              <EmptyState title="还没有用例" desc="点「新建用例」，写输入与断言规则" />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>用例</th>
                      <th>输入</th>
                      <th className="num">断言</th>
                      <th>状态</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {caseBook.cases.map((c) => (
                      <tr key={c.id}>
                        <td>{c.name || <span className="cell-sub">未命名</span>}</td>
                        <td><div className="cell-sub ellipsis" style={{ maxWidth: 300 }}>{c.input}</div></td>
                        <td className="num">{c.checks?.length || 0}</td>
                        <td><Badge tone={c.enabled ? 'ok' : 'default'}>{c.enabled ? '启用' : '停用'}</Badge></td>
                        <td className="num">
                          <button
                            className="btn ghost sm"
                            type="button"
                            onClick={() => setCaseForm({
                              id: c.id, set_id: c.set_id, name: c.name || '', input: c.input || '',
                              context: c.context || '', expect: c.expect || '',
                              checks: (c.checks || []).map((x) => ({ kind: x.kind, pattern: x.pattern, weight: x.weight || 1 })),
                              tags: (c.tags || []).join(', '), enabled: !!c.enabled,
                            })}
                          >
                            编辑
                          </button>
                          <button className="btn ghost sm" type="button" onClick={() => removeCase(c)}><IconTrash /></button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </FormDrawer>
      )}

      {/* —— 启动运行 —— */}
      {runForm && (
        <FormDrawer title="启动评测运行" desc="异步执行：立即返回，进度在「运行记录」轮询查看" onClose={() => setRunForm(null)}>
          <div className="form">
            {runForm.prompt_key && (
              <Alert type="info">
                本次运行将标注提示词版本：<strong>{runForm.prompt_key} v{runForm.prompt_version}</strong>，
                完成后得分自动回写提示词工程的版本历史
              </Alert>
            )}
            <label className="field">
              <span className="field-label">被测对象（留空用用例集默认）</span>
              <input className="input" value={runForm.target} onChange={(e) => setRunForm({ ...runForm, target: e.target.value })} placeholder="agent:xxx / model:xxx" />
            </label>
            <label className="field">
              <span className="field-label">LLM 评审模型（留空走默认数据源；用例含 llm_judge 断言时生效）</span>
              <input className="input" value={runForm.judge_model} onChange={(e) => setRunForm({ ...runForm, judge_model: e.target.value })} />
            </label>
            <div className="query-row">
              <label className="field inline">
                <span className="field-label">并发用例数</span>
                <input className="input" type="number" min="1" max="8" value={runForm.concurrency} onChange={(e) => setRunForm({ ...runForm, concurrency: e.target.value })} />
              </label>
              <label className="field inline">
                <span className="field-label">单用例超时（秒）</span>
                <input className="input" type="number" min="10" max="600" value={runForm.per_case_timeout_sec} onChange={(e) => setRunForm({ ...runForm, per_case_timeout_sec: e.target.value })} />
              </label>
            </div>
            <div className="form-actions">
              <button className="btn primary" type="button" disabled={busy === 'startRun'} onClick={startRun}>
                {busy === 'startRun' ? <Spinner text="启动" /> : '启动运行'}
              </button>
              <button className="btn ghost" type="button" onClick={() => setRunForm(null)}>取消</button>
            </div>
          </div>
        </FormDrawer>
      )}

      {/* —— 运行报告抽屉 —— */}
      {report && (
        <FormDrawer title={`评测报告 · 运行 #${report.run.id}`} desc={`${report.run.set_name} → ${report.run.target}`} onClose={() => setReport(null)} width={920}>
          <div className="form">
            <div className="stat-grid">
              <StatCard label="状态" value={report.run.status} hint={report.run.error || `共 ${report.run.total} 例`} tone={RUN_STATUS_TONES[report.run.status] || 'default'} />
              <StatCard label="通过" value={report.run.passed} hint={`失败 ${report.run.failed} · 异常 ${report.run.errored}`} tone="ok" />
              <StatCard label="得分" value={fmtScore(report.run.score)} hint="加权平均 0-100" tone="indigo" />
              <StatCard label="耗时" value={`${(report.run.cost_ms / 1000).toFixed(1)}s`} hint={fmtTime(report.run.finished_at)} />
            </div>
            {report.loading && <Spinner text="加载逐例结果" />}
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>用例</th>
                    <th>结果</th>
                    <th className="num">得分</th>
                    <th className="num">延迟</th>
                    <th className="num">Tokens</th>
                  </tr>
                </thead>
                <tbody>
                  {report.items.map((it) => (
                    <tr key={it.id} style={{ cursor: 'pointer' }} onClick={() => setReportItem(reportItem === it.id ? null : it.id)}>
                      <td>{it.case_name || `#${it.case_id}`}</td>
                      <td>
                        <Badge tone={it.error ? 'error' : it.passed ? 'ok' : 'warn'}>
                          {it.error ? '异常' : it.passed ? '通过' : '未过'}
                        </Badge>
                      </td>
                      <td className="num">{it.error ? '—' : fmtScore(it.score)}</td>
                      <td className="num">{it.latency_ms ? `${(it.latency_ms / 1000).toFixed(1)}s` : '—'}</td>
                      <td className="num">{it.prompt_tokens + it.completion_tokens || '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {reportItem != null && (() => {
              const it = report.items.find((x) => x.id === reportItem);
              if (!it) return null;
              return (
                <div style={{ border: '1px solid var(--border)', borderRadius: 10, padding: 14 }}>
                  <div className="field-label" style={{ marginBottom: 6 }}>逐条断言</div>
                  {(it.checks || []).length === 0 && <div className="cell-sub">该用例没有断言规则（只采集输出）</div>}
                  {(it.checks || []).map((c, i) => (
                    <div key={i} className="cell-sub" style={{ marginBottom: 4 }}>
                      <Badge tone={c.passed ? 'ok' : 'error'}>{c.passed ? '✓' : '✕'}</Badge>{' '}
                      <span className="mono">{c.kind}</span>{c.pattern ? ` · ${c.pattern.slice(0, 60)}` : ''}
                      {c.detail && <span style={{ color: 'var(--warn)' }}> — {c.detail}</span>}
                    </div>
                  ))}
                  {it.error && <Alert type="error">{it.error}</Alert>}
                  <div className="field-label" style={{ margin: '10px 0 6px' }}>被测对象输出</div>
                  <pre className="mono" style={{ whiteSpace: 'pre-wrap', maxHeight: 260, overflow: 'auto', margin: 0 }}>{it.output || '（无输出）'}</pre>
                </div>
              );
            })()}
            {lastRun && !report.loading && report.items.length === 0 && (
              <Alert type="info">还没有逐例结果：运行尚未开始写入或已被清空（重跑前会清空旧结果）。</Alert>
            )}
          </div>
        </FormDrawer>
      )}
    </div>
  );
}
