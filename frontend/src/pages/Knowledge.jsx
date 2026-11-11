import { useCallback, useEffect, useMemo, useState } from 'react';
import { api } from '../api.js';
import {
  Alert,
  Badge,
  EmptyState,
  FormDrawer,
  Pagination,
  Panel,
  Spinner,
  StatCard,
  usePagedList,
} from '../components/UI.jsx';
import {
  IconAlert,
  IconDatabase,
  IconDoc,
  IconGraph,
  IconRefresh,
  IconSearch,
  IconSpark,
  IconTrash,
  IconUpload,
} from '../components/Icons.jsx';
import { SCOPES, ScopeBadge, scopeLabel } from '../components/Scope.jsx';
import IngestForm from '../components/IngestForm.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import RagAnswer from '../components/knowledge/RagAnswer.jsx';
import GraphAnswer from '../components/knowledge/GraphAnswer.jsx';

// 实时库：一个库、一份文档、两种索引。
// 目录层面不再区分「RAG 知识库 / GraphRAG 知识图谱」两个入口，
// 区分收敛到内部：资产列表看每篇文档建了哪种索引，检索时再选检索方式。
const TABS = [
  { key: 'assets', label: '知识资产', icon: IconDatabase },
  { key: 'ingest', label: '数据接入', icon: IconUpload },
  { key: 'ask', label: '检索问答', icon: IconSearch },
];

const ASK_MODES = [
  { key: 'rag', label: '向量检索', hint: 'Dense + BM25 双路召回，答案带原文片段引用' },
  { key: 'graph', label: '图谱检索', hint: '实体召回 + 多跳关系扩展，答案带子图与证据' },
  { key: 'compare', label: '双路对比', hint: '同一问题同时走两种检索，直接对比答案与依据' },
];

const SUGGESTIONS = {
  rag: [
    'Milvus 3.0 在检索性能上有哪些增强？',
    'Zilliz 和 Milvus 是什么关系？',
    'Go SDK 应该如何初始化客户端？',
  ],
  graph: [
    'Milvus、Zilliz 与 Zilliz Cloud 之间是什么关系？',
    'Go SDK 的演进经历了哪些变化？',
    'Milvus 3.0 在稀疏检索上引入了什么新算法？',
  ],
  compare: ['Milvus 3.0 相比 2.6 有哪些变化？', 'Zilliz Cloud 与 Milvus 的关系是什么？'],
};

// kindOf 把一篇文档归到「双索引 / 仅向量 / 仅图谱 / 失败」四类里的一种。
// 兼容历史数据：mode 为空的旧行按「有分块就是向量索引」处理。
function kindOf(d) {
  if (d.status === 'failed') return 'failed';
  const hasVector = d.mode !== 'graph';
  const hasGraph = d.mode === 'graph' || d.mode === 'both';
  if (hasVector && hasGraph) return 'both';
  if (hasVector) return 'vector';
  return 'graph';
}

const KIND_LABEL = {
  both: '双索引',
  vector: '仅向量',
  graph: '仅图谱',
  failed: '失败',
};

function relTime(v) {
  if (!v) return '—';
  const t = new Date(v).getTime();
  if (Number.isNaN(t)) return v;
  const diff = Date.now() - t;
  if (diff < 60_000) return '刚刚';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;
  if (diff < 30 * 86_400_000) return `${Math.floor(diff / 86_400_000)} 天前`;
  return new Date(t).toLocaleDateString('zh-CN');
}

function fmtTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? v : d.toLocaleString('zh-CN', { hour12: false });
}

function readHashState() {
  const qs = window.location.hash.split('?')[1] || '';
  const p = new URLSearchParams(qs);
  const tab = TABS.some((t) => t.key === p.get('tab')) ? p.get('tab') : 'assets';
  const mode = ASK_MODES.some((m) => m.key === p.get('mode')) ? p.get('mode') : 'rag';
  return { tab, mode };
}

