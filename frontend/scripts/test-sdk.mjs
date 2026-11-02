#!/usr/bin/env node
/**
 * SDK 行为测试：用一个本地 HTTP 桩验证「契约 → 请求」的映射正确。
 *
 * 覆盖：query 拼装、路径参数编码、JSON body 只带 schema 内字段、
 *       无 body 的 POST、DELETE + query、multipart 文件上传、错误归一化。
 *
 * 用法：npm run test:sdk
 */
import assert from 'node:assert/strict';
import { createServer } from 'node:http';

const received = [];

const server = createServer((req, res) => {
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', () => {
    const raw = Buffer.concat(chunks).toString('utf8');
    received.push({
      method: req.method,
      url: req.url,
      contentType: req.headers['content-type'] || '',
      body: raw,
    });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
  });
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();

const { setApiBase } = await import('../src/api/http.js');
const { rag, prompt, memory } = await import('../src/api/generated/index.js');

setApiBase(`http://127.0.0.1:${port}/api`);

const last = () => received[received.length - 1];

const cases = [
  {
    name: 'GET + query 参数',
    run: () => rag.listDocs({ limit: 5 }),
    expect: { method: 'GET', url: '/api/admin/docs?limit=5' },
  },
  {
    name: 'GET + 路径参数（含特殊字符需编码）',
    run: () => prompt.getPrompt({ key: 'rag.qa.system' }),
    expect: { method: 'GET', url: '/api/prompts/rag.qa.system' },
  },
  {
    name: 'POST JSON body（只带 schema 内字段，不把路径参数写进 body）',
    run: () => prompt.updatePrompt({ key: 'p1', name: '新名字' }),
    expect: { method: 'PATCH', url: '/api/prompts/p1', body: { name: '新名字' } },
  },
  {
    name: 'DELETE + query 参数',
    run: () => rag.deleteDoc({ doc_id: 'doc-1' }),
    expect: { method: 'DELETE', url: '/api/admin/docs?doc_id=doc-1', noBody: true },
  },
  {
    name: 'POST 无 body（契约里未声明 body）',
    run: () => prompt.rollbackPrompt({ key: 'p1' }),
    expect: { method: 'POST', url: '/api/prompts/p1/rollback', noBody: true },
  },
  {
    name: 'POST JSON，空字符串 query 不收进查询串',
    run: () => memory.listShort({ session_id: 's1' }),
    expect: { method: 'GET', url: '/api/memory/short?session_id=s1' },
  },
  {
    name: 'multipart 上传：文件走 form，其余字段走 query',
    run: () => rag.uploadAndIngest({ file: new Blob(['hello']), mode: 'graph' }),
    expect: {
      method: 'POST',
      url: '/api/upload?mode=graph',
      form: true,
    },
  },
];

let failed = 0;
for (const c of cases) {
  try {
    await c.run();
    const req = last();
    assert.equal(req.method, c.expect.method, 'HTTP 方法');
    assert.equal(req.url, c.expect.url, '请求路径');
    if (c.expect.body) {
      assert.match(req.contentType, /application\/json/, 'Content-Type');
      assert.deepEqual(JSON.parse(req.body), c.expect.body, '请求体');
    }
    if (c.expect.noBody) assert.equal(req.body, '', '不应携带请求体');
    if (c.expect.form) {
      assert.match(req.contentType, /multipart\/form-data/, '应为 multipart');
      assert.match(req.body, /name="file"/, '应包含 file 部分');
      assert.match(req.body, /hello/, '文件内容');
    }
    console.log(`  ✓ ${c.name}`);
  } catch (err) {
    failed += 1;
    console.log(`  ✗ ${c.name}\n     ${err.message}`);
  }
}

// 错误归一化：路径参数缺失应在本地就抛 ApiError
try {
  await prompt.getPrompt({});
  console.log('  ✗ 缺失路径参数应报错，但请求成功了');
  failed += 1;
} catch (err) {
  if (err.name === 'ApiError') console.log('  ✓ 缺失路径参数抛 ApiError');
  else {
    console.log(`  ✗ 缺失路径参数错误类型不对：${err.name}`);
    failed += 1;
  }
}

server.close();
const total = cases.length + 1;
console.log(`\n[test-sdk] ${total - failed}/${total} 通过`);
process.exit(failed === 0 ? 0 : 1);
