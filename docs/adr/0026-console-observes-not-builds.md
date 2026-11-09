# 观察台只观察，不构建

Status: accepted

项目交付文档《03_企业级AI_Agent运行平台_项目交付文档》2.4 节第 236–237 行做了两件事：排除可视化**管理**界面（Dashboard UI，不提供创建、编排、发布 Agent 的界面），同时追加一个**观察台（Console）**，用于观察运行情况。本条把第二行落成决定。

观察台是 platform 进程的一部分：静态资源用 `go:embed` 打进二进制，原生 HTML/CSS/JS，没有 npm 与构建步骤，也没有独立的静态服务。它只做四件事：列出本租户的 Run 与详情、展开 Checkpoint/Span 序列、展示 `/healthz`、`/readyz` 与列表端点返回的本租户按状态计数、给 `waiting_human` 的 Run 提交人工应答。边界写成一句话：**不创建、不编排、不发布**。这正好是 [ADR 0011](0011-runtime-not-an-app-builder.md) 排除的"应用构建"，因此不属于交付文档上一行排除的 Dashboard UI。

观察台需要 Run 列表，而 API 里没有：新增租户级 `GET /v1/runs?status=&agent_id=&limit=`，同时返回本租户的按状态计数，复用既有 Bearer 鉴权与租户上下文，越权一律 404（[ADR 0021](0021-isolation-in-the-app-layer.md)）。页面只有一个身份：调用方自己的租户 API key，存在 localStorage，只看该租户；不引入角色，不引入跨租户视图。刷新方式是 1 秒轮询，平台不为它新增流式通道。人工应答复用既有的 `POST /v1/runs/{id}/human-input`，不新增接口面——观察台是这些既有能力的第二个入口，不是扩权。

## Considered Options

- **独立 SPA（React + Vite，产物再 embed）**：为几个只读视图引入 node 工具链，CI 与镜像各多一层，[ADR 0012](0012-one-binary-split-ready.md) 的"一个二进制"叙事出现裂缝。
- **独立前端服务**：平台从"一个二进制 + 两个容器"变成三个部署单元，收益不匹配。
- **UI 直连 store，不加公开端点**：观察台会拥有比公开 API 更大的读权限，与 ADR 0021"隔离在应用层"的答案相冲突。
- **SSE / WebSocket**：新增连接生命周期与背压，只为把 1 秒变成亚秒。
- **跨租户运维视图**：需要新的身份模型；为演示加一个角色，比不加更贵。

## Consequences

- `/metrics` 整体不进页面。它是进程级、无鉴权的运维面（独立监听 `METRICS_ADDR`，默认 `127.0.0.1:9091`，不在业务 mux 里），不是租户数据面；页面不去读它。`platform_runs` 只带 status 标签、`platform_tenant_inflight` 是没有标签的 Histogram（`internal/observability/metrics.go:36`、`:66`），没有任何一条指标能按租户切分，"只取不可归属的指标"这个切法在代码里不成立。租户 key 仍可直接 curl `/metrics`；给它加独立监听或鉴权是另开一条票，不是观察台的责任。
- 列表查询必须自己写 `where tenant_id = $1`，**不得复用 `CountRunsByStatus`**：它的语义是 metrics 专用、故意不带租户过滤（`internal/repository/store.go:198`，`internal/repository/counts_test.go:14` 专门在测这一点），挂到鉴权端点上就是跨租户信息泄露。
- `runs` 现有索引 `runs_tenant_idx (tenant_id)` 与 `runs_runnable_idx (status, next_attempt_at)`（`internal/migrate/sql/0003_tenancy.sql:35`），支撑不了"最新 N 条"。需要补 `(tenant_id, created_at desc)`，以新迁移追加，不改已发布的 0003。
- 观察台不改变 ADR 0012 的交付物：仍是 platform 一个二进制加 policy、Postgres 两个容器，只是多了一段 embed 的静态资源。
- README 的 "there is no dashboard UI, by design" 一句在观察台存在之前不改——现在改就是撒谎；它与 `GLOSSARY.md` 的 Console 词条一起，跟着实现的 PR 改。
- "loopback 下自动注入租户 key" 属于演示自举的便利，不能编译进观察台的产物，也不由本条决定。
- 演示自举（让可执行包自己拉起 Postgres 与 policy）不在本条内，是另一个决定；落它时必须正面回答与 ADR 0012 的关系。
