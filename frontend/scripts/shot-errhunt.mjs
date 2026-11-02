// 定位 unknown request error 来源：打开会话后监听网络失败
import puppeteer from 'puppeteer-core';
const EXEC = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';
const browser = await puppeteer.launch({ headless: 'new', executablePath: EXEC, args: ['--no-sandbox'] });
const page = await browser.newPage();
await page.setViewport({ width: 1500, height: 950 });
page.on('response', async (r) => {
  if (r.status() >= 400) console.log('HTTP', r.status(), r.url());
});
page.on('pageerror', (e) => console.log('PAGEERROR', String(e).slice(0, 120)));
await page.goto('http://localhost:5173/#/chats', { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 1200));
// 打开会话
const row = await page.evaluateHandle(() => [...document.querySelectorAll('.chat-dir-list > *')].find((el) => el.textContent.includes('链路合并验证')));
if (await row.asElement()) { await row.asElement().click(); await new Promise((r) => setTimeout(r, 2500)); }
const err = await page.evaluate(() => document.querySelector('.alert, .chat-error')?.textContent || null);
console.log('error alert:', err);
await browser.close();
