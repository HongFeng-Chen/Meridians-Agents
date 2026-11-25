# Skill Worker 参考实现（Python）

Meridians-Agents 的 Skill Runtime 用一套语言无关的 HTTP 协议调用远程技能。
这个目录给一个零依赖（只用标准库）的 Worker，用来验证协议、也作为接入模板。

## 协议

```
POST {endpoint}/execute
Authorization: Bearer <SKILL_WORKER_TOKEN>     # 平台设置了才校验
```

请求（SkillRequest）：

```json
{
  "task_id": "42",
  "execution_id": "exec_ab12cd34",
  "skill_name": "pdf_analysis",
  "skill_version": "1.0.0",
  "tenant_id": "public",
  "user_id": "u1",
  "agent_key": "doc-agent",
  "session_id": "c1",
  "idempotency_key": "42:pdf_analysis:9f2c",
  "timeout_ms": 60000,
  "trace_id": "5f2e...",
  "input": {"file_id": "f-001", "analysis_type": "architecture"}
}
```

响应（SkillResponse）：

```json
{"success": true,  "output": {"pages": 12}}
{"success": false, "error": "文件格式不支持"}
```

约定：

- `skill_name` 是 **handler**（Worker 代码里的实现名），由平台技能的 `execution.handler` 指定，
  缺省等于平台技能名。改技能标题不必改 Worker 代码。
- 业务失败也返回 HTTP 200 + `success=false`：平台据此区分「Worker 正常返回失败」与「Worker 挂了」。
- 直接返回业务 JSON（不带 `success`）也能被兼容，会被整体当作 `output`。
- 结果里不要放文件本体，大文件给对象存储地址。

## 跑起来

```bash
# 1) 起 Worker（自带 /healthz 与 15s 心跳）
python worker.py --port 9101 --platform http://127.0.0.1:8000

# 2) 注册（也可以让平台侧 POST /api/skills/{name}/instances）
python worker.py --register --port 9101 --platform http://127.0.0.1:8000 \
  --skills "pdf-analysis:pdf_analysis,text-stats:text_stats"
```

`--skills` 的格式是 `平台技能名[:handler]`，注册与心跳用平台技能名，`/execute` 收到的是 handler。

平台侧建技能（示例）：

```bash
curl -X POST http://127.0.0.1:8000/api/skills \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "pdf-analysis",
    "title": "PDF 分析",
    "content": "# PDF 分析\n先提取结构，再总结。",
    "execution": {
      "mode": "remote",
      "endpoint": "http://127.0.0.1:9101",
      "handler": "pdf_analysis",
      "timeout_sec": 30,
      "retry": 1,
      "idempotent": true,
      "input_schema": "{\"type\":\"object\",\"properties\":{\"file_id\":{\"type\":\"string\"}},\"required\":[\"file_id\"]}"
    }
  }'
```

执行：

```bash
curl -X POST http://127.0.0.1:8000/api/skills/pdf-analysis/execute \
  -H 'Content-Type: application/json' \
  -d '{"input":"{\"file_id\":\"f-001\"}"}'
```

## 接入检查清单

- [ ] `/execute` 幂等：同 `idempotency_key` 不重复产生副作用（平台已做一层回放，Worker 再兜一层）
- [ ] 尊重 `timeout_ms`：长任务自己分片，别把平台超时当默认值
- [ ] 心跳带上真实 `inflight`：调度器据此背压，虚报会导致过载
- [ ] 多租户场景按 `tenant_id` / `user_id` 校验数据归属，**不要**假设平台已鉴权过业务数据
- [ ] 返回结构化 `output`：Agent 会把结果原样喂回 LLM，散文越少越好
