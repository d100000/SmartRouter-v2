# 贡献指南 · Contributing

感谢你愿意改进 SmartRouter-v2。本文说明如何报告问题、提交代码，以及合并前需要通过哪些检查。English summary: [see below](#english).

## 报告问题与提出建议

- **缺陷和功能建议**：在 [Issues](https://github.com/d100000/SmartRouter-v2/issues) 中选择对应模板提交。提交前先搜索是否已有相同问题。
- **缺陷报告请包含**：版本（控制台右下角的提交号）、部署方式、数据库类型、复现步骤、已脱敏的日志。
- **安全漏洞**：不要公开提交，按 [README 的“安全”一节](./README.md#sr-security)私下报告。
- **较大的改动**：先开 Issue 说明目标和方案，取得一致后再写代码，避免返工。

## 开发环境

环境准备、启动方式和目录职责见 [README 的“本地开发”](./README.md#sr-development)。后端使用 Go，前端使用 Bun；`main.go` 会嵌入 `web/dist`，所以构建或测试根模块之前要先构建前端。

## 提交代码

1. Fork 本仓库，从 `main` 创建分支。
2. 只修改与本次任务相关的文件，一个 Pull Request 只做一件事。
3. 遵守 [AGENTS.md](./AGENTS.md) 中的项目约定；修改前端时同时遵守 [web/AGENTS.md](./web/AGENTS.md)；涉及计费时先阅读 [.agents/rules/billing.md](./.agents/rules/billing.md)。
4. 为行为变化补充测试，运行下面的检查。
5. 按 [Pull Request 模板](./.github/PULL_REQUEST_TEMPLATE.md)填写说明，写明你实际执行的验证命令和观察到的结果。

提交信息建议使用 `feat:`、`fix:`、`docs:`、`refactor:`、`test:` 等前缀，标题说明改了什么，正文说明原因。

## 合并前检查

```bash
# 前端（在 web/ 目录）
bun install --frozen-lockfile
bun run lint
bun run typecheck
bun run test
bun run build

# 后端（在仓库根目录，前端构建完成后）
go vet ./...
go test ./...
go build ./...
```

- 改动 `relaykit/` 或 `tokenkit/` 时，在对应目录执行 `GOWORK=off go test ./...` 和 `GOWORK=off go build ./...`。
- **涉及数据库的改动**必须在真实的 SQLite、MySQL 和 PostgreSQL 上验证，并在 Pull Request 中写明数据库版本、命令和结果。缺少其中一项时请明确说明，不要写成“已验证”。
- **界面文案**需要同时提供 7 种语言（en、zh、zh-TW、ja、fr、ru、vi），修改后执行 `bun run i18n:sync`。
- **调度行为**的改动请同步更新 [SCHEDULING.md](./SCHEDULING.md)。

## 需要保留的内容

本项目基于 new-api 开发。上游项目与作者的名称、署名、许可声明和链接受项目策略保护，不得删除、替换或改名，详见 [AGENTS.md](./AGENTS.md) 的 Project Governance 一节以及 [NOTICE](./NOTICE)。

## 许可

提交的贡献按本仓库的许可发布：[AGPLv3](./LICENSE) 及 [NOTICE](./NOTICE) 中的附加条款。

参与讨论和协作时请遵守[行为准则](./.github/CODE_OF_CONDUCT.md)。

<a id="english"></a>

## English

Thank you for helping improve SmartRouter-v2.

- **Bugs and feature requests**: open an [issue](https://github.com/d100000/SmartRouter-v2/issues) with the matching template. Include the version (the commit shown at the bottom right of the console), deployment method, database type, steps to reproduce and redacted logs.
- **Security issues**: do not file them publicly; follow the [Security section of the README](./README.en.md#sr-security).
- **Larger changes**: open an issue first and agree on the approach.
- **Code**: fork, branch from `main`, keep each pull request to one focused change, and follow [AGENTS.md](./AGENTS.md) (plus [web/AGENTS.md](./web/AGENTS.md) for frontend work and [.agents/rules/billing.md](./.agents/rules/billing.md) for billing).
- **Checks before review**: `bun run lint`, `bun run typecheck`, `bun run test` and `bun run build` in `web/`; then `go vet ./...`, `go test ./...` and `go build ./...` in the repository root. Changes that affect the database must be verified on real SQLite, MySQL and PostgreSQL; state the versions, commands and results, and say so plainly if one is missing.
- **User-facing text** must be provided in all seven console languages; run `bun run i18n:sync` afterwards.
- **Upstream attribution**: names, notices and links of the upstream new-api project and its authors are protected and must not be removed or replaced. See Project Governance in [AGENTS.md](./AGENTS.md) and [NOTICE](./NOTICE).
- **License**: contributions are released under [AGPLv3](./LICENSE) with the additional terms in [NOTICE](./NOTICE).

Please follow the [Code of Conduct](./.github/CODE_OF_CONDUCT.md).
