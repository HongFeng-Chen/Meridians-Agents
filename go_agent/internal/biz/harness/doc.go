// Package harness 评测 Harness（评测执行框架）：把被测对象（Agent / 模型）
// 放进标准化的评测环境 —— 用例加载 → 逐例执行 → 输出采集 → 断言判分 → 报告聚合。
//
// 定位与边界：
//   - harness 是「考官」，不是「考生」：它不实现推理循环（那是 agentfactory
//     Runtime 的职责），只负责把用例喂给被测对象并度量其表现；
//   - 被测对象是黑盒：target 为 agent:<agent_key>（走 agentfactory 同步执行）
//     或 model:<model>（走 llm.Manager 直连），harness 只关心输入与输出；
//   - 判分双轨制：规则断言（contains / regex / json_valid 等，零成本确定性）
//   - LLM-as-Judge（语义等价 / 质量评分，处理开放性回答）；
//   - 评测运行是异步任务：StartRun 立即返回，worker 池并发执行，
//     进度实时写回 Run 行（前端可轮询，与任务中心体验一致）。
//
// 与现有域的关系（详见 设计方案.md）：
//   - agentfactory：被测对象来源 + Runner 适配（Agent 即被测件）；
//   - llm：被测模型来源 + Judge 模型；
//   - history / llm.CallRecord：评测产生的调用自动带 meta，链路可追溯。
package harness
