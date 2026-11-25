// 技能运行时的展示与解析工具。
//
// 纯函数，不依赖 React：页面、详情抽屉、执行抽屉共用一套文案与色调，
// 避免「排队中 / queued / 待执行」三种说法同时出现在一个界面里。

export const MODE_LABELS = { local: '本地', remote: '远程', async: '异步' };
export const MODE_TONES = { local: 'default', remote: 'indigo', async: 'warn' };

// MODE_DESC 每种模式的执行语义，配置表单与提示语共用
export const MODE_DESC = {
  local: '把 SKILL.md 注入上下文，模型按说明自己完成，不经过 Worker',
  remote: '调度到 Worker 同步执行，等结果返回后继续',
  async: '先入队拿执行 ID，由派发协程投递，调用方按 ID 查结果',
};

export const EXEC_LABELS = {
  queued: '排队中',
  running: '执行中',
  succeeded: '成功',
  failed: '失败',
  timeout: '超时',
  canceled: '已取消',
};

export const EXEC_STATUS_OPTIONS = ['queued', 'running', 'succeeded', 'failed', 'timeout', 'canceled'];

export const INSTANCE_LABELS = { online: '在线', draining: '排空中', offline: '离线' };

const TERMINAL = new Set(['succeeded', 'failed', 'timeout', 'canceled']);
export const isTerminal = (status) => TERMINAL.has(status);

export const isExecutableMode = (mode) => mode === 'remote' || mode === 'async';

export function execTone(status) {
  if (status === 'succeeded') return 'ok';
  if (status === 'failed' || status === 'timeout') return 'warn';
  if (status === 'running') return 'indigo';
  return undefined;
}

export function instanceTone(status) {
  if (status === 'online') return 'ok';
  if (status === 'draining') return 'indigo';
  return 'warn';
}

// fmtTime 完整时间（执行详情用），后端给的是 RFC3339 带时区
export function fmtTime(v) {
  if (!v) return '—';
  return String(v).replace('T', ' ').slice(0, 19);
}

// shortTime 列表里的紧凑时间：11-17 00:12:03
export function shortTime(v) {
  if (!v) return '—';
  return String(v).replace('T', ' ').slice(5, 19);
}

// relativeTime 「多久之前」：列表/表格里展示心跳新鲜度，比绝对时间更直观
export function relativeTime(v) {
  if (!v) return '';
  const t = Date.parse(String(v));
  if (Number.isNaN(t)) return '';
  const sec = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (sec < 60) return `${sec} 秒前`;
  if (sec < 3600) return `${Math.round(sec / 60)} 分钟前`;
  if (sec < 86400) return `${Math.round(sec / 3600)} 小时前`;
  return `${Math.round(sec / 86400)} 天前`;
}

// registrationSnippet 集群模式下给 Worker 侧的注册 + 心跳接入示例。
// 心跳间隔 15s、判离线阈值 45s（SKILL_INSTANCE_TTL），与后端默认值保持一致。
export function registrationSnippet(name) {
  const base = `${typeof window === 'undefined' ? '' : window.location.origin}/api`;
  return [
    `# 1) 注册：Worker 启动时自报地址（返回实例 ID）`,
    `curl -X POST ${base}/skills/${name}/instances \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -d '{"endpoint":"http://10.0.1.5:9101","weight":1,"max_concurrency":4}'`,
    ``,
    `# 2) 心跳：每 15 秒一次，上报在途数与状态（online | draining）`,
    `curl -X POST ${base}/skills/${name}/instances/<实例ID>/heartbeat \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -d '{"inflight":0,"status":"online"}'`,
  ].join('\n');
}

export function fmtLatency(ms) {
  if (ms == null || ms === 0) return '—';
  return ms >= 1000 ? `${(ms / 1000).toFixed(2)} s` : `${ms} ms`;
}

// parseJSONLoose 解析用户在文本框里写的参数，容忍空值与前后空白
export function parseJSONLoose(text) {
  const raw = (text || '').trim();
  if (!raw) return { value: {}, error: '' };
  try {
    const value = JSON.parse(raw);
    if (value === null || typeof value !== 'object' || Array.isArray(value)) {
      return { value: null, error: '参数需为 JSON 对象，例如 {"file_id":"f1"}' };
    }
    return { value, error: '' };
  } catch (err) {
    return { value: null, error: `JSON 解析失败：${err.message}` };
  }
}

// exampleFromSchema 按技能的 input_schema 生成一份可直接改的示例参数。
// 只处理常见子集（object/array/string/number/boolean/enum），拿不准就留空串，
// 目的是让「试跑」不必从一片空白开始。
export function exampleFromSchema(schemaText) {
  const raw = (schemaText || '').trim();
  if (raw) {
    try {
      const sample = sampleOf(JSON.parse(raw), 0);
      if (sample !== undefined) return JSON.stringify(sample, null, 2);
    } catch {
      // schema 本身不是合法 JSON：不影响试跑，退回空对象
    }
  }
  return '{}';
}

function sampleOf(schema, depth) {
  if (!schema || typeof schema !== 'object' || depth > 3) return undefined;
  if (Array.isArray(schema.enum) && schema.enum.length) return schema.enum[0];
  const type = schema.type || (schema.properties ? 'object' : '');
  switch (type) {
    case 'object': {
      const out = {};
      for (const [key, sub] of Object.entries(schema.properties || {})) {
        out[key] = sampleOf(sub, depth + 1) ?? '';
      }
      return out;
    }
    case 'array':
      return [sampleOf(schema.items, depth + 1) ?? ''];
    case 'number':
    case 'integer':
      return 0;
    case 'boolean':
      return false;
    case 'string':
      return schema.default ?? '';
    default:
      return undefined;
  }
}
