// 知识作用域：三层可见性在前端的唯一来源。
//
// 文案与后端 biz/rag/scope.go 的 scopeName 保持一致：同一件事在两个地方
// 用不同措辞，用户会以为是两套机制。
export const SCOPES = [
  {
    value: 'user',
    label: '个人私有',
    tone: 'indigo',
    hint: '只有你自己能检索到；入库后也可以再把作用域改成租户共享',
  },
  {
    value: 'tenant',
    label: '租户共享',
    tone: 'teal',
    hint: '同租户的所有成员都能检索到；需要租户管理员（tenant:manage）权限',
  },
  {
    value: 'system',
    label: '平台通用',
    tone: 'violet',
    hint: '所有租户都能检索到，属于平台级知识；需要平台管理员（platform:config）权限',
  },
];

const BY_VALUE = Object.fromEntries(SCOPES.map((s) => [s.value, s]));

export function scopeLabel(value) {
  return BY_VALUE[value]?.label || '个人私有';
}

export function scopeHint(value) {
  return BY_VALUE[value]?.hint || '';
}

export function ScopeBadge({ scope }) {
  const item = BY_VALUE[scope] || BY_VALUE.user;
  return <span className={`badge ${item.tone}`}>{item.label}</span>;
}
