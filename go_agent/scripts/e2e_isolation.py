#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""会话 / 任务 / 知识库三层 user 隔离的端到端验证。

用法：python scripts/e2e_isolation.py [base_url]
前提：服务以 staticTokens={tok-alice: alice:t1, tok-bob: bob:t1} 启动（见 configs-isolation/）。
"""
import json
import sys
import urllib.error
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8011"
TOKENS = {"alice": "tok-alice", "bob": "tok-bob", "anon": None}

results = []


def call(user, method, path, body=None, raw=False):
    url = BASE + path
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    tok = TOKENS.get(user)
    if tok:
        headers["Authorization"] = "Bearer " + tok
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req) as resp:
            text = resp.read().decode("utf-8", "replace")
            status = resp.status
    except urllib.error.HTTPError as e:
        text = e.read().decode("utf-8", "replace")
        status = e.code
    except Exception as e:  # 连接失败等
        return 0, str(e)
    if raw:
        return status, text
    try:
        return status, json.loads(text) if text else {}
    except json.JSONDecodeError:
        return status, text


def check(name, ok, detail=""):
    results.append((ok, name, detail))
    print(("  PASS  " if ok else "  FAIL  ") + name + (("  → " + detail) if detail else ""))


def main():
    print("== 0. 认证基线")
    st, _ = call("anon", "GET", "/api/conversations")
    check("未带令牌访问会话列表被拒（401）", st == 401, "status=%d" % st)
    st, _ = call("alice", "GET", "/api/conversations")
    check("alice 带令牌通过认证（200）", st == 200, "status=%d" % st)

    print("\n== 1. 会话隔离")
    st, body = call("alice", "POST", "/api/conversations", {"title": "alice 的私有会话"})
    conv = (body or {}).get("conversation") or {}
    cid = conv.get("id")
    if not cid:
        check("alice 创建会话", False, "resp=%s" % str(body)[:200])
        return finish()
    check("alice 创建会话", True, "id=%s user_id=%s" % (cid, conv.get("user_id")))
    check("会话落库归属 alice", conv.get("user_id") == "alice", "user_id=%r" % conv.get("user_id"))

    st, body = call("bob", "GET", "/api/conversations")
    ids = [c.get("id") for c in (body or {}).get("conversations") or []]
    check("bob 的会话列表看不到 alice 的会话", cid not in ids, "bob 列表 %d 条" % len(ids))

    st, _ = call("bob", "GET", "/api/conversations/" + cid)
    check("bob 读 alice 会话详情被拒（403）", st == 403, "status=%d" % st)

    st, _ = call("bob", "POST", "/api/conversations/" + cid + "/chat", {"content": "hi"})
    check("bob 往 alice 会话发消息被拒（403）", st == 403, "status=%d" % st)

    st, _ = call("bob", "DELETE", "/api/conversations/" + cid)
    check("bob 删 alice 会话被拒（403）", st == 403, "status=%d" % st)

    st, body = call("alice", "GET", "/api/conversations/" + cid)
    check("alice 自己能读自己的会话（200）", st == 200, "status=%d" % st)

    print("\n== 2. 任务隔离")
    st, body = call("alice", "GET", "/api/agentfactory/agents")
    agents = (body or {}).get("agents") or []
    aid = None
    for a in agents:
        if a.get("id"):
            aid = a["id"]
            break
    if not aid:
        check("存在可用 Agent 用于提交任务", False, "跳过任务隔离验证")
    else:
        st, body = call("alice", "POST", "/api/agentfactory/tasks",
                        {"agent_def_id": aid, "input": "隔离验证任务：只回一句 ok"})
        task = body if isinstance(body, dict) else {}
        tid = task.get("id")
        if not tid:
            check("alice 提交任务", False, "resp=%s" % str(body)[:200])
        else:
            check("alice 提交任务", True, "id=%s user_id=%s" % (tid, task.get("user_id")))
            check("任务落库归属 alice", task.get("user_id") == "alice", "user_id=%r" % task.get("user_id"))

            st, body = call("bob", "GET", "/api/agentfactory/tasks?limit=200&root_only=true")
            tids = [t.get("id") for t in (body or {}).get("tasks") or []]
            check("bob 的任务列表看不到 alice 的任务", tid not in tids, "bob 列表 %d 条" % len(tids))

            st, _ = call("bob", "GET", "/api/agentfactory/tasks/%s" % tid)
            check("bob 读 alice 任务详情被拒（403）", st == 403, "status=%d" % st)

            st, _ = call("bob", "GET", "/api/agentfactory/tasks/%s/events" % tid)
            check("bob 读 alice 任务事件流被拒（403）", st == 403, "status=%d" % st)

            st, _ = call("bob", "POST", "/api/agentfactory/tasks/%s/cancel" % tid, {})
            check("bob 取消 alice 任务被拒（403）", st == 403, "status=%d" % st)

            st, _ = call("alice", "GET", "/api/agentfactory/tasks/%s" % tid)
            check("alice 自己能读自己的任务（200）", st == 200, "status=%d" % st)

    print("\n== 3. 知识库隔离")
    doc_id = "iso-e2e-alice"
    st, body = call("alice", "POST", "/api/ingest", {
        "doc_id": doc_id, "source": "alice-iso.txt", "mode": "rag",
        "text": "ALICE-SECRET-2026 是 alice 的私有知识片段，描述只有 alice 可见的隔离验证口令。" * 3,
    })
    if st == 200:
        check("alice 入库文档", True, "doc_id=%s" % doc_id)
        st, body = call("alice", "GET", "/api/admin/docs?limit=500")
        docs = [d.get("doc_id") for d in (body or {}).get("docs") or []]
        check("alice 文档列表包含自己的文档", doc_id in docs, "共 %d 条" % len(docs))
        st, body = call("bob", "GET", "/api/admin/docs?limit=500")
        docs_b = [d.get("doc_id") for d in (body or {}).get("docs") or []]
        check("bob 文档列表看不到 alice 的文档", doc_id not in docs_b, "bob 共 %d 条" % len(docs_b))

        st, _ = call("bob", "DELETE", "/api/admin/docs/" + doc_id)
        check("bob 删 alice 文档被拒（403/404）", st in (403, 404), "status=%d" % st)

        st, body = call("bob", "POST", "/api/query", {"question": "ALICE-SECRET-2026 是什么"})
        hits = (body or {}).get("citations") or []
        leaked = [h for h in hits if h.get("doc_id") == doc_id]
        check("bob 检索不到 alice 的文档内容", not leaked,
              "bob 命中 %d 条，其中 alice 文档 %d 条" % (len(hits), len(leaked)))

        st, body = call("alice", "POST", "/api/query", {"question": "ALICE-SECRET-2026 是什么"})
        hits = (body or {}).get("citations") or []
        check("alice 自己能检索到（隔离未过度拦截）", any(h.get("doc_id") == doc_id for h in hits),
              "alice 命中 %d 条" % len(hits))
    else:
        check("alice 入库文档", False, "status=%d（依赖 Milvus/DB，可能跳过）%s" % (st, str(body)[:160]))

    finish()


def finish():
    total = len(results)
    passed = sum(1 for ok, _, _ in results if ok)
    print("\n== 汇总：%d/%d 通过" % (passed, total))
    if passed != total:
        print("失败项：")
        for ok, name, detail in results:
            if not ok:
                print("  - " + name + ("  → " + detail if detail else ""))
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
