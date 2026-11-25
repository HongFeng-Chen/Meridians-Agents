// 文档站配图：用本机 Edge（headless）打开控制台，截取页面与链路抽屉，输出到 docs-site/src/assets/img/。
//
// 用法：
//   1) 起后端（:8000，库里有真实数据）+ 前端：npm run dev -- --port 5188
//   2) node scripts/docs-shots.mjs [baseURL] [outDir]
// 默认 baseURL=http://127.0.0.1:5188，outDir=../docs-site/src/assets/img
//
// 截图都按 viewport 截（不截全页），保证文档里比例统一；抽屉额外滚到 Trace 区再截。
import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const BASE = process.argv[2] || 'http://localhost:5188';
const OUT = path.resolve(process.argv[3] || path.join(process.cwd(), '..', '..', 'docs-site', 'src', 'assets', 'img'));
// 只重拍部分截图：第 4 个参数传逗号分隔的截图名（不带 .png），如 console-skills,console-skill-runtime
const ONLY = process.argv[4] ? process.argv[4].split(',').map((s) => s.trim()).filter(Boolean) : null;
const want = (name) => !ONLY || ONLY.includes(name);

const EDGE_CANDIDATES = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
];

function findBrowser() {
  for (const p of EDGE_CANDIDATES) if (fs.existsSync(p)) return p;
  throw new Error('未找到 Edge/Chrome');
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 页面清单：[文件名, 路由, 说明]
const PAGES = [
  ['console-dashboard', 'dashboard', '仪表盘'],
  ['console-tasks', 'tasks', '任务中心列表'],
  ['console-rag-query', 'rag-query', '混合检索问答'],
  ['console-rag-ingest', 'rag-ingest', 'RAG 文档导入（知识作用域）'],
  ['console-graph-query', 'graph-query', '图谱问答'],
  ['console-memory', 'memory', '记忆系统'],
  ['console-agents', 'agents', 'Agent 工厂'],
  ['console-prompts', 'prompts', '提示词工程'],
  ['console-eval', 'eval', '评测中心'],
  ['console-mcp', 'mcp', 'MCP 服务与工具'],
  ['console-skills', 'skills', '技能运行台（模式列与筛选）'],
  ['console-llm', 'llm', '大模型数据源'],
  ['console-admin', 'admin', '文档与系统（作用域筛选）'],
];

fs.mkdirSync(OUT, { recursive: true });

const browser = await puppeteer.launch({
  executablePath: findBrowser(),
  headless: 'new',
  args: ['--no-sandbox', '--disable-gpu', '--hide-scrollbars', '--window-size=1440,900'],
});

const HIDE_SCROLLBAR = `
  ::-webkit-scrollbar { width: 0 !important; height: 0 !important; }
  html { scrollbar-width: none; }
`;

async function open(route, width = 1440, height = 900) {
  const page = await browser.newPage();
  await page.setViewport({ width, height, deviceScaleFactor: 1 });
  await page.goto(`${BASE}/#/${route}`, { waitUntil: 'networkidle2', timeout: 45000 });
  await page.addStyleTag({ content: HIDE_SCROLLBAR });
  await sleep(1400);
  return page;
}

// 点某一行里的「链路」按钮：优先选文本命中 needles 的行，找不到就退回第一行带链路按钮的行
async function clickTrace(page, needles) {
  const ok = await page.evaluate((list) => {
    const rows = [...document.querySelectorAll('tr')];
    const withBtn = rows.filter((tr) =>
      [...tr.querySelectorAll('button')].some((b) => b.innerText.trim() === '链路'));
    if (!withBtn.length) return false;
    const row = withBtn.find((tr) => list.some((n) => tr.innerText.includes(n))) || withBtn[0];
    const btn = [...row.querySelectorAll('button')].find((b) => b.innerText.trim() === '链路');
    btn.click();
    return true;
  }, needles);
  if (!ok) throw new Error('任务列表里没有任何「链路」按钮');
  await sleep(2200);
}

// 把抽屉滚到 Trace 链路标题处
async function scrollToTrace(page) {
  const ok = await page.evaluate(() => {
    const el = [...document.querySelectorAll('.section-title')].find((n) => n.textContent.includes('Trace 链路'));
    if (!el) return false;
    el.scrollIntoView({ block: 'start' });
    window.scrollBy(0, -10);
    return true;
  });
  await sleep(600);
  return ok;
}

const report = [];

// React 受控输入：用原生 setter 触发，让 onChange 生效
async function fillTextarea(page, text) {
  await page.evaluate((v) => {
    const el = document.querySelector('.textarea, textarea');
    const setter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
    setter.call(el, v);
    el.dispatchEvent(new Event('input', { bubbles: true }));
  }, text);
}

// 点按钮并等待 loading 结束（按钮文字恢复）
async function runAction(page, btnText, timeoutMs = 120000) {
  const clicked = await page.evaluate((t) => {
    // 排除侧边导航按钮（同文案的 nav-item 在最前面），只点提交按钮
    const btn = [...document.querySelectorAll('button')].find(
      (b) => !b.className.includes('nav-item') && b.innerText.trim().includes(t),
    );
    if (!btn) return false;
    btn.click();
    return true;
  }, btnText);
  if (!clicked) throw new Error(`未找到按钮「${btnText}」`);
  // loading 时按钮文字会被 Spinner 替换（原按钮消失），等到按钮带原文字且无内嵌 spinner 才算完成
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    await sleep(1500);
    const done = await page.evaluate((t) => {
      const btn = [...document.querySelectorAll('button')].find(
        (b) => !b.className.includes('nav-item') && b.innerText.trim().includes(t),
      );
      return !!btn && !btn.querySelector('.spinner');
    }, btnText);
    if (done) { await sleep(1500); return; }
  }
  throw new Error(`「${btnText}」等待超时`);
}

