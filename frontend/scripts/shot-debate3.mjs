// 截图脚本 3：验证新入口/分组/NaN 修复
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

// 1) 会话页：侧边栏应无「新辩论」和会话列表
await page.goto(`${BASE}/#/chats`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 1500));
const sidebarText = await page.evaluate(() => document.querySelector('.sidebar .nav')?.innerText || '');
console.log('sidebar contains 新辩论:', sidebarText.includes('新辩论'));
console.log('sidebar contains 辩论: 历史 list?', sidebarText.split('\n').slice(0, 6));
await page.screenshot({ path: 'C:/Users/Administrator/WorkBuddy/2026-10-21-21-50-40/frontend/shots/v2-chats.png' });

// 2) 点目录里「⚖ 新辩论」→ 应切到 #/debate 且标题为「辩论」
const btns = await page.$$('.chat-dir-head button');
for (const b of btns) {
  const t = await page.evaluate((el) => el.textContent, b);
  if (t.includes('新辩论')) { await b.click(); break; }
}
await new Promise((r) => setTimeout(r, 1200));
console.log('hash now:', page.url());
const title = await page.evaluate(() => document.querySelector('.page-title')?.innerText || '');
console.log('page title:', title);
await page.screenshot({ path: 'C:/Users/Administrator/WorkBuddy/2026-10-21-21-50-40/frontend/shots/v2-debate-entry.png' });

// 3) 取消 → 回会话页，切到「辩论历史」tab 验证过滤
for (const b of await page.$$('.debate-actions button')) {
  const t = await page.evaluate((el) => el.textContent, b);
  if (t.includes('取消')) { await b.click(); break; }
}
await new Promise((r) => setTimeout(r, 1000));
const tabs = await page.$$('.chat-dir-tab');
for (const t of tabs) {
  const label = await page.evaluate((el) => el.textContent, t);
  if (label.includes('辩论历史')) { await t.click(); break; }
}
await new Promise((r) => setTimeout(r, 800));
const listTitles = await page.evaluate(() =>
  [...document.querySelectorAll('.chat-dir-name')].map((el) => el.innerText.trim()),
);
console.log('辩论历史 tab 列表:', listTitles);
// 4) 打开一个辩论会话，验证轮次不再 NaN
await page.click('.chat-dir-item');
await new Promise((r) => setTimeout(r, 2500));
const headText = await page.evaluate(() => document.querySelector('.chat-head')?.innerText || '');
console.log('chat head:', headText.replace(/\n/g, ' | '));
await page.screenshot({ path: 'C:/Users/Administrator/WorkBuddy/2026-10-21-21-50-40/frontend/shots/v2-debate-history.png' });

console.log('pageerrors:', errs.length ? errs : 'none');
await browser.close();
