import { useCallback, useEffect, useState } from 'react';
import Sidebar from './components/Sidebar.jsx';
import Dashboard from './pages/Dashboard.jsx';
import RagIngest from './pages/RagIngest.jsx';
import RagQuery from './pages/RagQuery.jsx';
import GraphIngest from './pages/GraphIngest.jsx';
import GraphQuery from './pages/GraphQuery.jsx';
import Admin from './pages/Admin.jsx';
import Prompts from './pages/Prompts.jsx';
import Memory from './pages/Memory.jsx';
import LLM from './pages/LLM.jsx';
import Skills from './pages/Skills.jsx';
import Mcp from './pages/Mcp.jsx';
import AIPlatforms from './pages/AIPlatforms.jsx';
import Eval from './pages/Eval.jsx';
import Agents from './pages/Agents.jsx';
import Tasks from './pages/Tasks.jsx';
import Chats from './pages/Chats.jsx';
import Tenants from './pages/Tenants.jsx';
import Users from './pages/Users.jsx';
import Login from './pages/Login.jsx';
import { api } from './api.js';
import { IconRefresh } from './components/Icons.jsx';

const PAGES = {
  dashboard: {
    title: '仪表盘',
    desc: '系统运行状态、知识与智能体规模一览',
    component: Dashboard,
  },
  'rag-ingest': {
    title: 'RAG · 文档导入',
    desc: '文档经分块、向量化后写入 Milvus，同时由服务端 BM25 生成稀疏索引',
    component: RagIngest,
  },
  'rag-query': {
    title: 'RAG · 混合检索问答',
    desc: 'Dense 语义召回 + BM25 关键词召回，RRF 融合后交给大模型生成答案',
    component: RagQuery,
  },
  'graph-ingest': {
    title: 'GraphRAG · 图谱构建',
    desc: 'LLM 抽取实体与关系，合并去重后构建知识图谱',
    component: GraphIngest,
  },
  'graph-query': {
    title: 'GraphRAG · 图谱问答',
    desc: '实体混合检索 + 关系多跳扩展，基于子图与原文证据推理',
    component: GraphQuery,
  },
  prompts: {
    title: '提示词工程',
    desc: '模板版本化：发布即生效、可回滚、可预览渲染，提示词之外不用改代码',
    component: Prompts,
  },
  memory: {
    title: '记忆系统',
    desc: '短期记忆存 Redis、长期记忆存 MySQL，支持向量召回与图谱召回',
    component: Memory,
  },
  llm: {
    title: '大模型数据源',
    desc: '多数据源并存、按场景路由、失败自动切换；配置落在 MySQL',
    component: LLM,
  },
  skills: {
    title: '技能管理',
    desc: '安装启停技能、从技能市场下载，启用后可落盘给运行时读取',
    component: Skills,
  },
  mcp: {
    title: 'MCP 服务与工具',
    desc: '服务配置存 MySQL、工具向量存 Milvus、热点缓存 Redis；http.Client 运行时注入，不落库',
    component: Mcp,
  },
  aiplatforms: {
    title: 'AI 平台',
    desc: '登记千问 / 百炼这类 MCP 广场：从平台目录搜索 MCP 服务并一键接入本地；目录不可用时按 mcpCode 直接拉取',
    component: AIPlatforms,
  },
  eval: {
    title: '评测中心',
    desc: '评测 Harness：用例集 → 异步运行 → 规则断言 + LLM 评审判分 → 报告聚合',
    component: Eval,
  },
  agents: {
    title: 'Agent 工厂',
    desc: 'Agent 是数据不是代码：低代码定义 + LLM 元编程造 Agent + 语义召回，护栏齐备',
    component: Agents,
  },
  tasks: {
    title: '任务中心',
    desc: '异步 worker 执行、多 Agent 协作链路树、trace 事件流实时跟中',
    component: Tasks,
  },
  chats: {
    title: '会话',
    desc: '多轮对话目录：会话内调用 Agent / MCP 工具与 RAG / GraphRAG / Memory 检索召回，链路全程记录（会话 id = 任务 id）',
    component: Chats,
  },
  debate: {
    title: '辩论',
    desc: '多角色自主辩论会话：你只给议题和人设，角色按立场自主交锋，可随时叫停、引导、增减角色，结束后中立汇总',
    component: Chats,
  },
  admin: {
    title: '文档与系统',
    desc: '管理已入库文档，查看连接配置与接口契约',
    component: Admin,
  },
  tenants: {
    title: '租户管理',
    desc: '多租户隔离域：租户键即 tenant_id，创建后不可改；public 是内置租户，不可删除',
    component: Tenants,
  },
  users: {
    title: '用户管理',
    desc: '成员名册：谁属于哪个租户、在该租户是什么角色。名册只做管理，不参与登录认证',
    component: Users,
  },
};