export default function Knowledge() {
  const [tab, setTab] = useState(() => readHashState().tab);
  const [askMode, setAskMode] = useState(() => readHashState().mode);

  const [stats, setStats] = useState(null);
  const [docs, setDocs] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [lastSync, setLastSync] = useState(null);

  const [keyword, setKeyword] = useState('');
  const [kind, setKind] = useState('all');
  const [scopeFilter, setScopeFilter] = useState('');

  const [detail, setDetail] = useState(null);
  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const [question, setQuestion] = useState('');
  const [asking, setAsking] = useState(false);
  const [askError, setAskError] = useState('');
  const [ragResult, setRagResult] = useState(null);
  const [graphResult, setGraphResult] = useState(null);

  const goTab = (key) => {
    setTab(key);
    window.location.hash = `#/knowledge?tab=${key}${key === 'ask' ? `&mode=${askMode}` : ''}`;
  };

  // 侧边栏 / 仪表盘带参数跳进来时，同步内部 Tab（App 只按 ? 前的 key 路由）
  useEffect(() => {
    const sync = () => {
      const s = readHashState();
      setTab(s.tab);
      setAskMode(s.mode);
    };
    window.addEventListener('hashchange', sync);
    return () => window.removeEventListener('hashchange', sync);
  }, []);

  const load = useCallback(async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      const [s, d] = await Promise.all([api.stats(), api.docs()]);
      setStats(s);
      setDocs(d.docs || []);
      setLastSync(new Date());
      setError('');
    } catch (err) {
      setError(err.message);
    } finally {
      if (!silent) setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 实时库：资产页开着时轻量轮询，别人写入的文档不用手动刷新就会出现
  useEffect(() => {
    if (!autoRefresh || tab !== 'assets') return undefined;
    const timer = setInterval(() => load(true), 15000);
    return () => clearInterval(timer);
  }, [autoRefresh, tab, load]);

  const counts = useMemo(() => {
    const base = { both: 0, vector: 0, graph: 0, failed: 0 };
    docs.forEach((d) => {
      base[kindOf(d)] += 1;
    });
    base.all = docs.length;
    return base;
  }, [docs]);

  const filtered = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    return docs
      .filter((d) => {
        if (kind !== 'all' && kindOf(d) !== kind) return false;
        if (scopeFilter && (d.scope || 'user') !== scopeFilter) return false;
        if (kw && !`${d.doc_id} ${d.source || ''}`.toLowerCase().includes(kw)) return false;
        return true;
      })
      .sort((a, b) => new Date(b.updated_at || 0) - new Date(a.updated_at || 0));
  }, [docs, keyword, kind, scopeFilter]);

  const pg = usePagedList(filtered, { size: 10, resetKey: `${keyword}|${kind}|${scopeFilter}` });

  const removeDoc = async () => {
    if (!detail) return;
    setDeleting(true);
    try {
      const r = await api.deleteDoc(detail.doc_id);
      setNotice(
        `已删除 ${detail.doc_id}：分块 ${r.deleted_chunks ?? 0} 个，图谱实体 ${r.deleted_entities ?? 0} 个、关系 ${r.deleted_relations ?? 0} 条`,
      );
      setDetail(null);
      setConfirming(false);
      await load(true);
    } catch (err) {
      setError(err.message);
    } finally {
      setDeleting(false);
    }
  };

  const ask = async (text) => {
    const q = (text ?? question).trim();
    if (!q || asking) return;
    setQuestion(q);
    setAsking(true);
    setAskError('');
    setRagResult(null);
    setGraphResult(null);
    try {
      if (askMode === 'compare') {
        const [a, b] = await Promise.allSettled([
          api.ragQuery({ question: q }),
          api.graphQuery({ question: q }),
        ]);
        if (a.status === 'fulfilled') setRagResult(a.value);
        if (b.status === 'fulfilled') setGraphResult(b.value);
        const errs = [a, b].filter((r) => r.status === 'rejected').map((r) => r.reason?.message);
        if (errs.length) setAskError(`部分检索失败：${errs.join('；')}`);
      } else if (askMode === 'graph') {
        setGraphResult(await api.graphQuery({ question: q }));
      } else {
        setRagResult(await api.ragQuery({ question: q }));
      }
    } catch (err) {
      setAskError(err.message);
    } finally {
      setAsking(false);
    }
  };

  const switchAskMode = (key) => {
    setAskMode(key);
    setRagResult(null);
    setGraphResult(null);
    setAskError('');
    window.location.hash = `#/knowledge?tab=ask&mode=${key}`;
  };

  const curTab = TABS.find((t) => t.key === tab) || TABS[0];

  return (
    <div className="stack">
      <div className="kb-bar">
        <div className="kb-tabs" role="tablist" aria-label="实时库分区">
          {TABS.map((t) => {
            const Icon = t.icon;
            return (
              <button
                key={t.key}
                type="button"
                role="tab"
                aria-selected={tab === t.key}
                className={`kb-tab${tab === t.key ? ' active' : ''}`}
                onClick={() => goTab(t.key)}
              >
                <Icon size={15} />
                {t.label}
              </button>
            );
          })}
        </div>
        <div className="kb-live">
          <span className={`live-dot${autoRefresh && tab === 'assets' ? ' on' : ''}`} />
          <span className="muted mono">{lastSync ? lastSync.toLocaleTimeString('zh-CN', { hour12: false }) : '—'}</span>
          <label className="kb-switch">
            <input
              type="checkbox"
              checked={autoRefresh}
              onChange={(e) => setAutoRefresh(e.target.checked)}
            />
            自动刷新
          </label>
          <button className="btn ghost sm" type="button" onClick={() => load()} disabled={loading}>
            {loading ? <Spinner text="加载中" /> : <><IconRefresh size={15} /> 刷新</>}
          </button>
        </div>
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      {tab === 'assets' && (
        <>
          <div className="stat-grid">
            <StatCard
              label="文档"
              value={docs.length}
              hint={`双索引 ${counts.both} · 仅向量 ${counts.vector} · 仅图谱 ${counts.graph}`}
              tone="blue"
            />
            <StatCard label="向量分块" value={stats?.chunks ?? 0} hint="Milvus 主集合" tone="indigo" />
            <StatCard label="图谱实体" value={stats?.entities ?? 0} hint="实体集合" tone="teal" />
            <StatCard label="图谱关系" value={stats?.relations ?? 0} hint={`扩展跳数 ${stats?.graph_hops ?? '—'}`} tone="violet" />
          </div>

          <Panel
            title="知识资产"
            desc="每篇文档建了哪种索引一目了然：点行看详情，双索引的文档两种检索都能命中"
            actions={
              <>
                <input
                  className="input sm"
                  style={{ width: 180 }}
                  placeholder="搜索文档 / 来源"
                  value={keyword}
                  onChange={(e) => setKeyword(e.target.value)}
                />
                <select
                  className="input sm"
                  style={{ width: 120 }}
                  value={scopeFilter}
                  onChange={(e) => setScopeFilter(e.target.value)}
                >
                  <option value="">全部作用域</option>
                  {SCOPES.map((s) => (
                    <option key={s.value} value={s.value}>
                      {s.label}
                    </option>
                  ))}
                </select>
                <button className="btn primary sm" type="button" onClick={() => goTab('ingest')}>
                  <IconUpload size={15} /> 接入数据
                </button>
              </>
            }
          >
            <div className="kb-filter">
              {[
                { key: 'all', label: `全部 ${counts.all}` },
                { key: 'both', label: `双索引 ${counts.both}` },
                { key: 'vector', label: `仅向量 ${counts.vector}` },
                { key: 'graph', label: `仅图谱 ${counts.graph}` },
                { key: 'failed', label: `失败 ${counts.failed}` },
              ].map((f) => (
                <button
                  key={f.key}
                  type="button"
                  className={`chip${kind === f.key ? ' active' : ''}`}
                  onClick={() => setKind(f.key)}
                  disabled={f.key !== 'all' && counts[f.key] === 0 && kind !== f.key}
                >
                  {f.label}
                </button>
              ))}
            </div>

            {loading && docs.length === 0 ? (
              <Spinner text="加载知识资产…" />
            ) : filtered.length === 0 ? (
              <EmptyState
                icon={IconDoc}
                title={docs.length === 0 ? '实时库还是空的' : '没有符合条件的文档'}
                desc={
                  docs.length === 0
                    ? '接入第一篇文档，选择双索引即可同时支持向量检索与图谱问答'
                    : '换个关键字或筛选条件试试'
                }
              />
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th>文档</th>
                      <th>索引</th>
                      <th>作用域</th>
                      <th>更新</th>
                      <th className="num">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pg.paged.map((d) => {
                      const hasVector = d.mode !== 'graph';
                      const hasGraph = d.mode === 'graph' || d.mode === 'both';
                      return (
                        <tr key={d.doc_id} className="row-click" onClick={() => { setDetail(d); setConfirming(false); }}>
                          <td>
                            <div className="doc-cell">
                              <span className="doc-name mono ellipsis" title={d.doc_id}>{d.doc_id}</span>
                              <span className="cell-sub ellipsis" title={d.source}>{d.source || '—'}</span>
                            </div>
                          </td>
                          <td>
                            <div className="idx-cell">
                              <span className={`idx-chip${hasVector ? ' on' : ''}`}>
                                <IconSearch size={12} />
                                {hasVector ? `${d.chunks || 0} 块` : '未建'}
                              </span>
                              <span className={`idx-chip graph${hasGraph ? ' on' : ''}`}>
                                <IconGraph size={12} />
                                {hasGraph ? `${d.entities || 0} 实体 / ${d.relations || 0} 关系` : '未建'}
                              </span>
                              {d.status === 'failed' && <Badge tone="error">失败</Badge>}
                            </div>
                          </td>
                          <td><ScopeBadge scope={d.scope} /></td>
                          <td>
                            <span title={fmtTime(d.updated_at)}>{relTime(d.updated_at)}</span>
                          </td>
                          <td className="num">
                            <button
                              className="btn ghost sm"
                              type="button"
                              onClick={(e) => {
                                e.stopPropagation();
                                setDetail(d);
                                setConfirming(false);
                              }}
                            >
                              详情
                            </button>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
            <Pagination
              page={pg.page}
              pages={pg.pages}
              total={pg.total}
              size={pg.size}
              onPage={pg.setPage}
              onSize={pg.setSize}
            />
          </Panel>

          <Panel title="实时状态" desc="实时库的运行口径">
            <div className="kb-facts">
              <div className="kb-fact">
                <span className="kb-fact-num">{counts.both}</span>
                <span className="kb-fact-label">双索引文档</span>
                <span className="cell-sub">两种检索都能命中</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{stats?.docs ?? docs.length}</span>
                <span className="kb-fact-label">元数据行</span>
                <span className="cell-sub">MySQL rag_documents 为权威源</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{stats?.embed_model || '—'}</span>
                <span className="kb-fact-label">Embedding</span>
                <span className="cell-sub">{stats?.embed_dim ? `${stats.embed_dim} 维` : '向量维度未知'}</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{stats?.llm_model || '—'}</span>
                <span className="kb-fact-label">图谱抽取模型</span>
                <span className="cell-sub">双索引入库时按分块调用</span>
              </div>
            </div>
          </Panel>
        </>
      )}

      {tab === 'ingest' && (
        <IngestForm
          initialMode="both"
          onDone={() => load(true)}
          onOpenTab={(t) => goTab(t)}
        />
      )}

      {tab === 'ask' && (
        <>
          <Panel
            title="检索问答"
            desc={ASK_MODES.find((m) => m.key === askMode)?.hint}
            actions={
              <div className="seg">
                {ASK_MODES.map((m) => (
                  <button
                    key={m.key}
                    type="button"
                    className={`seg-item${askMode === m.key ? ' active' : ''}`}
                    onClick={() => switchAskMode(m.key)}
                  >
                    {m.label}
                  </button>
                ))}
              </div>
            }
          >
            <div className="query-box">
              <textarea
                className="textarea"
                rows={3}
                placeholder={
                  askMode === 'graph'
                    ? '例如：Milvus 和 Zilliz Cloud 通过什么产生联系？'
                    : askMode === 'compare'
                      ? '例如：Milvus 3.0 相比 2.6 有哪些变化？（两种检索会各答一次）'
                      : '例如：Milvus 3.0 的 BM25 索引做了哪些压缩优化？'
                }
                value={question}
                onChange={(e) => setQuestion(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) ask();
                }}
              />
              <div className="query-row end">
                <span className="muted">⌘/Ctrl + Enter 发送 · 检索范围由服务端按身份收敛</span>
                <button className="btn primary" type="button" onClick={() => ask()} disabled={asking}>
                  {asking ? <Spinner text="检索中…" /> : <><IconSearch size={16} /> 提问</>}
                </button>
              </div>
              <div className="chips">
                {(SUGGESTIONS[askMode] || []).map((s) => (
                  <button key={s} className="chip" type="button" onClick={() => ask(s)} disabled={asking}>
                    {s}
                  </button>
                ))}
              </div>
            </div>
          </Panel>

          {askError && <Alert type="error">{askError}</Alert>}
          {asking && (
            <Panel>
              <Spinner
                text={
                  askMode === 'compare'
                    ? '正在同时执行向量检索与图谱检索…'
                    : askMode === 'graph'
                      ? '正在做实体召回与关系扩展…'
                      : '正在做混合检索并生成答案…'
                }
              />
            </Panel>
          )}

          {askMode === 'compare' && (ragResult || graphResult) && (
            <div className="compare-grid">
              <div className="stack">
                {ragResult ? (
                  <RagAnswer data={ragResult} title="向量检索 · RAG" desc="按片段召回，答案贴近原文" />
                ) : (
                  <Panel title="向量检索 · RAG">
                    <EmptyState icon={IconAlert} title="本次未返回结果" desc="可能没有可用的向量索引，或该问题检索失败" />
                  </Panel>
                )}
              </div>
              <div className="stack">
                {graphResult ? (
                  <GraphAnswer data={graphResult} title="图谱检索 · GraphRAG" desc="沿关系边走，答案呈现关联链路" compact />
                ) : (
                  <Panel title="图谱检索 · GraphRAG">
                    <EmptyState icon={IconAlert} title="本次未返回结果" desc="可能没有可用的图谱索引，或该问题检索失败" />
                  </Panel>
                )}
              </div>
            </div>
          )}

          {askMode === 'rag' && ragResult && <RagAnswer data={ragResult} />}
          {askMode === 'graph' && graphResult && <GraphAnswer data={graphResult} />}

          {!asking && !ragResult && !graphResult && !askError && (
            <Panel>
              <EmptyState
                icon={IconSpark}
                title="等待提问"
                desc={
                  askMode === 'compare'
                    ? '同一个问题会分别走向量检索与图谱检索，答案并列展示，方便判断哪种召回更贴合你的场景'
                    : '只检索已建对应索引的文档；在「知识资产」里可以确认每篇文档建了哪种索引'
                }
              />
            </Panel>
          )}

          {askMode === 'compare' ? (
            <>
              <HistoryPanel
                kind="rag_recall"
                title="向量召回历史"
                desc="每次混合检索（Dense + BM25）的命中、耗时与关联任务"
                emptyTitle="暂无召回记录"
                emptyDesc="提一个问题后这里会留下检索记录"
              />
              <HistoryPanel
                kind="graph_recall"
                title="图谱召回历史"
                desc="每次图谱检索的实体命中、关系扩展、耗时与关联任务"
                emptyTitle="暂无召回记录"
                emptyDesc="提一个图谱问题后这里会留下检索记录"
              />
            </>
          ) : (
            <HistoryPanel
              kind={askMode === 'graph' ? 'graph_recall' : 'rag_recall'}
              title={askMode === 'graph' ? '图谱召回历史' : '向量召回历史'}
              desc={
                askMode === 'graph'
                  ? '每次图谱检索的实体命中、关系扩展、耗时与关联任务'
                  : '每次混合检索（Dense + BM25）的命中、耗时与关联任务'
              }
              emptyTitle="暂无召回记录"
              emptyDesc="提一个问题后这里会留下检索记录；Agent 任务内的检索会带上任务号"
            />
          )}
        </>
      )}

      {detail && (
        <FormDrawer
          title={detail.doc_id}
          desc={`${curTab.label} · ${KIND_LABEL[kindOf(detail)]}`}
          eyebrow={detail.status === 'failed' ? '索引失败' : undefined}
          onClose={() => { setDetail(null); setConfirming(false); }}
          width="min(620px, 94vw)"
        >
          <div className="stack">
            <div className="detail-meta">
              <ScopeBadge scope={detail.scope} />
              {detail.mode === 'both' && <Badge tone="indigo">双索引</Badge>}
              {detail.mode === 'graph' && <Badge tone="violet">仅图谱</Badge>}
              {detail.mode !== 'graph' && detail.mode !== 'both' && <Badge tone="teal">仅向量</Badge>}
              {detail.status === 'failed' && <Badge tone="error">上次索引失败</Badge>}
            </div>

            <div className="kv">
              <div><span>来源</span><b>{detail.source || '—'}</b></div>
              <div><span>字符数</span><b className="mono">{detail.chars ?? '—'}</b></div>
              <div><span>向量分块</span><b className="mono">{detail.mode === 'graph' ? '未建' : detail.chunks ?? 0}</b></div>
              <div><span>图谱实体 / 关系</span><b className="mono">{detail.mode === 'graph' || detail.mode === 'both' ? `${detail.entities || 0} / ${detail.relations || 0}` : '未建'}</b></div>
              <div><span>作用域</span><b>{scopeLabel(detail.scope)}</b></div>
              <div><span>归属</span><b className="mono">{detail.user_id || '—'} @ {detail.tenant_id || '—'}</b></div>
              <div><span>创建时间</span><b className="mono">{fmtTime(detail.created_at)}</b></div>
              <div><span>更新时间</span><b className="mono">{fmtTime(detail.updated_at)}</b></div>
              <div><span>内容指纹</span><b className="mono ellipsis" title={detail.checksum}>{detail.checksum || '—'}</b></div>
            </div>

            <div className="form-note">
              <span className="form-note-icon"><IconAlert size={15} /></span>
              <div>
                <strong>想换索引方式？</strong>
                重新接入同一份文档并选择目标索引即可，另一侧索引会被清理；要两边都保留请选「双索引」。
              </div>
            </div>

            <div className="drawer-danger">
              {confirming ? (
                <>
                  <span className="muted">
                    将删除该文档的分块与专属图谱节点，不可撤销
                  </span>
                  <div className="drawer-danger-actions">
                    <button className="btn ghost sm" type="button" onClick={() => setConfirming(false)} disabled={deleting}>
                      取消
                    </button>
                    <button className="btn danger sm" type="button" onClick={removeDoc} disabled={deleting}>
                      {deleting ? <Spinner text="删除中" /> : '确认删除'}
                    </button>
                  </div>
                </>
              ) : (
                <>
                  <span className="muted">删除后检索立即失效</span>
                  <button className="btn danger sm" type="button" onClick={() => setConfirming(true)}>
                    <IconTrash size={14} /> 删除文档
                  </button>
                </>
              )}
            </div>
          </div>
        </FormDrawer>
      )}
    </div>
  );
}
