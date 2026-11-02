// 截图脚本：GraphRAG 召回历史详情弹层
import puppeteer from 'puppeteer-core';

const EXEC = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';
const BASE = 'http://localhost:5173';

const browser = await puppeteer.launch({
  executablePath: EXEC,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
  defaultViewport: { width: 1500, height: 1050 },
});
const page = await browser.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push(String(e)));

await page.goto(`${BASE}/#/graph-query`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 2500));

// 点召回历史列表第一行（有 detail 的记录才弹完整视图）
const clicked = await page.evaluate(() => {
  const panel = [...document.querySelectorAll('.panel')].find((p) => p.textContent.includes('GraphRAG 召回历史'));
  const row = panel?.querySelector('.table tbody tr.row-click');
  if (row) { row.click(); return row.textContent.slice(0, 60); }
  return null;
});
console.log('clicked row:', clicked);
await new Promise((r) => setTimeout(r, 1500));

const title = await page.evaluate(() => document.querySelector('.modal h3')?.innerText || 'NO DRAWER');
console.log('drawer title:', title.replace(/\n/g, ' '));
const text = await page.evaluate(() => document.querySelector('.modal-body')?.innerText || '');
console.log('has 回答:', text.includes('回答（当时生成）'));
console.log('has 检索子图:', text.includes('检索子图'));
console.log('has 命中实体:', text.includes('命中实体'));
console.log('has 命中关系:', text.includes('命中关系'));
console.log('has svg subgraph:', !!(await page.$('.modal-body .subgraph svg')));
await page.screenshot({ path: 'shots/graph-detail.png' });

// 滚到子图处再截一张
await page.evaluate(() => document.querySelector('.modal-body .subgraph')?.scrollIntoView({ block: 'center' }));
await new Promise((r) => setTimeout(r, 400));
await page.screenshot({ path: 'shots/graph-detail-subgraph.png' });
console.log('page errors:', errs.length ? errs : 'none');
await browser.close();