// 点击左侧会话列表中的指定会话
async function openConversation(page, title) {
  const ok = await page.evaluate((needle) => {
    const el = [...document.querySelectorAll('button, div, li, a')].find(
      (n) => n.children.length && n.innerText.startsWith(needle) && n.closest('.chat-list, aside, .list'),
    ) || [...document.querySelectorAll('button, div, li, a')].find((n) => n.innerText && n.innerText.split('\n')[0] === needle);
    if (!el) return false;
    el.click();
    return true;
  }, title);
  if (!ok) throw new Error(`未找到会话「${title}」`);
  await sleep(2500);
}

// 点击分段选择器（.seg-item）里的某个模式
async function pickSeg(page, label) {
  await page.evaluate((t) => {
    const el = [...document.querySelectorAll('.seg-item')].find((n) => n.innerText.trim() === t);
    if (el) el.click();
  }, label);
  await sleep(1000);
}

// 点击页签
async function clickTab(page, label) {
  await page.evaluate((t) => {
    const el = [...document.querySelectorAll('.tab, button')].find((n) => n.innerText.trim() === t);
    if (el) el.click();
  }, label);
  await sleep(1200);
}

try {
  for (const [name, route, desc] of PAGES) {
    if (!want(name)) continue;
    const page = await open(route);

    if (name === 'console-memory') await clickTab(page, '长期记忆');

    const file = path.join(OUT, `${name}.png`);
    await page.screenshot({ path: file });
    report.push([name, desc, fs.statSync(file).size]);
    console.log(`shot ${name} (${desc})`);
    await page.close();
  }

  // 技能运行台：详情抽屉的「运行配置」（集群注册：接入方式卡片 + 注册/心跳示例 + 实例表）
  if (want('console-skill-runtime')) {
    const page = await open('skills', 1440, 900);
    const opened = await page.evaluate(() => {
      const row = [...document.querySelectorAll('tbody tr')].find((r) => r.textContent.includes('text-stats'));
      const btn = row && [...row.querySelectorAll('button')].find((b) => b.innerText.trim() === '查看');
      if (!btn) return false;
      btn.click();
      return true;
    });
    if (!opened) throw new Error('技能列表里没有 text-stats（或没有「查看」按钮）');
    await sleep(1800);
    const file = path.join(OUT, 'console-skill-runtime.png');
    await page.screenshot({ path: file });
    report.push(['console-skill-runtime', '技能运行配置（集群注册发现）', fs.statSync(file).size]);
    console.log('shot console-skill-runtime');

    // 执行记录页：关抽屉 → 切页签
    await page.evaluate(() => {
      const close = [...document.querySelectorAll('.drawer button')].find((b) => b.innerText.trim() === '关闭');
      if (close) close.click();
    });
    await sleep(500);
    await page.evaluate(() => {
      const tab = [...document.querySelectorAll('button')].find((b) => b.innerText.trim() === '执行记录');
      if (tab) tab.click();
    });
    await sleep(1500);
    const file2 = path.join(OUT, 'console-skill-executions.png');
    await page.screenshot({ path: file2 });
    report.push(['console-skill-executions', '技能执行记录页', fs.statSync(file2).size]);
    console.log('shot console-skill-executions');
    await page.close();
  }

  // 会话页：打开有召回记录的会话
  if (want('console-chats')) {
    const page = await open('chats', 1440, 900);
    await openConversation(page, '你是那个模型，能做什么');
    const file = path.join(OUT, 'console-chats.png');
    await page.screenshot({ path: file });
    report.push(['console-chats', '会话与召回', fs.statSync(file).size]);
    console.log('shot console-chats');
    await page.close();
  }

  // 混合检索问答：真实跑一次检索 + 作答
  if (want('console-rag-query')) {
    const page = await open('rag-query', 1440, 1000);
    await fillTextarea(page, 'Milvus 3.0 在检索性能上有哪些增强？');
    await runAction(page, '提问');
    const file = path.join(OUT, 'console-rag-query.png');
    await page.screenshot({ path: file });
    report.push(['console-rag-query', '混合检索问答（含答案与片段）', fs.statSync(file).size]);
    console.log('shot console-rag-query');
    await page.close();
  }

  // 图谱问答：真实跑一次实体召回 + 多跳扩展（先切「图谱检索」模式再提问）
  if (want('console-graph-query')) {
    const page = await open('graph-query', 1440, 1000);
    await pickSeg(page, '图谱检索');
    await fillTextarea(page, 'Milvus、Zilliz 与 Zilliz Cloud 之间是什么关系？');
    await runAction(page, '提问');
    const file = path.join(OUT, 'console-graph-query.png');
    await page.screenshot({ path: file });
    report.push(['console-graph-query', '图谱问答（含子图与证据）', fs.statSync(file).size]);
    console.log('shot console-graph-query');
    await page.close();
  }

  // 任务链路抽屉：优先取 LLM 任务（span 说明），不在列表里就取第一行带链路的任务
  if (want('console-task-trace') || want('console-task-trace-events')) {
    const page = await open('tasks', 1440, 1000);
    await clickTrace(page, ['什么是链路追踪的 span', '链路追踪', 'trace']);
    const file = path.join(OUT, 'console-task-trace.png');
    await page.screenshot({ path: file });
    report.push(['console-task-trace', '任务链路详情', fs.statSync(file).size]);
    console.log('shot console-task-trace');

    if (await scrollToTrace(page)) {
      const file2 = path.join(OUT, 'console-task-trace-events.png');
      await page.screenshot({ path: file2 });
      report.push(['console-task-trace-events', 'Trace 事件时间线', fs.statSync(file2).size]);
      console.log('shot console-task-trace-events');
    }
    await page.close();
  }

  // 会话链路抽屉：会话页打开目标会话，点消息区的「查看任务链路」
  if (want('console-conv-trace')) {
    const page = await open('chats', 1440, 1000);
    await openConversation(page, '你是那个模型，能做什么');
    const clicked = await page.evaluate(() => {
      // 头部「查看任务链路」是跳转按钮；真正的抽屉入口是消息内嵌的 .chat-task-toggle
      const btn = document.querySelector('.chat-task-toggle') ||
        [...document.querySelectorAll('button')].find((b) => b.innerText.includes('任务链路（会话编排'));
      if (!btn) return false;
      btn.click();
      return true;
    });
    if (!clicked) throw new Error('会话里没有内嵌任务链路入口（.chat-task-toggle）');
    await sleep(2200);
    const file = path.join(OUT, 'console-conv-trace.png');
    await page.screenshot({ path: file });
    report.push(['console-conv-trace', '会话链路详情', fs.statSync(file).size]);
    console.log('shot console-conv-trace');
    await page.close();
  }
} finally {
  await browser.close();
}

console.log('\n[shots] 输出目录', OUT);
for (const [n, d, s] of report) console.log(`  ${n}.png  ${(s / 1024).toFixed(0)} KB  ${d}`);
