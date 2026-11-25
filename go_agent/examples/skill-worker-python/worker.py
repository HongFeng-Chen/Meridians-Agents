#!/usr/bin/env python3
"""Skill Worker 参考实现（零依赖，只用标准库）。

对应 Meridians-Agents 的 Skill Runtime 协议：

    POST /execute
    Content-Type: application/json
    Authorization: Bearer <token>        （设置了 SKILL_WORKER_TOKEN 才校验）

    请求 = SkillRequest：
      {
        "task_id": "42", "execution_id": "exec_ab12", "skill_name": "pdf_analysis",
        "skill_version": "1.0.0", "tenant_id": "public", "user_id": "u1",
        "agent_key": "doc-agent", "session_id": "c1",
        "idempotency_key": "42:pdf_analysis:9f2c", "timeout_ms": 60000,
        "trace_id": "...", "input": {"file_id": "f1"}
      }

    响应 = SkillResponse：
      {"success": true, "output": {...}}
      失败时 {"success": false, "error": "原因"}（HTTP 仍可 200，
      让平台区分「Worker 正常返回业务失败」和「Worker 本身挂了」）

启动：

    # 注册（一次性，平台地址与令牌按环境变量给）
    python worker.py --register --platform http://127.0.0.1:8000

    # 提供服务 + 每 15s 心跳
    python worker.py --port 9101

设计要点（与平台侧约定一致）：
  - 路由按 skill_name 分发，一个 Worker 可以承载多个技能；
  - 幂等键原样透传，业务侧可据此去重（平台已经做过一层回放，这里是双保险）；
  - 心跳上报 inflight，平台调度器据此做背压；
  - 响应不要返回文件本体，只返回可序列化的结果（大文件走对象存储给地址）。
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

WORKER_ID = os.getenv("SKILL_WORKER_ID") or f"{os.uname().nodename if hasattr(os, 'uname') else 'worker'}:{os.getenv('SKILL_WORKER_PORT', '9101')}"
TOKEN = os.getenv("SKILL_WORKER_TOKEN", "")
MAX_CONCURRENCY = int(os.getenv("SKILL_WORKER_CONCURRENCY", "4"))

_inflight_lock = threading.Lock()
_inflight = 0


def _inc(delta: int) -> int:
    global _inflight
    with _inflight_lock:
        _inflight = max(0, _inflight + delta)
        return _inflight


# —— 技能实现：新增能力就在这里注册 ——

def pdf_analysis(payload: dict) -> dict:
    """示例：假装解析 PDF，返回结构化结果。"""
    file_id = payload.get("file_id")
    if not file_id:
        raise ValueError("缺少 file_id")
    time.sleep(0.2)  # 模拟真实解析耗时
    return {
        "file_id": file_id,
        "analysis_type": payload.get("analysis_type", "summary"),
        "pages": 12,
        "sections": ["概述", "架构", "部署"],
        "summary": "文档描述了三个服务的部署方式与依赖关系。",
    }


def text_stats(payload: dict) -> dict:
    """示例：纯计算类技能，用来验证同步链路。"""
    text = payload.get("text", "")
    return {
        "chars": len(text),
        "lines": text.count("\n") + 1 if text else 0,
        "words": len(text.split()),
    }


# Handler 表：key 是「Worker 侧实现名」，平台在 ExecutionSpec.handler 里指定，
# 缺省等于平台技能名。改名不必改这里。
HANDLERS = {
    "pdf_analysis": pdf_analysis,
    "text_stats": text_stats,
}


class Handler(BaseHTTPRequestHandler):
    server_version = "SkillWorker/1.0"

    def log_message(self, fmt: str, *args) -> None:  # 简化日志
        sys.stderr.write("[worker] " + fmt % args + "\n")

    def _json(self, code: int, body: dict) -> None:
        raw = json.dumps(body, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(200, {"ok": True, "worker_id": WORKER_ID, "inflight": _inflight,
                             "handlers": sorted(HANDLERS)})
            return
        self._json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/execute":
            self._json(404, {"error": "not found"})
            return
        if TOKEN:
            auth = self.headers.get("Authorization", "")
            if auth != f"Bearer {TOKEN}":
                self._json(401, {"error": "unauthorized"})
                return
        length = int(self.headers.get("Content-Length", "0") or 0)
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError as exc:
            self._json(400, {"error": f"invalid json: {exc}"})
            return

        # 平台传的 skill_name 已是 handler 名（未配置 handler 时等于平台技能名）
        handler = req.get("skill_name", "")
        fn = HANDLERS.get(handler)
        if fn is None:
            # 未知技能：业务失败而不是 404——平台据此把错误原样回给 Agent
            self._json(200, {"success": False, "error": f"未实现的 handler: {handler}",
                             "execution_id": req.get("execution_id")})
            return

        _inc(1)
        started = time.time()
        try:
            output = fn(req.get("input") or {})
            self._json(200, {
                "task_id": req.get("task_id"),
                "execution_id": req.get("execution_id"),
                "success": True,
                "output": output,
                "worker_id": WORKER_ID,
                "latency_ms": int((time.time() - started) * 1000),
            })
        except Exception as exc:  # noqa: BLE001
            self._json(200, {
                "task_id": req.get("task_id"),
                "execution_id": req.get("execution_id"),
                "success": False,
                "error": str(exc),
                "worker_id": WORKER_ID,
            })
        finally:
            _inc(-1)


# —— 注册与心跳 ——

def _platform_call(platform: str, path: str, body: dict, method: str = "POST") -> dict:
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(platform.rstrip("/") + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if TOKEN:
        req.add_header("Authorization", f"Bearer {TOKEN}")
    with urllib.request.urlopen(req, timeout=10) as resp:
        raw = resp.read().decode("utf-8") or "{}"
        return json.loads(raw)


def parse_skills(raw: str) -> list[tuple[str, str]]:
    """解析 --skills：`平台技能名[:handler]`，逗号分隔。

    平台技能名（用户看到的能力标识）与 handler（Worker 代码里的实现名）分开：
    注册/心跳用平台技能名，/execute 收到的 skill_name 是 handler。
    """
    out: list[tuple[str, str]] = []
    for item in raw.split(","):
        item = item.strip()
        if not item:
            continue
        parts = item.split(":", 1)
        name = parts[0].strip()
        handler = parts[1].strip() if len(parts) > 1 and parts[1].strip() else name
        out.append((name, handler))
    return out


def register(platform: str, endpoint: str, skills: list[tuple[str, str]]) -> None:
    for skill, handler in skills:
        body = {
            "id": WORKER_ID,
            "endpoint": endpoint,
            "version": os.getenv("SKILL_WORKER_VERSION", "1.0.0"),
            "region": os.getenv("SKILL_WORKER_REGION", ""),
            "max_concurrency": MAX_CONCURRENCY,
        }
        try:
            out = _platform_call(platform, f"/api/skills/{skill}/instances", body)
            print(f"[register] {skill} (handler={handler}) -> {out.get('id')} @ {out.get('endpoint')}")
        except urllib.error.HTTPError as exc:
            print(f"[register] {skill} 失败: HTTP {exc.code} {exc.read()[:200]!r}")
        except Exception as exc:  # noqa: BLE001
            print(f"[register] {skill} 失败: {exc}")


def heartbeat_loop(platform: str, skills: list[tuple[str, str]], interval: int = 15) -> None:
    while True:
        for skill, _handler in skills:
            try:
                _platform_call(platform, f"/api/skills/{skill}/instances/{WORKER_ID}/heartbeat",
                               {"inflight": _inflight, "status": "online"})
            except Exception as exc:  # noqa: BLE001
                print(f"[heartbeat] {skill} 失败（会继续重试）: {exc}", file=sys.stderr)
        time.sleep(interval)


def main() -> None:
    ap = argparse.ArgumentParser(description="Meridians-Agents Skill Worker（参考实现）")
    ap.add_argument("--platform", default=os.getenv("PLATFORM_URL", "http://127.0.0.1:8000"),
                    help="平台地址，注册与心跳用")
    ap.add_argument("--port", type=int, default=int(os.getenv("SKILL_WORKER_PORT", "9101")))
    ap.add_argument("--host", default=os.getenv("SKILL_WORKER_HOST", "127.0.0.1"))
    ap.add_argument("--register", action="store_true", help="只注册后退出")
    ap.add_argument("--skills", default="pdf-analysis:pdf_analysis,text-stats:text_stats",
                    help="平台技能名[:handler]，逗号分隔")
    ap.add_argument("--endpoint", default="", help="对外地址（缺省 http://host:port）")
    args = ap.parse_args()

    skills = parse_skills(args.skills)
    endpoint = args.endpoint or f"http://{args.host}:{args.port}"

    if args.register:
        register(args.platform, endpoint, skills)
        return

    srv = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"[worker] listening on {endpoint} worker_id={WORKER_ID} skills={skills}")
    threading.Thread(target=heartbeat_loop, args=(args.platform, skills), daemon=True).start()
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("[worker] bye")


if __name__ == "__main__":
    main()
