import { useState } from 'react';
import { Alert } from './UI.jsx';

// 辩论会话创建面板：用户给初始议题 + 自定义临时角色（人设 / 立场 / 风格），
// 设定轮次上限与结束判定后，由后端多角色自主推演，用户仅做监督。
//
// 这里的角色只是本场辩论的临时设定，不写入 Agent 工厂，也不影响其他会话。

const MAX_ROLES = 6;
const MIN_ROLES = 2;

/** 角色预设：一键填充常见对立面 */
const PRESETS = [
  {
    label: '正反双方',
    roles: [
      { name: '主张派', persona: '十年一线业务负责人，习惯从增量与机会看问题', stance: '支持推进，认为收益大于风险', style: '果断、举例多、爱用类比' },
      { name: '质疑派', persona: '长期做风险审查的资深顾问，见惯失败案例', stance: '反对贸然推进，主张先验证再投入', style: '冷静、爱追问边界条件' },
    ],
  },
  {
    label: '三方会审',
    roles: [
      { name: '业务方', persona: '背 KPI 的一线负责人，关注落地速度', stance: '希望尽快拿到结果', style: '直接、讲数字' },
      { name: '技术方', persona: '架构师，关注可维护性与长期成本', stance: '倾向先还技术债再谈扩张', style: '结构化、列清单' },
      { name: '财务方', persona: '成本控制角色，关注投入产出比', stance: '要求先算清账', style: '谨慎、只认数据' },
    ],
  },
  {
    label: '红队压力测试',
    roles: [
      { name: '提案者', persona: '方案设计者，最了解细节', stance: '为方案辩护', style: '有条理、逐步论证' },
      { name: '红队', persona: '专门找漏洞的攻击者，假设方案一定会失败', stance: '系统性地证伪', style: '尖锐、专挑前提假设' },
      { name: '裁判', persona: '中立观察者，只关心论证是否成立', stance: '不站边，记录分歧', style: '简短、只做判定' },
    ],
  },
];

const emptyRole = () => ({ name: '', persona: '', stance: '', style: '' });

const colorOf = (name) => {
  let h = 0;
  for (let i = 0; i < (name || '').length; i += 1) h = (h * 31 + name.charCodeAt(i)) % 360;
  return h;
};

