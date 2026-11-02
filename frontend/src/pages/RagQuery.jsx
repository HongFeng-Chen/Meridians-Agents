import { useState } from 'react';
import { api } from '../api.js';
import { Alert, Badge, EmptyState, Panel, Spinner } from '../components/UI.jsx';
import HistoryPanel from '../components/HistoryPanel.jsx';
import { IconSearch, IconSpark } from '../components/Icons.jsx';

const SUGGESTIONS = [
  'Milvus 3.0 在检索性能上有哪些增强？',
  'Zilliz 和 Milvus 是什么关系？',
  'Go SDK 应该如何初始化客户端？',
];

export default function RagQuery() {
  const [question, setQuestion] = useState('');
  const [filter, setFilter] = useState('');
  const [loading, setLoading] = useState(false);
  const [answer, setAnswer] = useState(null);
  const [error, setError] = useState('');

  const ask = async (text) => {
    const q = (text ?? question).trim();
    if (!q || loading) return;
    setQuestion(q);
    setLoading(true);
    setError('');
    setAnswer(null);
    try {
      const data = await api.ragQuery({ question: q, filter: filter.trim() });
      setAnswer(data);
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  const maxScore = answer?.citations?.length
    ? Math.max(...answer.citations.map((c) => c.score || 0))
    : 0;

  return (
    <div className="stack">
      <Panel title="输入问题" desc="检索知识库并由大模型生成带引用的答案">
        <div className="query-box">
          <textarea
            className="textarea"
            rows={3}
            placeholder="例如：Milvus 3.0 的 BM25 索引做了哪些压缩优化？"
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) ask();
            }}
          />
          <div className="query-row">
            <label className="field inline">
              <span className="field-label">标量过滤（可选）</span>
              <input
                className="input mono"
                placeholder='source == "产品手册.md"'
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
              />
            </label>
            <button className="btn primary" type="button" onClick={() => ask()} disabled={loading}>
              {loading ? <Spinner text="检索中…" /> : <><IconSearch size={16} /> 提问</>}
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

      {answer && (
        <>
          <Panel
            title="回答"
            actions={<Badge tone="indigo">引用 {answer.citations?.length || 0} 个片段</Badge>}
          >
            <div className="answer">
              <div className="answer-icon">
                <IconSpark size={16} />
              </div>
              <div className="answer-text">{answer.answer}</div>
            </div>
          </Panel>

          <Panel title="检索片段" desc="Dense + BM25 双路召回，RRF 融合排序">
            {answer.citations?.length ? (
              <ol className="cite-list">
                {answer.citations.map((c, i) => {
                  const ratio = maxScore > 0 ? Math.max(6, (c.score / maxScore) * 100) : 0;
                  return (
                    <li key={`${c.id}-${i}`} className="cite">
                      <div className="cite-head">
                        <span className="cite-index">{i + 1}</span>
                        <span className="cite-source mono">{c.source || c.doc_id}</span>
                        <span className="muted mono">
                          {c.doc_id}#{c.chunk_idx}
                        </span>
                        <span className="cite-score mono">{Number(c.score).toFixed(4)}</span>
                      </div>
                      <div className="score-bar">
                        <span style={{ width: `${ratio}%` }} />
                      </div>
                      <p className="cite-text">{c.text}</p>
                    </li>
                  );
                })}
              </ol>
            ) : (
              <EmptyState icon={IconSearch} title="没有召回片段" desc="请先导入文档，或调整问题表述" />
            )}
          </Panel>
        </>
      )}

      {!answer && !loading && !error && (
        <Panel>
          <EmptyState
            icon={IconSpark}
            title="等待提问"
            desc="支持 ⌘/Ctrl + Enter 快捷发送。系统会先做混合检索，再让模型基于片段作答。"
          />
        </Panel>
      )}

      <HistoryPanel
        kind="rag_recall"
        title="RAG 召回历史"
        desc="每次混合检索（Dense + BM25）的命中情况、耗时与关联任务"
        emptyTitle="暂无召回记录"
        emptyDesc="提一个问题后，这里会留下检索记录；Agent 任务内的检索会带上任务号"
      />
    </div>
  );
}
