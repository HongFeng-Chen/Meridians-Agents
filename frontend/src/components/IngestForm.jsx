import { useEffect, useRef, useState } from 'react';
import { api } from '../api.js';
import { Alert, Panel, Spinner } from './UI.jsx';
import { IconDatabase, IconDoc, IconGraph, IconSearch, IconSpark, IconUpload } from './Icons.jsx';
import { SCOPES, scopeHint, scopeLabel } from './Scope.jsx';

const SAMPLE = `Milvus 是一款开源的向量数据库，由 Zilliz 公司主导研发，采用 Apache 2.0 协议开源。
Milvus 3.0 引入湖原生向量搜索与 S3 存储，并增强了检索能力：BM25 索引通过块压缩 postings 与量化降低占用，
在同等召回率下索引体积约为 2.6 版本的三分之一。稀疏检索默认算法升级为 SINDI，对 SPLADE 等学习型稀疏向量
的 QPS 最高可达 MaxScore 的十倍。Zilliz Cloud 是 Zilliz 提供的全托管 Milvus 服务，兼容 Milvus API。
Go SDK 自 v2.5 起迁入主仓库的 client/v2 子模块，使用 milvusclient 包完成集合管理与混合检索。`;

// 纯文本类由浏览器本地读取；二进制文档交给后端解析（PDF/Word/Excel）
const TEXT_EXTS = ['txt', 'md', 'markdown', 'csv', 'tsv', 'json', 'log'];
const ACCEPT = [
  '.pdf', '.docx', '.doc', '.xlsx', '.xls', '.xlsm',
  '.txt', '.md', '.markdown', '.csv', '.tsv', '.json', '.log',
].join(',');

const extOf = (name) => (name.split('.').pop() || '').toLowerCase();

// 索引方式是「实时库」的唯一分叉口：一份内容可以只建一种索引，也可以两种同建。
// 文案按「什么时候选它」来写，而不是按技术名——用户不关心 HNSW 还是 BM25。
const MODES = [
  {
    key: 'both',
    title: '双索引',
    tag: '推荐',
    icon: IconSpark,
    desc: '同一份文档同时写入向量库与知识图谱，检索时 RAG 与 GraphRAG 都能命中',
    meta: '最通用 · 耗时取决于图谱抽取',
  },
  {
    key: 'rag',
    title: '向量索引',
    icon: IconSearch,
    desc: '分块 + 向量化 + BM25，适合原文问答、条款查找这类「找得到原句」的问题',
    meta: '秒级完成 · 按片段召回',
  },
  {
    key: 'graph',
    title: '知识图谱',
    icon: IconGraph,
    desc: 'LLM 抽取实体与关系，适合跨片段、多跳的关系问题（谁和谁通过什么产生联系）',
    meta: '分钟级 · 会多次调用 LLM',
  },
];

const MODE_HINT = {
  both: '两个索引都会重建，入库后可分别用两种检索方式验证效果',
  rag: '只建向量索引；如果这篇文档之前建过图谱，旧图谱引用会被清理',
  graph: '只建图谱；如果这篇文档之前建过分块，旧分块会被清理',
};

