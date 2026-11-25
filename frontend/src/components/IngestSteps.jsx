// IngestSteps.jsx 入库执行步骤条：入库是同步 RPC，前端在请求进行中
// 用自生成的 progress_id 每 700ms 轮询 GET /api/v2/ingest-progress，
// 把后端各阶段真实上报（抓取 → 解析 → 清洗 → 分块 → 向量化 → 写 Milvus →
// 图谱抽取 → 完成）渲染成步骤条；失败时标红失败步骤并显示原因。
import { useEffect, useState } from 'react';
import { v2 } from '../api.js';

// 全量步骤（按入库方式裁剪后传给 <IngestSteps>）
export const INGEST_STEPS = {
  fetch: { key: 'fetch', label: '获取网页内容' },
  parse: { key: 'parse', label: '解析内容' },
  clean: { key: 'clean', label: '文档清洗' },
  chunk: { key: 'chunk', label: '拆分 chunk' },
  embed: { key: 'embed', label: '向量化' },
  milvus: { key: 'milvus', label: '写入 Milvus' },
  graph: { key: 'graph', label: '图谱抽取' },
  done: { key: 'done', label: '入库完成' },
};

// newProgressId 每次入库生成一个进度 ID（后端据此写进度，前端据此轮询）
export function newProgressId() {
  return (
    globalThis.crypto?.randomUUID?.() ||
    `p-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
  );
}

// useIngestProgress(progressId, active) 轮询进度；active=false 时清空
export function useIngestProgress(progressId, active) {
  const [prog, setProg] = useState(null);
  useEffect(() => {
    if (!progressId || !active) {
      setProg(null);
      return undefined;
    }
    let alive = true;
    const tick = () => {
      v2.ingestProgress(progressId)
        .then((r) => {
          if (alive && r?.found && r.progress) setProg(r.progress);
        })
        .catch(() => { /* 轮询失败不打扰 */ });
    };
    tick();
    const timer = setInterval(tick, 700);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [progressId, active]);
  return prog;
}

// IngestSteps 步骤条本体。steps: [{key,label}]；prog: 轮询结果（可为 null）。
// 状态推导：prog.step 命中 steps 下标 → 之前 done、当前 active、之后 pending；
// prog.done && !error → 全部 done；error → 当前步骤标红。
export function IngestSteps({ steps, prog }) {
  const idxOf = new Map(steps.map((s, i) => [s.key, i]));
  const failed = !!(prog?.done && prog?.error);
  const finished = !!(prog?.done && !prog?.error);
  let active = 0;
  if (prog && !failed) {
    active = idxOf.has(prog.step) ? idxOf.get(prog.step) : 0;
    if (finished) active = steps.length; // 全部完成
  } else if (failed) {
    active = idxOf.has(prog.step) ? idxOf.get(prog.step) : 0;
  }

  return (
    <div className="ing-steps">
      {steps.map((s, i) => {
        const state = finished
          ? 'done'
          : failed
            ? (i < active ? 'done' : i === active ? 'failed' : 'pending')
            : (i < active ? 'done' : i === active ? 'active' : 'pending');
        const icon = state === 'done' ? '✓' : state === 'failed' ? '✕' : state === 'active' ? '●' : '○';
        return (
          <div key={s.key} className={`ing-step ${state}`}>
            <span className="ing-step-icon">{icon}</span>
            <span className="ing-step-label">{s.label}</span>
            {state === 'active' && prog?.detail && (
              <span className="ing-step-detail">{prog.detail}</span>
            )}
          </div>
        );
      })}
      {failed && (
        <div className="ing-step-error">入库失败：{prog.error}</div>
      )}
    </div>
  );
}
