#!/usr/bin/env python3
"""score_trace.py —— 一条命令给 Agent 执行链路打分（任务后置评审）。

流程：拉任务链路 → 生成评审材料（trace_digest）→ 提交给「Agent执行链路评分员」
      → 轮询任务 → 打印评分卡。

用法：
    python scripts/score_trace.py 42                 # 评一个任务
    python scripts/score_trace.py 42 43 44           # 评多个任务
    python scripts/score_trace.py --recent 5         # 评最近 5 个已完成任务
    python scripts/score_trace.py --recent 10 --failed-only   # 只评失败的
    python scripts/score_trace.py 42 --dry           # 只打印材料，不提交
    python scripts/score_trace.py 42 --agent agent-trace-scorer --timeout 300

前置：后端在跑（默认 http://127.0.0.1:8000），且已注册评分员 Agent。
"""

import argparse
import json
import re
import sys
import time
import urllib.request

sys.path.insert(0, __file__.rsplit("scripts", 1)[0] + "scripts")
from trace_digest import build_digest, get  # noqa: E402

# 评分卡模板：维度固定，便于跨任务对比与汇总
RUBRIC = """你是 Agent 执行链路评分员。下面是任务 #{tid} 的完整执行链路材料。请按 5 个维度评审，每项 0-20 分，总分 100：

1. 目标达成度：是否回答了用户问题；无法回答时是否明确说明原因与边界
2. 工具选择与参数合理性：工具选型是否匹配子目标、参数是否正确、是否无效重复
3. 故障处理与止损：工具连续失败时是否及时止损、是否如实报告、是否编造结果
4. 过程效率：轮次、冗余调用、无效重试
5. 输出可交付性：结论是否明确、是否标注不确定性、是否给出可执行的下一步

输出格式（严格遵守）：
【总分】xx/100
【分项】1) 目标达成度 xx/20 · 依据 [事件序号] …
2) …
【关键证据】引用时间线序号说明判定依据
【扣分项】…
【改进建议】1-2 条具体可执行的动作

=== 链路材料开始 ===
"""

TERMINAL = ("succeeded", "failed", "cancelled")


def post_json(base, path, payload):
    body = json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(base.rstrip("/") + path, data=body, headers={"Content-Type": "application/json"})
    return json.loads(urllib.request.urlopen(req, timeout=30).read())


def final_answer(base, tid):
    evs = get(base, f"/api/agentfactory/tasks/{tid}/events?limit=500").get("events") or []
    for e in evs:
        if e.get("type") != "final":
            continue
        p = e.get("payload") or {}
        if isinstance(p, str):
            try:
                p = json.loads(p)
            except Exception:
                p = {"answer": p}
        if p.get("answer"):
            return p["answer"]
    return ""


def score_one(base, agent, source_id, timeout, dry=False, limits=None):
    digest = build_digest(base, source_id, limits)
    if dry:
        print(digest)
        return None
    task = post_json(base, "/api/agentfactory/tasks", {"agent_key": agent, "input": RUBRIC.replace("{tid}", str(source_id)) + digest})
    tid = task.get("id")
    print(f"[任务 #{source_id}] 已提交评分任务 #{tid} …", flush=True)

    deadline = time.time() + timeout
    while time.time() < deadline:
        time.sleep(2)
        t = get(base, f"/api/agentfactory/tasks/{tid}")
        if t.get("status") in TERMINAL:
            answer = final_answer(base, tid)
            m = re.search(r"【总分】\s*(\d+)\s*/\s*100", answer or "")
            total = m.group(1) if m else "?"
            print(f"[任务 #{source_id}] 评分完成：{total}/100（评分任务 #{tid}，状态 {t.get('status')}）")
            return {"source": source_id, "task": tid, "status": t.get("status"), "total": total, "answer": answer}
    print(f"[任务 #{source_id}] 超时未完成，稍后可在任务中心查看 #{tid}")
    return {"source": source_id, "task": tid, "status": "timeout", "total": "?", "answer": ""}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("task_ids", nargs="*", type=int)
    ap.add_argument("--recent", type=int, default=0, help="改评最近 N 个任务")
    ap.add_argument("--failed-only", action="store_true")
    ap.add_argument("--agent", default="agent-trace-scorer")
    ap.add_argument("--base", default="http://127.0.0.1:8000")
    ap.add_argument("--timeout", type=int, default=240)
    ap.add_argument("--dry", action="store_true")
    ap.add_argument("--verbose", action="store_true", help="打印完整评分卡")
    args = ap.parse_args()

    ids = list(args.task_ids)
    if args.recent:
        tasks = get(args.base, f"/api/agentfactory/tasks?page_size={min(args.recent * 3, 200)}").get("tasks") or []
        tasks = [t for t in tasks if t.get("status") in TERMINAL]
        if args.failed_only:
            tasks = [t for t in tasks if t.get("status") == "failed"]
        ids = [t["id"] for t in tasks][: args.recent]
    if not ids:
        ap.error("请给出任务号，或用 --recent N")

    results = []
    for tid in ids:
        r = score_one(args.base, args.agent, tid, args.timeout, args.dry)
        if r:
            results.append(r)
            if args.verbose and r["answer"]:
                print("-" * 60)
                print(r["answer"])
                print("-" * 60)

    if len(results) > 1:
        print("\n汇总：")
        for r in results:
            print(f"  任务 #{r['source']} → {r['total']}/100（评分任务 #{r['task']}）")


if __name__ == "__main__":
    main()
