// 统一 API 入口。
//
// 所有接口都由 api/generated/ 下的 SDK 提供（该目录由 proto 契约生成，
// 见 frontend/scripts/gen-sdk.mjs）。页面直接使用 rag / prompt / memory / llm / skill / mcp
// 等命名空间；文件底部保留 api.* 旧签名，兼容早期页面，避免一次性大改。
import {
  rag,
  prompt,
  memory,
  llm,
  skill,
  mcp,
  agentfactory,
  history,
  conversation,
  aiplatform,
  harness,
} from './api/generated/index.js';
import { ApiError, OPERATIONS } from './api/http.js';

export {
  rag,
  prompt,
  memory,
  llm,
  skill,
  mcp,
  agentfactory,
  history,
  conversation,
  aiplatform,
  harness,
  ApiError,
  OPERATIONS,
};

/** 旧版调用方式（Dashboard / Admin / 问答页仍在使用） */
export const api = {
  health: () => rag.health(),

  // 业务线 1：混合检索 RAG
  ragIngest: (payload) => rag.ingest(payload),
  ragQuery: (payload) => rag.query(payload),

  // 业务线 2：GraphRAG
  graphIngest: (payload) => rag.graphIngest(payload),
  graphQuery: (payload) => rag.graphQuery(payload),

  // 文件解析与上传（multipart，file 字段）
  formats: () => rag.listFormats(),
  extract: (file) => rag.extractText({ file }),
  upload: (file, { mode = 'rag', docId = '', source = '' } = {}) =>
    rag.uploadAndIngest({ file, mode, doc_id: docId, source }),

  // 管理
  // stats 把 rag.Info() 的子字段摊平，兼容旧页面对 stats.embed_model 的直接取值
  stats: async () => {
    const data = await rag.getStats();
    return { ...(data.rag || {}), ...data };
  },
  docs: () => rag.listDocs(),
  deleteDoc: (docId) => rag.deleteDoc({ doc_id: docId }),
};
