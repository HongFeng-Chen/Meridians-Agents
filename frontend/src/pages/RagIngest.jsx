import IngestForm from '../components/IngestForm.jsx';
import { Panel } from '../components/UI.jsx';

export default function RagIngest() {
  return (
    <div className="stack two-col">
      <IngestForm mode="rag" />
      <aside className="stack">
        <Panel title="处理流程" desc="Hybrid RAG 导入链路">
          <ol className="steps">
            <li>
              <b>递归分块</b>
              <span>按段落 / 句子切分，块间保留重叠窗口，避免语义被截断</span>
            </li>
            <li>
              <b>批量向量化</b>
              <span>调用 OpenAI 兼容的 Embedding 接口（默认 bge-m3，1024 维）</span>
            </li>
            <li>
              <b>双索引写入</b>
              <span>
                dense 字段写入 HNSW 索引；text 字段由 Milvus 内置 BM25 Function
                在服务端分词并生成稀疏向量
              </span>
            </li>
          </ol>
        </Panel>
        <Panel title="小提示">
          <ul className="tips">
            <li>中文分词使用 Milvus 的 chinese(jieba) 分析器，专有名词、型号类查询更准</li>
            <li>同一 doc_id 重复导入会产生重复分块，如需覆盖请先在管理页删除</li>
            <li>单次导入上限取决于模型与网络，建议单篇 &lt; 200KB</li>
          </ul>
        </Panel>
      </aside>
    </div>
  );
}
