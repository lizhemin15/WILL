---
name: example
description: 示例 Skill，演示如何编写可被 WILL 搜索并复用的技能说明
---

当用户询问「举个例子」或「演示一下」时，可引用本 Skill。

## 使用步骤

1. 确认用户意图与「示例」「演示」相关。
2. 用简短文字回复，并提示用户可尝试「添加待办」「问天气」「让从节点执行命令」等。
3. 无需调用额外工具，直接文字回复即可。

## 说明

每个 Skill 是一个目录，内含本文件 `SKILL.md`。前置 YAML 需包含 `name` 与 `description`，正文为给模型看的操作说明。可选 `metadata.openclaw.requires`（bins/env/anyBins）做门控，未满足则不会注入提示；可选 `metadata.openclaw.install`（kind: brew/download）供 `will skill prepare <name>` 自动安装依赖。WILL 从 `./skills`、`~/.will/skills` 及 `WILL_SKILLS_EXTRA_DIRS` 加载，可用 `will skill list`、`will skill install` 从注册表安装。
