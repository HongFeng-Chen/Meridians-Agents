import {
  IconBox,
  IconBrain,
  IconCloud,
  IconDashboard,
  IconDatabase,
  IconFlask,
  IconGraph,
  IconLink,
  IconPlug,
  IconPrompt,
  IconSearch,
  IconSettings,
  IconSpark,
  IconTenant,
  IconUpload,
  IconUser,
} from './Icons.jsx';

const NAV_GROUPS = [
  {
    label: '总览',
    items: [{ key: 'dashboard', label: '仪表盘', icon: IconDashboard }],
  },
  {
    label: 'RAG 知识库',
    items: [
      { key: 'rag-ingest', label: '文档导入', icon: IconUpload },
      { key: 'rag-query', label: '混合检索问答', icon: IconSearch },
    ],
  },
  {
    label: 'GraphRAG 知识图谱',
    items: [
      { key: 'graph-ingest', label: '图谱构建', icon: IconDatabase },
      { key: 'graph-query', label: '图谱问答', icon: IconGraph },
    ],
  },
  {
    label: '平台能力',
    items: [
      { key: 'prompts', label: '提示词工程', icon: IconPrompt },
      { key: 'memory', label: '记忆系统', icon: IconBrain },
      { key: 'llm', label: '模型数据源', icon: IconPlug },
      { key: 'skills', label: '技能管理', icon: IconBox },
      { key: 'mcp', label: 'MCP 工具', icon: IconLink },
      { key: 'aiplatforms', label: 'AI 平台', icon: IconCloud },
      { key: 'agents', label: 'Agent 工厂', icon: IconBrain },
      { key: 'eval', label: '评测中心', icon: IconFlask },
      { key: 'tasks', label: '任务中心', icon: IconSpark },
    ],
  },
  {
    label: '系统管理',
    items: [
      { key: 'tenants', label: '租户管理', icon: IconTenant },
      { key: 'users', label: '用户管理', icon: IconUser },
      { key: 'policies', label: '授权策略', icon: IconSettings },
      { key: 'admin', label: '文档与系统', icon: IconSettings },
    ],
  },
];

export default function Sidebar({ page, onNavigate, health, onNewChat }) {
  const healthMeta = {
    ok: { cls: 'ok', text: '服务正常' },
    down: { cls: 'down', text: '后端未连接' },
    checking: { cls: 'idle', text: '检测中…' },
  }[health] || { cls: 'idle', text: '未知' };

  return (
    <aside className="sidebar">
      <div className="brand">
        <div className="brand-mark">
          <IconSpark size={20} />
        </div>
        <div className="brand-text">
          <strong>Meridians</strong>
          <span>经纬 · Agent Platform</span>
        </div>
      </div>

      <nav className="nav">
        {/* 会话：只留新对话入口，最近会话 / 辩论在会话页内管理 */}
        <div className="nav-group">
          <div className="nav-label">会话</div>
          <button className={`nav-item${page === 'chats' || page === 'debate' ? ' active' : ''}`} type="button" onClick={onNewChat}>
            <IconSpark size={17} />
            <span>新对话</span>
          </button>
        </div>
        {NAV_GROUPS.map((group) => (
          <div className="nav-group" key={group.label}>
            <div className="nav-label">{group.label}</div>
            {group.items.map((item) => {
              const Icon = item.icon;
              const active = page === item.key;
              return (
                <button
                  key={item.key}
                  className={`nav-item${active ? ' active' : ''}`}
                  onClick={() => onNavigate(item.key)}
                  type="button"
                >
                  <Icon size={17} />
                  <span>{item.label}</span>
                </button>
              );
            })}
          </div>
        ))}
      </nav>

      <div className="sidebar-footer">
        <div className="health">
          <span className={`dot ${healthMeta.cls}`} />
          <div>
            <div className="health-title">{healthMeta.text}</div>
            <div className="health-sub">API · {import.meta.env.VITE_API_BASE || '/api'}</div>
          </div>
        </div>
      </div>
    </aside>
  );
}
