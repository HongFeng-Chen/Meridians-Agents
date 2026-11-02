// 临时脚本：验证 MCP 页三个 Tab 的交互（截图到 frontend/.smoke/mcp-*.png）
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import fs from 'node:fs';

const BASE = process.argv[2] || 'http://localhost:5173';
const OUT = path.resolve('.smoke');
const EDGE = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
].find((p) => fs.existsSync(p));

const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox', '--disable-gpu'] });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900 });
const errors = [];
page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));
page.on('console', (m) => m.type() === 'error' && !/Failed to load resource/.test(m.text()) && errors.push(m.text()));

await page.goto(`${BASE}/#/mcp`, { waitUntil: 'networkidle2', timeout: 30000 });
await new Promise((r) => setTimeout(r, 600));

// Tab 2：工具
await page.evaluate(() => [...document.querySelectorAll('.tab')].find((b) => b.textContent.trim() === '工具')?.click());
await new Promise((r) => setTimeout(r, 900));
// 打开第一个工具的调用面板
await page.evaluate(() => [...document.querySelectorAll('button')].find((b) => b.textContent.trim() === '调用')?.click());
await new Promise((r) => setTimeout(r, 500));
await page.screenshot({ path: path.join(OUT, 'mcp-tools.png') });

// Tab 3：语义召回
await page.evaluate(() => [...document.querySelectorAll('.tab')].find((b) => b.textContent.trim() === '语义召回')?.click());
await new Promise((r) => setTimeout(r, 400));
await page.type('.panel-actions input.input', '读一下这个仓库的 README 文件');
await page.evaluate(() => [...document.querySelectorAll('button')].find((b) => b.textContent.trim() === '召回')?.click());
await new Promise((r) => setTimeout(r, 2500));
await page.screenshot({ path: path.join(OUT, 'mcp-recall.png') });

// Tab 1：打开服务详情（运行时 client 表单）
await page.evaluate(() => [...document.querySelectorAll('.tab')].find((b) => b.textContent.trim() === '服务')?.click());
await new Promise((r) => setTimeout(r, 600));
await page.evaluate(() => [...document.querySelectorAll('button')].find((b) => b.textContent.trim() === '详情')?.click());
await new Promise((r) => setTimeout(r, 800));
await page.screenshot({ path: path.join(OUT, 'mcp-detail.png') });

console.log(errors.length ? `ERRORS:\n${errors.join('\n')}` : 'no console/page errors');
await browser.close();
