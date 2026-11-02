#!/usr/bin/env node
// LLM 调用记录 UI 截图：LLM 页「调用历史」面板 + 任务详情的 LLM 调用记录段
import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const BASE = process.argv[2] || 'http://localhost:5173';
const OUT = path.resolve(process.cwd(), '.smoke');
fs.mkdirSync(OUT, { recursive: true });

const EDGE_CANDIDATES = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
];
function findBrowser() {
  for (const p of EDGE_CANDIDATES) if (fs.existsSync(p)) return p;
  throw new Error('未找到 Edge/Chrome');
}

const browser = await puppeteer.launch({
  executablePath: findBrowser(),
  headless: 'new',
  args: ['--no-sandbox', '--disable-gpu', '--window-size=1440,960'],
});

try {
  // 1. LLM 页调用历史面板（页面底部）
  let page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 960 });
  await page.goto(`${BASE}/#/llm`, { waitUntil: 'networkidle2', timeout: 30000 });
  await new Promise((r) => setTimeout(r, 1200));
  // 滚到调用历史标题
  await page.evaluate(() => {
    const els = [...document.querySelectorAll('*')];
    const t = els.find((e) => e.children.length === 0 && e.textContent.trim() === '调用历史');
    if (t) t.scrollIntoView({ block: 'start' });
  });
  await new Promise((r) => setTimeout(r, 500));
  await page.screenshot({ path: path.join(OUT, 'llm-history.png') });
  await page.close();

  // 2. 任务详情面板（含 LLM 调用记录段）
  page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 960 });
  await page.goto(`${BASE}/#/tasks`, { waitUntil: 'networkidle2', timeout: 30000 });
  await new Promise((r) => setTimeout(r, 1000));
  const clicked = await page.evaluate(() => {
    const btn = [...document.querySelectorAll('button')].find((b) => b.textContent.trim() === '链路');
    if (btn) {
      btn.click();
      return true;
    }
    return false;
  });
  if (clicked) {
    await new Promise((r) => setTimeout(r, 1500));
    await page.evaluate(() => {
      const els = [...document.querySelectorAll('*')];
      const t = els.find((e) => e.children.length === 0 && /LLM 调用记录/.test(e.textContent));
      if (t) t.scrollIntoView({ block: 'center' });
    });
    await new Promise((r) => setTimeout(r, 400));
    await page.screenshot({ path: path.join(OUT, 'task-llm-calls.png') });
  }
  console.log('[preview] saved llm-history.png' + (clicked ? ' / task-llm-calls.png' : ''));
} finally {
  await browser.close();
}
