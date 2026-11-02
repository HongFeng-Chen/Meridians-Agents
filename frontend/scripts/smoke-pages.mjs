// 页面冒烟：用本机 Edge（headless）逐页打开前端，收集控制台/运行时错误并截图。
//
// 用法：
//   1) 先起前端：npm run dev
//   2) node scripts/smoke-pages.mjs [baseURL]
// 默认 baseURL=http://127.0.0.1:5173，截图输出到 .smoke/。
import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const BASE = process.argv[2] || 'http://127.0.0.1:5173';
const OUT = path.resolve(process.cwd(), '.smoke');

const EDGE_CANDIDATES = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
];

const PAGES = [
  ['dashboard', '仪表盘'],
  ['rag-ingest', '文档导入'],
  ['rag-query', '混合检索问答'],
  ['graph-ingest', '图谱构建'],
  ['graph-query', '图谱问答'],
  ['prompts', '提示词工程'],
  ['memory', '记忆系统'],
  ['llm', '大模型数据源'],
  ['skills', '技能管理'],
  ['agents', 'Agent 工厂'],
  ['tasks', '任务中心'],
  ['mcp', 'MCP 服务与工具'],
  ['admin', '文档与系统'],
];

function findBrowser() {
  for (const p of EDGE_CANDIDATES) if (fs.existsSync(p)) return p;
  throw new Error('未找到 Edge/Chrome，可手动传入可执行文件路径');
}

// 这几个接口在空库下返回 404 属正常（例如按 id 取记忆），不算页面错误
const IGNORE = [/favicon/i, /net::ERR_ABORTED/];

fs.mkdirSync(OUT, { recursive: true });

const browser = await puppeteer.launch({
  executablePath: findBrowser(),
  headless: 'new',
  args: ['--no-sandbox', '--disable-gpu', '--window-size=1440,900'],
});

let failed = 0;
try {
  for (const [key, expect] of PAGES) {
    const page = await browser.newPage();
    await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1 });

    const errors = [];
    // 记录非 2xx 响应，带上 URL，便于区分「页面接口挂了」和「favicon 404」这类噪声
    page.on('response', (r) => {
      const url = r.url();
      if (r.status() >= 400 && !IGNORE.some((re) => re.test(url))) {
        errors.push(`http ${r.status()} ${url}`);
      }
    });
    page.on('console', (m) => {
      const text = m.text();
      // 浏览器对 4xx 只会输出一句「Failed to load resource」，URL 由上面的 response 事件补齐
      if (m.type() === 'error' && !/Failed to load resource/.test(text) && !IGNORE.some((r) => r.test(text))) {
        errors.push(`console: ${text}`);
      }
    });
    page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
    page.on('requestfailed', (r) => {
      const msg = `request failed: ${r.url()} ${r.failure()?.errorText ?? ''}`;
      if (!IGNORE.some((re) => re.test(msg))) errors.push(msg);
    });

    await page.goto(`${BASE}/#/${key}`, { waitUntil: 'networkidle2', timeout: 30000 });
    await new Promise((r) => setTimeout(r, 900));

    const title = await page.$eval('.page-title', (el) => el.textContent.trim()).catch(() => '');
    const bodyLen = (await page.$eval('.content', (el) => el.innerText.trim().length).catch(() => 0)) || 0;
    const shot = path.join(OUT, `${key}.png`);
    await page.screenshot({ path: shot, fullPage: false });

    const ok = title.includes(expect) && bodyLen > 20 && errors.length === 0;
    if (!ok) failed++;
    console.log(`${ok ? 'PASS' : 'FAIL'} #/${key} title="${title}" contentLen=${bodyLen}`);
    for (const e of errors.slice(0, 5)) console.log(`      ${e}`);
    await page.close();
  }
} finally {
  await browser.close();
}

console.log(`\n[smoke] ${PAGES.length - failed}/${PAGES.length} 通过，截图目录 ${OUT}`);
process.exit(failed === 0 ? 0 : 1);
