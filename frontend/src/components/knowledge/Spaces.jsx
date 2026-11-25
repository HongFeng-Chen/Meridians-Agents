import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, v2, admin } from '../../api.js';
import {
  Alert,
  Badge,
  EmptyState,
  FormDrawer,
  Pagination,
  Panel,
  Spinner,
  usePagedList,
} from '../UI.jsx';
import {
  IconAlert,
  IconDoc,
  IconGraph,
  IconRefresh,
  IconSearch,
  IconSpark,
  IconTrash,
  IconUpload,
  IconUser,
} from '../Icons.jsx';
import IngestForm from '../IngestForm.jsx';
import { INGEST_STEPS, IngestSteps, newProgressId, useIngestProgress } from '../IngestSteps.jsx';

// 知识空间：Confluence 式管理 —— 工作空间（workspace）+ 无限层级子目录 + 成员授权。
//
// 后端在 /api/v2/spaces*（biz/space）：空间树（邻接表 + 物化路径，移动自动平移子树）、
// 继承模式（inherit / restricted / isolated）、显式共享（principal + actions）。
// 文档挂载：入库时带 space_id（要求对空间有管理权），或对已有文档 MoveDoc。
//
// 权限模型与 Confluence 对齐：
//   - 空间权限页 = 显式共享清单（谁 · 什么动作）；
//   - inherit 模式下子空间继承父空间授权，restricted 只认显式授权，
//     isolated 完全独立（个人空间默认）。

const SPACE_TYPES = [
  { value: 'custom', label: '工作空间', hint: '团队协作空间，可自由建子目录' },
  { value: 'department', label: '部门空间', hint: '按部门组织，同租户管理员可管理' },
  { value: 'tenant', label: '租户空间', hint: '整个租户共享' },
];

const INHERIT_MODES = [
  { value: 'inherit', label: '继承', hint: '子空间继承父空间的授权（默认）' },
  { value: 'restricted', label: '受限', hint: '只认本空间的显式授权，不再向上继承' },
  { value: 'isolated', label: '隔离', hint: '完全独立，与父空间和兄弟空间互不可见' },
];

const ACTIONS = [
  { value: 'read', label: '阅读', hint: '可以看到空间内的文档' },
  { value: 'write', label: '写入', hint: '可以往空间里放文档' },
  { value: 'manage_permission', label: '管理', hint: '可以改设置、授权、整理文档' },
];

const TYPE_TONE = {
  platform: 'violet',
  tenant: 'indigo',
  department: 'teal',
  personal: 'default',
  custom: 'indigo',
};

const TYPE_LABEL = {
  platform: '平台',
  tenant: '租户',
  department: '部门',
  personal: '个人',
  custom: '工作空间',
};

const PRINCIPAL_LABEL = {
  user: '用户',
  department: '部门',
  tenant: '租户',
  role: '角色',
  service: '服务',
};

const ACTION_TONE = {
  read: 'indigo',
  write: 'teal',
  manage_permission: 'violet',
};

function inheritLabel(v) {
  return INHERIT_MODES.find((m) => m.value === v)?.label || v || '继承';
}

// buildTree 把平铺的空间列表按 parent_space_id 组装成树。
// 孤儿节点（父空间不可见/已删）提升为根，保证数据异常时界面仍可用。
function buildTree(spaces) {
  const byId = new Map(spaces.map((s) => [s.space_id, { ...s, children: [] }]));
  const roots = [];
  for (const node of byId.values()) {
    const parent = byId.get(node.parent_space_id || '');
    if (parent && parent !== node) {
      parent.children.push(node);
    } else {
      roots.push(node);
    }
  }
  const sortRec = (nodes) => {
    nodes.sort((a, b) => (a.name || '').localeCompare(b.name || '', 'zh-CN'));
    nodes.forEach((n) => sortRec(n.children));
  };
  sortRec(roots);
  return roots;
}

// ancestorsOf 返回 space_id 的祖先链（不含自身），面包屑用。
function ancestorsOf(spaceId, byId) {
  const chain = [];
  let cur = byId.get(spaceId)?.parent_space_id;
  const guard = new Set();
  while (cur && byId.has(cur) && !guard.has(cur)) {
    guard.add(cur);
    chain.unshift(byId.get(cur));
    cur = byId.get(cur).parent_space_id;
  }
  return chain;
}

// descendantsOf 返回自身 + 全部后代 ID（移动空间时禁止挪进自己的子树，防环）。
function descendantsOf(node) {
  const ids = [node.space_id];
  node.children.forEach((c) => ids.push(...descendantsOf(c)));
  return ids;
}

// flattenTree 按树序展平空间（带层级深度），供「移动文档」目标空间下拉的缩进展示。
function flattenTree(nodes, depth = 0, out = []) {
  for (const n of nodes) {
    out.push({ space_id: n.space_id, name: n.name, depth });
    if (n.children.length) flattenTree(n.children, depth + 1, out);
  }
  return out;
}

