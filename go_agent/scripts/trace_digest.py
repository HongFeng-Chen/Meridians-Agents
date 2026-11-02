#!/usr/bin/env python3
"""trace_digest.py —— 把一次 Agent 任务执行链路导出为「评审材料」Markdown。

用途：Agent 执行链路评分员（agent-trace-scorer）没有工具能力，
需要把链路以文本形式喂进任务输入。本脚本直接读平台 API，生成紧凑、
结构化、可引用的时间线，避免人工复制粘贴。

用法：
    python scripts/trace_digest.py 42                 # 任务号
    python scripts/trace_digest.py 42 --input-limit 800 --answer-limit 1200
    python scripts/trace_digest.py 42 --base http://127.0.0.1:8000 --out /tmp/trace42.md

输出到 stdout，可重定向为文件；配合 submit_score.py 可直接提交评分任务。
"""

import argparse
import json
import sys
import urllib.request

# 事件类型 → 中文标签（与 internal/biz/agentfactory/types.go 的 Event* 常量对应）
LABELS = {
    "status": "状态",
    "thought": "思考",
    "tool_call": "工具调用",
    "tool_result": "工具结果",
    "delegate": "委派",
    "supervise": "监察",
    "risk_assess": "风险评估",
    "node": "编排节点",
    "plan": "计划",
    "evaluate": "自评",
    "reflect": "反思",
    "route": "路由",
    "final": "最终答案",
    "error": "错误",
}


def get(base, path):
    url = base.rstrip("/") + path
    with urllib.request.urlopen(url, timeout=15) as r:
        return json.loads(r.read().decode("utf-8"))


def clip(text, limit):
    text = "" if text is None else str(text)
    text = text.replace("\r\n", "\n").strip()
    if limit and len(text) > limit:
        return text[:limit] + f"…（截断，原 {len(text)} 字）"
    return text


def brief(value, limit=300):
    if isinstance(value, (dict, list)):
        s = json.dumps(value, ensure_ascii=False)
    else:
        s = "" if value is None else str(value)
    return clip(s.replace("\n", " "), limit)


def fmt_event(i, e, limits):
    t = e.get("type") or "?"
    p = e.get("payload") or {}
    if isinstance(p, str):
        try:
            p = json.loads(p)
        except Exception:
            p = {"raw": p}
    label = LABELS.get(t, t)
    turn = p.get("turn")
    tag = f"第{turn}轮 " if turn else ""

    if t == "status":
        return f"[{i}] {label}：{p.get('from') or '∅'} → {p.get('to') or '∅'}"
    if t == "thought":
        return f"[{i}] {label}（{tag.strip() or '—'}）：{clip(p.get('thought'), limits['thought'])}"
    if t == "tool_call":
        return f"[{i}] {label}（{tag.strip() or '—'}）{p.get('name')} 参数={brief(p.get('args'), limits['args'])}"
    if t == "tool_result":
        flag = "❌ 失败" if p.get("is_error") else "✅ 成功"
        return (
            f"[{i}] {label}（{tag.strip() or '—'}）{p.get('name')} {flag}"
            f" 耗时={p.get('elapsed_ms') or 0}ms：{clip(p.get('result'), limits['result'])}"
        )
    if t == "delegate":
        return f"[{i}] {label} → {p.get('agent')}（子任务 #{p.get('child_task_id')}）：{clip(p.get('input'), limits['thought'])}"
    if t == "supervise":
        return f"[{i}] {label} {p.get('supervisor')} 结论={'通过' if p.get('passed') else '不通过'}：{clip(p.get('issues'), limits['result'])}"
    if t == "risk_assess":
        return f"[{i}] {label} {p.get('tool')} 等级={p.get('level')} 放行={p.get('allow')}：{clip(p.get('reason'), limits['result'])}"
    if t == "node":
        return f"[{i}] {label} {p.get('node')}({p.get('kind')}) 状态={p.get('status')}：{clip(p.get('output'), limits['result'])}"
    if t == "plan":
        return f"[{i}] {label}：{brief(p.get('steps') or p.get('questions'), limits['result'])}"
    if t == "evaluate":
        return f"[{i}] {label} 第{p.get('round')}轮 通过={p.get('pass')} 分数={p.get('score')} 问题：{clip(p.get('issues'), limits['result'])}"
    if t == "reflect":
        return f"[{i}] {label} 第{p.get('round')}轮 教训：{clip(p.get('lesson'), limits['result'])}"
    if t == "route":
        return f"[{i}] {label} → {p.get('target')} 理由：{clip(p.get('reason'), limits['thought'])}"
    if t == "final":
        return f"[{i}] {label}：\n{clip(p.get('answer'), limits['answer'])}"
    if t == "error":
        return f"[{i}] {label}：{clip(p.get('message'), limits['result'])}"
    return f"[{i}] {label}：{brief(p, limits['result'])}"


