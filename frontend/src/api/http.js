// 协议驱动的 HTTP 客户端：所有请求都由 generated/paths.js 里的元数据描述，
// 页面只调用 generated/index.js 里的类型化函数，不手写 URL。
//
// 约定（由 proto 的 google.api.http 注解决定）：
//   - pathParams 走 URL 模板替换；
//   - queryParams 走查询串（空字符串 / null / undefined 会被忽略）；
//   - hasBody 时把 bodyProps 里的字段序列化为 JSON；
//   - multipart 时文件固定放在 form 的 file 部分，其余字段走 query。
import { OPERATIONS } from './generated/paths.js';

// 运行时可覆盖（Node 侧的 SDK 测试、自定义网关都靠它），默认取 Vite 环境变量
let overrideBase = null;
export function setApiBase(base) {
  overrideBase = base == null ? null : String(base).replace(/\/+$/, '');
}
function apiBase() {
  if (overrideBase !== null) return overrideBase;
  const fromEnv = import.meta.env?.VITE_API_BASE || '/api';
  return String(fromEnv).replace(/\/+$/, '');
}

/** 统一错误类型：保留 HTTP 状态码与响应体，便于页面区分 400/404/409 */
export class ApiError extends Error {
  constructor(message, status, body) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.body = body;
  }
}

function buildPath(meta, input) {
  let path = meta.path;
  for (const name of meta.pathParams) {
    const value = input[name];
    if (value === undefined || value === null || value === '') {
      throw new ApiError(`缺少路径参数 ${name}`, 0, null);
    }
    path = path.replace(`{${name}}`, encodeURIComponent(String(value)));
  }
  return path;
}

function buildQuery(meta, input) {
  const search = new URLSearchParams();
  for (const name of meta.queryParams) {
    const value = input[name];
    if (value === undefined || value === null || value === '') continue;
    if (Array.isArray(value)) {
      // repeated 字段：逐项展开为同名参数（a=1&a=2），空项跳过
      for (const v of value) {
        if (v === undefined || v === null || v === '') continue;
        search.append(name, String(v));
      }
      continue;
    }
    search.append(name, String(value));
  }
  const qs = search.toString();
  return qs ? `?${qs}` : '';
}

function buildBody(meta, input) {
  const body = {};
  for (const name of meta.bodyProps) {
    if (input[name] !== undefined) body[name] = input[name];
  }
  return body;
}

function joinURL(meta, input) {
  // 契约里的路径自带 /api 前缀，VITE_API_BASE 已含 /api 时去掉，避免重复
  const path = buildPath(meta, input).replace(/^\/api/, '');
  return `${apiBase()}${path}${buildQuery(meta, input)}`;
}

async function parseResponse(res) {
  const text = await res.text();
  let data;
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    data = { error: text };
  }
  if (!res.ok) {
    throw new ApiError(data.error || data.message || `请求失败（HTTP ${res.status}）`, res.status, data);
  }
  return data;
}

/**
 * 调用一个契约方法。
 * @param {string} opKey operationId，如 RagService_Query
 * @param {object} input 路径参数 / query 参数 / body 字段的合集
 * @param {{signal?: AbortSignal, headers?: object, init?: object}} [options]
 */
export async function call(opKey, input = {}, options = {}) {
  const meta = OPERATIONS[opKey];
  if (!meta) throw new ApiError(`未定义的接口：${opKey}`, 0, null);

  const headers = { Accept: 'application/json', ...(options.headers || {}) };
  const init = { method: meta.method, ...(options.init || {}), headers };
  if (options.signal) init.signal = options.signal;

  if (meta.multipart) {
    const file = input.file;
    if (!file) throw new ApiError('请选择要上传的文件', 0, null);
    const form = new FormData();
    form.append('file', file);
    // 不设置 Content-Type，交给浏览器写 boundary
    init.body = form;
  } else if (meta.hasBody) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(buildBody(meta, input));
  }

  const res = await fetch(joinURL(meta, input), init);
  return parseResponse(res);
}

/** 所有已声明的接口元数据（调试面板用） */
export { OPERATIONS };
