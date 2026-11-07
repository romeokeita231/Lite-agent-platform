# Kubernetes 清单

镜像需自行构建：`docker build --target platform -t lite-agent-platform:local .`；
`docker build --target policy-example -t lite-agent-platform-policy-example:local .`。
`secret.example.yaml` 只有占位符，真实 Secret 需手工创建（`kubectl create secret generic platform-secrets ...`）后 `kubectl apply -f k8s/`。

## 滚动发布时手上的 Run 怎么办

1. 旧 Pod 收到 SIGTERM，`/readyz` 立刻转 503，Service 摘除该副本，新 `POST /v1/runs` 被拒绝。
2. Dispatcher 停止领新 Run；已在飞的 Step 在 `STEP_TIMEOUT + 5s` 内跑完并写下 Checkpoint。
3. 进程调用 `ResetRunningRuns`，把手上未完成的 Run 归零为 `queued`，然后退出；Checkpoint 不会半写。
4. 新副本从账本续跑这些 `queued` Run，`/readyz` 恢复 200 后重新接收请求。
5. `terminationGracePeriodSeconds: 30` 覆盖 drain 15s 加 HTTP 停机 10s，不会被提前强杀。