export default function DebateSetup({
  initialTopic = '',
  onSubmit,
  onCancel,
  busy = false,
  live = null, // 进行中 / 已暂停的辩论：锁定表单，actions 切换为暂停 / 叫停 / 进入会话
  onPause,
  onResume,
  onStopDebate,
  onEnterChat,
  children, // 下方「辩论过程」实时区域
}) {
  const locked = !!live;
  const paused = live?.status === 'paused';
  const [topic, setTopic] = useState(initialTopic);
  const [roles, setRoles] = useState(() => PRESETS[0].roles.map((r) => ({ ...r })));
  const [maxRounds, setMaxRounds] = useState(3);
  const [endRule, setEndRule] = useState('rounds');
  const [err, setErr] = useState('');

  const patchRole = (idx, key, value) => {
    setRoles((prev) => prev.map((r, i) => (i === idx ? { ...r, [key]: value } : r)));
  };

  const addRole = () => {
    if (roles.length >= MAX_ROLES) return;
    setRoles((prev) => [...prev, emptyRole()]);
  };

  const removeRole = (idx) => {
    if (roles.length <= MIN_ROLES) return;
    setRoles((prev) => prev.filter((_, i) => i !== idx));
  };

  const submit = () => {
    const t = topic.trim();
    if (!t) {
      setErr('请先填写辩论议题');
      return;
    }
    const named = roles.filter((r) => r.name.trim());
    if (named.length < MIN_ROLES) {
      setErr(`至少需要 ${MIN_ROLES} 个有名称的角色`);
      return;
    }
    const seen = new Set();
    for (const r of named) {
      const n = r.name.trim();
      if (seen.has(n)) {
        setErr(`角色名重复：${n}`);
        return;
      }
      seen.add(n);
    }
    setErr('');
    onSubmit({
      topic: t,
      roles: named.map((r) => ({
        name: r.name.trim(),
        persona: r.persona.trim(),
        stance: r.stance.trim(),
        style: r.style.trim(),
      })),
      maxRounds,
      endRule,
    });
  };

  return (
    <div className="debate-setup">
      <header className="debate-setup-head">
        <div>
          <h2>新辩论会话</h2>
          <p>
            你来定议题和角色，其余交给角色自己吵：每轮按人设自主发言、互相回应，
            你只在旁边监督——可以随时叫停、补充引导或增减角色。
          </p>
        </div>
      </header>

      <div className="debate-form">
        <label className="debate-field">
          <span className="debate-label">辩论议题</span>
          <textarea
            className="input"
            rows={3}
            placeholder="例如：我们是否应该在下季度全面切换到自研 Agent 框架？"
            value={topic}
            disabled={busy || locked}
            onChange={(e) => setTopic(e.target.value)}
          />
        </label>

        <div className="debate-section">
          <div className="debate-section-head">
            <span className="debate-label">角色（{roles.length}/{MAX_ROLES}）</span>
            <div className="debate-presets">
              {PRESETS.map((p) => (
                <button
                  key={p.label}
                  className="btn ghost sm"
                  type="button"
                  disabled={busy || locked}
                  onClick={() => setRoles(p.roles.map((r) => ({ ...r })))}
                >
                  {p.label}
                </button>
              ))}
              <button className="btn ghost sm" type="button" disabled={busy || roles.length >= MAX_ROLES} onClick={addRole}>
                ＋ 加角色
              </button>
            </div>
          </div>

          <div className="debate-role-grid">
            {roles.map((r, idx) => (
              <div className="debate-role-card" key={idx}>
                <div className="debate-role-top">
                  <span
                    className="debate-role-dot"
                    style={{ background: `hsl(${colorOf(r.name || String(idx))} 62% 46%)` }}
                  />
                  <input
                    className="input"
                    placeholder={`角色 ${idx + 1} 名称（如：质疑派）`}
                    value={r.name}
                    disabled={busy || locked}
                    onChange={(e) => patchRole(idx, 'name', e.target.value)}
                  />
                  <button
                    className="chat-dir-btn"
                    type="button"
                    title="移除该角色"
                    disabled={busy || roles.length <= MIN_ROLES}
                    onClick={() => removeRole(idx)}
                  >
                    ✕
                  </button>
                </div>
                <input
                  className="input"
                  placeholder="人设：这个角色是谁、有什么经验"
                  value={r.persona}
                  disabled={busy || locked}
                  onChange={(e) => patchRole(idx, 'persona', e.target.value)}
                />
                <input
                  className="input"
                  placeholder="立场：他主张什么、反对什么"
                  value={r.stance}
                  disabled={busy || locked}
                  onChange={(e) => patchRole(idx, 'stance', e.target.value)}
                />
                <input
                  className="input"
                  placeholder="风格：尖锐 / 冷静 / 只列数据…"
                  value={r.style}
                  disabled={busy || locked}
                  onChange={(e) => patchRole(idx, 'style', e.target.value)}
                />
              </div>
            ))}
          </div>
        </div>

        <div className="debate-config">
          <label className="debate-field">
            <span className="debate-label">轮次上限</span>
            <select
              className="input"
              value={maxRounds}
              disabled={busy || locked}
              onChange={(e) => setMaxRounds(Number(e.target.value))}
            >
              {[1, 2, 3, 4, 5, 6, 8, 10, 12].map((n) => (
                <option key={n} value={n}>
                  {n} 轮（每轮各角色各发言一次）
                </option>
              ))}
            </select>
          </label>
          <label className="debate-field">
            <span className="debate-label">结束判定</span>
            <select className="input" value={endRule} disabled={busy || locked} onChange={(e) => setEndRule(e.target.value)}>
              <option value="rounds">跑满轮次即结束</option>
              <option value="consensus">每轮后由裁判判定，观点收敛则提前结束</option>
            </select>
          </label>
        </div>

        {err && <Alert type="error">{err}</Alert>}

        <div className="debate-actions">
          {locked ? (
            <>
              {paused ? (
                <button className="btn primary" type="button" disabled={busy} onClick={onResume}>
                  {busy ? '处理中…' : '▶ 继续推演'}
                </button>
              ) : (
                <button className="btn ghost" type="button" disabled={busy} onClick={onPause}>
                  {busy ? '处理中…' : '⏸ 暂停'}
                </button>
              )}
              <button className="btn danger" type="button" disabled={busy} onClick={onStopDebate}>
                叫停并汇总
              </button>
              <button className="btn ghost" type="button" onClick={onEnterChat}>
                进入会话视图
              </button>
            </>
          ) : (
            <>
              <button className="btn ghost" type="button" disabled={busy || locked} onClick={onCancel}>
                取消
              </button>
              <button className="btn primary" type="button" disabled={busy || locked} onClick={submit}>
                {busy ? '正在开场…' : '开始辩论'}
              </button>
            </>
          )}
        </div>
      </div>

      {children}
    </div>
  );
}
