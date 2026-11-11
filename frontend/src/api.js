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
  admin,
  auth,
} from './api/generated/index.js';
import { ApiError, OPERATIONS, setAuthToken, authToken, clearAuthToken, apiBase } from './api/http.js';

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
  admin,
  auth,
  ApiError,
  OPERATIONS,
  // 登录态工具：Login 页与顶栏登出按钮用
  setAuthToken,
  authToken,
  clearAuthToken,
};

/**
 * V2 接口（/api/v2）。
 *
 * 这些路由还没进 proto 契约（见架构文档「已知边界」），SDK 里没有对应函数，
 * 所以在这里手写一层 fetch：鉴权头、基地址、错误口径与 SDK 保持一致，
 * 页面侧仍然是 `await v2.traces({ task_id })` 这样的调用方式。
 *
 * 调用方要容忍「追踪未启用」：/traces 会返回 { enabled: false, traces: [] }。
 */
async function v2Get(path, params = {}) {
  const qs = new URLSearchParams(
    Object.entries(params).filter(([, v]) => v !== '' && v !== null && v !== undefined),
  ).toString();
  const url = `${apiBase()}/v2${path}${qs ? `?${qs}` : ''}`;
  const headers = {};
  const token = authToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  const res = await fetch(url, { headers });
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    const msg = body?.message || body?.error || `HTTP ${res.status}`;
    throw new ApiError(msg, res.status, body);
  }
  return body;
}

export const v2 = {
  /** 任务执行快照（snapshot_id / delegated_caps / trace_id 等 v1 未覆盖的字段） */
  taskDetail: (id) => v2Get('/tasks/detail', { id }),
  /** 链路：按 task_id 查（服务端校验任务归属），不传则浏览全局（需运维权限） */
  traces: (params = {}) => v2Get('/traces', params),
  traceDetail: (traceId) => v2Get('/traces/detail', { trace_id: traceId }),
  /** 成本：单任务成本传 task_id；group_by 支持 agent/task/model/kind/day */
  usageSummary: (params = {}) => v2Get('/usage/summary', params),
  workers: () => v2Get('/workers'),
  leases: () => v2Get('/tasks/leases'),
  deadLetters: () => v2Get('/tasks/dead-letters'),
};

/** 旧版调用方式（Dashboard / Admin / 问答页仍在使用） */
export const api = {  health: () => rag.health(),

  // 业务线 1：混合检索 RAG
  ragIngest: (payload) => rag.ingest(payload),
  ragQuery: (payload) => rag.query(payload),

  // 业务线 2：GraphRAG
  graphIngest: (payload) => rag.graphIngest(payload),
  graphQuery: (payload) => rag.graphQuery(payload),

  // 文件解析与上传（multipart，file 字段）
  formats: () => rag.listFormats(),
  extract: (file) => rag.extractText({ file }),
  upload: (file, { mode = 'rag', docId = '', source = '', scope = '' } = {}) =>
    rag.uploadAndIngest({ file, mode, doc_id: docId, source, scope }),

  // 管理
  // stats 把 rag.Info() 的子字段摊平，兼容旧页面对 stats.embed_model 的直接取值
  stats: async () => {
    const data = await rag.getStats();
    return { ...(data.rag || {}), ...data };
  },
  docs: (params) => rag.listDocs(params),
  deleteDoc: (docId) => rag.deleteDoc({ doc_id: docId }),

  // 登录与身份（后端未开启 login 时 me 返回 404，App 据此跳过登录门）
  login: (payload) => auth.login(payload),
  authMe: () => auth.me({}),
};