def build_digest(base, task_id, limits=None, answer_limit=None):
    """拉取任务 + 事件，返回评审材料 Markdown 字符串（可被其它脚本复用）。"""
    limits = limits or {"thought": 300, "args": 240, "result": 400, "answer": 1500}
    task = get(base, f"/api/agentfactory/tasks/{task_id}")
    events = get(base, f"/api/agentfactory/tasks/{task_id}/events?limit=500").get("events") or []

    # 统计：工具调用数、失败数、委派数、轮次
    tool_calls = sum(1 for e in events if e.get("type") == "tool_call")
    tool_fails = 0
    for e in events:
        p = e.get("payload") or {}
        if isinstance(p, str):
            try:
                p = json.loads(p)
            except Exception:
                p = {}
        if e.get("type") == "tool_result" and p.get("is_error"):
            tool_fails += 1
    delegates = sum(1 for e in events if e.get("type") == "delegate")
    retries = sum(1 for e in events if e.get("type") == "evaluate")
    answer = ""
    for e in reversed(events):
        p = e.get("payload") or {}
        if isinstance(p, str):
            try:
                p = json.loads(p)
            except Exception:
                p = {}
        if e.get("type") == "final" and p.get("answer"):
            answer = p["answer"]
            break

    lines = [
        f"# 执行链路评审材料（任务 #{task_id}）",
        "",
        "## 一、任务元信息",
        f"- 任务号：#{task_id}",
        f"- Agent：{task.get('agent_key') or '—'}（{task.get('agent_name') or '—'}）",
        f"- 状态：{task.get('status') or '—'}",
        f"- 轮次：{task.get('turns') or task.get('turn') or '—'}",
        f"- 创建时间：{(task.get('created_at') or '').replace('T', ' ')[:19]}",
        f"- 结束时间：{(task.get('finished_at') or task.get('updated_at') or '').replace('T', ' ')[:19]}",
        f"- 事件数：{len(events)}（工具调用 {tool_calls} 次，失败 {tool_fails} 次，委派 {delegates} 次，自评/重试 {retries} 次）",
        "",
        "## 二、用户目标（任务输入）",
        "```text",
        clip(task.get("input"), 2000),
        "```",
        "",
        "## 三、链路时间线",
        "",
    ]
    for i, e in enumerate(events, 1):
        lines.append(fmt_event(i, e, limits))
        lines.append("")
    if answer:
        lines += ["## 四、最终答案", "```text", clip(answer, answer_limit or limits["answer"]), "```", ""]
    return "\n".join(lines).rstrip() + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("task_id", type=int)
    ap.add_argument("--base", default="http://127.0.0.1:8000")
    ap.add_argument("--thought-limit", type=int, default=300)
    ap.add_argument("--args-limit", type=int, default=240)
    ap.add_argument("--result-limit", type=int, default=400)
    ap.add_argument("--answer-limit", type=int, default=1500)
    ap.add_argument("--out", default="")
    args = ap.parse_args()

    out = build_digest(
        args.base,
        args.task_id,
        {
            "thought": args.thought_limit,
            "args": args.args_limit,
            "result": args.result_limit,
            "answer": args.answer_limit,
        },
        args.answer_limit,
    )
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(out)
        sys.stderr.write(f"written: {args.out} ({len(out)} chars)\n")
    else:
        sys.stdout.write(out)


if __name__ == "__main__":
    main()