// TreeNode 单个空间节点（递归渲染无限层级）。
// 展开时：节点下方先列出该空间的文档名（50 篇/页，「加载下一页」续拉），再渲染子目录。
// 行支持拖拽：拖到同父兄弟上方/下方松手即重排（dnd.onDrop 交给主组件算新顺序）。
function TreeNode({ node, depth, selectedId, expanded, onToggle, onSelect, onAction, hasChildren, treeDocs, onLoadDocs, dnd }) {
  const isOpen = expanded.has(node.space_id);
  const docsEntry = treeDocs ? treeDocs[node.space_id] : undefined;
  const actions = ['sub', 'perm', 'rename', 'move', 'del'];
  const PAGE = 50;
  const isDragging = dnd && dnd.dragId === node.space_id;
  const dropPos = dnd && dnd.dropId === node.space_id ? dnd.dropPos : '';
  return (
    <div className="sp-tree-node">
      <div
        className={`sp-tree-row${selectedId === node.space_id ? ' selected' : ''}${isDragging ? ' dragging' : ''}${dropPos ? ` drop-${dropPos}` : ''}`}
        style={{ paddingLeft: 8 + depth * 16 }}
        draggable
        onDragStart={(e) => dnd && dnd.onDragStart(e, node.space_id)}
        onDragOver={(e) => dnd && dnd.onDragOver(e, node)}
        onDragLeave={() => dnd && dnd.onDragLeave(node.space_id)}
        onDrop={(e) => dnd && dnd.onDrop(e, node)}
        onDragEnd={() => dnd && dnd.onDragEnd()}
      >
        <button
          type="button"
          className="sp-caret"
          aria-label={isOpen ? '折叠' : '展开'}
          disabled={!hasChildren}
          onClick={() => onToggle(node.space_id)}
        >
          {hasChildren ? (isOpen ? '▾' : '▸') : '·'}
        </button>
        <button type="button" className="sp-tree-name" onClick={() => onSelect(node.space_id)}>
          {node.name}
        </button>
        <span className="sp-tree-badges">
          <Badge tone={TYPE_TONE[node.space_type] || 'default'}>{TYPE_LABEL[node.space_type] || node.space_type}</Badge>
          {node.inheritance_mode && node.inheritance_mode !== 'inherit' && (
            <Badge tone="warn">{inheritLabel(node.inheritance_mode)}</Badge>
          )}
        </span>
        <span className="sp-tree-actions">
          {actions.map((k) => (
            <button
              key={k}
              type="button"
              className="sp-node-btn"
              title={{
                sub: '新建子目录',
                perm: '成员与权限',
                rename: '重命名',
                move: '移动',
                del: '删除',
              }[k]}
              onClick={() => onAction(k, node)}
            >
              {{ sub: '＋', perm: '👥', rename: '✎', move: '⇄', del: '🗑' }[k]}
            </button>
          ))}
        </span>
      </div>
      {isOpen && (
        <>
          {/* 空间内文档名清单（树内展示，50 篇/页） */}
          {docsEntry && docsEntry.items.length > 0 && (
            <div className="sp-doc-list" style={{ marginLeft: 26 + depth * 16 }}>
              {docsEntry.items.map((d) => (
                <div
                  key={d.doc_id}
                  className="sp-doc-row"
                  title={`${d.source || d.doc_id} · ${d.chunks || 0} 块 / ${d.entities || 0} 实体`}
                  onClick={() => onSelect(node.space_id)}
                >
                  <span className="sp-doc-icon">📄</span>
                  <span className="sp-doc-name ellipsis">{d.source || d.doc_id}</span>
                </div>
              ))}
              {docsEntry.items.length >= docsEntry.page * PAGE && (
                <button
                  type="button"
                  className="sp-doc-more"
                  disabled={docsEntry.loading}
                  onClick={() => onLoadDocs(node.space_id, docsEntry.page + 1)}
                >
                  {docsEntry.loading ? '加载中…' : `加载下一页（第 ${docsEntry.page + 1} 页 · 已载 ${docsEntry.items.length} 篇）`}
                </button>
              )}
            </div>
          )}
          {docsEntry && docsEntry.items.length === 0 && !docsEntry.loading && node.children.length === 0 && (
            <div className="sp-doc-empty" style={{ marginLeft: 26 + depth * 16 }}>（空间内暂无文档）</div>
          )}
          {node.children.map((c) => (
            <TreeNode
              key={c.space_id}
              node={c}
              depth={depth + 1}
              selectedId={selectedId}
              expanded={expanded}
              onToggle={onToggle}
              onSelect={onSelect}
              onAction={onAction}
              hasChildren={c.children.length > 0}
              treeDocs={treeDocs}
              onLoadDocs={onLoadDocs}
              dnd={dnd}
            />
          ))}
        </>
      )}
    </div>
  );
}

