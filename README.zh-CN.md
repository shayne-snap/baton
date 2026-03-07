# Baton

[English](README.md) | [简体中文](README.zh-CN.md)

Baton 是 [Symphony](https://github.com/openai/symphony) 的 Go 语言实现。

## Baton 的能力
- 轮询你的任务追踪器（默认是 Linear），查找可领取 issue，并为每个 issue 预留隔离工作区。
- 在工作区中启动 `codex app-server`，注入工作流提示词，并持续保持会话，直到 issue 进入
  `Done`、`Closed`、`Cancelled` 或 `Duplicate`。
- 提供注入式 `tracker_*` 工具，让不同 runtime 都能直接访问当前配置的 tracker，避免重复鉴权。
- 当 issue 退出活跃状态后清理工作区，并安全关闭 agent 进程。

## 支持矩阵
- Agent runtime：`codex`、`opencode`、`claudecode`。
- Tracker：`linear`、`jira`。

## 准备你的仓库
1. 先让仓库符合 harness 工程实践，确保 Baton 运行稳定。
2. 编写 `WORKFLOW.md`，遵循 SPEC 中 tracker/workspace/hooks/agent/agent_runtime 的 schema，并使用
   YAML front matter 进行配置。
3. 导出追踪器凭据（例如 `LINEAR_API_KEY`），供 hooks 与工具鉴权。
4. 复制需要的辅助技能（`commit`、`push`、`pull`、`land`、`linear`），并确保所用 runtime 能注入
   Baton 提供的 `tracker_*` 工具。
5. 把自定义状态（例如 `Rework`、`Human Review`、`Merging`）同步到工作流配置和追踪器设置中，
   保证 Baton 生命周期与预期一致。

## 配置要点
- 将工作区初始化命令（如 `git clone ... .`）放到 `hooks.after_create`。
- `codex.command` 应启动 `codex app-server` 并指定你的沙箱策略；Baton 默认按工作区作用域创建沙箱，
  并支持字符串或对象两种审批策略写法。
- `agent_runtime.kind` 支持 `codex`、`opencode` 和 `claudecode`。
- `tracker.kind` 支持 `linear` 和 `jira`。
- 路径字段支持 `~` 与 `$VAR`；Baton 在启动子进程前会先做解析。
- `tracker.linear.api_key` 这类环境变量字段可以写成 `$LINEAR_API_KEY`，Baton 会在运行时读取实际值。
- 如果 `WORKFLOW.md` 缺失或无效，Baton 会拒绝启动，避免你在错误配置下继续运行。

## 运行 Baton
```sh
cd /Users/goranka/Engineer/ai/backagent/baton
go build -o bin/baton ./cmd/baton
./bin/baton --i-understand-that-this-will-be-running-without-the-usual-guardrails WORKFLOW.md
```
- 工作流路径作为唯一位置参数传入；默认值是 `WORKFLOW.md`。
- 可用参数：
  - `--logs-root`：覆盖默认日志目录（当前工作目录）。
  - `--i-understand-that-this-will-be-running-without-the-usual-guardrails`：确认你理解 agent runner
    仍处于实验阶段；启动 Baton 时必须显式提供。
- Baton 会注册信号处理器，因此 `Ctrl+C` 可以优雅停止 agents 并关闭 Codex 会话。

## 可观测性与测试
- 日志会写入配置的日志根目录（默认：`logs/`），每次 agent 调用都会记录工作区路径与工作流元信息。
- 可选 HTTP API 暴露 `/api/v1/state`、`/api/v1/<issue_identifier>` 与 `/api/v1/refresh`，用于排障。
- 运行 `go test ./...` 可覆盖 CLI、工作流解析与编排逻辑；当前没有统一 `make` 目标，但 Go 测试集
  已覆盖运行 Baton 的核心包。
