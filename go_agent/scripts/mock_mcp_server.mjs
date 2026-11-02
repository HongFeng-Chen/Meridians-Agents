#!/usr/bin/env node
// 本地 MCP 桩服务：联调 / 单测时替代真实的 MCP Server，覆盖三条链路：
//
//   node scripts/mock_mcp_server.mjs --stdio              # stdio：换行分隔 JSON-RPC，子进程模式
//   node scripts/mock_mcp_server.mjs --http 7788          # Streamable HTTP，入口 POST /mcp
//   node scripts/mock_mcp_server.mjs --http 7788 --sse    # 同上，但响应走 text/event-stream
//
// HTTP 模式也支持用查询串临时切换形态：/mcp?mode=json、/mcp?mode=sse，
// 这样一份桩服务可以同时挂两个 MCP 服务配置，验证客户端两条解析分支。
//
// 支持的方法：initialize / notifications/initialized / ping / tools/list / tools/call；
// 其余方法返回 -32601。tools/list 支持 cursor 翻页（每页 2 条），用于验证客户端翻页逻辑。

import http from 'node:http';
import readline from 'node:readline';

const TOOLS = [
  {
    name: 'search_issues',
    title: '搜索 Issue',
    description: '按关键字/状态搜索仓库 issue，返回标题、编号与状态。适合回答“有哪些未关闭的 issue”。',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: '仓库所有者' },
        repo: { type: 'string', description: '仓库名' },
        state: { type: 'string', enum: ['open', 'closed', 'all'], description: '状态过滤' },
        keyword: { type: 'string', description: '标题关键字' },
      },
      required: ['owner', 'repo'],
    },
    annotations: { readOnlyHint: true },
  },
  {
    name: 'create_issue',
    title: '新建 Issue',
    description: '在指定仓库创建一个 issue，可带正文与标签。属于写操作。',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string' },
        repo: { type: 'string' },
        title: { type: 'string' },
        body: { type: 'string' },
        labels: { type: 'array', items: { type: 'string' } },
      },
      required: ['owner', 'repo', 'title'],
    },
    annotations: { destructiveHint: true },
  },
  {
    name: 'read_file',
    title: '读取文件',
    description: '读取仓库中某个文件的文本内容，支持指定分支。',
    inputSchema: {
      type: 'object',
      properties: {
        path: { type: 'string', description: '文件路径' },
        ref: { type: 'string', description: '分支或 commit，默认 main' },
      },
      required: ['path'],
    },
    annotations: { readOnlyHint: true },
  },
];

const PAGE_SIZE = 2;

function toolPage(cursor) {
  const start = Number(cursor || 0);
  const slice = TOOLS.slice(start, start + PAGE_SIZE);
  const next = start + PAGE_SIZE < TOOLS.length ? String(start + PAGE_SIZE) : undefined;
  return {
    tools: slice.map((t) => ({
      name: t.name,
      title: t.title,
      description: t.description,
      inputSchema: t.inputSchema,
      annotations: t.annotations,
    })),
    ...(next ? { nextCursor: next } : {}),
  };
}

function callTool(params) {
  const name = params?.name;
  const args = params?.arguments ?? {};
  if (name === 'search_issues') {
    if (!args.owner || !args.repo) {
      return { isError: true, content: [{ type: 'text', text: '缺少 owner 或 repo' }] };
    }
    const state = args.state || 'open';
    const items = [
      { number: 12, title: 'Milvus 连接超时重试', state },
      { number: 27, title: `[${args.repo}] 支持混合检索`, state },
    ];
    return {
      content: [{ type: 'text', text: `找到 ${items.length} 条 ${state} issue` }],
      structuredContent: { owner: args.owner, repo: args.repo, state, items },
    };
  }
  if (name === 'create_issue') {
    if (!args.title) return { isError: true, content: [{ type: 'text', text: '缺少 title' }] };
    return {
      content: [{ type: 'text', text: `已创建 #101：${args.title}` }],
      structuredContent: { number: 101, title: args.title, labels: args.labels ?? [] },
    };
  }
  if (name === 'read_file') {
    if (!args.path) return { isError: true, content: [{ type: 'text', text: '缺少 path' }] };
    return {
      content: [{ type: 'text', text: `# ${args.path}\n\n(mock content, ref=${args.ref || 'main'})` }],
    };
  }
  return { isError: true, content: [{ type: 'text', text: `未知工具 ${name}` }] };
}

