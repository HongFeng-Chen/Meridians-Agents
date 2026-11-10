// 登录页：用户名密码（本地 bcrypt 或 LDAP/AD 验密，后端决定走哪条路径）。
//
// 登录成功后令牌由 setAuthToken 写入 localStorage.meridians_token，
// http.js 的统一拦截器会在后续请求自动带上 Authorization 头。
// 登录失败统一提示「用户名或密码错误」——后端刻意不区分细节，前端不做过度解读。
import { useState } from 'react';
import { Alert, Panel, Spinner } from '../components/UI.jsx';
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
    <div className="login-wrap">
      <Panel title="登录 Meridians 控制台" desc="用户名密码登录；接入 AD/LDAP 的环境直接用域账号">
        <form className="login-form" onSubmit={submit}>
          {error && <Alert type="error">{error}</Alert>}
          <label className="field">
            <span className="field-label">用户名</span>
            <input
              className="input"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              autoFocus
              placeholder="域账号或本地用户名"
            />
          </label>
          <label className="field">
            <span className="field-label">密码</span>
            <input
              className="input"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
              placeholder="密码"
            />
          </label>
          <label className="field">
            <span className="field-label">租户（可选，LDAP 登录忽略）</span>
            <input
              className="input"
              value={tenant}
              onChange={(e) => setTenant(e.target.value)}
              placeholder="默认 public"
            />
          </label>
          <button className="btn primary" type="submit" disabled={busy}>
            {busy ? <Spinner text="验证中" /> : '登录'}
          </button>
        </form>
      </Panel>
    </div>
  );
}
