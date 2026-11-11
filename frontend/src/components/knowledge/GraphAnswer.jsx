import { Badge, EmptyState, Panel } from '../UI.jsx';
import { IconGraph, IconSpark } from '../Icons.jsx';
import Subgraph from '../Subgraph.jsx';

// GraphAnswer 图谱检索（GraphRAG）结果：回答 + 检索子图 + 命中实体 / 关系 + 原文证据。
// compact = true 用于「双路对比」的窄列：省略子图，实体与关系改为单列堆叠。
export default function GraphAnswer({ data, title = '回答', desc = '实体召回后沿关系边多跳扩展，基于子图推理作答', compact = false }) {
  const entities = data?.entities || [];
  const relations = data?.relations || [];
  const evidence = data?.evidence || [];

  return (
    <>
      <Panel
        title={title}
        desc={desc}
        actions={
          <div className="badges-row inline">
            <Badge tone="teal">实体 {entities.length}</Badge>
            <Badge tone="violet">关系 {relations.length}</Badge>
            <Badge tone="default">证据 {evidence.length}</Badge>
          </div>
        }
      >
        <div className="answer">
          <div className="answer-icon">
            <IconSpark size={16} />
          </div>
          <div className="answer-text">{data?.answer}</div>
        </div>

        {!compact && (
          <div className="answer-graph">
            <div className="field-label">检索子图</div>
            <Subgraph entities={entities} relations={relations} width={860} height={360} />
          </div>
        )}
      </Panel>

      <div className={compact ? 'stack' : 'grid-2 align-start'}>
        <Panel title="命中实体" desc="按出现频次排序">
          {entities.length ? (
            <ul className="entity-list">
              {entities.slice(0, compact ? 8 : 20).map((e) => (
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
            <EmptyState icon={IconGraph} title="未命中实体" desc="确认这篇文档建了图谱索引" />
          )}
        </Panel>

        <Panel title="命中关系" desc="权重降序">
          {relations.length ? (
            <ul className="relation-list">
              {relations.slice(0, compact ? 10 : 40).map((r, i) => (
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

      {evidence.length > 0 && (
        <Panel title="原文证据" desc="由图谱节点的 chunk 引用回溯得到">
          <ol className="evidence-list">
            {evidence.slice(0, 10).map((t, i) => (
              <li key={i}>
                <span className="cite-index">{i + 1}</span>
                <p>{t}</p>
              </li>
            ))}
          </ol>
        </Panel>
      )}
    </>
  );
}
