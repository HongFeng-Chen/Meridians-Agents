import { useCallback, useEffect, useState } from 'react';
import { memory } from '../api.js';
import { Alert, Badge, EmptyState, FormDrawer, Panel, Spinner, StatCard } from '../components/UI.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import { IconBrain, IconRefresh, IconSearch, IconSpark, IconTrash } from '../components/Icons.jsx';

const KINDS = ['fact', 'preference', 'event', 'insight', 'task', 'profile', 'skill'];
const RULE_TYPES = { fact: '事实记忆', profile: '用户画像', skill: '技能记忆', custom: '自定义' };
const TABS = [
  { key: 'short', label: '短期记忆 · 会话' },
  { key: 'long', label: '长期记忆' },
  { key: 'rules', label: '记忆规则' },
  { key: 'recall', label: '召回与问答' },
  { key: 'logs', label: '检索历史' },
];

export default function Memory() {
  const [tab, setTab] = useState('short');
  const [stats, setStats] = useState(null);
  const [info, setInfo] = useState(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  // 短期
  const [sessionId, setSessionId] = useState('console');
  const [messages, setMessages] = useState([]);
  const [historyTotal, setHistoryTotal] = useState(0);
  const [draft, setDraft] = useState({ role: 'user', content: '' });

  // 长期
  const [mems, setMems] = useState([]);
  const [form, setForm] = useState({ content: '', kind: 'fact', importance: 3, tags: '', ttl_days: 0, graph: true });

  // 召回
  const [recallQuery, setRecallQuery] = useState('');
  const [topK, setTopK] = useState(5);
  const [withGraph, setWithGraph] = useState(true);
  const [recall, setRecall] = useState(null);
  const [answer, setAnswer] = useState(null);

  // 记忆详情
  const [detail, setDetail] = useState(null);
  const [detailLoading, setDetailLoading] = useState(false);

  // 记忆规则
  const [rules, setRules] = useState([]);
  const [ruleEdit, setRuleEdit] = useState(null); // {id, name, type, kind, instruction, enabled} 编辑中
  const [ruleForm, setRuleForm] = useState({ name: '', kind: 'fact', instruction: '', enabled: true });

  // 记忆提取
  const [extractForm, setExtractForm] = useState({ session_id: '', last_n: 50 });
  const [extractRuleIds, setExtractRuleIds] = useState(null); // null = 全部启用规则
  const [extractRes, setExtractRes] = useState(null);

  // 检索历史
  const [logs, setLogs] = useState([]);

  const loadStats = useCallback(async () => {
    try {
      const [st, inf] = await Promise.all([memory.getMemoryStats(), memory.getMemoryInfo()]);
      setStats(st);
      setInfo(inf);
    } catch (err) {
      setError(err.message);
    }
  }, []);

  useEffect(() => {
    loadStats();
  }, [loadStats]);

  const loadHistory = useCallback(async () => {
    if (!sessionId.trim()) return;
    setBusy('history');
    setError('');
    try {
      const data = await memory.listShort({ session_id: sessionId.trim(), limit: 50 });
      setMessages(data.messages || []);
      setHistoryTotal(data.total || 0);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  }, [sessionId]);

  const loadLong = useCallback(async () => {
    setBusy('long');
    setError('');
    try {
      const data = await memory.listLong({ limit: 100, include_expired: true });
      setMems(data.memories || []);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  }, []);

  useEffect(() => {
    loadHistory();
    loadLong();
    // 仅首次进入时加载
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const append = async () => {
    if (!draft.content.trim()) return;
    setBusy('append');
    setError('');
    setNotice('');
    try {
      await memory.appendShort({ session_id: sessionId.trim(), role: draft.role, content: draft.content });
      setDraft({ ...draft, content: '' });
      await loadHistory();
      setNotice('已写入短期记忆（Redis List，写入即续期）');
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const clearSession = async () => {
    setBusy('clear');
    try {
      await memory.clearShort({ session_id: sessionId.trim() });
      setMessages([]);
      setHistoryTotal(0);
      setNotice('会话已清空');
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const remember = async () => {
    if (!form.content.trim()) return;
    setBusy('remember');
    setError('');
    setNotice('');
    try {
      const res = await memory.remember({
        content: form.content,
        kind: form.kind,
        importance: Number(form.importance) || 3,
        tags: form.tags ? form.tags.split(/[,，\s]+/).filter(Boolean) : [],
        ttl_days: Number(form.ttl_days) || 0,
        graph: form.graph,
        source: 'console',
      });
      setNotice(
        `已写入（${res.deduped ? '内容未变，复用已有记录' : '新建'}）：图谱实体 ${res.entities} 个、关系 ${res.relations} 条${
          res.graph_warning ? `；图谱告警：${res.graph_warning}` : ''
        }`,
      );
      setForm({ ...form, content: '', tags: '' });
      await Promise.all([loadLong(), loadStats()]);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const removeMem = async (id) => {
    setBusy(id);
    try {
      await memory.deleteLong({ id });
      await Promise.all([loadLong(), loadStats()]);
      setNotice('已删除该条记忆（含向量）');
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const doRecall = async () => {
    if (!recallQuery.trim()) return;
    setBusy('recall');
    setError('');
    setRecall(null);
    setAnswer(null);
    try {
      setRecall(
        await memory.recall({ query: recallQuery, top_k: Number(topK) || 5, with_graph: withGraph }),
      );
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const doAnswer = async () => {
    if (!recallQuery.trim()) return;
    setBusy('answer');
    setError('');
    setAnswer(null);
    try {
      setAnswer(await memory.answer({ session_id: sessionId || 'console', question: recallQuery }));
      await loadHistory();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  // ---------- 记忆详情 ----------
  const openDetail = async (id) => {
    setDetailLoading(true);
    try {
      setDetail(await memory.getLong({ id }));
    } catch (err) {
      setError(err.message);
    } finally {
      setDetailLoading(false);
    }
  };

  // ---------- 记忆规则 ----------
  const loadRules = useCallback(async () => {
    setBusy('rules');
    try {
      const data = await memory.listMemoryRules();
      setRules(data.rules || []);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  }, []);

  const saveRule = async () => {
    setBusy('ruleSave');
    setError('');
    try {
      if (ruleEdit?.id) {
        await memory.updateMemoryRule({
          id: ruleEdit.id,
          name: ruleEdit.name,
          kind: ruleEdit.kind,
          instruction: ruleEdit.instruction,
          enabled: ruleEdit.enabled,
        });
        setNotice('规则已更新');
      } else {
        await memory.createMemoryRule(ruleForm);
        setNotice('自定义规则已创建');
        setRuleForm({ name: '', kind: 'fact', instruction: '', enabled: true });
      }
      setRuleEdit(null);
      await loadRules();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const toggleRule = async (r) => {
    setBusy(`rule-${r.id}`);
    try {
      await memory.updateMemoryRule({ id: r.id, name: r.name, instruction: r.instruction, enabled: !r.enabled });
      await loadRules();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const removeRule = async (id) => {
    setBusy(`rule-${id}`);
    try {
      await memory.deleteMemoryRule({ id });
      setNotice('规则已删除');
      await loadRules();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  // ---------- 记忆提取 ----------
  const doExtract = async () => {
    const sid = (extractForm.session_id || sessionId || '').trim();
    if (!sid) {
      setError('请填写要提取的会话 ID');
      return;
    }
    setBusy('extract');
    setError('');
    setExtractRes(null);
    try {
      const res = await memory.extractMemories({
        session_id: sid,
        last_n: Number(extractForm.last_n) || 50,
        ...(extractRuleIds ? { rule_ids: extractRuleIds } : {}),
      });
      setExtractRes(res);
      setNotice(`提取完成：新写入 ${res.written} 条，内容去重复用 ${res.deduped} 条`);
      await Promise.all([loadLong(), loadStats()]);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  // ---------- 检索历史 ----------
  const loadLogs = useCallback(async () => {
    setBusy('logs');
    try {
      const data = await memory.listRecallLogs({ limit: 50 });
      setLogs(data.logs || []);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  }, []);

  useEffect(() => {
    if (tab === 'rules' && rules.length === 0) loadRules();
    if (tab === 'logs') loadLogs();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tab]);

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="长期记忆" value={stats?.memories_total ?? 0} hint="MySQL 权威记录" tone="blue" />
        <StatCard label="已过期" value={stats?.memories_expired ?? 0} hint="可由 purge 清理" tone="violet" />
        <StatCard label="向量条目" value={stats?.vector_entities ?? 0} hint="Milvus 记忆集合" tone="indigo" />
        <StatCard
          label="图谱规模"
          value={stats ? `${stats.graph_entities ?? 0} / ${stats.graph_relations ?? 0}` : '—'}
          hint="实体 / 关系"
          tone="teal"
        />
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
        <div style={{ marginLeft: 'auto', display: 'flex', gap: 8, alignItems: 'center' }}>
          <span className="muted mono">
            短期 TTL {info?.short_ttl || '—'} · 召回 top_k {info?.recall_top_k ?? '—'}
          </span>
          <button className="btn ghost sm" type="button" onClick={loadStats}>
            <IconRefresh size={15} /> 刷新
          </button>
        </div>
      </div>

      {tab === 'short' && (
        <div className="grid-2 align-start">
          <Panel
            title="写入会话消息"
            desc="Redis List + TTL，写入即续期，支撑多轮上下文"
            actions={<Badge tone="indigo">{historyTotal} 条</Badge>}
          >
            <div className="form">
              <label className="field">
                <span className="field-label">会话 ID</span>
                <div style={{ display: 'flex', gap: 8 }}>
                  <input className="input mono" value={sessionId} onChange={(e) => setSessionId(e.target.value)} />
                  <button className="btn ghost sm" type="button" onClick={loadHistory}>
                    加载
                  </button>
                  <button className="btn danger sm" type="button" onClick={clearSession} disabled={busy === 'clear'}>
                    清空
                  </button>
                </div>
              </label>
              <label className="field">
                <span className="field-label">角色</span>
                <select
                  className="input"
                  value={draft.role}
                  onChange={(e) => setDraft({ ...draft, role: e.target.value })}
                >
                  {['user', 'assistant', 'system', 'tool'].map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
              </label>
              <label className="field">
                <span className="field-label">内容</span>
                <textarea
                  className="textarea"
                  rows={4}
                  value={draft.content}
                  onChange={(e) => setDraft({ ...draft, content: e.target.value })}
                />
              </label>
              <div className="form-actions">
                <button className="btn primary sm" type="button" onClick={append} disabled={busy === 'append'}>
                  {busy === 'append' ? <Spinner text="写入中" /> : '追加消息'}
                </button>
              </div>
            </div>
          </Panel>

          <Panel title="会话历史" desc={sessionId || '未指定会话'}>
            {messages.length === 0 ? (
              <EmptyState icon={IconBrain} title="暂无消息" desc="Redis 无数据时会自动从会话消息回源；若仍为空，说明该会话 ID 无历史记录，可在左侧写入" />
            ) : (
              <ul className="list-reset">
                {messages.map((m, i) => (
                  <li key={`${m.at}-${i}`}>
                    <Badge tone={m.role === 'user' ? 'indigo' : 'teal'}>{m.role}</Badge>
                    <span className="ellipsis" title={m.content} style={{ maxWidth: 'none' }}>
                      {m.content}
                    </span>
                    <span className="muted mono" style={{ marginLeft: 'auto', flex: 'none' }}>
                      {(m.at || '').slice(11, 19)}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
        </div>
      )}

      {tab === 'long' && (
        <>
          <Panel title="写入长期记忆" desc="MySQL 落库 + 向量化；勾选图谱则额外做实体/关系抽取">
            <div className="form">
              <label className="field">
                <span className="field-label">内容</span>
                <textarea
                  className="textarea"
                  rows={3}
                  placeholder="例如：用户偏好用 Go 写后端，倾向于 Kratos 框架"
                  value={form.content}
                  onChange={(e) => setForm({ ...form, content: e.target.value })}
                />
              </label>
              <div className="query-row">
                <label className="field inline">
                  <span className="field-label">类型</span>
                  <select
                    className="input"
                    value={form.kind}
                    onChange={(e) => setForm({ ...form, kind: e.target.value })}
                  >
                    {KINDS.map((k) => (
                      <option key={k} value={k}>
                        {k}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="field inline">
                  <span className="field-label">重要度 1~5</span>
                  <input
                    className="input"
                    type="number"
                    min="1"
                    max="5"
                    value={form.importance}
                    onChange={(e) => setForm({ ...form, importance: e.target.value })}
                  />
                </label>
                <label className="field inline">
                  <span className="field-label">过期天数（0=永久）</span>
                  <input
                    className="input"
                    type="number"
                    min="0"
                    value={form.ttl_days}
                    onChange={(e) => setForm({ ...form, ttl_days: e.target.value })}
                  />
                </label>
                <label className="field inline">
                  <span className="field-label">标签（逗号分隔）</span>
                  <input
                    className="input"
                    value={form.tags}
                    onChange={(e) => setForm({ ...form, tags: e.target.value })}
                  />
                </label>
              </div>
              <div className="form-actions">
                <label className="chips" style={{ alignItems: 'center' }}>
                  <label className="chip" style={{ cursor: 'pointer' }}>
                    <input
                      type="checkbox"
                      checked={form.graph}
                      onChange={(e) => setForm({ ...form, graph: e.target.checked })}
                    />{' '}
                    同步抽取图谱
                  </label>
                </label>
                <button className="btn primary sm" type="button" onClick={remember} disabled={busy === 'remember'}>
                  {busy === 'remember' ? <Spinner text="写入中" /> : '记住'}
                </button>
                <button className="btn ghost sm" type="button" onClick={loadLong}>
                  <IconRefresh size={15} /> 刷新列表
                </button>
                <button
                  className="btn ghost sm"
                  type="button"
                  onClick={async () => {
                    try {
                      const r = await memory.purgeExpired({ limit: 200 });
                      setNotice(`已清理 ${r.purged} 条过期记忆`);
                      await Promise.all([loadLong(), loadStats()]);
                    } catch (err) {
                      setError(err.message);
                    }
                  }}
                >
                  清理过期
                </button>
              </div>
            </div>
          </Panel>

          <Panel title="记忆列表" desc="按创建时间倒序，过期记录已标红" actions={<Badge tone="indigo">{mems.length}</Badge>}>
            {mems.length === 0 ? (
              <EmptyState icon={IconBrain} title="暂无长期记忆" desc="上方表单可写入第一条" />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>内容</th>
                      <th>类型</th>
                      <th>来源规则</th>
                      <th className="num">召回</th>
                      <th className="num">重要度</th>
                      <th>过期</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {mems.map((m) => (
                      <tr key={m.id}>
                        <td>
                          <button
                            type="button"
                            className="link-btn ellipsis"
                            style={{ maxWidth: 360 }}
                            title={`${m.content}\n（点击查看详情）`}
                            onClick={() => openDetail(m.id)}
                          >
                            {m.content}
                          </button>
                        </td>
                        <td>
                          <Badge>{m.kind}</Badge>
                        </td>
                        <td className="muted">{m.rule_name || <span className="muted">—</span>}</td>
                        <td className="num mono" title={m.last_recalled_at ? `最近召回 ${m.last_recalled_at.slice(0, 19)}` : '从未被召回'}>
                          {m.recall_count || 0}
                        </td>
                        <td className="num">{m.importance}</td>
                        <td className="mono muted">
                          {m.expired ? <Badge tone="warn">已过期</Badge> : (m.expires_at || '永久').slice(0, 19)}
                        </td>
                        <td className="num">
                          <button
                            className="btn danger sm"
                            type="button"
                            onClick={() => removeMem(m.id)}
                            disabled={busy === m.id}
                          >
                            {busy === m.id ? <Spinner text="删除" /> : <IconTrash size={14} />}
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>
        </>
      )}

      {tab === 'recall' && (
        <>
          <Panel title="记忆召回" desc="向量召回长期记忆 + 图谱子图（与 RAG 共享图集合）">
            <div className="query-box">
              <textarea
                className="textarea"
                rows={2}
                placeholder="例如：用户的开发偏好是什么？"
                value={recallQuery}
                onChange={(e) => setRecallQuery(e.target.value)}
              />
              <div className="query-row">
                <label className="field inline">
                  <span className="field-label">top_k</span>
                  <input
                    className="input"
                    type="number"
                    min="1"
                    value={topK}
                    onChange={(e) => setTopK(e.target.value)}
                  />
                </label>
                <label className="chip" style={{ cursor: 'pointer' }}>
                  <input
                    type="checkbox"
                    checked={withGraph}
                    onChange={(e) => setWithGraph(e.target.checked)}
                  />{' '}
                  带图谱
                </label>
                <button className="btn primary" type="button" onClick={doRecall} disabled={busy === 'recall'}>
                  {busy === 'recall' ? <Spinner text="召回中" /> : <><IconSearch size={16} /> 召回</>}
                </button>
                <button className="btn ghost" type="button" onClick={doAnswer} disabled={busy === 'answer'}>
                  {busy === 'answer' ? <Spinner text="生成中" /> : '记忆问答'}
                </button>
              </div>
            </div>
          </Panel>

          {recall && (
            <Panel title="召回结果" desc={`query = ${recall.query}`} actions={<Badge tone="indigo">{recall.memories?.length || 0} 条</Badge>}>
              {recall.memories?.length ? (
                <ul className="cite-list">
                  {recall.memories.map((m, i) => (
                    <li className="cite" key={m.id}>
                      <div className="cite-head">
                        <span className="cite-index">{i + 1}</span>
                        <span className="cite-source">{m.kind}</span>
                        <span className="muted mono">{m.id}</span>
                        <span className="cite-score mono">{Number(m.score).toFixed(4)}</span>
                      </div>
                      <p className="cite-text">{m.content}</p>
                    </li>
                  ))}
                </ul>
              ) : (
                <EmptyState icon={IconSearch} title="没有召回记忆" desc="先写入几条长期记忆，或调整问题" />
              )}
              {recall.graph?.entities?.length > 0 && (
                <>
                  <div className="field-label" style={{ marginTop: 16 }}>
                    图谱上下文
                  </div>
                  <div className="chips">
                    {recall.graph.entities.map((e) => (
                      <span className="chip" key={e.name}>
                        {e.name}
                        <span className="muted"> · {e.type}</span>
                      </span>
                    ))}
                  </div>
                </>
              )}
            </Panel>
          )}

          {answer && (
            <Panel title="基于记忆的回答" actions={<Badge tone="indigo">{answer.session_id}</Badge>}>
              <div className="answer">
                <div className="answer-icon">
                  <IconBrain size={16} />
                </div>
                <div className="answer-text">{answer.answer}</div>
              </div>
            </Panel>
          )}

          <HistoryPanel
            kind="memory_recall"
            title="记忆召回历史"
            desc="每次长期记忆召回的命中数量、图谱规模、耗时与关联任务"
            emptyTitle="暂无召回记录"
            emptyDesc="提一次记忆召回后，这里会留下记录；Agent 任务内的召回会带上任务号"
          />
        </>
      )}

      {tab === 'rules' && (
        <>
          <Panel
            title="从会话提取记忆"
            desc="按启用的记忆规则（事实 / 画像 / 技能 / 自定义）用 LLM 从会话中提取长期记忆，内容级自动去重"
          >
            <div className="form">
              <div className="query-row">
                <label className="field inline" style={{ flex: 2 }}>
                  <span className="field-label">会话 ID（平台会话 ID 或短期记忆 session）</span>
                  <input
                    className="input mono"
                    placeholder="留空则使用「短期记忆」页签的会话 ID"
                    value={extractForm.session_id}
                    onChange={(e) => setExtractForm({ ...extractForm, session_id: e.target.value })}
                  />
                </label>
                <label className="field inline">
                  <span className="field-label">最近 N 条消息</span>
                  <input
                    className="input"
                    type="number"
                    min="1"
                    max="200"
                    value={extractForm.last_n}
                    onChange={(e) => setExtractForm({ ...extractForm, last_n: e.target.value })}
                  />
                </label>
                <div className="field inline" style={{ alignSelf: 'flex-end' }}>
                  <button className="btn primary" type="button" onClick={doExtract} disabled={busy === 'extract'}>
                    {busy === 'extract' ? <Spinner text="提取中" /> : <><IconSpark size={15} /> 开始提取</>}
                  </button>
                </div>
              </div>
              <div className="field">
                <span className="field-label">参与提取的规则（不选 = 全部启用规则）</span>
                <div className="chips">
                  {rules.length === 0 && <span className="muted">暂无规则</span>}
                  {rules
                    .filter((r) => r.enabled)
                    .map((r) => {
                      const on = extractRuleIds === null || extractRuleIds.includes(r.id);
                      return (
                        <button
                          key={r.id}
                          type="button"
                          className={`chip${on ? ' active' : ''}`}
                          style={{ cursor: 'pointer' }}
                          onClick={() => {
                            const cur = extractRuleIds === null ? rules.filter((x) => x.enabled).map((x) => x.id) : [...extractRuleIds];
                            const idx = cur.indexOf(r.id);
                            if (idx >= 0) cur.splice(idx, 1);
                            else cur.push(r.id);
                            setExtractRuleIds(cur.length === rules.filter((x) => x.enabled).length ? null : cur);
                          }}
                        >
                          {r.name}
                        </button>
                      );
                    })}
                </div>
              </div>
            </div>
          </Panel>

          {extractRes && (
            <Panel
              title="提取结果"
              desc={`会话 ${extractRes.session_id} · 新写入 ${extractRes.written} 条 · 去重复用 ${extractRes.deduped} 条`}
              actions={<Badge tone="indigo">{extractRes.results?.length || 0} 条规则</Badge>}
            >
              {(extractRes.results || []).map((r) => (
                <div key={r.rule_id} style={{ marginBottom: 14 }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
                    <Badge tone={r.error ? 'warn' : 'blue'}>{r.rule_name}</Badge>
                    {r.error ? (
                      <span className="mono muted" style={{ color: 'var(--warn, #b45309)' }}>{r.error}</span>
                    ) : (
                      <span className="muted">{r.memories?.length || 0} 条</span>
                    )}
                  </div>
                  {!r.error && (r.memories?.length || 0) === 0 && (
                    <div className="muted" style={{ paddingLeft: 8 }}>该规则未提取出内容</div>
                  )}
                  <ul className="list-reset">
                    {(r.memories || []).map((m, i) => (
                      <li key={i} style={{ alignItems: 'flex-start' }}>
                        <Badge>{m.kind}</Badge>
                        <span style={{ flex: 1 }}>{m.content}</span>
                        <span className="muted mono" style={{ flex: 'none' }}>
                          重要度 {m.importance}
                          {m.deduped ? ' · 复用已有' : ''}
                          {m.error ? ` · 失败：${m.error}` : ''}
                        </span>
                      </li>
                    ))}
                  </ul>
                </div>
              ))}
            </Panel>
          )}

          <Panel
            title="记忆规则"
            desc="内置事实 / 用户画像 / 技能三种规则（可停用、可改指令），支持新建自定义规则"
            actions={
              <button className="btn primary sm" type="button" onClick={() => setRuleEdit({ id: 0, name: '', kind: 'fact', instruction: '', enabled: true })}>
                新建规则
              </button>
            }
          >
            {rules.length === 0 ? (
              <EmptyState icon={IconBrain} title="暂无记忆规则" desc="内置规则在服务启动时自动种入" />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>名称</th>
                      <th>类型</th>
                      <th>提取类型</th>
                      <th>提取指令</th>
                      <th>状态</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {rules.map((r) => (
                      <tr key={r.id}>
                        <td>
                          {r.name} {r.builtin && <Badge tone="violet">内置</Badge>}
                        </td>
                        <td>{RULE_TYPES[r.type] || r.type}</td>
                        <td>
                          <Badge>{r.kind}</Badge>
                        </td>
                        <td className="ellipsis" style={{ maxWidth: 320 }} title={r.instruction}>
                          {r.instruction}
                        </td>
                        <td>
                          <button
                            type="button"
                            className={`chip${r.enabled ? ' active' : ''}`}
                            style={{ cursor: 'pointer' }}
                            onClick={() => toggleRule(r)}
                            disabled={busy === `rule-${r.id}`}
                          >
                            {r.enabled ? '启用' : '停用'}
                          </button>
                        </td>
                        <td className="num" style={{ whiteSpace: 'nowrap' }}>
                          <button className="btn ghost sm" type="button" onClick={() => setRuleEdit({ ...r })}>
                            编辑
                          </button>
                          {!r.builtin && (
                            <button className="btn danger sm" type="button" onClick={() => removeRule(r.id)} disabled={busy === `rule-${r.id}`}>
                              <IconTrash size={14} />
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

          {ruleEdit && (
            <FormDrawer
              title={ruleEdit.id ? `编辑规则 · ${ruleEdit.name}` : '新建自定义记忆规则'}
              desc={ruleEdit.id ? '内置规则可修改名称与提取指令，可停用' : 'type 默认为自定义；指令决定 LLM 从会话中提取什么'}
              onClose={() => setRuleEdit(null)}
            >
              <div className="form">
                <label className="field">
                  <span className="field-label">规则名称</span>
                  <input
                    className="input"
                    value={ruleEdit.name}
                    onChange={(e) => setRuleEdit({ ...ruleEdit, name: e.target.value })}
                  />
                </label>
                <div className="query-row">
                  <label className="field inline">
                    <span className="field-label">提取类型（写入记忆的 kind）</span>
                    <select
                      className="input"
                      value={ruleEdit.kind}
                      disabled={!!ruleEdit.builtin}
                      onChange={(e) => setRuleEdit({ ...ruleEdit, kind: e.target.value })}
                    >
                      {KINDS.map((k) => (
                        <option key={k} value={k}>
                          {k}
                        </option>
                      ))}
                    </select>
                  </label>
                  <label className="chip" style={{ cursor: 'pointer', alignSelf: 'flex-end', marginBottom: 10 }}>
                    <input
                      type="checkbox"
                      checked={ruleEdit.enabled}
                      onChange={(e) => setRuleEdit({ ...ruleEdit, enabled: e.target.checked })}
                    />{' '}
                    启用
                  </label>
                </div>
                <label className="field">
                  <span className="field-label">提取指令（告诉 LLM 从会话中提取什么）</span>
                  <textarea
                    className="textarea"
                    rows={6}
                    value={ruleEdit.instruction}
                    onChange={(e) => setRuleEdit({ ...ruleEdit, instruction: e.target.value })}
                  />
                </label>
                <div className="form-actions">
                  <button className="btn primary sm" type="button" onClick={saveRule} disabled={busy === 'ruleSave'}>
                    {busy === 'ruleSave' ? <Spinner text="保存中" /> : '保存'}
                  </button>
                  <button className="btn ghost sm" type="button" onClick={() => setRuleEdit(null)}>
                    取消
                  </button>
                </div>
              </div>
            </FormDrawer>
          )}
        </>
      )}

      {tab === 'logs' && (
        <Panel
          title="记忆检索历史"
          desc="每次记忆召回（会话聊天 / 控制台 / 记忆问答）的流水，命中会回填对应记忆的召回次数"
          actions={<button className="btn ghost sm" type="button" onClick={loadLogs}><IconRefresh size={15} /> 刷新</button>}
        >
          {logs.length === 0 ? (
            <EmptyState icon={IconSearch} title="暂无检索记录" desc="做一次记忆召回（或发起一轮会话）后这里会出现记录" />
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>查询</th>
                    <th>来源</th>
                    <th className="num">命中</th>
                    <th className="num">top_k</th>
                    <th className="num">耗时</th>
                    <th>命中记忆</th>
                  </tr>
                </thead>
                <tbody>
                  {logs.map((l) => (
                    <tr key={l.id}>
                      <td className="mono muted">{(l.created_at || '').slice(0, 19)}</td>
                      <td className="ellipsis" style={{ maxWidth: 300 }} title={l.query}>
                        {l.query}
                      </td>
                      <td>
                        <Badge tone={l.source === 'chat' ? 'indigo' : l.source === 'answer' ? 'teal' : 'default'}>{l.source}</Badge>
                      </td>
                      <td className="num">{l.hits}</td>
                      <td className="num">{l.top_k}</td>
                      <td className="num mono">{l.latency_ms}ms</td>
                      <td className="mono muted ellipsis" style={{ maxWidth: 220 }} title={(l.memory_ids || []).join(', ')}>
                        {(l.memory_ids || []).join(', ') || '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      )}

      {detail && (
        <FormDrawer
          title="记忆详情"
          desc={detail.id}
          onClose={() => setDetail(null)}
          width={640}
        >
          <div className="stack" style={{ gap: 12 }}>
            <div className="chips">
              <Badge tone="blue">{detail.kind}</Badge>
              <Badge>重要度 {detail.importance}</Badge>
              {detail.expired ? <Badge tone="warn">已过期</Badge> : null}
              {detail.rule_name ? <Badge tone="violet">规则提取 · {detail.rule_name}</Badge> : null}
            </div>
            <div className="panel" style={{ padding: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>
              {detail.content}
            </div>
            <div className="table-wrap">
              <table className="table">
                <tbody>
                  <tr><td className="muted" style={{ width: 110 }}>用户</td><td className="mono">{detail.user_id}</td></tr>
                  <tr><td className="muted">标签</td><td>{(detail.tags || []).join('、') || '—'}</td></tr>
                  <tr><td className="muted">来源</td><td>{detail.source || '—'}</td></tr>
                  <tr><td className="muted">召回次数</td><td>{detail.recall_count || 0}{detail.last_recalled_at ? `（最近 ${(detail.last_recalled_at || '').slice(0, 19)}）` : '（从未被召回）'}</td></tr>
                  <tr><td className="muted">图谱</td><td>实体 {detail.entities} · 关系 {detail.relations}</td></tr>
                  <tr><td className="muted">过期时间</td><td className="mono">{detail.expires_at ? detail.expires_at.slice(0, 19) : '永久'}</td></tr>
                  <tr><td className="muted">创建时间</td><td className="mono">{(detail.created_at || '').slice(0, 19)}</td></tr>
                  <tr><td className="muted">更新时间</td><td className="mono">{(detail.updated_at || '').slice(0, 19)}</td></tr>
                </tbody>
              </table>
            </div>
          </div>
        </FormDrawer>
      )}
    </div>
  );
}