export default function IngestForm({ initialMode = 'both', onDone, onOpenTab }) {
  const [mode, setMode] = useState(initialMode);
  const [docId, setDocId] = useState('');
  const [source, setSource] = useState('');
  // 作用域决定这条知识「谁看得见」，默认个人私有（最保守）
  const [scope, setScope] = useState('user');
  const [text, setText] = useState('');
  const [loading, setLoading] = useState(false);
  const [parsing, setParsing] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState(null);
  const [fileName, setFileName] = useState('');
  const [fileType, setFileType] = useState('');
  const [elapsed, setElapsed] = useState(0);
  const fileRef = useRef(null);

  const isGraph = mode === 'graph';
  const needGraph = mode !== 'rag';
  const needVector = mode !== 'graph';
  const current = MODES.find((m) => m.key === mode) || MODES[0];

  // 入库是同步请求，拿不到逐阶段进度；这里只展示「已用时」，
  // 不做假进度条——用户看到的时间是真的，才不会误判卡死。
  useEffect(() => {
    if (!loading) {
      setElapsed(0);
      return undefined;
    }
    const timer = setInterval(() => setElapsed((v) => v + 1), 1000);
    return () => clearInterval(timer);
  }, [loading]);

  const readFile = async (file) => {
    if (!file) return;
    setError('');
    setResult(null);
    setFileName(file.name);
    setFileType('');
    if (!source) setSource(file.name);
    if (!docId) setDocId(file.name.replace(/\.[^.]+$/, ''));

    const ext = extOf(file.name);
    if (TEXT_EXTS.includes(ext)) {
      const reader = new FileReader();
      reader.onload = () => setText(String(reader.result || ''));
      reader.onerror = () => setError('文件读取失败');
      reader.readAsText(file, 'utf-8');
      return;
    }

    // 二进制文档：走后端解析
    setParsing(true);
    setText('');
    try {
      const data = await api.extract(file);
      setText(data.text || '');
      setFileType(data.type || '');
      if (!data.chars) setError('该文件没有解析出文本（可能是扫描件或空文档）');
    } catch (err) {
      setError(`解析失败：${err.message}`);
    } finally {
      setParsing(false);
    }
  };

  const onSubmit = async (e) => {
    e.preventDefault();
    setError('');
    setResult(null);
    if (!text.trim()) {
      setError('请粘贴文本或上传文件');
      return;
    }
    setLoading(true);
    try {
      const payload = {
        doc_id: docId.trim() || `doc-${Date.now()}`,
        source: source.trim() || docId.trim() || '手动输入',
        text,
        scope,
        mode,
      };
      const data = await api.ragIngest(payload);
      const done = { ...data, mode: data.mode || mode, scope };
      setResult(done);
      onDone?.(done);
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  // 一次入库会经过的环节：双索引是「向量 + 图谱」串行，这里如实列出来
  const phases = [
    { label: '解析与切分', on: true },
    { label: '向量化写入', on: needVector },
    { label: '图谱抽取', on: needGraph },
    { label: '写元数据', on: true },
  ].filter((p) => p.on);

  return (
    <form className="form" onSubmit={onSubmit}>
      <Panel title="选择索引方式" desc="实时库把「组织」与「索引」分开：文档只存一份，索引按需建">
        <div className="mode-grid">
          {MODES.map((m) => {
            const Icon = m.icon;
            const active = mode === m.key;
            return (
              <button
                key={m.key}
                type="button"
                className={`mode-card${active ? ' active' : ''}`}
                onClick={() => setMode(m.key)}
                disabled={loading}
                aria-pressed={active}
              >
                <span className="mode-icon">
                  <Icon size={18} />
                </span>
                <span className="mode-title">
                  {m.title}
                  {m.tag && <em className="mode-tag">{m.tag}</em>}
                </span>
                <span className="mode-desc">{m.desc}</span>
                <span className="mode-meta">{m.meta}</span>
                <span className="mode-check" aria-hidden="true">{active ? '✓' : ''}</span>
              </button>
            );
          })}
        </div>
        <div className="mode-hint">
          <span className="mode-hint-label">{current.title}</span>
          <span>{MODE_HINT[mode]}</span>
        </div>
      </Panel>

      <Panel
        title="文档信息"
        desc={isGraph ? '实体与关系的抽取结果会关联到这里的文档标识' : '文档标识用于检索时按来源过滤'}
      >
        <div className="grid-2">
          <label className="field">
            <span className="field-label">文档 ID</span>
            <input
              className="input"
              placeholder="如 product-manual"
              value={docId}
              onChange={(e) => setDocId(e.target.value)}
            />
          </label>
          <label className="field">
            <span className="field-label">来源 / 文件名</span>
            <input
              className="input"
              placeholder="如 产品手册.pdf"
              value={source}
              onChange={(e) => setSource(e.target.value)}
            />
          </label>
        </div>
        <div className="grid-2" style={{ marginTop: 12 }}>
          <label className="field">
            <span className="field-label">知识作用域</span>
            <select className="input" value={scope} onChange={(e) => setScope(e.target.value)}>
              {SCOPES.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
          </label>
          <div className="field">
            <span className="field-label">可见范围</span>
            <span className="muted">{scopeHint(scope)}</span>
          </div>
        </div>
      </Panel>

      <Panel
        title="文档内容"
        desc={`支持 PDF / Word(.docx/.doc) / Excel(.xlsx/.xls) / Markdown / 纯文本，${
          needGraph ? '图谱环节会按分块调用 LLM 抽取实体与关系' : '系统自动分块并向量化'
        }`}
        actions={
          <>
            <button type="button" className="btn ghost sm" onClick={() => setText(SAMPLE)}>
              填入示例
            </button>
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => fileRef.current?.click()}
              disabled={parsing}
            >
              <IconUpload size={15} />
              选择文件
            </button>
            <input
              ref={fileRef}
              type="file"
              accept={ACCEPT}
              hidden
              onChange={(e) => {
                readFile(e.target.files?.[0]);
                e.target.value = ''; // 允许重复选择同一文件
              }}
            />
          </>
        }
      >
        <div
          className="dropzone"
          onDragOver={(e) => e.preventDefault()}
          onDrop={(e) => {
            e.preventDefault();
            readFile(e.dataTransfer.files?.[0]);
          }}
        >
          <textarea
            className="textarea"
            rows={12}
            placeholder="在此粘贴文档内容，或把 PDF / Word / Excel / Markdown / 文本文件拖进来（二进制文档会自动解析为文本，可先检查再入库）…"
            value={text}
            onChange={(e) => setText(e.target.value)}
            disabled={parsing}
          />
          <div className="dropzone-meta">
            <span className="muted">
              {parsing ? (
                <Spinner text="正在解析文档…" />
              ) : fileName ? (
                <>
                  <IconDoc size={14} /> {fileName}
                  {fileType ? ` · ${fileType}` : ''}
                </>
              ) : (
                '支持拖拽上传'
              )}
            </span>
            <span className="muted">
              {text.length.toLocaleString()} 字符
              {fileName && fileType ? ' · 已解析' : ''}
            </span>
          </div>
        </div>
      </Panel>

      {error && <Alert type="error">{error}</Alert>}

      {loading && (
        <div className="ingest-live">
          <div className="ingest-live-head">
            <Spinner text={`正在执行${current.title}入库`} />
            <span className="mono muted">{elapsed}s</span>
          </div>
          <div className="pipe">
            {phases.map((p, i) => (
              <span key={p.label} className="pipe-node">
                <span className="pipe-index">{i + 1}</span>
                {p.label}
              </span>
            ))}
          </div>
          <p className="muted ingest-live-hint">
            {needGraph
              ? '图谱抽取按分块并发调用 LLM，长文档需要数分钟；中途离开页面不会中断后端任务'
              : '向量化按批写入 Milvus，通常几秒内完成'}
          </p>
        </div>
      )}

      {result && (
        <div className="ingest-done">
          <Alert type="success">
            <b>{result.mode === 'both' ? '双索引构建完成' : result.mode === 'graph' ? '图谱构建完成' : '导入成功'}</b>
            {' · '}文档 <b className="mono">{result.doc_id}</b>
            {result.chunks > 0 && <> · 分块 <b>{result.chunks}</b></>}
            {result.entities > 0 && <> · 实体 <b>{result.entities}</b></>}
            {result.relations > 0 && <> · 关系 <b>{result.relations}</b></>}
            {' · '}作用域 <b>{scopeLabel(result.scope || scope)}</b>
            {result.chunks > 0 && <span className="muted">（已可被检索命中）</span>}
          </Alert>
          <div className="ingest-done-actions">
            <button className="btn ghost sm" type="button" onClick={() => onOpenTab?.('assets')}>
              <IconDatabase size={15} /> 查看知识资产
            </button>
            <button className="btn ghost sm" type="button" onClick={() => onOpenTab?.('ask')}>
              <IconSearch size={15} /> 去检索验证
            </button>
            <button
              className="btn ghost sm"
              type="button"
              onClick={() => {
                setResult(null);
                setText('');
                setDocId('');
                setSource('');
                setFileName('');
                setFileType('');
              }}
            >
              再导一篇
            </button>
          </div>
        </div>
      )}

      <div className="form-actions">
        <button className="btn primary" type="submit" disabled={loading || parsing}>
          {loading ? (
            <Spinner text="正在写入…" />
          ) : mode === 'both' ? (
            '双索引入库'
          ) : mode === 'graph' ? (
            '构建知识图谱'
          ) : (
            '导入向量索引'
          )}
        </button>
        <span className="muted">
          {needGraph
            ? '图谱抽取会多次调用 LLM，长文档耗时较长，请耐心等待'
            : '写入后即可在「检索问答」里用向量检索命中'}
        </span>
      </div>
    </form>
  );
}
