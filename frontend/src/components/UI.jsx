import React, { useEffect, useState } from 'react';
import { IconAlert, IconCheck } from './Icons.jsx';

export function Panel({ title, desc, actions, children, className = '' }) {
  return (
    <section className={`panel ${className}`}>
      {(title || actions) && (
        <div className="panel-head">
          <div>
            {title && <h2 className="panel-title">{title}</h2>}
            {desc && <p className="panel-desc">{desc}</p>}
          </div>
          {actions && <div className="panel-actions">{actions}</div>}
        </div>
      )}
      <div className="panel-body">{children}</div>
    </section>
  );
}

export function StatCard({ label, value, hint, tone = 'default' }) {
  return (
    <div className={`stat-card tone-${tone}`}>
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value == null ? '—' : value}</div>
      {hint && <div className="stat-hint">{hint}</div>}
    </div>
  );
}

export function Alert({ type = 'info', children }) {
  return (
    <div className={`alert ${type}`}>
      {type === 'error' ? <IconAlert size={16} /> : <IconCheck size={16} />}
      <span>{children}</span>
    </div>
  );
}

export function Spinner({ text = '处理中…' }) {
  return (
    <span className="spinner-wrap">
      <span className="spinner" />
      {text}
    </span>
  );
}

export function EmptyState({ title, desc, icon: Icon }) {
  return (
    <div className="empty">
      {Icon && (
        <div className="empty-icon">
          <Icon size={22} />
        </div>
      )}
      <div className="empty-title">{title}</div>
      {desc && <div className="empty-desc">{desc}</div>}
    </div>
  );
}

export function Badge({ children, tone = 'default', style }) {
  return <span className={`badge ${tone}`} style={style}>{children}</span>;
}

// FormDrawer 新增/修改操作的通用弹层：
// 居中弹窗、遮罩点击与 ESC 关闭、打开时锁定背景滚动；表单内容与底部操作由调用方提供。
// 复杂表单可传 tabs（分组导航）与 footer（固定底栏），header / footer 不随内容滚动。
// 任务链路等宽详情仍走右侧抽屉（Tasks.jsx 直接用 .drawer 样式）。
export function FormDrawer({ title, desc, eyebrow, tabs, footer, onClose, children, width }) {
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      window.removeEventListener('keydown', onKey);
      document.body.style.overflow = prev;
    };
  }, [onClose]);

  return (
    <div className="modal-mask" role="dialog" aria-modal="true" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="modal" style={width ? { width } : undefined}>
        <div className="modal-header">
          <div style={{ flex: 1, minWidth: 0 }}>
            <div className="modal-title-row">
              <h3>{title}</h3>
              {eyebrow && <span className="modal-eyebrow">{eyebrow}</span>}
            </div>
            {desc && <div className="drawer-sub">{desc}</div>}
          </div>
          <button className="btn ghost sm icon-btn" type="button" aria-label="关闭" onClick={onClose}>
            ✕
          </button>
        </div>
        {tabs}
        <div className="modal-body">{children}</div>
        {footer}
      </div>
    </div>
  );
}

