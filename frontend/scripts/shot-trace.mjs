// 截图脚本：任务链路合并 trace（事件 + 操作历史 + 存储标签）
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

await page.goto(`${BASE}/#/tasks`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 2200));

// 找任务 #42 行，点「链路」或行按钮打开详情
const clicked = await page.evaluate(() => {
  const rows = [...document.querySelectorAll('.table tbody tr')];
  for (const r of rows) {
    if (r.textContent.includes('#42') || r.textContent.includes('Milvus 3.0 在检索性能')) {
      const btn = r.querySelector('button');
      if (btn) { btn.click(); return r.textContent.slice(0, 60); }
    }
  }
  return null;
});
console.log('clicked row:', clicked);
await new Promise((r) => setTimeout(r, 2000));

const title = await page.evaluate(() => document.querySelector('.drawer-header h3')?.innerText || 'NO DRAWER');
console.log('drawer:', title.replace(/\n/g, ' '));
const sectionTitle = await page.evaluate(() => [...document.querySelectorAll('.section-title')].map(e=>e.innerText).find(t=>t.includes('Trace')));
console.log('trace title:', sectionTitle);
const storeTags = await page.$$eval('.store-tag', (els) => els.map((e) => e.innerText.replace(/\n/g, ' ')));
console.log('store tags:', storeTags);
const nodes = await page.$$eval('.trace-node', (els) => els.map((e) => e.textContent.slice(0, 50).replace(/\n/g, ' ')));
console.log('trace nodes:', nodes.length, nodes.slice(0, 8));
await page.screenshot({ path: 'shots/trace-merged.png' });
console.log('page errors:', errs.length ? errs : 'none');
await browser.close();
