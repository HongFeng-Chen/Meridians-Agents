import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const BASE = 'http://localhost:5173';
const OUT = path.resolve(process.cwd(), '.smoke');
const EDGE = [
  'C://Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C://Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C://Program Files\\Google\\Chrome\\Application\\chrome.exe',
].find((p) => fs.existsSync(p));
fs.mkdirSync(OUT, { recursive: true });

const browser = await puppeteer.launch({ executablePath: EDGE, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--window-size=1440,900'] });
const page = await browser.newPage();
await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1 });
const errors = [];
page.on('response', (r) => { if (r.status() >= 400 && !/favicon/.test(r.url())) errors.push(`http ${r.status()} ${r.url()}`); });
page.on('console', (m) => { if (m.type() === 'error' && !/Failed to load resource/.test(m.text())) errors.push(`console: ${m.text()}`); });
page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`));

await page.goto(`${BASE}/#/cost`, { waitUntil: 'networkidle2', timeout: 30000 });
await new Promise((r) => setTimeout(r, 1200));
await page.screenshot({ path: path.join(OUT, 'cost.png'), fullPage: true });
const title = await page.$eval('.page-title', (el) => el.textContent.trim()).catch(() => '');
const rows = await page.$$eval('.table tbody tr', (els) => els.length).catch(() => 0);
console.log('title=', title, 'rows=', rows, 'errors=', errors.slice(0, 5));
await browser.close();
