import IngestForm from '../components/IngestForm.jsx';
import { Panel } from '../components/UI.jsx';

export default function GraphIngest() {
  return (
    <div className="stack two-col">
      <IngestForm mode="graph" />
      <aside className="stack">
        <Panel title="图谱构建流程" desc="GraphRAG 导入链路">
          <ol className="steps">
            <li>
              <b>分块 + LLM 抽取</b>
              <span>每个分块调用大模型抽取实体（名称/类型/描述）与有向关系，并发执行</span>
            </li>
            <li>
              <b>合并去重</b>
              <span>实体按名称归一化、关系按「头实体｜尾实体｜谓词」归一化，累计频次与原文引用</span>
            </li>
            <li>
              <b>跨文档合并</b>
              <span>入库前按名称回查已有实体，合并描述、频次与引用，形成全局图谱</span>
            </li>
            <li>
              <b>双索引写入</b>
              <span>实体集合启用 BM25 + Dense 双索引；关系描述同样向量化，支持语义边检索</span>
            </li>
          </ol>
        </Panel>
        <Panel title="注意">
          <ul className="tips">
            <li>抽取质量依赖 LLM 能力，建议使用 7B 以上指令模型</li>
            <li>长文档会产生大量 LLM 调用，请留意耗时与费用</li>
            <li>图谱与向量库共享 chunk 引用，删除文档时仅清理「专属」节点</li>
          </ul>
        </Panel>
      </aside>
    </div>
  );
}
