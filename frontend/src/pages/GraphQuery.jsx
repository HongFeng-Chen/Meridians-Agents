import { useState } from 'react';
import { api } from '../api.js';
import { Alert, Badge, EmptyState, Panel, Spinner } from '../components/UI.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import { IconGraph, IconSearch, IconSpark } from '../components/Icons.jsx';
import Subgraph from '../components/Subgraph.jsx';

const SUGGESTIONS = [
  'Milvus、Zilliz 与 Zilliz Cloud 之间是什么关系？',
  'Go SDK 的演进经历了哪些变化？',
  'Milvus 3.0 在稀疏检索上引入了什么新算法？',
];

export default function GraphQuery() {
  const [question, setQuestion] = useState('');
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState(null);
  const [error, setError] = useState('');

  const ask = async (text) => {
    const q = (text ?? question).trim();
    if (!q || loading) return;
    setQuestion(q);
    setLoading(true);
    setError('');
    setResult(null);
    try {
      setResult(await api.graphQuery({ question: q }));
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="stack">
      <Panel title="输入问题" desc="实体召回后沿关系边多跳扩展，基于子图推理作答">
        <div className="query-box">
          <textarea
            className="textarea"
            rows={3}
            placeholder="例如：Milvus 和 Zilliz Cloud 通过什么产生联系？"
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) ask();
            }}
          />
          <div className="query-row end">
            <button className="btn primary" type="button" onClick={() => ask()} disabled={loading}>
              {loading ? <Spinner text="推理中…" /> : <><IconSearch size={16} /> 图谱问答</>}
            </button>
          </div>
          <div className="chips">
            {SUGGESTIONS.map((s) => (
              <button key={s} className="chip" type="button" onClick={() => ask(s)}>
                {s}
              </button>
            ))}
          </div>
        </div>
      </Panel>

      {error && <Alert type="error">{error}</Alert>}

      {result && (
        <>
          <Panel
            title="回答"
            actions={
              <div className="badges-row inline">
                <Badge tone="teal">实体 {result.entities?.length || 0}</Badge>
                <Badge tone="violet">关系 {result.relations?.length || 0}</Badge>
                <Badge tone="default">证据 {result.evidence?.length || 0}</Badge>
              </div>
            }
          >
            <div className="answer">
              <div className="answer-icon">
                <IconSpark size={16} />
              </div>
              <div className="answer-text">{result.answer}</div>
            </div>
          </Panel>

          <Panel title="检索子图" desc="实体为节点、关系为有向边，悬停可查看详情">
            <Subgraph entities={result.entities} relations={result.relations} />
          </Panel>

          <div className="grid-2 align-start">
            <Panel title="命中实体" desc="按出现频次排序">
              {result.entities?.length ? (
                <ul className="entity-list">
                  {result.entities.map((e) => (
                    <li key={e.name}>
                      <div className="entity-head">
                        <b>{e.name}</b>
                        <Badge tone="default">{e.type || '实体'}</Badge>
                        <span className="muted">×{e.freq ?? 1}</span>
                      </div>
                      <p className="entity-desc">{e.description || '—'}</p>
                    </li>
                  ))}
                </ul>
              ) : (
                <EmptyState icon={IconGraph} title="未命中实体" />
              )}
            </Panel>

            <Panel title="命中关系" desc="权重降序，最多 40 条">
              {result.relations?.length ? (
                <ul className="relation-list">
                  {result.relations.map((r, i) => (
                    <li key={`${r.source}-${r.target}-${i}`}>
                      <div className="relation-line">
                        <span className="node">{r.source}</span>
                        <span className="predicate">{r.relation}</span>
                        <span className="node">{r.target}</span>
                      </div>
                      {r.description && <p className="relation-desc">{r.description}</p>}
                    </li>
                  ))}
                </ul>
              ) : (
                <EmptyState icon={IconGraph} title="未命中关系" />
              )}
            </Panel>
          </div>

          {result.evidence?.length > 0 && (
            <Panel title="原文证据" desc="由图谱节点的 chunk 引用回溯得到">
              <ol className="evidence-list">
                {result.evidence.map((t, i) => (
                  <li key={i}>
                    <span className="cite-index">{i + 1}</span>
                    <p>{t}</p>
                  </li>
                ))}
              </ol>
            </Panel>
          )}
        </>
      )}

      {!result && !loading && !error && (
        <Panel>
          <EmptyState
            icon={IconGraph}
            title="等待提问"
            desc="GraphRAG 适合跨片段、多跳的全局性问题，例如实体间的关系链路。"
          />
        </Panel>
      )}

      <HistoryPanel
        kind="graph_recall"
        title="GraphRAG 召回历史"
        desc="每次图谱检索的实体命中、关系扩展、耗时与关联任务"
        emptyTitle="暂无召回记录"
        emptyDesc="提一个图谱问题后，这里会留下检索记录；Agent 任务内的检索会带上任务号"
      />
    </div>
  );
}
