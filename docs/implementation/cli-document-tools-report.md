# Claude / Kimi 文档工具修复报告

本任务以本地 main `9d1b227` 为基点，在分支 `DongShan03/library-agent-retrieval`、worktree `/Users/bytedance/orca/workspaces/NeoRead/library-agent-retrieval` 完成。按后续范围变更，Claude/Kimi 优先自主调用文档工具；额外的模型查询规划已删除，没有新增规划调用。未 push、合并或打包，未读取用户 NeoRead 数据目录，未修改翻译文件或 release notes。

## 提交

| 提交      | 范围                                                                                     |
| --------- | ---------------------------------------------------------------------------------------- |
| `56c38a4` | Claude/Kimi 单文档与书库工具路径、进程隔离、工具验证、draft/reset、Kimi 预检索回退及回归 |
| `c747cc9` | 重启后历史、references、继续工具调用及 interrupted 快照恢复回归                          |
| `b879693` | Kimi 工具会话保留模型元数据；无 MCP 时不宣称工具已注册                                   |
| `1df5064` | Claude 遵守现有 CLI 联网开关、来源测试与唯一一处设置文案修改                             |
| `e1e1365` | 诊断状态/数量断言，以及诊断和流式合同文档                                                |
| `60975ae` | CLI 文档工具的图像事件传输上限，以及大于 8 MiB 的事件回归                                |

`c747cc9` 之后，`claude_document.go` 的额外实现修改只有文档流 scanner 上限：文字流原来的 8 MiB 不足以承载现有页面工具允许的图像结果，改为与多模态 API 一致的 48 MiB，并补充超过 8 MiB 的合成图像事件回归。`cli_document.go` 此后没有修改；原生 Claude 会话及隐藏设置由协调者的独立任务处理，本任务仍保留 `--no-session-persistence`。

## Claude 隔离与实测

文档对话使用独立的 `claudeDocumentArgs()`；原 `claudeArgs()` 没有修改，翻译、公式等非对话调用继续使用原来的 safe-mode 参数。文档路径不使用 safe-mode，因为实际 CLI 在该模式下不会加载显式 MCP。

选择 `--setting-sources user` 加显式 settings：`disableAllHooks:true`、`autoMemoryEnabled:false`、`claudeMdExcludes:["**"]`。使用独立临时 `CLAUDE_CONFIG_DIR`、临时用户及工作目录 CLAUDE.md、SessionStart hook 和本机合成模型服务验证，实际 CLI 为 Claude Code `2.1.318`。

| 方案                                                   | settings.env 网关生效        | hook 触发 | 合成 CLAUDE.md 进入模型输入 | init 自动 memory 路径 |
| ------------------------------------------------------ | ---------------------------- | --------- | --------------------------- | --------------------- |
| `--restricted`                                         | 否，落到进程环境里的测试网关 | 否        | 否                          | 无                    |
| user settings + 显式禁用 hooks/auto-memory             | 是                           | 否        | 是                          | 无                    |
| user settings + 上述禁用项 + `claudeMdExcludes:["**"]` | 是                           | 否        | 否                          | 无                    |

验证命令为 `python3 /tmp/neoread-claude-isolation.py` 和 `python3 /tmp/neoread-claude-isolation-extra.py`；均只使用临时设置和合成请求，没有修改真实 Claude 配置，也没有读取凭据文件。选择最后一种方案以保留用户配置的 Key、网关和模型默认值，由 CLI 自身加载这些配置。

每轮使用私有临时工作目录；MCP 配置写入该目录的 0600 文件，token 不进入 argv，结束后清理。`--strict-mcp-config` 排除继承 MCP，`--tools ""` 排除内置工具，`--allowedTools` 仅列四个确切的 `mcp__neoread_document__...` 名称。只有本轮已授权联网时，内置工具和白名单才增加 WebSearch、WebFetch。审批使用 dontAsk/none。

正文或草稿输出前解析 `system/init`，完整工具集合必须等于预期，唯一 MCP 必须为 neoread_document 且 connected；缺工具、多工具都返回 ErrorCapability。Claude 没有 Codex 同等的操作系统只读沙箱，因此没有开放 Bash、文件读写或其他内置执行工具；对齐的是本轮只读文档工具和联网授权边界。

