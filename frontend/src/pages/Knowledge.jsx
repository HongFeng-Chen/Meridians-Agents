import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, v2 } from '../api.js';
import {
  Alert,
  EmptyState,
  Panel,
  Spinner,
  StatCard,
} from '../components/UI.jsx';
import {
  IconAlert,
  IconClock,
  IconDatabase,
  IconGraph,
  IconSearch,
  IconSpark,
  IconTenant,
  IconUpload,
} from '../components/Icons.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import RagAnswer from '../components/knowledge/RagAnswer.jsx';
import GraphAnswer from '../components/knowledge/GraphAnswer.jsx';
import Spaces from '../components/knowledge/Spaces.jsx';

// 实时库：一个库、一份文档、两种索引。
// 目录层面不再区分「RAG 知识库 / GraphRAG 知识图谱」两个入口，
// 区分收敛到内部：资产列表看每篇文档建了哪种索引，检索时再选检索方式。
const TABS = [
  { key: 'assets', label: '知识统计', icon: IconDatabase },
  { key: 'spaces', label: '知识空间', icon: IconTenant },
  { key: 'ask', label: '检索问答', icon: IconSearch },
  { key: 'recall', label: '向量召回历史', icon: IconClock },
  { key: 'graph_recall', label: '图谱召回历史', icon: IconGraph },
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
  const [spaceCount, setSpaceCount] = useState(null); // 知识空间总数（树含全部子目录）
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

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

  const load = useCallback(async () => {
    try {
      const [s, d, sp] = await Promise.all([
        api.stats(),
        api.docs(),
        v2.spaces().catch(() => null), // 空间接口失败不阻塞统计页
      ]);
      setStats(s);
      setDocs(d.docs || []);
      setSpaceCount(sp?.spaces?.length ?? null);
      setError('');
    } catch (err) {
      setError(err.message);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  // 实时库：资产页开着时轻量轮询，别人写入的文档不用手动刷新就会出现
  useEffect(() => {
    if (tab !== 'assets') return undefined;
    const timer = setInterval(() => load(), 15000);
    return () => clearInterval(timer);
  }, [tab, load]);

  const counts = useMemo(() => {
    const base = { both: 0, vector: 0, graph: 0, failed: 0 };
    docs.forEach((d) => {
      base[kindOf(d)] += 1;
    });
    base.all = docs.length;
    return base;
  }, [docs]);

  // 知识统计的派生指标：字符规模、分块密度、作用域分布、最近更新。
  const metrics = useMemo(() => {
    const totalChars = docs.reduce((sum, d) => sum + (d.chars || 0), 0);
    const vectorDocs = docs.filter((d) => d.mode !== 'graph');
    const totalChunks = vectorDocs.reduce((sum, d) => sum + (d.chunks || 0), 0);
    const scopeCounts = { system: 0, tenant: 0, user: 0 };
    docs.forEach((d) => {
      const sc = d.scope || 'user';
      if (scopeCounts[sc] !== undefined) scopeCounts[sc] += 1;
    });
    const latest = docs.reduce(
      (max, d) => ((new Date(d.updated_at || 0).getTime() || 0) > max ? new Date(d.updated_at).getTime() : max),
      0,
    );
    return {
      totalChars,
      avgChunks: vectorDocs.length ? Math.round(totalChunks / vectorDocs.length) : 0,
      bothPct: docs.length ? Math.round((counts.both / docs.length) * 100) : 0,
      scopeCounts,
      latest: latest ? relTime(new Date(latest).toISOString()) : '—',
    };
  }, [docs, counts]);

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
            title="知识统计"
            desc="从文档规模、索引结构、作用域分布三个维度汇总实时库现状"
            actions={
              <button className="btn primary sm" type="button" onClick={() => goTab('spaces')}>
                <IconUpload size={15} /> 接入数据
              </button>
            }
          >
            <div className="kb-facts">
              <div className="kb-fact">
                <span className="kb-fact-num">{metrics.bothPct}%</span>
                <span className="kb-fact-label">双索引覆盖率</span>
                <span className="cell-sub">{counts.both} / {docs.length} 篇两种检索都能命中</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{counts.failed}</span>
                <span className="kb-fact-label">失败文档</span>
                <span className="cell-sub">{counts.failed ? '需重建索引或重新入库' : '全部索引就绪'}</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{metrics.totalChars.toLocaleString('zh-CN')}</span>
                <span className="kb-fact-label">正文字符总量</span>
                <span className="cell-sub">全部文档正文规模</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{metrics.avgChunks}</span>
                <span className="kb-fact-label">平均分块 / 文档</span>
                <span className="cell-sub">向量索引文档的分块密度</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{spaceCount ?? '—'}</span>
                <span className="kb-fact-label">知识空间</span>
                <span className="cell-sub">工作空间 + 全部子目录</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{metrics.scopeCounts.system}</span>
                <span className="kb-fact-label">系统共享文档</span>
                <span className="cell-sub">平台级通用知识，全租户可见</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{metrics.scopeCounts.tenant}</span>
                <span className="kb-fact-label">租户共享文档</span>
                <span className="cell-sub">同租户内可见</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{metrics.scopeCounts.user}</span>
                <span className="kb-fact-label">个人私有文档</span>
                <span className="cell-sub">仅本人可见</span>
              </div>
              <div className="kb-fact">
                <span className="kb-fact-num">{metrics.latest}</span>
                <span className="kb-fact-label">最近更新</span>
                <span className="cell-sub">最新一篇文档的入库时间</span>
              </div>
            </div>
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

      {tab === 'spaces' && (
        <Spaces onChanged={() => load()} />
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
                    : '只检索已建对应索引的文档；在「知识空间」里可以查看每篇文档建了哪种索引'
                }
              />
            </Panel>
          )}
        </>
      )}

      {/* —— 向量召回历史：独立 Tab，从检索问答里拆出来 —— */}
      {tab === 'recall' && (
        <HistoryPanel
          kind="rag_recall"
          title="向量召回历史"
          desc="每次混合检索（Dense + BM25）的命中、耗时与关联任务"
          emptyTitle="暂无召回记录"
          emptyDesc="提一个问题后这里会留下检索记录；Agent 任务内的检索会带上任务号"
        />
      )}

      {/* —— 图谱召回历史：独立 Tab —— */}
      {tab === 'graph_recall' && (
        <HistoryPanel
          kind="graph_recall"
          title="图谱召回历史"
          desc="每次图谱检索的实体命中、关系扩展、耗时与关联任务"
          emptyTitle="暂无召回记录"
          emptyDesc="提一个图谱问题后这里会留下检索记录；Agent 任务内的检索会带上任务号"
        />
      )}

    </div>
  );
}
