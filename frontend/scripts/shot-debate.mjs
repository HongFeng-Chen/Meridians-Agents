// 截图脚本：会话页辩论视图（本地校验用）
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
page.on('console', (m) => m.type() === 'error' && errs.push(m.text()));
page.on('pageerror', (e) => errs.push(String(e)));

await page.goto(`${BASE}/#/chats`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 1200));

// 1) 点「＋ 新对话」旁边的「⚖ 新辩论」
const btns = await page.$$('.chat-dir-head button');
for (const b of btns) {
  const t = await page.evaluate((el) => el.textContent, b);
  if (t.includes('新辩论')) {
    await b.click();
    break;
  }
}
await new Promise((r) => setTimeout(r, 700));
await page.screenshot({ path: 'C:/Users/Administrator/WorkBuddy/2026-10-21-21-50-40/frontend/shots/debate-setup.png' });

// 2) 填议题并开始
await page.type('.debate-field textarea', '我们是否应该在下季度全面切换到自研 Agent 框架？');
for (const b of await page.$$('.debate-actions button')) {
  const t = await page.evaluate((el) => el.textContent, b);
  if (t.includes('开始辩论')) {
    await b.click();
    break;
  }
}
await new Promise((r) => setTimeout(r, 9000));
await page.screenshot({ path: 'C:/Users/Administrator/WorkBuddy/2026-10-21-21-50-40/frontend/shots/debate-live.png' });

console.log('console errors:', errs.length ? errs.slice(0, 5) : 'none');
await browser.close();
