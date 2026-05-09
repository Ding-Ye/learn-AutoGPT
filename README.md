# learn-AutoGPT

> 用 Go 从零渐进构建一个 AutoGPT-classic 风格的 autonomous agent，每节末尾对照上游 Python 源码。
> Build an AutoGPT-classic-style autonomous agent from scratch in Go, session by session — each chapter ends with the upstream Python source.

[English version below / 英文版见下方](#english)

---

## 这是什么 · What is this

`Significant-Gravitas/AutoGPT` 是 2023 年最具里程碑意义的 autonomous agent 仓库——它把 GPT-4 包进一个 *think → act → observe* 循环，让模型自己规划、调工具、读写文件、循环至完成。代码量 ≈ 21M LOC，跨 Python 后端 + TypeScript 前端 + 多个子项目，一次性读完不现实。

**`learn-AutoGPT` 把上游 `classic/` 子目录（原始 Python agent，MIT 许可）拆成 10 节渐进的 Go 教学实现**：每节加一个机制，每节都是独立可运行的 `package main`，每节末尾把 Go mini 版与 AutoGPT 上游源码对照阅读。

不动 `autogpt_platform/`（那部分用 Polyform Shield 许可，不在我们的 derivative 范围内）。

## 课程地图 · Curriculum

| # | 章节 / Chapter | 上游机制 | 状态 |
|---|---|---|---|
| s01 | [最小 think→act→observe 循环 / Minimal think→act→observe loop](docs/zh/s01-minimal-loop.md) | `app/main.py:run_interaction_loop` + `agents/agent.py:propose_action` | ✅ |
| s02 | 显式命令注册表 / Explicit command registry | `forge/command/decorator.py` (@command) | ⏳ |
| s03 | LLM Provider 多后端 / LLM provider with multiple backends | `forge/llm/providers/multi.py` | ⏳ |
| s04 | Prompt 策略与解析 / Prompt strategies & response parsing | `agents/prompt_strategies/one_shot.py` | ⏳ |
| s05 | 情节式动作历史 / Episodic action history | `forge/components/action_history/` | ⏳ |
| s06 | 沙箱化 Workspace / Sandboxed workspace storage | `forge/file_storage/local.py` | ⏳ |
| s07 | 分层权限管理 / Layered permission system | `forge/permissions.py` | ⏳ |
| s08 | 可插拔 Component 系统 / Pluggable component system | `forge/agent/protocols.py` + `forge/components/` | ⏳ |
| s09 | 持续运行模式与 UI / Continuous mode & UI feedback | `app/main.py:655-768` (cycle budget + signal) | ⏳ |
| s10 | Reflexion 与 AfterParse hooks / Reflexion & AfterParse pipeline | `agents/prompt_strategies/reflexion.py` + `forge/agent/protocols.py` (AfterParse) | ⏳ |
| s_full | 端到端集成 / End-to-end integration | (16-step trace) | ⏳ |
| App. A | Classic vs 现代 Agent 架构 / Classic vs Modern agent architectures | (mental model) | ⏳ |
| App. B | 上游源码导读地图 / Upstream source-reading map | (reference) | ⏳ |

## 快速跑起来 · Quickstart

```bash
# Go ≥ 1.22 + 任一 LLM API key
git clone https://github.com/Ding-Ye/learn-AutoGPT.git
cd learn-AutoGPT/agents/s01-minimal-loop

# 1) Anthropic 默认
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "say hi via the echo tool"

# 2) OpenAI / DeepSeek / Qwen / Moonshot / Groq / OpenRouter / 本地 vLLM
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "say hi via the echo tool"

# 3) 跑测试 / run tests
go test -v ./...
```

## 文档站 · Doc viewer

```bash
cd web
npm install
npm run dev    # http://localhost:3000
```

中文 / 英文双语并排，章节侧边栏，每节末尾自动嵌入上游 Python 源码片段。

## 教学法 · Pedagogy

仿照 [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) 的六段式：

1. **Problem** — 这一节要解决的痛点
2. **Solution** — 心智模型（先于代码）
3. **How It Works** — ASCII 图 + 30-60 行核心代码 + 非显然之处
4. **What Changed** — 与上一节的 diff
5. **Try It** — 可复制的命令 + 期望输出形态
6. **Upstream Source Reading** — 真实上游片段对照

每节都是独立的 Go module，**没有跨节 import**——你可以把 s01 当作 250 行的「最小可运行 agent」单独读，再 diff 到 s02 看「加了什么」。

## 致谢 · Acknowledgments

- 上游：[Significant-Gravitas/AutoGPT](https://github.com/Significant-Gravitas/AutoGPT)（classic/ 子目录，MIT）
- 教学法：[shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code)
- 生成器：本仓库由 [learn-repo-generator skill](https://github.com/anthropics/claude-code) 渐进式生成

## License

MIT — see [LICENSE](LICENSE).
本学习仓库为 Go 重写实现，未拷贝上游代码；`upstream-readings/` 中的短片段为 MIT 许可下的教学引用。

---

## English

`Significant-Gravitas/AutoGPT` is the 2023 milestone autonomous-agent repo — wrapping GPT-4 in a *think → act → observe* loop where the model itself plans, calls tools, reads/writes files, and iterates to completion. At ≈21M LOC across a Python backend, a TypeScript frontend, and several sub-projects, reading the whole thing in one sitting is not realistic.

**`learn-AutoGPT` decomposes the upstream `classic/` subtree (the original Python agent, MIT-licensed) into 10 progressively-built Go teaching sessions**. Each session adds one mechanism, each is a self-contained runnable `package main`, and each ends with a Go-vs-upstream side-by-side reading of the AutoGPT source.

We deliberately leave `autogpt_platform/` alone (Polyform Shield license, outside our derivative scope).

See [the Curriculum table above](#课程地图--curriculum), [Quickstart](#快速跑起来--quickstart), and [Pedagogy](#教学法--pedagogy).

The English documentation lives at [docs/en/](docs/en/); Chinese at [docs/zh/](docs/zh/).
