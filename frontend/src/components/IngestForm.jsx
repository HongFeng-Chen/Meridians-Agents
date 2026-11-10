import { useRef, useState } from 'react';
import { api } from '../api.js';
import { Alert, Panel, Spinner } from './UI.jsx';
import { IconDoc, IconUpload } from './Icons.jsx';
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

export default function IngestForm({ mode }) {
  const isGraph = mode === 'graph';
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
  const fileRef = useRef(null);

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
      };
      const data = isGraph ? await api.graphIngest(payload) : await api.ragIngest(payload);
      setResult({ ...data, mode });
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  return (
    <form className="form" onSubmit={onSubmit}>
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
          isGraph ? '每个分块会调用 LLM 抽取实体与关系' : '系统自动分块并向量化'
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
            rows={14}
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
              {fileName && fileType ? ` · 已解析` : ''}
            </span>
          </div>
        </div>
      </Panel>

      {error && <Alert type="error">{error}</Alert>}
      {result && (
        <Alert type="success">
          {result.mode === 'graph' ? (
            <>
              图谱构建完成 · 文档 <b>{result.doc_id}</b> · 新增/合并实体{' '}
              <b>{result.entities}</b> 个，关系 <b>{result.relations}</b> 条
            </>
          ) : (
            <>
              导入成功 · 文档 <b>{result.doc_id}</b> · 共写入 <b>{result.chunks}</b> 个分块 ·{' '}
              作用域 <b>{scopeLabel(scope)}</b>
            </>
          )}
        </Alert>
      )}

      <div className="form-actions">
        <button className="btn primary" type="submit" disabled={loading || parsing}>
          {loading ? (
            <Spinner text={isGraph ? '正在抽取图谱…' : '正在写入…'} />
          ) : isGraph ? (
            '构建知识图谱'
          ) : (
            '导入知识库'
          )}
        </button>
        <span className="muted">
          {isGraph
            ? '抽取过程会多次调用 LLM，长文档耗时较长，请耐心等待'
            : '写入后即可在「混合检索问答」中检索到该文档'}
        </span>
      </div>
    </form>
  );
}