export default function Spaces({ onChanged }) {
  const [spaces, setSpaces] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [selectedId, setSelectedId] = useState('');
  const [expanded, setExpanded] = useState(new Set());

  const [users, setUsers] = useState([]); // 授权下拉用的成员名册

  const [createOpen, setCreateOpen] = useState(false);
  const [createParent, setCreateParent] = useState('');
  const [permSpace, setPermSpace] = useState(null); // 权限抽屉的目标空间
  const [renameSpace, setRenameSpace] = useState(null);
  const [moveSpace, setMoveSpace] = useState(null);
  const [confirmDel, setConfirmDel] = useState(null);
  const [busy, setBusy] = useState(false);

  const [docs, setDocs] = useState([]);
  const [docsLoading, setDocsLoading] = useState(false);
  // 文档清单版本号：移动/移入文档后主动重拉当前空间的文档列表
  const [docsVersion, setDocsVersion] = useState(0);
  // 空间树内文档名清单：{ [space_id]: { items, page, loading } }，50 篇/页按需拉取
  const [treeDocs, setTreeDocs] = useState({});

  // 新增文档弹层：'' 关闭 | choose 选接入方式 | upload 自定义上传 | url 在线 URL 导入
  const [addOpen, setAddOpen] = useState('');
  // 文档内容弹层的目标文档
  const [detailDoc, setDetailDoc] = useState(null);

  const load = useCallback(async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      const [s, u] = await Promise.all([v2.spaces(), admin.listUsers({ limit: 200 }).catch(() => ({ users: [] }))]);
      const list = s?.spaces || [];
      setSpaces(list);
      setUsers(u?.users || []);
      if (!silent && !selectedId && list.length) {
        setSelectedId(list[0].space_id);
        setExpanded(new Set(list.map((x) => x.space_id)));
      }
      setError('');
    } catch (err) {
      setError(err.message);
    } finally {
      if (!silent) setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const byId = useMemo(
    () => new Map(spaces.map((s) => [s.space_id, s])),
    [spaces],
  );
  const tree = useMemo(() => buildTree(spaces), [spaces]);
  const selected = byId.get(selectedId) || null;
  const trail = useMemo(
    () => (selectedId ? ancestorsOf(selectedId, byId) : []),
    [selectedId, byId],
  );

  // 选中空间的文档清单（后端按 space_id 精确过滤）
  useEffect(() => {
    if (!selectedId) {
      setDocs([]);
      return;
    }
    setDocsLoading(true);
    api.docs({ space_id: selectedId, limit: 200 })
      .then((r) => setDocs(r?.docs || []))
      .catch((err) => setError(err.message))
      .finally(() => setDocsLoading(false));
  }, [selectedId, docsVersion]);

  const toggle = (id) => {
    const wasOpen = expanded.has(id);
    setExpanded((prev) => {
      const next = new Set(prev);
      if (wasOpen) next.delete(id);
      else next.add(id);
      return next;
    });
    // 首次展开该节点时拉取树内文档名（50 篇/页）
    if (!wasOpen && !treeDocs[id]) loadNodeDocs(id, 1);
  };

  // loadNodeDocs 树内文档名分页拉取：page 页对应 limit = 50 * page（累积重取，避免后端 offset 改造）。
  const loadNodeDocs = (nodeId, page) => {
    const TREE_PAGE = 50;
    setTreeDocs((prev) => ({
      ...prev,
      [nodeId]: { items: prev[nodeId]?.items || [], page: prev[nodeId]?.page || page, loading: true },
    }));
    api.docs({ space_id: nodeId, limit: TREE_PAGE * page })
      .then((r) => {
        const items = r?.docs || [];
        setTreeDocs((prev) => ({ ...prev, [nodeId]: { items, page, loading: false } }));
      })
      .catch(() => {
        setTreeDocs((prev) => ({
          ...prev,
          [nodeId]: { items: prev[nodeId]?.items || [], page: prev[nodeId]?.page || 1, loading: false },
        }));
      });
  };

  // 文档移动/删除后，已加载过树内文档的节点按当前页数重拉
  useEffect(() => {
    Object.entries(treeDocs).forEach(([id, e]) => {
      if (e?.page && expanded.has(id)) loadNodeDocs(id, e.page);
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [docsVersion]);

  // findSiblingIds 在树中找到 node 所在的兄弟数组，返回其 space_id 顺序
  const findSiblingIds = (nodes, id) => {
    for (const n of nodes) {
      if (n.space_id === id) return nodes.map((x) => x.space_id);
      const hit = findSiblingIds(n.children, id);
      if (hit) return hit;
    }
    return null;
  };

  // —— 拖拽排序（HTML5 DnD）：仅同父兄弟间排序，插入位置由松手时相对行高决定 ——
  const [dragId, setDragId] = useState('');
  const [dropId, setDropId] = useState('');
  const [dropPos, setDropPos] = useState(''); // before | after

  const applyReorder = async (orderedIds, movedName) => {
    try {
      await v2.reorderSpaces(orderedIds);
      await refresh(`已调整「${movedName}」的排序`);
    } catch (err) {
      setError(err.message);
    }
  };

  const dnd = useMemo(() => ({
    dragId,
    dropId,
    dropPos,
    onDragStart: (e, id) => {
      setDragId(id);
      e.dataTransfer.effectAllowed = 'move';
      try { e.dataTransfer.setData('text/plain', id); } catch { /* IE 兼容忽略 */ }
    },
    onDragOver: (e, target) => {
      if (!dragId || target.space_id === dragId) return;
      const drag = byId[dragId];
      if (!drag || drag.parent_space_id !== target.parent_space_id) return; // 只允许兄弟间排序
      e.preventDefault(); // 允许 drop
      e.dataTransfer.dropEffect = 'move';
      const rect = e.currentTarget.getBoundingClientRect();
      const pos = e.clientY < rect.top + rect.height / 2 ? 'before' : 'after';
      setDropId(target.space_id);
      setDropPos(pos);
    },
    onDragLeave: (id) => {
      setDropId((cur) => (cur === id ? '' : cur));
    },
    onDrop: (e, target) => {
      e.preventDefault();
      const src = dragId;
      const pos = dropPos;
      setDragId(''); setDropId(''); setDropPos('');
      if (!src || target.space_id === src || !pos) return;
      const drag = byId[src];
      if (!drag || drag.parent_space_id !== target.parent_space_id) return;
      const ids = findSiblingIds(tree, src);
      if (!ids) return;
      const next = ids.filter((x) => x !== src);
      const insertAt = next.indexOf(target.space_id) + (pos === 'after' ? 1 : 0);
      next.splice(insertAt, 0, src);
      if (next.join('|') === ids.join('|')) return; // 顺序没变不落库
      applyReorder(next, byId[src]?.name || src);
    },
    onDragEnd: () => {
      setDragId(''); setDropId(''); setDropPos('');
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }), [dragId, dropId, dropPos, byId, tree]);

  const onNodeAction = (kind, node) => {
    if (kind === 'sub') {
      setCreateParent(node.space_id);
      setCreateOpen(true);
    } else if (kind === 'perm') {
      setPermSpace(node);
    } else if (kind === 'rename') {
      setRenameSpace(node);
    } else if (kind === 'move') {
      setMoveSpace(node);
    } else if (kind === 'del') {
      setConfirmDel(node);
    }
  };

  const refresh = async (msg) => {
    await load(true);
    setDocsVersion((v) => v + 1); // 文档移动/移入后同步刷新当前空间文档列表
    if (msg) setNotice(msg);
    if (onChanged) onChanged();
  };

  const removeSpace = async () => {
    if (!confirmDel) return;
    setBusy(true);
    try {
      await v2.deleteSpace(confirmDel.space_id);
      setConfirmDel(null);
      if (selectedId === confirmDel.space_id) setSelectedId('');
      await refresh(`已删除空间「${confirmDel.name}」`);
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="sp-layout">
      <Panel
        title="空间目录"
        className="sp-left"
        actions={
          <>
            <button className="btn ghost sm" type="button" onClick={() => load()} disabled={loading}>
              <IconRefresh size={14} />
            </button>
            <button
              className="btn primary sm"
              type="button"
              onClick={() => { setCreateParent(''); setCreateOpen(true); }}
            >
              ＋ 新建工作空间
            </button>
          </>
        }
      >
        {loading ? (
          <Spinner text="加载空间…" />
        ) : tree.length === 0 ? (
          <EmptyState
            icon={IconDoc}
            title="还没有空间"
            desc="建一个工作空间开始组织知识；建完可以在里面继续建子目录"
          />
        ) : (
          <div className="sp-tree">
            {tree.map((n) => (
              <TreeNode
                key={n.space_id}
                node={n}
                depth={0}
                selectedId={selectedId}
                expanded={expanded}
                onToggle={toggle}
                onSelect={(id) => setSelectedId(id)}
                onAction={onNodeAction}
                hasChildren={n.children.length > 0}
                treeDocs={treeDocs}
                onLoadDocs={loadNodeDocs}
                dnd={dnd}
              />
            ))}
          </div>
        )}
      </Panel>

      <div className="sp-right stack">
        {error && <Alert type="error">{error}</Alert>}
        {notice && <Alert type="success">{notice}</Alert>}

        {!selected ? (
          <Panel>
            <EmptyState
              icon={IconSearch}
              title="选择一个空间"
              desc="左侧选空间后，这里展示该空间的文档清单与成员权限（Confluence 式权限页）"
            />
          </Panel>
        ) : (
          <>
            <Panel
              className="sp-head-tight"
              title={selected.name}
              actions={
                <>
                  <button className="btn primary sm" type="button" onClick={() => setAddOpen('choose')}>
                    ＋ 新增文档
                  </button>
                  <button className="btn ghost sm" type="button" onClick={() => { setCreateParent(selectedId); setCreateOpen(true); }}>
                    ＋ 新建子目录
                  </button>
                </>
              }
            >
              <div className="sp-headline">
                <Badge tone={TYPE_TONE[selected.space_type] || 'default'}>
                  {TYPE_LABEL[selected.space_type] || selected.space_type}
                </Badge>
                <Badge tone="warn">继承：{inheritLabel(selected.inheritance_mode)}</Badge>
                {selected.description && <span className="muted" title={selected.description}>{selected.description}</span>}
                <span className="muted mono">{selected.space_id}</span>
                {trail.length > 0 && (
                  <span className="sp-breadcrumb">
                    {trail.map((a) => (
                      <button key={a.space_id} type="button" className="sp-crumb" onClick={() => setSelectedId(a.space_id)}>
                        {a.name}
                      </button>
                    ))}
                    <span className="sp-crumb current">{selected.name}</span>
                  </span>
                )}
              </div>
            </Panel>

            <Panel
              title={`空间内文档（${docs.length}）`}
              desc="点击行查看文档原文分块；文档挂在本节点下，空间成员按授权可见"
            >
              <SpaceDocs
                docs={docs}
                loading={docsLoading}
                spaceId={selectedId}
                tree={tree}
                onMoved={refresh}
                onError={setError}
                onOpenDoc={setDetailDoc}
              />
            </Panel>
          </>
        )}
      </div>

      {/* —— 新建空间（工作空间/子目录） —— */}
      {createOpen && (
        <CreateSpaceDrawer
          parent={createParent ? byId.get(createParent) : null}
          onClose={() => setCreateOpen(false)}
          onCreated={async (s) => {
            setCreateOpen(false);
            setSelectedId(s?.space_id || selectedId);
            if (s?.space_id) setExpanded((prev) => new Set([...prev, createParent, s.space_id].filter(Boolean)));
            await refresh(`已创建空间「${s?.name || ''}」`);
          }}
        />
      )}

      {/* —— 成员与权限弹层（空间树行级 👥 按钮打开） —— */}
      {permSpace && (
        <MembersDrawer
          space={permSpace}
          users={users}
          onClose={() => setPermSpace(null)}
          onDone={refresh}
          onError={setError}
        />
      )}

      {/* —— 重命名 / 编辑 —— */}
      {renameSpace && (
        <RenameSpaceDrawer
          space={renameSpace}
          onClose={() => setRenameSpace(null)}
          onSaved={async () => { setRenameSpace(null); await refresh(); }}
        />
      )}

      {/* —— 移动空间 —— */}
      {moveSpace && (
        <FormDrawer
          title={`移动空间 · ${moveSpace.name}`}
          desc="选择新的父空间；子树整体平移（物化路径自动更新），不能挪进自己的子树"
          onClose={() => setMoveSpace(null)}
          width="min(520px, 94vw)"
        >
          <MoveSpaceDrawer
            space={moveSpace}
            spaces={spaces}
            onClose={() => setMoveSpace(null)}
            onMoved={async () => { setMoveSpace(null); await refresh('空间已移动'); }}
          />
        </FormDrawer>
      )}

      {/* —— 新增文档：选方式 / 自定义上传 / 在线 URL 导入 —— */}
      {addOpen && selected && (
        <AddDocDrawer
          space={selected}
          step={addOpen}
          onStep={setAddOpen}
          onClose={() => setAddOpen('')}
          onDone={async (msg) => {
            setAddOpen('');
            await refresh(msg || '文档已入库');
          }}
          onError={setError}
        />
      )}

      {/* —— 文档内容弹层 —— */}
      {detailDoc && (
        <DocContentDrawer
          doc={detailDoc}
          spaceName={selected?.name || ''}
          onClose={() => setDetailDoc(null)}
        />
      )}

      {/* —— 删除确认 —— */}
      {confirmDel && (
        <FormDrawer
          title={`删除空间 · ${confirmDel.name}`}
          eyebrow="危险操作"
          onClose={() => setConfirmDel(null)}
          width="min(480px, 94vw)"
        >
          <div className="stack">
            <Alert type="error">
              <IconAlert size={15} /> 将删除空间「{confirmDel.name}」（{confirmDel.space_id}）。
              {confirmDel.children?.length ? '' : '其下的文档元数据归属会悬空，检索将按作用域兜底。'}
            </Alert>
            <div className="drawer-danger" style={{ marginTop: 'auto' }}>
              <span className="muted">删除不可撤销</span>
              <div className="drawer-danger-actions">
                <button className="btn ghost sm" type="button" onClick={() => setConfirmDel(null)} disabled={busy}>取消</button>
                <button className="btn danger sm" type="button" onClick={removeSpace} disabled={busy}>
                  {busy ? <Spinner text="删除中" /> : '确认删除'}
                </button>
              </div>
            </div>
          </div>
        </FormDrawer>
      )}
    </div>
  );
}

// SpaceShares 权限页内嵌的授权清单（只展示 + 撤销；添加走 ShareEditor，version 变化自动重拉）。
function SpaceShares({ space, users, version = 0, onDone, onError }) {
  const [shares, setShares] = useState([]);
  const [loading, setLoading] = useState(true);
  const [revoking, setRevoking] = useState(0);

  const load = useCallback(() => {
    setLoading(true);
    v2.spaceShares(space.space_id)
      .then((r) => setShares(r?.shares || []))
      .catch((err) => onError(err.message))
      .finally(() => setLoading(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [space.space_id]);

  useEffect(() => {
    load();
  }, [load, version]);

  const revoke = async (id) => {
    setRevoking(id);
    try {
      await v2.revokeSpace(id);
      load();
      onDone();
    } catch (err) {
      onError(err.message);
    } finally {
      setRevoking(0);
    }
  };

  if (loading) return <Spinner text="加载授权…" />;
  if (!shares.length) {
    return (
      <EmptyState
        icon={IconUser}
        title="还没有显式授权"
        desc="空间 owner / 管理员天然有权限；其他用户需要在「添加授权」里显式授权后才能看见这个空间"
      />
    );
  }
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            <th>主体</th>
            <th>动作</th>
            <th className="num">操作</th>
          </tr>
        </thead>
        <tbody>
          {shares.map((sh) => (
            <tr key={sh.share_id ?? sh.id}>
              <td>
                <Badge tone="default">{PRINCIPAL_LABEL[sh.principal_type] || sh.principal_type}</Badge>{' '}
                <span className="mono">{sh.principal_id}</span>
              </td>
              <td>
                <span className="chips" style={{ gap: 6 }}>
                  {(sh.actions || []).map((a) => (
                    <Badge key={a} tone={ACTION_TONE[a] || 'default'}>{a}</Badge>
                  ))}
                </span>
              </td>
              <td className="num">
                <button className="btn ghost sm" type="button" onClick={() => revoke(sh.share_id ?? sh.id)} disabled={revoking === (sh.share_id ?? sh.id)}>
                  {revoking === (sh.share_id ?? sh.id) ? <Spinner text="撤销中" /> : <><IconTrash size={13} /> 撤销</>}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ShareEditor 授权表单（抽屉内）：主体类型 + 主体 ID（用户走名册下拉）+ 动作勾选。
function ShareEditor({ space, users, onDone, onError }) {
  const [ptype, setPtype] = useState('user');
  const [pid, setPid] = useState('');
  const [actions, setActions] = useState(['read']);
  const [busy, setBusy] = useState(false);

  const toggleAction = (a) => {
    setActions((prev) => (prev.includes(a) ? prev.filter((x) => x !== a) : [...prev, a]));
  };

  const submit = async () => {
    setBusy(true);
    try {
      await v2.grantSpace(space.space_id, { principal_type: ptype, principal_id: pid, actions });
      onDone();
    } catch (err) {
      onError(err.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="stack">
      <div className="form-grid">
        <label className="field">
          <span>主体类型</span>
          <select className="input" value={ptype} onChange={(e) => { setPtype(e.target.value); setPid(''); }}>
            <option value="user">用户</option>
            <option value="department">部门</option>
            <option value="tenant">租户</option>
          </select>
        </label>
        <label className="field">
          <span>{ptype === 'user' ? '用户（成员名册）' : ptype === 'department' ? '部门 ID' : '租户 ID'}</span>
          {ptype === 'user' ? (
            <select className="input" value={pid} onChange={(e) => setPid(e.target.value)}>
              <option value="">选择用户…</option>
              {users.map((u) => (
                <option key={u.username} value={u.username}>
                  {u.display_name ? `${u.display_name}（${u.username}）` : u.username}
                </option>
              ))}
            </select>
          ) : (
            <input className="input" value={pid} onChange={(e) => setPid(e.target.value)} placeholder={ptype === 'department' ? '如 rd' : '如 acme'} />
          )}
        </label>
      </div>

      <div className="field">
        <span>动作</span>
        <div className="chips">
          {ACTIONS.map((a) => (
            <button
              key={a.value}
              type="button"
              className={`chip${actions.includes(a.value) ? ' active' : ''}`}
              title={a.hint}
              onClick={() => toggleAction(a.value)}
            >
              {a.label}
            </button>
          ))}
        </div>
      </div>

      <div className="query-row end">
        <button className="btn primary" type="button" onClick={submit} disabled={busy || !pid || actions.length === 0}>
          {busy ? <Spinner text="授权中" /> : '确认授权'}
        </button>
      </div>
    </div>
  );
}

// MembersDrawer 成员与权限弹层：空间档案（类型/继承模式）+ 授权清单 + 添加授权，
// 一个抽屉里完成 Confluence 式空间权限页的全部操作。授权清单在添加后自动重拉（version）。
function MembersDrawer({ space, users, onClose, onDone, onError }) {
  const [version, setVersion] = useState(0);
  const mode = INHERIT_MODES.find((m) => m.value === (space.inheritance_mode || 'inherit'));

  return (
    <FormDrawer
      title={`成员与权限 · ${space.name}`}
      desc="空间权限页：查看显式授权、添加新授权、撤销授权；子空间按继承模式生效"
      onClose={onClose}
      width="min(720px, 94vw)"
    >
      <div className="stack">
        {/* 空间档案 */}
        <div className="detail-meta">
          <Badge tone={TYPE_TONE[space.space_type] || 'default'}>
            {TYPE_LABEL[space.space_type] || space.space_type}
          </Badge>
          <Badge tone="warn">继承模式：{inheritLabel(space.inheritance_mode)}</Badge>
          <span className="muted mono ellipsis" title={space.space_id}>{space.space_id}</span>
        </div>
        <p className="muted" style={{ marginTop: -6 }}>
          {mode?.hint}；{space.inheritance_mode === 'restricted'
            ? '本空间只认下方显式授权。'
            : space.inheritance_mode === 'isolated'
              ? '授权只对本空间生效，与父/兄弟空间互不影响。'
              : '子目录会继承本页的授权。'}
        </p>

        <div className="perm-sec-title">授权清单</div>
        <SpaceShares
          space={space}
          users={users}
          version={version}
          onDone={onDone}
          onError={onError}
        />

        <div className="perm-sec-title">添加授权</div>
        <ShareEditor
          space={space}
          users={users}
          onDone={() => {
            setVersion((v) => v + 1); // 清单自动重拉
            if (onDone) onDone();
          }}
          onError={onError}
        />
      </div>
    </FormDrawer>
  );
}

// SpaceDocs 空间内文档清单（分页 + 点击行看原文）+ 行级「移动 / 同步索引 / 删除」。
function SpaceDocs({ docs, loading, spaceId, tree, onMoved, onError, onOpenDoc }) {
  // 并行同步：同一批文档可同时补图谱/补向量（后端按 doc 各自处理，无全局锁）
  const [syncingIds, setSyncingIds] = useState(() => new Set());
  // 行级移动：待移出的文档（null 关闭抽屉）
  const [moveOutDoc, setMoveOutDoc] = useState(null);
  // 行级删除：待删除的文档（null 关闭确认抽屉）
  const [confirmDelDoc, setConfirmDelDoc] = useState(null);
  const [deletingDoc, setDeletingDoc] = useState(false);

  const pg = usePagedList(docs, { size: 8, resetKey: `${spaceId}|${docs.length}` });

  // syncIndex 补建文档缺失的索引：仅向量 → 补图谱；仅图谱 → 补向量。
  // 成功后文档升级为双索引，刷新列表即可看到「79 块 + N 实体」两个徽标都点亮。
  const syncIndex = async (d) => {
    setSyncingIds((prev) => {
      const next = new Set(prev);
      next.add(d.doc_id);
      return next;
    });
    try {
      const res = await v2.syncDocIndex(d.doc_id);
      const msg = (d.mode === 'graph')
        ? `已补建向量索引（分块 ${res?.chunks ?? 0}），文档已升级双索引`
        : `已补建图谱索引（实体 ${res?.entities ?? 0} / 关系 ${res?.relations ?? 0}），文档已升级双索引`;
      onMoved(msg);
    } catch (err) {
      onError(err.message);
    } finally {
      setSyncingIds((prev) => {
        const next = new Set(prev);
        next.delete(d.doc_id);
        return next;
      });
    }
  };

  // removeDoc 删除文档：向量分块 + 图谱节点 + 元数据一并清掉（后端 rag.DeleteDoc）。
  // 成功后刷新当前空间文档列表与全局统计。
  const removeDoc = async () => {
    if (!confirmDelDoc) return;
    setDeletingDoc(true);
    try {
      await api.deleteDoc(confirmDelDoc.doc_id);
      setConfirmDelDoc(null);
      onMoved(`已删除文档「${confirmDelDoc.source || confirmDelDoc.doc_id}」`);
    } catch (err) {
      onError(err.message);
    } finally {
      setDeletingDoc(false);
    }
  };

  return (
    <div className="stack">
      {loading ? (
        <Spinner text="加载文档…" />
      ) : docs.length === 0 ? (
        <EmptyState
          icon={IconDoc}
          title="空间里还没有文档"
          desc="点右上角「新增文档」接入第一篇，或把已有文档移进来"
        />
      ) : (
        <>
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>文档</th>
                  <th>索引</th>
                  <th>更新</th>
                  <th>状态</th>
                  <th className="num">操作</th>
                </tr>
              </thead>
              <tbody>
                {pg.paged.map((d) => (
                  <tr key={d.doc_id} className="row-click" onClick={() => onOpenDoc?.(d)}>
                    <td>
                      <div className="doc-cell">
                        <span className="doc-name mono ellipsis" title={d.doc_id}>{d.source || d.doc_id}</span>
                        <span className="cell-sub ellipsis">{d.doc_id}</span>
                      </div>
                    </td>
                    <td>
                      <span className="idx-cell">
                        <span className={`idx-chip${d.mode !== 'graph' ? ' on' : ''}`}>
                          <IconSearch size={12} />
                          {d.mode !== 'graph' ? `${d.chunks || 0} 块` : '未建'}
                        </span>
                        <span className={`idx-chip graph${d.mode === 'graph' || d.mode === 'both' ? ' on' : ''}`}>
                          <IconGraph size={12} />
                          {d.mode === 'graph' || d.mode === 'both' ? `${d.entities || 0} 实体` : '未建'}
                        </span>
                      </span>
                    </td>
                    <td>
                      <span className="muted" title={d.updated_at}>
                        {d.updated_at ? new Date(d.updated_at).toLocaleDateString('zh-CN') : '—'}
                      </span>
                    </td>
                    <td>
                      {d.status === 'failed'
                        ? <Badge tone="error">失败</Badge>
                        : <Badge tone="ok">就绪</Badge>}
                    </td>
                    <td className="num" onClick={(e) => e.stopPropagation()}>
                      <span className="chips" style={{ gap: 6 }}>
                        {(d.mode || 'rag') !== 'both' && (
                          <button
                            className="btn ghost sm"
                            type="button"
                            title={(d.mode || 'rag') === 'graph'
                              ? '补建向量索引（同步为双索引）'
                              : '补建图谱索引（同步为双索引）'}
                            onClick={() => syncIndex(d)}
                            disabled={syncingIds.has(d.doc_id)}
                          >
                            {syncingIds.has(d.doc_id)
                              ? <Spinner text="同步中" />
                              : (d.mode || 'rag') === 'graph' ? '⧗ 补向量' : '⚡ 补图谱'}
                          </button>
                        )}
                        <button
                          className="btn ghost sm"
                          type="button"
                          title="移动到其它空间"
                          onClick={() => setMoveOutDoc(d)}
                        >
                          ⇄ 移动
                        </button>
                        <button
                          className="btn ghost sm"
                          type="button"
                          title="删除文档（向量分块 + 图谱 + 元数据）"
                          onClick={() => setConfirmDelDoc(d)}
                        >
                          <IconTrash size={13} /> 删除
                        </button>
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            page={pg.page}
            pages={pg.pages}
            total={pg.total}
            size={pg.size}
            onPage={pg.setPage}
            onSize={pg.setSize}
          />
        </>
      )}

      {/* —— 行级移动：选择目标空间目录 —— */}
      {moveOutDoc && (
        <FormDrawer
          title={`移动文档 · ${moveOutDoc.source || moveOutDoc.doc_id}`}
          desc="选择目标空间目录，确认后文档挂到该空间，按目标空间的授权可见"
          onClose={() => setMoveOutDoc(null)}
          width="min(500px, 94vw)"
        >
          <MoveDocDrawer
            doc={moveOutDoc}
            spaceId={spaceId}
            tree={tree}
            onClose={() => setMoveOutDoc(null)}
            onMoved={async () => {
              setMoveOutDoc(null);
              onMoved();
            }}
          />
        </FormDrawer>
      )}

      {/* —— 行级删除确认 —— */}
      {confirmDelDoc && (
        <FormDrawer
          title={`删除文档 · ${confirmDelDoc.source || confirmDelDoc.doc_id}`}
          eyebrow="危险操作"
          onClose={() => setConfirmDelDoc(null)}
          width="min(480px, 94vw)"
        >
          <div className="stack">
            <Alert type="error">
              <IconAlert size={15} /> 将删除文档「{confirmDelDoc.source || confirmDelDoc.doc_id}」：
              向量分块（{confirmDelDoc.mode !== 'graph' ? `${confirmDelDoc.chunks || 0} 块` : '未建'}）、
              图谱（{confirmDelDoc.mode === 'graph' || confirmDelDoc.mode === 'both' ? `${confirmDelDoc.entities || 0} 实体` : '未建'}）
              与元数据会一并清除。
            </Alert>
            <div className="drawer-danger" style={{ marginTop: 'auto' }}>
              <span className="muted">删除不可撤销</span>
              <div className="drawer-danger-actions">
                <button className="btn ghost sm" type="button" onClick={() => setConfirmDelDoc(null)} disabled={deletingDoc}>取消</button>
                <button className="btn danger sm" type="button" onClick={removeDoc} disabled={deletingDoc}>
                  {deletingDoc ? <Spinner text="删除中" /> : '确认删除'}
                </button>
              </div>
            </div>
          </div>
        </FormDrawer>
      )}
    </div>
  );
}

// MoveDocDrawer 把文档移动到其它空间（树形下拉选目标目录；排除当前空间）。
function MoveDocDrawer({ doc, spaceId, tree, onClose, onMoved }) {
  const options = useMemo(
    () => flattenTree(tree).filter((o) => o.space_id !== spaceId),
    [tree, spaceId],
  );
  const [target, setTarget] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const submit = async () => {
    if (!target) return;
    setBusy(true);
    setErr('');
    try {
      await v2.moveDoc(doc.doc_id, target);
      onMoved();
    } catch (e) {
      setErr(e.message);
      setBusy(false);
    }
  };

  return (
    <div className="stack">
      {err && <Alert type="error">{err}</Alert>}
      <label className="field">
        <span>目标空间目录 *</span>
        <select className="input" value={target} onChange={(e) => setTarget(e.target.value)} autoFocus>
          <option value="">选择空间目录…</option>
          {options.map((o) => (
            <option key={o.space_id} value={o.space_id}>
              {'\u00A0'.repeat(o.depth * 3)}{o.depth ? '└ ' : ''}{o.name}
            </option>
          ))}
        </select>
      </label>
      {options.length === 0 && (
        <p className="muted">当前可见的空间里没有其它可选目录，请先在左侧创建目标空间。</p>
      )}
      <p className="muted">
        移动需要同时拥有：该文档的写权限 + 目标空间的管理权限；文档索引（向量 / 图谱）不受影响。
      </p>
      <div className="query-row end">
        <button className="btn ghost" type="button" onClick={onClose} disabled={busy}>取消</button>
        <button className="btn primary" type="button" onClick={submit} disabled={busy || !target}>
          {busy ? <Spinner text="移动中" /> : '确认移动'}
        </button>
      </div>
    </div>
  );
}

// AddDocDrawer 新增文档弹层：先选接入方式（自定义上传 / 在线 URL 导入），再进对应表单。
function AddDocDrawer({ space, step, onStep, onClose, onDone, onError }) {
  // —— 在线 URL 导入表单状态 ——
  const [url, setUrl] = useState('');
  const [urlMode, setUrlMode] = useState('both');
  const [urlSource, setUrlSource] = useState('');
  const [urlBusy, setUrlBusy] = useState(false);
  const [urlErr, setUrlErr] = useState('');
  // 入库执行进度：progressId 绑定本次请求，入库期间轮询步骤条
  const [progressId, setProgressId] = useState('');
  const prog = useIngestProgress(progressId, urlBusy);

  const submitUrl = async () => {
    if (!url.trim()) {
      setUrlErr('请填写网页地址');
      return;
    }
    setUrlBusy(true);
    setUrlErr('');
    const pid = newProgressId();
    setProgressId(pid);
    try {
      const r = await v2.ingestUrl({ url: url.trim(), mode: urlMode, source: urlSource.trim(), spaceId: space.space_id, progressId: pid });
      if (r?.graph_error) {
        // 双索引部分成功：向量已就绪（文档降级为仅向量），仅图谱失败
        onDone(`已从 URL 导入文档 ${r?.doc_id || ''}（向量 ${r?.chunks ?? 0} 块就绪）。图谱抽取失败，可稍后在文档列表点「补图谱」。原因：${r.graph_error}`);
      } else {
        onDone(`已从 URL 导入文档 ${r?.doc_id || ''}（分块 ${r?.chunks ?? 0}，实体 ${r?.entities ?? 0}）`);
      }
    } catch (err) {
      setUrlErr(err.message);
    } finally {
      setUrlBusy(false);
    }
  };

  // URL 导入的执行步骤：按索引方式裁剪
  const urlSteps = [
    INGEST_STEPS.fetch,
    INGEST_STEPS.parse,
    INGEST_STEPS.clean,
    INGEST_STEPS.chunk,
    ...(urlMode !== 'graph' ? [INGEST_STEPS.embed, INGEST_STEPS.milvus] : []),
    ...(urlMode !== 'rag' ? [INGEST_STEPS.graph] : []),
    INGEST_STEPS.done,
  ];

  return (
    <FormDrawer
      title={step === 'choose' ? `新增文档 · ${space.name}` : step === 'upload' ? `自定义上传 · ${space.name}` : `在线 URL 导入 · ${space.name}`}
      desc={step === 'choose'
        ? '文档会挂到当前选中的空间，空间成员按授权可见'
        : step === 'upload'
          ? '粘贴文本或上传文件（PDF / Word / Excel / Markdown），建好索引即可被检索命中'
          : '填写网页地址，服务端会拉取正文、转为纯文本后入库'}
      eyebrow={step === 'choose' ? '选择接入方式' : undefined}
      onClose={onClose}
      width={step === 'choose' ? 'min(560px, 94vw)' : 'min(760px, 96vw)'}
    >
      {step === 'choose' && (
        <div className="stack">
          <button type="button" className="addcard" onClick={() => onStep('upload')}>
            <span className="addcard-icon"><IconUpload size={20} /></span>
            <span className="addcard-body">
              <b>自定义上传</b>
              <span className="muted">粘贴文本，或上传 PDF / Word / Excel / Markdown / 纯文本文件；可选双索引、向量或图谱</span>
            </span>
            <span className="addcard-arrow">›</span>
          </button>
          <button type="button" className="addcard" onClick={() => onStep('url')}>
            <span className="addcard-icon"><IconSpark size={20} /></span>
            <span className="addcard-body">
              <b>在线 URL 导入</b>
              <span className="muted">填一个网页地址，服务端抓取正文自动转纯文本入库，适合文档站 / 帮助中心页面</span>
            </span>
            <span className="addcard-arrow">›</span>
          </button>
        </div>
      )}

      {step === 'upload' && (
        <IngestForm
          initialMode="both"
          presetSpaceId={space.space_id}
          onDone={() => onDone('文档已入库，索引构建完成后即可被检索命中')}
        />
      )}

      {step === 'url' && (
        <div className="stack">
          {urlErr && <Alert type="error">{urlErr}</Alert>}
          <label className="field">
            <span className="field-label">网页地址 *</span>
            <input
              className="input"
              placeholder="https://example.com/docs/getting-started"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') submitUrl(); }}
              autoFocus
            />
          </label>
          <div className="grid-2">
            <label className="field">
              <span className="field-label">来源名称（留空取网页标题）</span>
              <input className="input" value={urlSource} onChange={(e) => setUrlSource(e.target.value)} placeholder="如：Milvus 官方文档" />
            </label>
            <label className="field">
              <span className="field-label">索引方式</span>
              <select className="input" value={urlMode} onChange={(e) => setUrlMode(e.target.value)}>
                <option value="rag">仅 RAG（只建向量索引，秒级）</option>
                <option value="graph">仅 GraphRAG（只建知识图谱，较慢）</option>
                <option value="both">双索引（向量 + 图谱都建，推荐）</option>
              </select>
            </label>
          </div>
          <p className="muted">
            出站地址受服务端出站策略校验：只支持 http/https，私网地址需在 EGRESS 白名单内；单页上限 10MB。
          </p>

          {/* —— 入库执行步骤条：真实阶段上报（抓取/解析/清洗/分块/向量化/写 Milvus/图谱） —— */}
          {(urlBusy || (prog && prog.done && prog.error)) && (
            <IngestSteps steps={urlSteps} prog={prog} />
          )}

          <div className="query-row end">
            <button className="btn ghost" type="button" onClick={onClose}>取消</button>
            <button className="btn primary" type="button" onClick={submitUrl} disabled={urlBusy || !url.trim()}>
              {urlBusy ? <Spinner text="抓取入库中…" /> : '抓取并入库'}
            </button>
          </div>
        </div>
      )}
    </FormDrawer>
  );
}

// DocContentDrawer 文档原文弹层：元数据 + 「向量分块 / 图谱实体关系」双 Tag 切换。
function DocContentDrawer({ doc, spaceName, onClose }) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [openIdx, setOpenIdx] = useState(0);
  // 内容 Tag：'rag' 向量分块 | 'graph' 图谱实体/关系；默认落在已建的那一侧
  const [tag, setTag] = useState(doc.mode === 'graph' ? 'graph' : 'rag');

  useEffect(() => {
    setLoading(true);
    setError('');
    api.docContent(doc.doc_id, 100)
      .then((r) => setData(r))
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, [doc.doc_id]);

  return (
    <FormDrawer
      title={doc.source || doc.doc_id}
      desc={`文档原文 · ${spaceName || '知识空间'}`}
      eyebrow={doc.status === 'failed' ? '索引失败' : undefined}
      onClose={onClose}
      width="min(720px, 94vw)"
    >
      <div className="stack">
        {error && <Alert type="error">{error}</Alert>}
        {loading && <Spinner text="读取文档原文…" />}

        {!loading && !error && (
          <>
            <div className="detail-meta">
              <Badge tone={doc.mode === 'both' ? 'indigo' : doc.mode === 'graph' ? 'violet' : 'teal'}>
                {doc.mode === 'both' ? '双索引' : doc.mode === 'graph' ? '仅图谱' : '仅向量'}
              </Badge>
              <span className="muted mono ellipsis" title={doc.doc_id}>{doc.doc_id}</span>
            </div>

            <div className="kv">
              <div><span>来源</span><b>{doc.source || '—'}</b></div>
              <div><span>字符数</span><b className="mono">{doc.chars ?? '—'}</b></div>
              <div><span>向量分块</span><b className="mono">{doc.mode === 'graph' ? '未建' : doc.chunks ?? 0}</b></div>
              <div><span>图谱实体 / 关系</span><b className="mono">{doc.mode === 'graph' || doc.mode === 'both' ? `${doc.entities || 0} / ${doc.relations || 0}` : '未建'}</b></div>
            </div>

            {(() => {
              const chunks = data?.items || [];
              const ents = data?.graph_entities || [];
              const rels = data?.graph_relations || [];
              const hasVector = doc.mode !== 'graph' && chunks.length > 0;
              const hasGraph = (doc.mode === 'graph' || doc.mode === 'both') && ents.length + rels.length > 0;
              // 双侧都空：旧仅图谱文档没有原文，或索引还没就绪
              if (!hasVector && !hasGraph) {
                return (
                  <EmptyState
                    icon={IconDoc}
                    title="没有可展示的索引内容"
                    desc={
                      doc.mode === 'graph'
                        ? '仅建图谱索引的旧文档不保留向量分块，抽取结果也未读取到实体；可删除后重新入库'
                        : '该文档尚未完成索引，稍等入库结束后再打开'
                    }
                  />
                );
              }
              return (
                <div className="stack">
                  {/* —— 向量分块 / 图谱 双 Tag 切换 —— */}
                  <div className="dcctabs" role="tablist" aria-label="索引内容切换">
                    {[
                      { key: 'rag', label: `向量分块（${chunks.length}）`, on: hasVector },
                      { key: 'graph', label: `图谱 实体/关系（${ents.length} / ${rels.length}）`, on: hasGraph },
                    ].map((t) => (
                      <button
                        key={t.key}
                        type="button"
                        role="tab"
                        aria-selected={tag === t.key}
                        className={`dcctab${tag === t.key ? ' active' : ''}`}
                        onClick={() => setTag(t.key)}
                      >
                        {t.label}
                        {!t.on && <em className="dcctab-miss">缺</em>}
                      </button>
                    ))}
                  </div>

                  {tag === 'rag' && (
                    hasVector ? (
                      <>
                        <div className="muted" style={{ fontSize: 12.5 }}>
                          原文分块 {chunks.length} 段（与检索命中同一份数据，点击展开）
                        </div>
                        {chunks.map((h) => (
                          <div key={`${h.doc_id}-${h.chunk_idx}`} className="docchunk">
                            <button
                              type="button"
                              className="docchunk-head"
                              onClick={() => setOpenIdx((v) => (v === h.chunk_idx ? -1 : h.chunk_idx))}
                            >
                              <span className="mono">#{h.chunk_idx}</span>
                              <span className="muted ellipsis">{(h.text || '').slice(0, 60)}</span>
                              <span className="docchunk-caret">{openIdx === h.chunk_idx ? '▾' : '▸'}</span>
                            </button>
                            {openIdx === h.chunk_idx && (
                              <pre className="docchunk-body">{h.text}</pre>
                            )}
                          </div>
                        ))}
                      </>
                    ) : (
                      <EmptyState
                        icon={IconSearch}
                        title="缺少向量分块"
                        desc="该文档未建向量索引（仅图谱模式不生成分块数据）；可在文档列表点「⧗ 补向量」补齐为双索引"
                      />
                    )
                  )}

                  {tag === 'graph' && (
                    hasGraph ? (
                      <>
                        <div className="muted" style={{ fontSize: 12.5 }}>
                          该文档贡献的实体 {ents.length} 个、关系 {rels.length} 条（GraphRAG 抽取结果）
                        </div>
                        <div className="graph-entities">
                          {ents.map((e) => (
                            <div key={e.name} className="graph-ent" title={e.description}>
                              <span className="graph-ent-name">{e.name}</span>
                              {e.type && <Badge tone="violet">{e.type}</Badge>}
                              <span className="muted mono">×{e.freq}</span>
                            </div>
                          ))}
                        </div>
                        {rels.length > 0 && (
                          <div className="stack" style={{ gap: 4 }}>
                            {rels.map((r, i) => (
                              <div key={`${r.source}-${r.relation}-${r.target}-${i}`} className="graph-rel" title={r.description}>
                                <b>{r.source}</b>
                                <span className="graph-rel-arrow">— {r.relation} →</span>
                                <b>{r.target}</b>
                              </div>
                            ))}
                          </div>
                        )}
                      </>
                    ) : (
                      <EmptyState
                        icon={IconGraph}
                        title="缺少图谱内容"
                        desc={
                          doc.mode === 'graph' || doc.mode === 'both'
                            ? '图谱抽取没有从该文档读出实体/关系（或读取失败）；可在文档列表重新「⚡ 补图谱」'
                            : '该文档未建图谱索引；可在文档列表点「⚡ 补图谱」补齐为双索引'
                        }
                      />
                    )
                  )}
                </div>
              );
            })()}
          </>
        )}
      </div>
    </FormDrawer>
  );
}

// CreateSpaceDrawer 新建工作空间 / 子目录。
function CreateSpaceDrawer({ parent, onClose, onCreated }) {
  const [name, setName] = useState('');
  const [desc, setDesc] = useState('');
  const [type, setType] = useState(parent ? 'custom' : 'custom');
  const [inherit, setInherit] = useState('inherit');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const submit = async () => {
    setBusy(true);
    setErr('');
    try {
      const s = await v2.createSpace({
        name,
        description: desc,
        space_type: type,
        inheritance_mode: inherit,
        ...(parent ? { parent_space_id: parent.space_id } : {}),
      });
      onCreated(s);
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <FormDrawer
      title={parent ? `新建子目录 · 挂在「${parent.name}」下` : '新建工作空间'}
      desc={parent ? '子目录继承父空间的授权（可在继承模式里改）' : '工作空间是知识组织的顶层单位，可以在里面继续建无限层级子目录'}
      onClose={onClose}
      width="min(520px, 94vw)"
    >
      <div className="stack">
        {err && <Alert type="error">{err}</Alert>}
        <label className="field">
          <span>名称 *</span>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="如：安全合规 / 产品文档" autoFocus />
        </label>
        <label className="field">
          <span>描述</span>
          <textarea className="textarea" rows={2} value={desc} onChange={(e) => setDesc(e.target.value)} />
        </label>
        <div className="form-grid">
          <label className="field">
            <span>类型</span>
            <select className="input" value={type} onChange={(e) => setType(e.target.value)}>
              {SPACE_TYPES.map((t) => (
                <option key={t.value} value={t.value}>{t.label}</option>
              ))}
            </select>
          </label>
          <label className="field">
            <span>继承模式</span>
            <select className="input" value={inherit} onChange={(e) => setInherit(e.target.value)}>
              {INHERIT_MODES.map((m) => (
                <option key={m.value} value={m.value}>{m.label}</option>
              ))}
            </select>
          </label>
        </div>
        <p className="muted">{INHERIT_MODES.find((m) => m.value === inherit)?.hint}</p>
        <div className="query-row end">
          <button className="btn ghost" type="button" onClick={onClose}>取消</button>
          <button className="btn primary" type="button" onClick={submit} disabled={busy || !name.trim()}>
            {busy ? <Spinner text="创建中" /> : '创建'}
          </button>
        </div>
      </div>
    </FormDrawer>
  );
}

// RenameSpaceDrawer 重命名 / 改描述 / 切继承模式。
function RenameSpaceDrawer({ space, onClose, onSaved }) {
  const [name, setName] = useState(space.name);
  const [desc, setDesc] = useState(space.description || '');
  const [inherit, setInherit] = useState(space.inheritance_mode || 'inherit');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const submit = async () => {
    setBusy(true);
    setErr('');
    try {
      await v2.updateSpace(space.space_id, { name, description: desc, inheritance_mode: inherit });
      onSaved();
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <FormDrawer
      title={`编辑空间 · ${space.name}`}
      onClose={onClose}
      width="min(520px, 94vw)"
    >
      <div className="stack">
        {err && <Alert type="error">{err}</Alert>}
        <label className="field">
          <span>名称</span>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="field">
          <span>描述</span>
          <textarea className="textarea" rows={2} value={desc} onChange={(e) => setDesc(e.target.value)} />
        </label>
        <label className="field">
          <span>继承模式</span>
          <select className="input" value={inherit} onChange={(e) => setInherit(e.target.value)}>
            {INHERIT_MODES.map((m) => (
              <option key={m.value} value={m.value}>{m.label} — {m.hint}</option>
            ))}
          </select>
        </label>
        <div className="query-row end">
          <button className="btn ghost" type="button" onClick={onClose}>取消</button>
          <button className="btn primary" type="button" onClick={submit} disabled={busy || !name.trim()}>
            {busy ? <Spinner text="保存中" /> : '保存'}
          </button>
        </div>
      </div>
    </FormDrawer>
  );
}

// MoveSpaceDrawer 移动空间（选新父空间；排除自身子树防环）。
function MoveSpaceDrawer({ space, spaces, onClose, onMoved }) {
  const forbidden = useMemo(() => new Set(descendantsOf(space)), [space]);
  const [target, setTarget] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const submit = async () => {
    setBusy(true);
    setErr('');
    try {
      await v2.updateSpace(space.space_id, { parent_space_id: target });
      onMoved();
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="stack">
      {err && <Alert type="error">{err}</Alert>}
      <label className="field">
        <span>新父空间（留空 = 移到顶层）</span>
        <select className="input" value={target} onChange={(e) => setTarget(e.target.value)}>
          <option value="">（顶层）</option>
          {spaces.filter((s) => !forbidden.has(s.space_id) && s.space_id !== space.parent_space_id).map((s) => (
            <option key={s.space_id} value={s.space_id}>{s.name}</option>
          ))}
        </select>
      </label>
      <div className="query-row end">
        <button className="btn ghost" type="button" onClick={onClose}>取消</button>
        <button className="btn primary" type="button" onClick={submit} disabled={busy}>
          {busy ? <Spinner text="移动中" /> : '移动'}
        </button>
      </div>
    </div>
  );
}
