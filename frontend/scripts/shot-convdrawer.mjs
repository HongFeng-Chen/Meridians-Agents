// 验证：会话页点击任务链路 → 右侧 50vw 弹层展示任务链路详情
import puppeteer from 'puppeteer-core';
const EXEC = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';
const browser = await puppeteer.launch({ headless: 'new', executablePath: EXEC, args: ['--no-sandbox'] });
const page = await browser.newPage();
await page.setViewport({ width: 1500, height: 950 });
const errors = [];
page.on('pageerror', (e) => errors.push(String(e).slice(0, 100)));

await page.goto('http://localhost:5173/#/chats', { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 1500));
// 打开「链路合并验证」会话
const row = await page.evaluateHandle(() => [...document.querySelectorAll('.chat-dir-list > *')].find((el) => el.textContent.includes('链路合并验证')));
if (await row.asElement()) { await row.asElement().click(); await new Promise((r) => setTimeout(r, 2200)); }

// 点击最后一条消息的任务链路入口（#44）
const toggles = await page.$$('.chat-task-toggle');
console.log('toggles:', toggles.length);
if (toggles.length) {
  await toggles[toggles.length - 1].click();
  await new Promise((r) => setTimeout(r, 2000));
  const info = await page.evaluate(() => {
    const d = document.querySelector('.drawer');
    if (!d) return null;
    const body = d.querySelector('.drawer-body');
    return {
      width: d.getBoundingClientRect().width,
      vw: window.innerWidth,
      right: Math.round(window.innerWidth - d.getBoundingClientRect().right),
      sections: body ? body.textContent.replace(/\s+/g, ' ').slice(0, 200) : '',
      traceNodes: body ? body.querySelectorAll('.trace-node').length : 0,
      tables: body ? body.querySelectorAll('table').length : 0,
    };
  });
  console.log('drawer:', info);
  await page.screenshot({ path: 'shots/conv-drawer.png' });
  // ESC 关闭
  await page.keyboard.press('Escape');
  await new Promise((r) => setTimeout(r, 400));
  const closed = await page.evaluate(() => !document.querySelector('.drawer'));
  console.log('esc closed:', closed);
}
console.log('pageerrors:', errors.length ? errors : 'none');
await browser.close();