function handle(msg) {
  const { id, method, params } = msg;
  const reply = (result) => ({ jsonrpc: '2.0', id, result });
  switch (method) {
    case 'initialize':
      return reply({
        protocolVersion: '2025-06-18',
        capabilities: { tools: { listChanged: false } },
        serverInfo: { name: 'mock-mcp', version: '0.1.0' },
        instructions: '本地桩服务，仅用于联调',
      });
    case 'ping':
      return reply({});
    case 'tools/list':
      return reply(toolPage(params?.cursor));
    case 'tools/call':
      return reply(callTool(params));
    case 'notifications/initialized':
    case 'notifications/cancelled':
      return null;
    default:
      if (id === undefined || id === null) return null;
      return { jsonrpc: '2.0', id, error: { code: -32601, message: `method not found: ${method}` } };
  }
}

// ---------- stdio ----------
function runStdio() {
  const rl = readline.createInterface({ input: process.stdin });
  rl.on('line', (line) => {
    const text = line.trim();
    if (!text) return;
    let msg;
    try {
      msg = JSON.parse(text);
    } catch {
      process.stdout.write(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error' } }) + '\n');
      return;
    }
    const out = handle(msg);
    if (out) process.stdout.write(JSON.stringify(out) + '\n');
  });
}

// ---------- http ----------
function runHTTP(port, forceSSE) {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, `http://${req.headers.host}`);
    if (req.method === 'DELETE') {
      res.writeHead(204).end();
      return;
    }
    if (req.method !== 'POST' || url.pathname !== '/mcp') {
      res.writeHead(404, { 'Content-Type': 'application/json' }).end('{"error":"not found"}');
      return;
    }
    let raw = '';
    req.setEncoding('utf8');
    req.on('data', (c) => {
      raw += c;
    });
    req.on('end', () => {
      let msg;
      try {
        msg = JSON.parse(raw || '{}');
      } catch {
        res.writeHead(400, { 'Content-Type': 'application/json' }).end('{"error":"bad json"}');
        return;
      }
      const out = handle(msg);
      const mode = url.searchParams.get('mode');
      const sse = mode === 'sse' || (mode !== 'json' && forceSSE);

      if (!out) {
        // 通知：无响应体，但要带上会话头，客户端据此记住会话
        res.writeHead(202, { 'Mcp-Session-Id': 'mock-session-1' }).end();
        return;
      }
      if (sse) {
        res.writeHead(200, {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache',
          Connection: 'keep-alive',
          'Mcp-Session-Id': 'mock-session-1',
        });
        // 先发一条无关事件，验证客户端会按 id 过滤
        res.write('event: message\ndata: {"jsonrpc":"2.0","method":"notifications/message","params":{}}\n\n');
        res.write(`event: message\ndata: ${JSON.stringify(out)}\n\n`);
        res.end();
        return;
      }
      res.writeHead(200, { 'Content-Type': 'application/json', 'Mcp-Session-Id': 'mock-session-1' });
      res.end(JSON.stringify(out));
    });
  });
  server.listen(port, '127.0.0.1', () => {
    console.log(`[mock-mcp] http://127.0.0.1:${port}/mcp  sse=${forceSSE}  tools=${TOOLS.length}`);
  });
}

const argv = process.argv.slice(2);
if (argv.includes('--stdio')) {
  runStdio();
} else {
  const idx = argv.indexOf('--http');
  const port = Number(argv[idx + 1]) || 7788;
  runHTTP(port, argv.includes('--sse'));
}