## 工具、回退和证据

Claude 与支持 HTTP MCP 的 Kimi 复用 `serveBoundDocumentMCP`、`documentToolBinding` 及现有四工具：get_outline、search_document、read_passage、read_pdf_pages。单文档、书库和带图对话使用相同入口。没有更改 Codex/API 的工具执行分支；共用的空证据序列化按任务要求改为 `[]`。

模型可以多次搜索和读取；提示词要求使用原文语言的简短关键词，中文问题讨论英文论文时使用英文。工具继续负责范围、版本、sourceTokenLimit、证据数量及 `[S编号]` 校验，回答仍经 `saveDocumentAgentAnswer` 保存。

Kimi 的 initialize 必须声明 HTTP MCP 能力，session/new 只传应用自己的 HTTP MCP URL 和 Authorization 头，agent-file 保持 `tools: []`、`subagents: []`，不开放终端/文件客户端能力。先独立核验应用 MCP 的四工具清单；ACP 没有远端实际 inventory/connected 状态，诊断明确记 inventory_unavailable。

Kimi 权限请求默认拒绝，只允许四个文档工具的 allow_once。工具事件也逐次核验，额外工具立即触发 session/cancel、返回 ErrorCapability，不保存回答。当前 Kimi 协议使用裸工具名及可选 `: 参数` 标题，无法据此证明工具来自哪个远端服务器；用户本地 MCP 是否会被真实 CLI 额外加载，尚未实测。

只有缺少 HTTP MCP 能力或建立 MCP 会话被拒绝时，Kimi 才回退到预检索；进入回答阶段后失败不回退。回退沿用 search_document/read_passage：分离中文里相邻的拉丁词，所有检索文本在 rune 边界截断至 512 字节以内，解析工具错误并与零命中区分。按论文轮流读候选；没有命中的论文从前 32 个段落中优先选择 Abstract/摘要及其后首个足够长的段落，没有摘要标记时取首个正文段，跳过重复页眉、标题、少于 80 rune 或无句子标点的文字。全部读取仍受既有预算约束。

无证据时传入空数组，说明是没有找到可读段落还是预算不足；空证据不再以 null 进入提示词。仅补读开头的论文与相关命中分开说明，避免把摘要覆盖当成全文阅读。

## 流式、保存与重启接续

Claude 的正文片段先走 draft，stop_reason=tool_use 时 reset；Kimi 在工具开始时 reset，此前的中间说明不进入最终答案。Claude 必须成功 result、最终段落一致且进程正常退出；Kimi 必须 prompt end_turn，才提交当前最终段落。来源校验、事务保存及 done 仍由现有聊天流程负责，已经有可见草稿后不会追加备用答案。

每轮由 NeoRead 把已保存的问题、回答和 references 带给新进程，不依赖原生线程，也不把 continuous 设为 true。正式回归覆盖 Claude/Kimi × 单文档/书库四种组合：首轮工具读取与引用保存后关闭 Server/Store，重新 OpenStore/NewServer，第二轮实际进程提示词包含首轮问题、回答及历史来源，旧 references 保持一致，第二轮仍可调用工具并生成新引用。

另四种组合从突然退出留下的 running + draft 磁盘快照恢复：OpenStore 后状态为 interrupted，问题保留，没有助手答案，下一轮实际收到被中断的问题，草稿不被带入历史。这里模拟的是崩溃后的持久化状态，没有强杀用户的实际应用。

## 诊断

新增 `chat.library_retrieval`，只记录模式/状态枚举、检索调用数、去重命中数、已读段数、覆盖/范围论文数及是否回退。`chat.document_tools` 记录 Claude 的 passed/missing_tools/extra_tools，或 Kimi 的 inventory_unavailable/unsupported_http/session_rejected。私有问题和正文标记测试验证日志不含问题、关键词或原文；保存是否成功继续看 chat.finish 和 run 状态。说明已写入 `docs/diagnostics.md`。

