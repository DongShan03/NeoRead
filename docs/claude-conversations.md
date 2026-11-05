# Claude 原生阅读会话

Claude 的文档和书库 Agent 对话保存原生会话。后续问题通过官方 `--resume <完整 transcript 路径>` 恢复同一个会话 ID，保留此前的工具结果和模型上下文。NeoRead 同时在书库数据库保存可见问答、引用和工具活动，用于界面、备份及切换提供方。翻译、公式识别等独立任务继续使用 `--no-session-persistence`。

已进入原生会话的问答不再每轮重复发送。切换提供方后尚未同步的消息仍会补入；原文版本改变时提醒重新核查来源。每次恢复都重新绑定本轮文档 MCP 服务，并在输出前校验工具范围。联网开关、禁用 hooks/自动记忆/CLAUDE.md，以及不开放命令和文件工具的限制仍然生效。

## 隐藏与恢复

「设置 → 诊断 → 在 Codex/Claude 中隐藏会话」沿用已有设置，默认关闭。

- Codex 使用官方归档与恢复接口，详见 [Codex 历史可见性](codex-history-visibility.md)。
- Claude 在一轮进程退出后，将 NeoRead 自己创建的 `~/.claude/projects/.../<会话ID>.jsonl` 移到同一目录下的 `claude-history-hidden/<会话ID>.jsonl`。文件内容保持原样；后续通过完整文件路径恢复，继续写入同一份隐藏会话。使用自定义 `CLAUDE_CONFIG_DIR` 时，同样保留在该配置目录的原项目路径内。
- 关闭隐藏后，完整 transcript 恢复到原来的 Claude 历史位置。生成期间切换开关，在本轮结束后应用最新设置。

Claude CLI 没有使用 Codex 的归档接口；隐藏依靠 transcript 的存放位置。只移动本机书库记录归属的会话，不扫描其他会话正文，不读取或复制 Claude 凭据。移动采用完整复制、同步落盘后移除源文件；出现不同内容的两个副本时保留两者并报错。Claude 的原生会话和恢复副本全部位于 Claude 配置目录，不写入书库。

## 中断与本地记录

回答只有在提供方完成、引用有效且数据库提交成功后才成为正式聊天记录。每轮提交前保存一份原生 transcript，数据库在保存回答的同一事务中指向它。若退出、取消或保存失败导致原生会话包含未提交内容，下次从恢复副本接续，再补入本地仍未回答的问题。新一轮开始前更新恢复点，以保留用户在可见会话中的外部续写。

最近一次已提交的原生副本保存在原项目的 `claude-history-hidden/committed/`，避免 Claude 自身的历史保留策略清理可见文件后丢失工具上下文。原生文件和副本都不可用，或文件位置属于其他机器时，使用 NeoRead 保存的问答、引用和工具记录重新建立原生会话；这时此前未保存到这些记录中的工具内容需要重新读取。

原生文件及其本机归属不随书库 ZIP、独立书库复制或 WebDAV 迁移，也不进入诊断包。迁移后的书库保留可见聊天和引用，首次提问从这些记录接续。旧版本的 Claude 对话首次使用新版本时同样从已有 NeoRead 记录建立原生会话。

## 验证

自动回归覆盖单文档和书库的隐藏/恢复、重启后使用同一原生会话、工具结果与引用保留、生成或保存失败后的恢复、生成中切换开关及独立书库的归属隔离。参数测试分别断言 Agent 对话开启持久化、独立任务保留 `--no-session-persistence`。

本机安装的 Claude CLI 可通过 loopback 模拟模型和合成文档验证，不需要真实提供方凭据或付费调用：

```sh
NEOREAD_CLAUDE_HISTORY_INSTALLED=1 go -C apps/server test ./internal/neoread -run '^TestClaudeNativeHistoryInstalledCLI$' -count=1 -v
```

人工验收：用 Claude 在文档或书库中提问并读取原文；开启隐藏后确认普通 Claude 历史中不再出现该会话；完全退出 NeoRead 再打开并追问此前的内容，核对引用；关闭隐藏后确认完整会话重新可见。界面和 Claude 历史选择器采用人工验收。

协议依据：[Claude CLI 参数](https://code.claude.com/docs/en/cli-reference#flags)、[会话与 transcript](https://code.claude.com/docs/en/sessions)。