// usePagedList 通用前端分页 Hook：
//   items 为完整列表（页面已一次性拉取），本 Hook 负责切片与翻页状态。
//   resetKey 变化时（如筛选条件、tab 切换）自动回到第 1 页；
//   数据变少时自动收敛到最后一页，不会出现空页。
//   返回 { page, pages, size, total, paged, setPage, setSize }。
export function usePagedList(items, { size: initialSize = 10, resetKey } = {}) {
  const [page, setPageRaw] = useState(1);
  const [size, setSizeRaw] = useState(initialSize);
  const total = Array.isArray(items) ? items.length : 0;
  const pages = Math.max(1, Math.ceil(total / size));

  useEffect(() => {
    setPageRaw(1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resetKey]);

  const safePage = Math.min(Math.max(1, page), pages);
  useEffect(() => {
    if (page !== safePage) setPageRaw(safePage);
  }, [page, safePage]);

  const start = (safePage - 1) * size;
  return {
    page: safePage,
    pages,
    size,
    total,
    paged: items.slice(start, start + size),
    setPage: setPageRaw,
    setSize: (s) => {
      setSizeRaw(s);
      setPageRaw(1);
    },
  };
}

// Pagination 通用分页条：总数信息 + 每页条数 + 数字页码（带省略号）+ 上一页/下一页。
// 与 usePagedList 搭配使用：<Pagination {...pg} onPage={pg.setPage} onSize={pg.setSize} />
export function Pagination({ page, pages, total, size, onPage, onSize, className = '' }) {
  if (!total) return null;
  // 生成页码序列：首尾页 + 当前页前后各 1 页，间隔用省略号
  const nums = [1];
  for (let p = page - 1; p <= page + 1; p++) {
    if (p > 1 && p < pages) nums.push(p);
  }
  if (pages > 1) nums.push(pages);
  nums.sort((a, b) => a - b);
  const items = [];
  let prev = 0;
  for (const n of nums) {
    if (n - prev > 1) items.push('…');
    items.push(n);
    prev = n;
  }
  return (
    <div className={`pagination ${className}`.trim()}>
      <span className="pagination-info">
        共 {total} 条 · 第 {page}/{pages} 页
      </span>
      <select
        className="input sm"
        value={size}
        title="每页条数"
        onChange={(e) => onSize(Number(e.target.value))}
      >
        {[10, 20, 50].map((s) => (
          <option key={s} value={s}>
            {s} 条/页
          </option>
        ))}
      </select>
      <button className="btn ghost sm" type="button" disabled={page <= 1} onClick={() => onPage(page - 1)}>
        上一页
      </button>
      {items.map((it, i) =>
        it === '…' ? (
          <span key={`e${i}`} className="pagination-ellipsis">
            …
          </span>
        ) : (
          <button
            key={it}
            type="button"
            className={`btn ghost sm${it === page ? ' active' : ''}`}
            onClick={() => onPage(it)}
          >
            {it}
          </button>
        ),
      )}
      <button className="btn ghost sm" type="button" disabled={page >= pages} onClick={() => onPage(page + 1)}>
        下一页
      </button>
    </div>
  );
}

// 执行图（Agent 架构模式）：单 Agent 推理 / 多 Agent 协作（Agent 页与编排画布共用）
export const GRAPH_GROUPS = [
  {
    label: '自动',
    options: [
      ['auto', 'auto · 自动判定架构 + 自动组队'],
    ],
  },
  {
    label: '单 Agent',
    options: [
      ['simple', 'simple · 单轮直答'],
      ['react', 'react · 思考-行动-观察循环'],
      ['plan_execute', 'plan_execute · 规划-执行分离'],
      ['reflexion', 'reflexion · 执行-评估-反思重试'],
      ['router', 'router · 意图路由到技能'],
      ['self_ask', 'self_ask · 自问自答'],
      ['function_call', 'function_call · 原生单步工具调用'],
      ['state_machine', 'state_machine · 固定状态机'],
    ],
  },
  {
    label: '多 Agent',
    options: [
      ['orchestrator', 'orchestrator · 协调器派活汇总'],
      ['hierarchical', 'hierarchical · 分层规划执行'],
      ['debate', 'debate · 正反辩论收敛'],
      ['blackboard', 'blackboard · 共享黑板协作'],
    ],
  },
];
export const GRAPH_LABELS = Object.fromEntries(GRAPH_GROUPS.flatMap((g) => g.options));
export const GRAPH_HINTS = {
  auto: '不预设架构：每次任务先让 LLM 判定该用哪种执行图（单 Agent 还是多 Agent）、需要几个成员、各自负责什么，缺角色时自动创建子 Agent 再执行。适合任务形态不固定的通用助手。',
  simple: '一次模型调用直接作答，最快最省；适合分类、抽取、改写这类不需要工具的短任务。',
  react: '走一步想一步：思考 → 调用能力 → 观察结果，循环到得出答案；通用性最好。',
  plan_execute: '先一次性生成完整步骤清单，再逐步执行，某步失败自动重规划；步骤可审计，适合长任务与固定流程。',
  reflexion: '先做一版 → 评审打分 → 反思教训 → 带教训重做；质量高但 token 消耗翻倍，适合推理、写作、代码。',
  router: '只做意图判断并分发到对应技能，不让模型自由循环；稳定快，适合客服、业务问答。',
  self_ask: '把问题拆成子问题逐个求解再汇总；轻量检索问答场景。',
  function_call: '不输出思考文本，直接选工具并作答；追求简洁与低延迟。',
  state_machine: '固定状态流转，模型只判断当前该进哪个状态；可控性最强，适合强合规流程（金融、政务）。',
  orchestrator: '总指挥把子任务派给不同专长成员，最后汇总；需要在画布上连入成员 Agent。',
  hierarchical: '规划器拆子目标，成员各自细化执行再逐层汇总；适合跨领域大任务。',
  debate: '正方出方案、反方挑错、多轮修订后收敛；至少需要 2 个成员。',
  blackboard: '成员围绕同一块共享状态板读写推进；适合方案推演、创意写作。',
};
