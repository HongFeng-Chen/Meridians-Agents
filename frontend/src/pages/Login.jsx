// 登录页：用户名密码（本地 bcrypt 或 LDAP/AD 验密，后端决定走哪条路径）。
//
// 登录成功后令牌由 setAuthToken 写入 localStorage.meridians_token，
// http.js 的统一拦截器会在后续请求自动带上 Authorization 头。
// 登录失败统一提示「用户名或密码错误」——后端刻意不区分细节，前端不做过度解读。
//
// 视觉：全屏深空科幻风 —— 网格地平线 / 星尘粒子 / 星云光斑 / 玻璃拟态控制台卡片。
import { useState } from 'react';
import { auth, setAuthToken } from '../api.js';

export default function Login({ onSuccess }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [tenant, setTenant] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  const submit = async (e) => {
    e.preventDefault();
    if (!username.trim() || !password) {
      setError('请输入用户名与密码');
      return;
    }
    setBusy(true);
    setError('');
    try {
      const res = await auth.login({
        username: username.trim(),
        password,
        tenant: tenant.trim(),
      });
      setAuthToken(res.token);
      onSuccess?.(res);
    } catch (err) {
      const msg = err?.body?.message || err?.message || '';
      setError(/停用/.test(msg) ? msg : '用户名或密码错误');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login-scene">
      {/* 背景层：星云光斑 / 透视网格 / 星尘 / 扫描线 */}
      <div className="ls-bg" aria-hidden="true">
        <div className="ls-orb ls-orb-a" />
        <div className="ls-orb ls-orb-b" />
        <div className="ls-orb ls-orb-c" />
        <div className="ls-grid" />
        <div className="ls-stars">
          {Array.from({ length: 42 }, (_, i) => {
            const seed = (i * 2654435761) % 4294967296;
            const x = (seed % 1000) / 10;
            const y = (((seed / 1000) | 0) % 1000) / 10;
            const d = 2 + ((seed >> 8) % 40) / 10;
            const s = 1 + ((seed >> 16) % 20) / 10;
            return (
              <i
                key={i}
                style={{
                  left: `${x}%`,
                  top: `${y}%`,
                  animationDelay: `${d}s`,
                  width: `${s}px`,
                  height: `${s}px`,
                }}
              />
            );
          })}
        </div>
        <div className="ls-scanline" />
      </div>

      {/* HUD 装饰角标 */}
      <div className="ls-hud ls-hud-tl" aria-hidden="true">SYS.01 // IDENTITY GATE</div>
      <div className="ls-hud ls-hud-tr" aria-hidden="true">SECURE CHANNEL · TLS</div>

      {/* 控制台卡片 */}
      <main className="ls-card">
        <div className="ls-corners" aria-hidden="true">
          <i /><i /><i /><i />
        </div>

        <header className="ls-head">
          <div className="ls-logo" aria-hidden="true">
            <svg viewBox="0 0 48 48" width="44" height="44">
              <defs>
                <linearGradient id="ls-g" x1="0" y1="0" x2="1" y2="1">
                  <stop offset="0" stopColor="#22d3ee" />
                  <stop offset="0.55" stopColor="#6366f1" />
                  <stop offset="1" stopColor="#c084fc" />
                </linearGradient>
              </defs>
              <polygon
                points="24,3 42,13.5 42,34.5 24,45 6,34.5 6,13.5"
                fill="none"
                stroke="url(#ls-g)"
                strokeWidth="2.4"
              />
              <circle cx="24" cy="24" r="5.5" fill="url(#ls-g)" />
              <g stroke="url(#ls-g)" strokeWidth="1.6" opacity="0.75">
                <line x1="24" y1="18.5" x2="24" y2="7" />
                <line x1="28.8" y1="26.7" x2="38.6" y2="32.3" />
                <line x1="19.2" y1="26.7" x2="9.4" y2="32.3" />
              </g>
            </svg>
          </div>
          <h1 className="ls-title">MERIDIANS</h1>
          <p className="ls-sub">AGENT CONTROL CONSOLE</p>
          <div className="ls-divider" aria-hidden="true"><i /><span>身份认证</span><i /></div>
        </header>

        <form className="ls-form" onSubmit={submit}>
          {error && (
            <div className="ls-error" role="alert">
              <span className="ls-error-dot" aria-hidden="true" />
              {error}
            </div>
          )}

          <label className="ls-field">
            <span className="ls-field-label">用户名</span>
            <span className="ls-input-shell">
              <svg className="ls-icon" viewBox="0 0 24 24" width="16" height="16" aria-hidden="true">
                <circle cx="12" cy="8" r="4" fill="none" stroke="currentColor" strokeWidth="1.8" />
                <path d="M4 20c0-3.6 3.6-6 8-6s8 2.4 8 6" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
              </svg>
              <input
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                autoComplete="username"
                autoFocus
                placeholder="域账号或本地用户名"
              />
            </span>
          </label>

          <label className="ls-field">
            <span className="ls-field-label">密码</span>
            <span className="ls-input-shell">
              <svg className="ls-icon" viewBox="0 0 24 24" width="16" height="16" aria-hidden="true">
                <rect x="5" y="10.5" width="14" height="9" rx="2" fill="none" stroke="currentColor" strokeWidth="1.8" />
                <path d="M8 10.5V8a4 4 0 0 1 8 0v2.5" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
                <circle cx="12" cy="15" r="1.6" fill="currentColor" />
              </svg>
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
                placeholder="密码"
              />
            </span>
          </label>

          <label className="ls-field">
            <span className="ls-field-label">租户 <em>（可选，LDAP 登录忽略）</em></span>
            <span className="ls-input-shell">
              <svg className="ls-icon" viewBox="0 0 24 24" width="16" height="16" aria-hidden="true">
                <rect x="4" y="3" width="16" height="18" rx="2" fill="none" stroke="currentColor" strokeWidth="1.8" />
                <path d="M9 8h2M13 8h2M9 12h2M13 12h2M10 21v-4h4v4" fill="none" stroke="currentColor" strokeWidth="1.6" />
              </svg>
              <input
                value={tenant}
                onChange={(e) => setTenant(e.target.value)}
                placeholder="默认 public"
              />
            </span>
          </label>

          <button className="ls-btn" type="submit" disabled={busy}>
            <span className="ls-btn-label">{busy ? '身份验证中…' : '接入控制台'}</span>
            {!busy && <span className="ls-btn-arrow" aria-hidden="true">→</span>}
            <span className="ls-btn-shine" aria-hidden="true" />
          </button>
        </form>

        <footer className="ls-foot">
          <span className="ls-foot-dot" aria-hidden="true" />
          SYSTEM ONLINE · ACCESS LEVEL: OPERATOR
        </footer>
      </main>
    </div>
  );
}
