# 演示自举是一个 zip，不是新的交付形态

Status: accepted

观察台解决"看不见运行"，剩下的是"不用命令行准备环境"：启动一个东西，Postgres、迁移、seed、Policy、HTTP 与观察台全部就位。它必须同时兑现两个承诺，缺一个就不算数：**离线可用**，以及 **Policy 仍在独立进程里**（[ADR 0008](0008-go-for-platform-and-policy.md)）。

演示分发物是一个 zip，内容是 `platform.exe`、`policy-example.exe`、`QUICKSTART.md`、`THIRD-PARTY-NOTICES.txt`（Postgres 与 Go 的许可证说明）与 Postgres 归档（归档 per-OS，因此分开打包）。v1 只承诺 **Windows amd64**，其他平台不承诺。首次运行不联网：`platform demo` 把归档解压到 `%LOCALAPPDATA%\LiteAgentPlatform\demo\`，起 Postgres、跑迁移与 seed，用 `os.Executable()` 定位同目录的 `policy-example.exe` 并作为子进程拉起（不按 CWD 找），最后起 HTTP 与观察台；进程退出时回收全部子进程。端口固定 8080 / 5433 / 9090，并额外绑定仅回环的指标端口 9091；8080、9090 与 9091 都与 compose 栈重合。

## Considered Options

- **单文件 `platform.exe`（内嵌归档 + demo 模式重执行自身承载示例 Policy）**：只有一个文件，但把示例业务代码编进 platform 产物，ADR 0008"两个二进制"的界线和交付叙事混在一起。
- **首次运行联网下载 Postgres**：包最小，但断网演示与 CI 正好是最需要它的场景，而且每个平台都要各验一遍。
- **依赖宿主机已装的 Postgres**：把"准备环境"又还给了用户，与本条要解决的句子是同一句。
- **zip 里再放一个 docker-compose**：把"不用命令行"换成"先装 Docker"，没有前进。

## Consequences

- [ADR 0012](0012-one-binary-split-ready.md) 不变：生产交付仍是 platform 一个二进制加 policy、Postgres 两个容器。zip 只是演示分发，不进 k8s 清单，也不改变镜像构建。
- 支持平台只有 Windows amd64。Postgres 归档 per-OS，别的平台要么各自一份归档、各自验收，不做默认承诺。
- Windows 的 initdb 依赖 VC++ 运行时。缺失时的失败信息必须翻成人话（需要安装 Visual C++ 运行库并给链接），不能把原始 stderr 丢给用户。
- 端口 8080 / 9090 与 compose 栈完全重合，两种模式互斥，不支持并存。失败信息直接指出"你可能正在跑 compose 栈"，README 写明二选一；5433 不冲突（compose 的 db 没有发布端口），仅回环的指标端口 9091 与 compose 的 `127.0.0.1:9091` 重合。
- 数据目录在 `%LOCALAPPDATA%` 下，持久保留，`platform demo --reset` 清空重建，不写仓库目录。
- 任一步失败即整体退出，打印哪一步、被谁占用、怎么改；不自动换端口，不做半启动重试，不降级为联网下载。
- 观察台的票先做、自举的票后做：`platform demo` 的验收依赖页面已经能跑，反过来不成立。
