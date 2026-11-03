# Fork 功能分支

本 Fork 的 `main` 基于上游 [`71eddad`](https://github.com/cynicalight/NeoRead/commit/71eddad759644b2332ff2f81e444bcf1a532e762)，按功能形成普通提交，线性集成下列 13 组功能及下文的 Fork 性能体验改动；相对该基线没有新增 merge commit。02、05–08、11 的来源历史按最新前置整理，每组自身改动保留一个普通提交；11 保留一个汇合 09、10 的前置合并点。其他来源分支保持原有历史，所有 PR 编号保持不变。正文进度补齐了既有 PDF 缩放测试替身的可见页接口，迁移分支同步该前置修正；历史整理保持各组独立功能差异。

## 功能与上游 PR

| 组 | 功能 | 上游 PR | 前置 | 功能分支与独立差异 | main 提交 |
| --- | --- | --- | --- | --- | --- |
| 01 | 多标签生命周期 | [#214](https://github.com/cynicalight/NeoRead/pull/214) | 上游 main | [`codex/reader-tab-lifecycle`](https://github.com/DongShan03/NeoRead/tree/codex/reader-tab-lifecycle) · [差异](https://github.com/DongShan03/NeoRead/compare/71eddad759644b2332ff2f81e444bcf1a532e762...380538bf3ae3dd3c286b0d5b799325babdb0daba) | [`a52ded4`](https://github.com/DongShan03/NeoRead/commit/a52ded45250f836cf0b4f88e8128c250810da3dd) |
| 02 | PDF 渲染与图片预览 | [#228](https://github.com/cynicalight/NeoRead/pull/228) | #214 | [`codex/rendering-and-image-preview`](https://github.com/DongShan03/NeoRead/tree/codex/rendering-and-image-preview) · [差异](https://github.com/DongShan03/NeoRead/compare/380538bf3ae3dd3c286b0d5b799325babdb0daba...e4d972ad6c03fb2c4a9256f8dc09ee1f46931512) | [`1de216d`](https://github.com/DongShan03/NeoRead/commit/1de216d424a5859c213e42cded21c1d7af72bf62) |
| 03 | Windows Codex 启动 | [#215](https://github.com/cynicalight/NeoRead/pull/215) | 上游 main | [`codex/windows-codex-launcher`](https://github.com/DongShan03/NeoRead/tree/codex/windows-codex-launcher) · [差异](https://github.com/DongShan03/NeoRead/compare/71eddad759644b2332ff2f81e444bcf1a532e762...90290c7d3ae47531463cb6a3bbbe7cd7e46f48f9) | [`e0e2a74`](https://github.com/DongShan03/NeoRead/commit/e0e2a74891b39d082f43a4adc15a45bb318ad28a) |
| 04 | API Agent 切换 | [#222](https://github.com/cynicalight/NeoRead/pull/222) | 上游 main | [`codex/api-agent-switch`](https://github.com/DongShan03/NeoRead/tree/codex/api-agent-switch) · [差异](https://github.com/DongShan03/NeoRead/compare/71eddad759644b2332ff2f81e444bcf1a532e762...cad28a9cd6fdfa50c7237ecfb1319305faa48a01) | [`9618ad7`](https://github.com/DongShan03/NeoRead/commit/9618ad72fcae5cb84e0b462fbeac0cb461aed40a) |
| 05 | 公式复制与引用 | [#229](https://github.com/cynicalight/NeoRead/pull/229) | #228 | [`codex/formula-clipboard`](https://github.com/DongShan03/NeoRead/tree/codex/formula-clipboard) · [差异](https://github.com/DongShan03/NeoRead/compare/e4d972ad6c03fb2c4a9256f8dc09ee1f46931512...209d9593cc523966b655acf27d5294bc714bfcee) | [`ffa468d`](https://github.com/DongShan03/NeoRead/commit/ffa468d34e5fca9784409c3639ac7432a24e28cc) |
| 06 | 整篇译文缩放 | [#230](https://github.com/cynicalight/NeoRead/pull/230) | #228 | [`codex/translation-zoom`](https://github.com/DongShan03/NeoRead/tree/codex/translation-zoom) · [差异](https://github.com/DongShan03/NeoRead/compare/e4d972ad6c03fb2c4a9256f8dc09ee1f46931512...2ccfb3cf5e23039cb6dbe9d8e4a487e986bb2e37) | [`17c7597`](https://github.com/DongShan03/NeoRead/commit/17c75972f367e756780c899e6640b17e5fa76280) |
| 07 | 划词翻译浮窗 | [#231](https://github.com/cynicalight/NeoRead/pull/231) | #228 | [`codex/selection-translation-popover`](https://github.com/DongShan03/NeoRead/tree/codex/selection-translation-popover) · [差异](https://github.com/DongShan03/NeoRead/compare/e4d972ad6c03fb2c4a9256f8dc09ee1f46931512...a38fa01dfc91c948c88102a3866a32ebea207542) | [`f7b2575`](https://github.com/DongShan03/NeoRead/commit/f7b25751a9a4aff31df70a915bee7d562d40d893) |
| 08 | 原文译文对齐手势 | [#232](https://github.com/cynicalight/NeoRead/pull/232) | #228 | [`codex/reading-alignment-gesture`](https://github.com/DongShan03/NeoRead/tree/codex/reading-alignment-gesture) · [差异](https://github.com/DongShan03/NeoRead/compare/e4d972ad6c03fb2c4a9256f8dc09ee1f46931512...e14f49c5a03a1c208de2abad57c1fd9b9d621077) | [`b7fa67b`](https://github.com/DongShan03/NeoRead/commit/b7fa67bbff00d3d1f591fb12f509e52225e65a96) |
| 09 | 正文阅读进度与表格默认项 | [#223](https://github.com/cynicalight/NeoRead/pull/223) | 上游 main | [`codex/body-reading-progress`](https://github.com/DongShan03/NeoRead/tree/codex/body-reading-progress) · [差异](https://github.com/DongShan03/NeoRead/compare/71eddad759644b2332ff2f81e444bcf1a532e762...6985ce75952241df9645cd68fcc0b79db3899a77) | [`e951429`](https://github.com/DongShan03/NeoRead/commit/e951429726ad68ed8fb9f8052281d860a35e431e) |
| 10 | 聊天编辑、撤回与草稿 | [#224](https://github.com/cynicalight/NeoRead/pull/224) | 上游 main | [`codex/chat-message-editing`](https://github.com/DongShan03/NeoRead/tree/codex/chat-message-editing) · [差异](https://github.com/DongShan03/NeoRead/compare/71eddad759644b2332ff2f81e444bcf1a532e762...d8c57480473f12a8a5d2574aa9377a65ea40a396) | [`b140338`](https://github.com/DongShan03/NeoRead/commit/b1403383fae8039847146da2e9d53dde5bfed65c) |
| 11 | ZIP 迁移与多书库管理 | [#227](https://github.com/cynicalight/NeoRead/pull/227) | #223、#224 | [`codex/library-transfer-and-management`](https://github.com/DongShan03/NeoRead/tree/codex/library-transfer-and-management) · [差异](https://github.com/DongShan03/NeoRead/compare/1905fead066a7557c5da1e8c5137c0c1189297bd...c9c93cd64beab7b715a4a619800a75f0345d7ae5) | [`d43213c`](https://github.com/DongShan03/NeoRead/commit/d43213c7240057fca8938a9c45f90df9e2cb752d) |
| 12 | 原生标题栏交互 | [#225](https://github.com/cynicalight/NeoRead/pull/225) | 上游 main | [`codex/native-titlebar-interaction`](https://github.com/DongShan03/NeoRead/tree/codex/native-titlebar-interaction) · [差异](https://github.com/DongShan03/NeoRead/compare/71eddad759644b2332ff2f81e444bcf1a532e762...e88b5f7a61ded85af9c7aaff3c950a08ded0f74d) | [`d8709e3`](https://github.com/DongShan03/NeoRead/commit/d8709e3b61d101f33ceb4ac76e1f915196294b56) |
| 13 | 书库导航侧栏宽度 | [#226](https://github.com/cynicalight/NeoRead/pull/226) | 上游 main | [`codex/library-sidebar-resize`](https://github.com/DongShan03/NeoRead/tree/codex/library-sidebar-resize) · [差异](https://github.com/DongShan03/NeoRead/compare/71eddad759644b2332ff2f81e444bcf1a532e762...ad29d9e518b11545d1f6af84a1f950f4aa4f41f4) | [`5f15bbc`](https://github.com/DongShan03/NeoRead/commit/5f15bbc53186fb06150781082fa21b6a58eb9503) |

有前置的 PR 保持草稿。前置合入上游后，在对应功能分支普通合并最新上游 `main`，更新独立差异说明，再转为正式审核。02 的四个后续分支相互独立；11 的独立差异基准是 09、10 最新版本的合并点。前置发生多项修复时，先完成前置的验证，再集中同步后续分支，避免每次修复都传播一轮合并节点。已发布来源历史的再次整理须取得明确授权，并使用精确租约推送。

[#214](https://github.com/cynicalight/NeoRead/pull/214) 的远端来源仍为 `codex/pr-01-reader-tab-lifecycle`，与语义分支同步；[#215](https://github.com/cynicalight/NeoRead/pull/215) 的来源仍为 `codex/pr-03-windows-codex`。保留这两条引用以延续现有审核。

## 待本地体验的 PDF 性能改动

Fork 性能分支 [`codex/pdf-viewport-ahead`](https://github.com/DongShan03/NeoRead/tree/codex/pdf-viewport-ahead) 的提交 [`b0ec405`](https://github.com/DongShan03/NeoRead/commit/b0ec40540af9c2d9f18478581571deaa76c83b18)，以 [`4912cfa`](https://github.com/DongShan03/NeoRead/commit/4912cfa2632359b85385358a4087d763dd5174df) 集成到包含全部功能的 `main`。原文优先绘制当前视口，再按视口高度准备上下各 100% 范围内的页面；译文高清图片使用同样的范围，连续跟随直接对齐，点击定位保留平滑动画。

本轮先同步本地和 Fork 的 `main` 供用户体验，流畅度由实际文档确认。新增性能改动暂未更新到上游 #228 及其后续 PR；上表仍指向现有审核版本。确认后再集中整理依赖和更新 PR，避免每个小修都传播一轮 merge。

## 后续维护

本地 `origin` 指向自己的 Fork，`upstream` 指向原仓库。先在功能分支完成修改、必要验证并更新对应上游 PR，再更新 `main` 的集成提交。来源分支只同步上游和声明的前置，保持各项功能可独立审核。

前置合入上游后的操作示例：

```sh
git fetch upstream main
git switch codex/translation-zoom
git merge upstream/main
pnpm typecheck
pnpm test
pnpm build
git push origin codex/translation-zoom
```

重建完整 `main` 时，从上游基线建立候选分支，按 01、03、04、09、10、12、13、02、05、06、07、08、11 的顺序，以各组独立差异形成普通提交。解决冲突时保留组合功能；上游已有完整等价实现的部分沿用上游。组合验证完成后再同步 Fork `main`，使用精确租约保护远端后续更新。

版本号、更新说明和构建发布流程沿用上游，发布要求见[构建与发布](releasing.md)。

## 手动验证

使用独立测试书库：

1. 连续打开多份 PDF，切换标签、返回书库再恢复，检查位置、草稿、后台聊天与原文画布恢复；见[多标签阅读](reading-tabs.md)。
2. 打开长 PDF，检查首屏、后页恢复、不同页尺寸、双语滚动、译文缩放、单击/双击对齐、公式复制与划词浮窗；原文和译文分别读到末尾后回翻，检查正文峰值不下降。性能体感与 CPU、内存变化需要人工测量；见[PDF 翻译](pdf-translation.md)。
3. 编辑/撤回聊天问题并检查引用和附件，再用测试书库验证 ZIP 导出、导入、后台传输和切库；见[书库迁移步骤](library-archive.md)。
