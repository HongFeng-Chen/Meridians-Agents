import { Badge, EmptyState, Panel } from '../UI.jsx';
import { IconSearch, IconSpark } from '../Icons.jsx';

// RagAnswer 向量检索（RAG）结果：回答 + 命中片段 + 匹配度条。
// 与 GraphAnswer 并列使用（双路对比），所以不假设自己独占整行。
export default function RagAnswer({ data, title = '回答', desc = 'Dense + BM25 双路召回，RRF 融合排序' }) {
  const citations = data?.citations || [];
  const filteredOut = Number(data?.filtered_out || 0);
  const noContext = Boolean(data?.no_context);
  const maxScore = citations.length ? Math.max(...citations.map((c) => c.score || 0)) : 0;

  // 管线兜底：候选全被分数过滤或范围外，展示空态而不是把兜底话术当正常回答
  if (noContext) {
    return (
      <Panel title={title} desc={desc}>
        <EmptyState
          icon={IconSearch}
          title="知识库暂无相关资料"
          desc={filteredOut > 0 ? `本次召回 ${filteredOut} 条候选，全部低于相关分数线，已拦截不进入回答` : '未在知识库中召回相关内容'}
        />
      </Panel>
    );
  }

  return (
    <Panel
      title={title}
      desc={desc}
      actions={
        <>
          {filteredOut > 0 ? (
            <Badge tone="default" style={{ marginRight: 8 }}>
              已过滤 {filteredOut} 条低相关
            </Badge>
          ) : null}
          <Badge tone="indigo">引用 {citations.length} 个片段</Badge>
        </>
      }
    >
      <div className="answer">
        <div className="answer-icon">
          <IconSpark size={16} />
        </div>
        <div className="answer-text">{data?.answer}</div>
      </div>

      <div className="answer-subhead">
        <span>检索片段</span>
        <span className="muted">按融合分排序</span>
      </div>

      {citations.length ? (
        <ol className="cite-list">
          {citations.map((c, i) => {
            const ratio = maxScore > 0 ? Math.max(6, (c.score / maxScore) * 100) : 0;
            return (
              <li key={`${c.id}-${i}`} className="cite">
                <div className="cite-head">
                  <span className="cite-index">{i + 1}</span>
                  <span className="cite-source mono ellipsis" title={c.source || c.doc_id}>
                    {c.source || c.doc_id}
                  </span>
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
        <EmptyState icon={IconSearch} title="没有召回片段" desc="换个问法，或确认这篇文档建了向量索引" />
      )}
    </Panel>
  );
}
