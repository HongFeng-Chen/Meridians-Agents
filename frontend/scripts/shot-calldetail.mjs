// 截图脚本：LLM 调用历史「详情」弹层验证
import puppeteer from 'puppeteer-core';

const EXEC = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';
const BASE = 'http://localhost:5173';

const browser = await puppeteer.launch({
  executablePath: EXEC,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
  defaultViewport: { width: 1500, height: 980 },
});
const page = await browser.newPage();
const errs = [];
page.on('pageerror', (e) => errs.push(String(e)));

await page.goto(`${BASE}/#/llm`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 2500));

// 找调用历史面板里的第一个「详情」按钮
const btns = await page.$$('.table button');
let clicked = false;
for (const b of btns) {
  const t = await page.evaluate((el) => el.textContent, b);
  if (t.trim() === '详情') { await b.click(); clicked = true; break; }
}
console.log('clicked detail:', clicked);
await new Promise((r) => setTimeout(r, 1200));

const drawerTitle = await page.evaluate(() => document.querySelector('.drawer-header h3')?.innerText || '');
console.log('drawer title:', drawerTitle);
const bodyText = await page.evaluate(() => document.querySelector('.call-detail')?.innerText?.slice(0, 400) || 'NO DETAIL');
console.log('detail head:', bodyText.replace(/\n/g, ' | ').slice(0, 300));
await page.screenshot({ path: 'shots/call-detail.png' });

// ESC 关闭
await page.keyboard.press('Escape');
await new Promise((r) => setTimeout(r, 500));
console.log('drawer closed:', !(await page.$('.drawer-mask')));
console.log('page errors:', errs.length ? errs : 'none');
await browser.close();
