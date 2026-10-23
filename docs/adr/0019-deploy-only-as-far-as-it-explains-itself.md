# 部署只做到能讲清楚，不抄 HPA

Status: accepted

K8s 只交 Deployment + Service + ConfigMap/Secret + 探针 + requests/limits；不做 HPA、PDB、Helm。但必须做两样：readiness 与 liveness 分离、优雅停机。

## Consequences

- 探针不分，数据库抖两秒就会重启全部 Pod，打断正在跑的 Run。虽然能从 Checkpoint 恢复，但没有理由白白打断。
- 优雅停机：收到 SIGTERM 后停止接新 Run、把手上的 Run 标成可恢复、再退出。没有它，ADR 0009 与 0014 的可恢复性会在每一次滚动发布时被浪费掉。
- HPA 在 demo 里没有负载可扩，加了只是抄 yaml；单 chart 的 Helm 是仪式。能讲清"滚动发布时手上的活怎么办"，比"我配了 HPA"值钱。
