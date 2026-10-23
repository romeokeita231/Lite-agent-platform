# 模型网关可以脱离 Run 使用，run_id 只是分摊维度

Status: accepted

`model_call_logs.run_id` 原设计标了 nullable 却没说何时为空。定死：为空是正常的。计费的原子单位是 **(tenant, model, call)**，不是 Run；带 run_id 的调用额外参与 Run 级的成本归集，不带的自成一条只有一个 Span 的 Trace。

## Considered Options

- **强制带 run_id（直连也要先开一个单步 Run）**：账本完整，计费永远能追到 Run。代价是网关不再是"服务"——别的团队要用模型接入，得先理解 Run 是什么。JD 把模块②叫"统一模型接入与调用服务"，这个代价直接否掉了它的独立价值。

## Consequences

- 用量报表的口径必须写成"按 (tenant, model) 汇总，可下钻到 run"，而不是"按 Run 汇总"。口径写错，报表和计费对不上。
- 归集成本时不能假设 Run 覆盖全部调用：一次 Run 的 token 成本 ≤ 该租户同时段的总消耗。
- 网关的鉴权边界与 Run 的鉴权边界因此不同：前者只到 tenant，后者到 tenant + run。
