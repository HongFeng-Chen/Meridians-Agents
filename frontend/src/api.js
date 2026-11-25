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
async function v2Request(method, path, params = {}, body) {
  const qs = new URLSearchParams(
    Object.entries(params).filter(([, v]) => v !== '' && v !== null && v !== undefined),
  ).toString();
  const url = `${apiBase()}/v2${path}${qs ? `?${qs}` : ''}`;
  const headers = {};
  const token = authToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  const init = { method, headers };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(url, init);
  const payload = await res.json().catch(() => null);
  if (!res.ok) {
    const msg = payload?.message || payload?.error || `HTTP ${res.status}`;
    throw new ApiError(msg, res.status, payload);
  }
  return payload;
}

const v2Get = (path, params) => v2Request('GET', path, params);

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

  // —— 知识空间（Confluence 式：工作空间 + 无限层级子目录 + 成员授权） ——
  /** 全部可见空间（含平台/租户/部门/个人/自定义；前端据此组树） */
  spaces: () => v2Get('/spaces'),
  spaceChildren: (id) => v2Get('/spaces/children', { space_id: id }),
  spaceDetail: (id) => v2Get('/spaces/detail', { space_id: id }),
  /** 创建空间：{ name, parent_space_id, space_type, inheritance_mode, description } */
  createSpace: (payload) => v2Request('POST', '/spaces', {}, payload),
  /** 更新/移动：{ name?, description?, inheritance_mode?, parent_space_id? }（parent_space_id 出现即移动） */
  updateSpace: (id, payload) => v2Request('PATCH', '/spaces', { space_id: id }, payload),
  deleteSpace: (id) => v2Request('DELETE', '/spaces', { space_id: id }),
  /** 空间显式授权清单 */
  spaceShares: (id) => v2Get('/spaces/shares', { space_id: id }),
  /** 授权：{ principal_type: user|department|tenant, principal_id, actions: [read, write, manage_permission] } */
  grantSpace: (id, payload) => v2Request('POST', '/spaces/shares', { space_id: id }, payload),
  revokeSpace: (shareId) => v2Request('DELETE', '/spaces/shares', { share_id: shareId }),
  /** 把文档移动到目标空间（权限双闸：文档可写 + 空间可管理） */
  moveDoc: (docId, spaceId) => v2Request('PATCH', '/spaces/documents', {}, { doc_id: docId, space_id: spaceId }),
  /** 同步索引：仅向量 → 补建图谱；仅图谱 → 补建向量；补齐后升级双索引 */
  syncDocIndex: (docId) => v2Request('POST', '/spaces/documents/sync-index', {}, { doc_id: docId }),
  /** 兄弟空间排序：orderedIds 顺序即展示顺序（同一父节点下的兄弟整体重排） */
  reorderSpaces: (orderedIds) => v2Request('PATCH', '/spaces/reorder', {}, { ordered_ids: orderedIds }),

  // —— 入库执行进度（前端步骤条：抓取→解析→清洗→分块→向量化→写 Milvus→图谱→完成） ——
  /** 在线 URL 导入（progressId 自生成，入库期间用 ingestProgress 轮询） */
  ingestUrl: ({ url, docId = '', source = '', mode = 'both', spaceId = '', progressId = '' } = {}) =>
    v2Request('POST', '/ingest-url', {}, {
      url, doc_id: docId, source, mode, space_id: spaceId, progress_id: progressId,
    }),
  /** 文本入库（progressId 自生成，入库期间用 ingestProgress 轮询） */
  ingest: ({ text, docId = '', source = '', mode = 'both', scope = '', spaceId = '', progressId = '' } = {}) =>
    v2Request('POST', '/ingest', {}, {
      text, doc_id: docId, source, mode, scope, space_id: spaceId, progress_id: progressId,
    }),
  /** 轮询入库进度：{ found, progress: { step, detail, done, error } } */
  ingestProgress: (id) => v2Get('/ingest-progress', { id }),
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
  upload: (file, { mode = 'rag', docId = '', source = '', scope = '', spaceId = '' } = {}) =>
    rag.uploadAndIngest({ file, mode, doc_id: docId, source, scope, space_id: spaceId }),

  // 管理
  // stats 把 rag.Info() 的子字段摊平，兼容旧页面对 stats.embed_model 的直接取值
  stats: async () => {
    const data = await rag.getStats();
    return { ...(data.rag || {}), ...data };
  },
  docs: (params) => rag.listDocs(params),
  deleteDoc: (docId) => rag.deleteDoc({ doc_id: docId }),
  /** 文档原文分块（知识空间文档详情弹层） */
  docContent: (docId, limit = 100) => rag.docContent({ doc_id: docId, limit }),
  /** 在线 URL 导入：服务端拉取网页正文并入库（{ url, docId?, source?, mode?, spaceId? }） */
  ingestUrl: ({ url, docId = '', source = '', mode = 'rag', spaceId = '' } = {}) =>
    rag.ingestUrl({ url, doc_id: docId, source, mode, space_id: spaceId }),

  // 登录与身份（后端未开启 login 时 me 返回 404，App 据此跳过登录门）
  login: (payload) => auth.login(payload),
  authMe: () => auth.me({}),
};
