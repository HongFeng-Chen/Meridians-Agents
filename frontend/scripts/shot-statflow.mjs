// 截图脚本：仪表盘统计卡片流式布局验证
import puppeteer from 'puppeteer-core';

const EXEC = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';
const BASE = 'http://localhost:5173';

const browser = await puppeteer.launch({
  executablePath: EXEC,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
  defaultViewport: { width: 1500, height: 900 },
});
const page = await browser.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push(String(e)));

await page.goto(`${BASE}/#/dashboard`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 2000));

// 宽屏：卡片应填满整行
const wide = await page.evaluate(() => {
  const g = document.querySelector('.stat-grid');
  const cards = [...g.children];
  const grid = g.getBoundingClientRect();
  const last = cards[cards.length - 1].getBoundingClientRect();
  return { cards: cards.length, gridW: Math.round(grid.width), lastRight: Math.round(last.right - grid.left), display: getComputedStyle(g).display };
});
console.log('wide:', JSON.stringify(wide));
await page.screenshot({ path: 'shots/stat-wide.png' });

// 窄屏：自动换行
await page.setViewport({ width: 900, height: 900 });
await new Promise((r) => setTimeout(r, 800));
const rows = await page.evaluate(() => {
  const g = document.querySelector('.stat-grid');
  const tops = [...g.children].map((c) => Math.round(c.getBoundingClientRect().top));
  return new Set(tops).size;
});
console.log('narrow rows:', rows);
await page.screenshot({ path: 'shots/stat-narrow.png' });
console.log('page errors:', errs.length ? errs : 'none');
await browser.close();
