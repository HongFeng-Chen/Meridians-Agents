import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, OPERATIONS } from '../api.js';
import { Alert, Badge, EmptyState, Panel, Spinner, StatCard } from '../components/UI.jsx';
import { IconDoc, IconRefresh, IconTrash } from '../components/Icons.jsx';
import { SCOPES, ScopeBadge } from '../components/Scope.jsx';

export default function Admin() {
  const [stats, setStats] = useState(null);
  const [docs, setDocs] = useState([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [pending, setPending] = useState(null);
  // 作用域筛选：列表本来就只返回「可见的三层知识」，这里只是换个视角
  const [scopeFilter, setScopeFilter] = useState('');

  const load = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const [s, d] = await Promise.all([api.stats(), api.docs()]);
      setStats(s);
      setDocs(d.docs || []);
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const removeDoc = async (doc) => {
    setBusy(doc.doc_id);
    setNotice('');
    setError('');
    try {
      const r = await api.deleteDoc(doc.doc_id);
      setNotice(
        `已删除 ${doc.doc_id}：分块 ${r.deleted_chunks} 个，图谱实体 ${r.deleted_entities} 个，关系 ${r.deleted_relations} 条`,
      );
      setPending(null);
      await load();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy('');
    }
  };

  const totalChunks = docs.reduce((sum, d) => sum + d.chunks, 0);
  const shown = useMemo(
    () => (scopeFilter ? docs.filter((d) => (d.scope || 'user') === scopeFilter) : docs),
    [docs, scopeFilter],
  );

  // 接口清单直接来自 proto 生成的契约（OPERATIONS），不再手写维护
  const ops = useMemo(
    () =>
      Object.entries(OPERATIONS).sort(
        (a, b) => a[1].path.localeCompare(b[1].path) || a[1].method.localeCompare(b[1].method),
      ),
    [],
  );

  return (
    <div className="stack">
      <div className="stat-grid">
        <StatCard label="文档数" value={docs.length} hint="按 doc_id 去重" tone="blue" />
        <StatCard label="分块总数" value={stats?.chunks ?? totalChunks} hint="Milvus 主集合" tone="indigo" />
        <StatCard label="图谱实体" value={stats?.entities ?? 0} hint="entity collection" tone="teal" />
        <StatCard label="图谱关系" value={stats?.relations ?? 0} hint="relation collection" tone="violet" />
      </div>

      {error && <Alert type="error">{error}</Alert>}
      {notice && <Alert type="success">{notice}</Alert>}

      <Panel
        title="文档管理"
        desc="删除文档会一并清理仅被该文档引用的图谱节点；跨文档共享实体将保留"
        actions={
          <>
            <div className="chips">
              <button
                type="button"
                className={`chip ${scopeFilter === '' ? 'active' : ''}`}
                onClick={() => setScopeFilter('')}
              >
                全部
              </button>
              {SCOPES.map((s) => (
                <button
                  key={s.value}
                  type="button"
                  className={`chip ${scopeFilter === s.value ? 'active' : ''}`}
                  onClick={() => setScopeFilter(s.value)}
                >
                  {s.label}
                </button>
              ))}
            </div>
            <button className="btn ghost sm" type="button" onClick={load} disabled={loading}>
              {loading ? <Spinner text="加载中" /> : <><IconRefresh size={15} /> 刷新</>}
            </button>
          </>
        }
      >
        {shown.length === 0 ? (
          <EmptyState icon={IconDoc} title="暂无已入库文档" desc="前往「RAG · 文档导入」写入内容" />
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>文档 ID</th>
                  <th>作用域</th>
                  <th>归属</th>
                  <th>来源</th>
                  <th className="num">分块数</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {shown.map((d) => (
                  <tr key={d.doc_id}>
                    <td className="mono ellipsis" title={d.doc_id}>{d.doc_id}</td>
                    <td><ScopeBadge scope={d.scope} /></td>
                    <td className="mono ellipsis" title={`${d.user_id || '—'} @ ${d.tenant_id || '—'}`}>
                      {d.user_id || '—'} @ {d.tenant_id || '—'}
                    </td>
                    <td className="ellipsis" title={d.source}>{d.source || '—'}</td>
                    <td className="num">{d.chunks}</td>
                    <td className="num">
                      <button
                        className="btn danger sm"
                        type="button"
                        onClick={() => setPending(d)}
                        disabled={busy === d.doc_id}
                      >
                        {busy === d.doc_id ? <Spinner text="删除中" /> : <><IconTrash size={14} /> 删除</>}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>

      <div className="grid-2 align-start">
        <Panel title="连接与模型" desc="由后端环境变量决定，修改后需重启服务">
          <div className="kv">
            <div><span>Milvus 地址</span><b className="mono">{stats?.milvus_uri || '—'}</b></div>
            <div><span>主集合</span><b className="mono">{stats?.collection || '—'}</b></div>
            <div><span>实体集合</span><b className="mono">{stats?.collection ? `${stats.collection}_graph_entities` : '—'}</b></div>
            <div><span>关系集合</span><b className="mono">{stats?.collection ? `${stats.collection}_graph_relations` : '—'}</b></div>
            <div><span>Embedding</span><b>{stats?.embed_model || '—'}{stats?.embed_dim ? ` · ${stats.embed_dim}d` : ''}</b></div>
            <div><span>LLM</span><b>{stats?.llm_model || '—'}</b></div>
            <div><span>图谱跳数</span><b>{stats?.graph_hops ?? '—'}</b></div>
          </div>
        </Panel>

        <Panel
          title="接口契约"
          desc={`由 go_agent/api/**/*.proto 生成，共 ${ops.length} 个操作`}
        >
          <ul className="api-list" style={{ maxHeight: 380, overflow: 'auto' }}>
            {ops.map(([key, meta]) => (
              <li key={key}>
                <Badge tone={meta.method === 'DELETE' ? 'warn' : 'ok'}>{meta.method}</Badge>
                <code>{meta.path}</code>
                <span>{meta.summary || key}</span>
              </li>
            ))}
          </ul>
        </Panel>
      </div>

      {pending && (
        <div className="modal-mask" role="dialog" aria-modal="true">
          <div className="modal">
            <h3>确认删除文档？</h3>
            <p>
              将删除 <b className="mono">{pending.doc_id}</b> 的 {pending.chunks} 个分块，
              以及仅被该文档引用的图谱实体与关系。此操作不可撤销。
            </p>
            <div className="modal-actions">
              <button className="btn ghost" type="button" onClick={() => setPending(null)}>
                取消
              </button>
              <button
                className="btn danger"
                type="button"
                onClick={() => removeDoc(pending)}
                disabled={busy === pending.doc_id}
              >
                {busy === pending.doc_id ? <Spinner text="删除中" /> : '确认删除'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
