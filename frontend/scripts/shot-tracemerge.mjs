// 验证：任务中心「任务编号与 Agent 同时排列 + 💬 会话徽标」→ 点击直达会话 → 会话内嵌任务链路展开
import puppeteer from 'puppeteer-core';

const EXEC = 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';
import fs from 'node:fs';

const convId = 'c-18e3392f8d23bc88-6102';
const base = 'http://localhost:5173';
const shot = (name) => `shots/trace-merge-${name}.png`;

const browser = await puppeteer.launch({ headless: 'new', executablePath: EXEC, args: ['--no-sandbox'] });
const page = await browser.newPage();
await page.setViewport({ width: 1500, height: 950 });
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));

// 1) 任务中心
await page.goto(`${base}/#/tasks`, { waitUntil: 'networkidle2' });
await new Promise((r) => setTimeout(r, 1500));
const rowText = await page.evaluate(() => {
  const rows = [...document.querySelectorAll('table tbody tr')];
  const row = rows.find((r) => r.textContent.includes('#44'));
  return row ? row.textContent.replace(/\s+/g, ' ').slice(0, 140) : null;
});
console.log('tasks row #44:', rowText);
await page.screenshot({ path: shot('tasks') });

// 2) 点 💬 会话 → 直达会话
const convBtn = await page.evaluateHandle(() => {
  const rows = [...document.querySelectorAll('table tbody tr')];
  const row = rows.find((r) => r.textContent.includes('#44'));
  return row ? [...row.querySelectorAll('button')].find((b) => b.textContent.includes('会话')) : null;
});
if (convBtn && await convBtn.asElement()) {
  await convBtn.asElement().click();
  await new Promise((r) => setTimeout(r, 2000));
  const loc = await page.evaluate(() => location.hash);
  console.log('after click hash:', loc);
  const convTitle = await page.evaluate(() => document.querySelector('.chat-head-title, .chat-head h3, .chat-head b')?.textContent || document.body.textContent.slice(0, 0));
  console.log('conv head:', convTitle);
  await page.screenshot({ path: shot('conv') });

  // 3) 展开内嵌任务链路
  const toggle = await page.$('.chat-task-toggle');
  if (toggle) {
    await toggle.click();
    await new Promise((r) => setTimeout(r, 1800));
    const traceInfo = await page.evaluate(() => {
      const body = document.querySelector('.chat-task-body');
      if (!body) return null;
      return { nodes: body.querySelectorAll('.trace-node').length, text: body.textContent.replace(/\s+/g, ' ').slice(0, 120) };
    });
    console.log('inline trace:', traceInfo);
  } else {
    console.log('inline trace toggle NOT FOUND');
  }
  await page.evaluate(() => document.querySelector('.chat-stream')?.scrollTo(0, 999999));
  await new Promise((r) => setTimeout(r, 600));
  await page.screenshot({ path: shot('inline') });
} else {
  console.log('conv button NOT FOUND');
}

console.log('pageerrors:', errors.length ? errors : 'none');
await browser.close();
