// 截图脚本 2：辩论消息流（角色气泡 + 主持人汇总）
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

await page.goto(`${BASE}/#/chats`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 1200));

// 打开列表里的第一个会话（刚开局的辩论）
const item = await page.$('.chat-dir-item');
if (item) await item.click();
// 等角色发言逐条出现
await new Promise((r) => setTimeout(r, 26000));
await page.screenshot({ path: 'C:/Users/Administrator/WorkBuddy/2026-10-21-21-50-40/frontend/shots/debate-messages.png' });

// 输入监督引导并发送
await page.type('.chat-input-row textarea', '请聚焦成本维度，给出可量化的对比。');
for (const b of await page.$$('.chat-composer button')) {
  const t = await page.evaluate((el) => el.textContent, b);
  if (t.includes('提交引导')) {
    await b.click();
    break;
  }
}
await new Promise((r) => setTimeout(r, 2500));
await page.screenshot({ path: 'C:/Users/Administrator/WorkBuddy/2026-10-21-21-50-40/frontend/shots/debate-supervise.png' });

await browser.close();
console.log('done');
