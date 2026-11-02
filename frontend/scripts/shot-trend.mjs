// 截图脚本：用量趋势筛选（模型多选 + 时间范围）
import puppeteer from 'puppeteer-core';

const EXEC = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';
const BASE = 'http://localhost:5173';

const browser = await puppeteer.launch({
  executablePath: EXEC,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
  defaultViewport: { width: 1500, height: 1100 },
});
const page = await browser.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push(String(e)));

await page.goto(`${BASE}/#/dashboard`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 2500));

// 工具条存在性
const chips = await page.$$eval('.trend-chips .chip', (els) => els.map((e) => e.textContent.trim()));
console.log('chips:', chips);
const desc1 = await page.evaluate(() => [...document.querySelectorAll('.panel-desc')].map(e=>e.textContent).find(t=>t.includes('聚合')) || '');
console.log('desc:', desc1);
await page.screenshot({ path: 'shots/trend-all.png' });

// 点选一个模型 chip
const chipEls = await page.$$('.trend-chips .chip');
if (chipEls.length > 1) {
  await chipEls[1].click();
  await new Promise((r) => setTimeout(r, 1500));
  const active = await page.$$eval('.trend-chips .chip.active', (els) => els.map((e) => e.textContent.trim()));
  console.log('active chips:', active);
}

// 切到近 24 小时
await page.select('.trend-range select', '24h');
await new Promise((r) => setTimeout(r, 1200));
console.log('range 24h desc:', await page.evaluate(() => [...document.querySelectorAll('.panel-desc')].map(e=>e.textContent).find(t=>t.includes('聚合')) || ''));
await page.screenshot({ path: 'shots/trend-24h.png' });

// 自定义范围
await page.select('.trend-range select', 'custom');
await new Promise((r) => setTimeout(r, 400));
const inputs = await page.$$('.trend-range input[type="datetime-local"]');
console.log('datetime inputs:', inputs.length);
if (inputs.length === 2) {
  await inputs[0].type('2026-10-29T00:00');
  await inputs[1].type('2026-10-30T23:59');
  await new Promise((r) => setTimeout(r, 1500));
  console.log('custom desc:', await page.evaluate(() => [...document.querySelectorAll('.panel-desc')].map(e=>e.textContent).find(t=>t.includes('聚合')) || ''));
}
await page.screenshot({ path: 'shots/trend-custom.png' });
console.log('page errors:', errs.length ? errs : 'none');
await browser.close();