function pageFromHash() {
  // 支持 hash 查询参数（如 #/prompts?agent=xxx）：页面 key 取 ? 之前的部分
  const key = window.location.hash.replace(/^#\/?/, '').split('?')[0];
  return PAGES[key] ? key : 'dashboard';
}

export default function App() {
  const [page, setPage] = useState(pageFromHash);
  const [health, setHealth] = useState('checking');
  const [refreshTick, setRefreshTick] = useState(0);
  // 认证探测：pending=查询中，ok=已登录（或后端未启用认证），need=需要登录。
  // 未开启认证时 /api/auth/me 返回开发态身份，同样落在 ok。
  const [authState, setAuthState] = useState('pending');

  const newChat = () => {
    setRefreshTick((t) => t + 1);
    if (page !== 'chats') window.location.hash = '#/chats';
    else setPage('chats');
  };

  /** 新辩论：进入辩论页（独立标题），直接展开辩论配置面板 */
  const newDebate = () => {
    setRefreshTick((t) => t + 1);
    if (page !== 'debate') window.location.hash = '#/debate';
    else setPage('debate');
  };

  useEffect(() => {
    const onHash = () => setPage(pageFromHash());
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  const checkHealth = useCallback(async () => {
    setHealth('checking');
    try {
      await api.health();
      setHealth('ok');
    } catch {
      setHealth('down');
    }
  }, []);

  useEffect(() => {
    checkHealth();
    const timer = setInterval(checkHealth, 30000);
    return () => clearInterval(timer);
  }, [checkHealth]);

  // 启动探测一次登录态；登录成功 / 登出后由回调驱动
  const checkAuth = useCallback(async () => {
    try {
      await api.authMe();
      setAuthState('ok');
    } catch (err) {
      // 404 = 后端未开启 login（老版本行为），401 = 需要登录；403（停用）也进登录页
      setAuthState(err?.status === 404 ? 'ok' : 'need');
    }
  }, []);

  useEffect(() => {
    checkAuth();
  }, [checkAuth]);

  const navigate = (key) => {
    if (key === page) return;
    window.location.hash = `#/${key}`;
  };

  // 登录门：探测期间保持骨架（不闪登录页），需要登录时整页替换
  if (authState !== 'ok') {
    return (
      <div className="app">
        <main className="main login-main">
          {authState === 'need' ? (
            <Login onSuccess={() => { setAuthState('pending'); checkAuth(); window.location.hash = '#/dashboard'; }} />
          ) : null}
        </main>
      </div>
    );
  }

  const meta = PAGES[page];
  const Page = meta.component;

  return (
    <div className="app">
      <Sidebar page={page} onNavigate={navigate} health={health} onNewChat={newChat} />
      <main className="main">
        <header className="topbar">
          <div>
            <h1 className="page-title">{meta.title}</h1>
            <p className="page-desc">{meta.desc}</p>
          </div>
          <div className="topbar-actions">
            <button
              className="btn ghost sm"
              type="button"
              onClick={() => {
                setRefreshTick((t) => t + 1);
                checkHealth();
              }}
              title="刷新数据"
            >
              <IconRefresh size={16} />
              刷新
            </button>
          </div>
        </header>
        <div className="content" key={`${page}-${refreshTick}`}>
          {page === 'chats' || page === 'debate' ? (
            <Chats debateEntry={page === 'debate'} onNewDebate={newDebate} onNavigate={navigate} />
          ) : (
            <Page onNavigate={navigate} health={health} />
          )}
        </div>
      </main>
    </div>
  );
}
