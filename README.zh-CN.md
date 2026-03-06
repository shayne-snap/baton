# Baton

[English](README.md) | [简体中文](README.zh-CN.md)

Baton 是 [Symphony](https://github.com/openai/symphony) 的 Go 实现。

## Baton 可以做什么
- 轮询你的跟踪器（默认是 Linear）以领取可处理的 issue，并为每个 issue 预留隔离的工作区。
- 在工作区中启动 `codex app-server`，发送工作流提示，并持续保持会话，直到 issue 进入 `Done`、`Closed`、`Cancelled` 或 `Duplicate`。
- 在需要时提供 `linear_graphql` 辅助工具，使 skills 无需重复鉴权即可访问 Linear。
- 当 issue 离开活跃状态后清理工作区，并干净地关闭 agents。

## 准备你的仓库
1. 先让仓库符合 harness 工程实践，确保 Baton 能稳定运行。
2. 编写符合 SPEC 中 tracker/workspace/hooks/agent/codex 结构的 `WORKFLOW.md`，并使用 YAML front matter 进行配置。
3. 导出跟踪器凭据（例如 `LINEAR_API_KEY`），以便 hooks 和 tools 完成鉴权。
4. 复制你需要的辅助 skills（`commit`、`push`、`pull`、`land`、`linear`），并确保 `linear` skill 可以访问 Baton 提供的 `linear_graphql` 工具。
5. 在工作流配置和跟踪器设置中同步任何自定义状态（例如 `Rework`、`Human Review`、`Merging`），保证 Baton 生命周期与预期一致。

## 配置要点
- 将工作区初始化命令（例如 `git clone ... .`）放到 `hooks.after_create`。
- `codex.command` 应启动 `codex app-server` 并配置所需的沙箱策略；Baton 默认使用工作区级沙箱，并同时支持字符串或对象形式的审批策略。
- 路径值支持 `~` 和 `$VAR`；Baton 在启动子进程前会先解析这些值。
- `tracker.api_key` 这类支持环境变量的字段可以配置为 `$LINEAR_API_KEY`，让 Baton 在运行时读取正确值。
- 如果 `WORKFLOW.md` 缺失或无效，Baton 会拒绝启动，确保你能及时发现配置问题。

## 运行 Baton
```sh
cd /Users/goranka/Engineer/ai/backagent/baton
go build -o bin/baton ./cmd/baton
./bin/baton --i-understand-that-this-will-be-running-without-the-usual-guardrails WORKFLOW.md
```
- 将工作流路径作为唯一的位置参数传入；默认值是 `WORKFLOW.md`。
- Flags:
  - `--logs-root` 可覆盖默认日志目录（当前工作目录）。
  - `--i-understand-that-this-will-be-running-without-the-usual-guardrails` 用于确认你了解该 agent runner 的实验性质；这是启动 Baton 的必需参数。
- Baton 会安装信号处理器，因此按 `Ctrl+C` 时会优雅停止 agents 并关闭 Codex 会话。

## 可观测性与测试
- 日志会写入已配置的 logs root（默认为 `logs/`），并记录每次 agent 调用的工作区路径与工作流元数据。
- 可选 HTTP API 暴露 `/api/v1/state`、`/api/v1/<issue_identifier>` 和 `/api/v1/refresh`，便于排查问题。
- 运行 `go test ./...` 可覆盖 CLI、工作流解析和编排逻辑；目前还没有统一的 `make` 目标，但 Go 测试套件已覆盖 Baton 运行的核心包。