## 真实冒烟

在 apps/server 中执行：

```sh
NEOREAD_TEST_CLAUDE_DOCUMENT=1 go test ./internal/neoread -run '^TestLiveClaudeDocumentTools$' -count=1 -v
NEOREAD_TEST_CLAUDE_WEB=1 go test ./internal/neoread -run '^TestLiveClaudeDocumentWebTools$' -count=1 -v
```

两次均通过，未受额度限制影响。合成文档回合约 7.7 秒，调用 get_outline、search_document、read_passage 两次，读取两篇的两段原文，34 个 draft 片段、3 次 reset，来源校验及保存通过；联网关闭时 init 清单验证只有四个文档工具。联网回合约 12.1 秒，WebSearch 和 WebFetch 均 completed，25 个网页来源随回答保存。

这些是两次完成耗时，不是首字延迟测量；没有额外查询规划调用。CLI 内部随文档工具产生的模型轮次由提供方负责，应用没有另开规划请求。Kimi 未做真实 CLI 验证，包括其用户本地 MCP 加载行为；本机未安装 Kimi，仅完成假 ACP、真实本机 MCP 传输与保存回归。

## 手动验证

1. 在本 worktree 运行 `pnpm dev`，打开终端打印的带 token 的 127.0.0.1 地址；开发书库位于仓库 `.neoread`，无需使用用户正式书库目录。导入两篇英文样例，在设置中选已登录的 Claude，关闭联网。
2. 在书库 Agent 用中文询问攻击方向与机制研究的比较。展开工具步骤，确认有真实检索/读取，回答有 `[S编号]`，点击能回到对应论文。单文档对话重复一次。
3. 让首轮记住一个口令并引用原文，关闭开发进程再启动，进入同一会话继续询问；核对旧问答、旧引用和新一轮工具读取。
4. 开启 CLI 联网，要求搜索 IANA Example Domains 并读取 example.com，核对搜索、读取、网页链接和保存后的执行记录；关闭后再问，确认不提供网页工具。
5. 在生成中取消，确认没有保存半截助手答案；已中断 run 的恢复及再次提问按现有会话状态显示。Kimi 安装后在同一合成样例库复核 MCP 支持/回退、工具事件及取消；本报告不把假 ACP 当作真实 Kimi 验证。

## 校验结果

| 检查                           | 实际结果                                                                                                                   |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `pnpm typecheck`               | 退出码 0                                                                                                                   |
| `pnpm test`                    | 退出码 0；Go 各包通过，最终全量 neoread 包约 188.4 秒；Vitest 237 个文件通过、3 个跳过，2036 个测试通过、9 个跳过          |
| 最后图像帧修正后的定向 Go 回归 | 下方命令退出码 0，约 16.9 秒，包含重启、诊断、权限、回退、预算和大图像事件                                                 |
| `pnpm build`                   | 在最终实现上退出码 0，完成 agent、processor、server、web、desktop 和 connector 构建；仅有既有的大 chunk 提示，没有打安装包 |

在 apps/server 执行的最后定向命令：

```sh
go test ./internal/neoread -run 'TestCLIDocument|TestClaudeDocumentWeb|TestKimiDocument|TestLibraryRetrieval' -count=1
```

检查按顺序执行，没有把测试与构建并行。新 worktree 使用 `pnpm install --frozen-lockfile` 安装了既有锁定依赖，构建准备了既有布局模型，未改变依赖清单或 lockfile。全量测试验证了 Codex/API 的现有路径；最后的 CLI 图像传输改动另由上述定向回归覆盖，未改变 Codex/API 的执行路径或文档证据预算。

真实 CLI 验证限于 Claude Code 2.1.318 的原文读取和原生网页工具；PDF 图像本次验证了合成事件传输及既有页面 MCP 回归，没有做真实 Claude 图像理解验收。Kimi 没有真实 CLI 验证，远端实际工具清单及用户本地 MCP 加载行为仍不可核验；遇到可观察的额外工具事件会取消本轮。人工界面验收步骤见上文，没有使用浏览器自动化。
