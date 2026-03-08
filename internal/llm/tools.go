package llm

// toolDefs 是传给 LLM 的 Function Calling 工具列表（OpenAI tools 格式）
var toolDefs = []map[string]interface{}{
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "todo_list",
			"description": "查看当前用户的待办事项列表",
			"parameters":  emptyParams(),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "todo_add",
			"description": "添加一条待办事项",
			"parameters": objectParams(map[string]interface{}{
				"title": strParam("待办内容"),
			}, "title"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "todo_done",
			"description": "将一条或多条待办标为已完成，indices 为1开始的序号，多个用逗号分隔如 \"1,2\"",
			"parameters": objectParams(map[string]interface{}{
				"indices": strParam("待办序号，1开始，多个逗号分隔，如 \"1\" 或 \"1,2,3\""),
			}, "indices"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "todo_update",
			"description": "修改某条待办的标题内容，不会删除重建，直接在原条目上编辑。用户说「改」「修改」「更新」「把第X条改成」时使用此工具",
			"parameters": objectParams(map[string]interface{}{
				"index":     strParam("待办序号，1开始的数字"),
				"new_title": strParam("修改后的待办标题"),
			}, "index", "new_title"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "todo_delete",
			"description": "删除一条或多条待办，indices 为1开始的序号，多个用逗号分隔",
			"parameters": objectParams(map[string]interface{}{
				"indices": strParam("待办序号，1开始，多个逗号分隔"),
			}, "indices"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "version_check",
			"description": "检查是否有新版本，并提示用户是否更新",
			"parameters":  emptyParams(),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "schedule_list",
			"description": "查看用户的定时任务列表",
			"parameters":  emptyParams(),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name": "schedule_add",
			"description": `添加一个定时任务。使用标准5字段 cron 表达式（分 时 日 月 周）指定执行时间。
示例：
- 每天9点 → "0 9 * * *"
- 每天11:30 → "30 11 * * *"  
- 每天6点和21点 → 调用两次，分别用 "0 6 * * *" 和 "0 21 * * *"
- 每4小时 → "0 */4 * * *"
- 每小时 → "0 * * * *"
如果用户指定多个时间点，请多次调用此工具。
执行时系统会自动注入用户的待办列表和最近对话，instruction 只需描述任务目标。`,
			"parameters": objectParams(map[string]interface{}{
				"instruction": strParam("任务内容，如「作为严厉导师，分析当前待办和近期对话，给出下一步建议」"),
				"cron_expr":   strParam("标准5字段cron表达式，如 \"0 9 * * *\"（每天9点）"),
			}, "instruction", "cron_expr"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "schedule_delete",
			"description": "删除一个定时任务",
			"parameters": objectParams(map[string]interface{}{
				"id": strParam("要删除的任务编号（数字字符串）"),
			}, "id"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "schedule_update",
			"description": "修改定时任务的内容或执行时间",
			"parameters": objectParams(map[string]interface{}{
				"id":          strParam("任务编号"),
				"instruction": strParam("新的任务内容（不改则传空字符串）"),
				"cron_expr":   strParam("新的cron表达式（不改则传空字符串）"),
			}, "id"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "schedule_run_now",
			"description": "立即执行指定的定时任务并返回结果，用于用户说「马上执行」「立即运行」「现在执行定时任务X」的情况",
			"parameters": objectParams(map[string]interface{}{
				"id": strParam("要立即执行的任务编号"),
			}, "id"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "memory_set",
			"description": "记住用户说的某个偏好、信息或上下文，下次对话时可使用",
			"parameters": objectParams(map[string]interface{}{
				"key":   strParam("记忆的键名，如 \"角色\" \"偏好\" \"项目\""),
				"value": strParam("要记住的内容"),
			}, "key", "value"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "config_change",
			"description": "修改系统配置（如 LLM API key、飞书凭证等），会先向用户确认再生效",
			"parameters": objectParams(map[string]interface{}{
				"key":   strParam("配置键名：llm_api_key / llm_base_url / llm_model / feishu_app_id / feishu_app_secret / timezone / mode"),
				"value": strParam("新值"),
			}, "key", "value"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "shell_exec",
			"description": "在当前节点执行 shell 命令并返回输出",
			"parameters": objectParams(map[string]interface{}{
				"command": strParam("要执行的 shell 命令"),
			}, "command"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "worker_list",
			"description": "列出所有已连接的从节点（子机器人）名称和状态，主节点模式下可用",
			"parameters":  emptyParams(),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "worker_exec",
			"description": "在指定名称的从节点上执行 shell 命令，用于控制其他机器上的机器人。用户说「让<名称>执行」「在<名称>上运行」时使用此工具",
			"parameters": objectParams(map[string]interface{}{
				"worker_name": strParam("从节点名称，必须与 worker_list 返回的名称完全一致"),
				"command":     strParam("要在从节点上执行的 shell 命令"),
			}, "worker_name", "command"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "worker_update",
			"description": "触发指定从节点（子机器人）升级到最新版本，升级完成后从节点会自动重启并重新连接",
			"parameters": objectParams(map[string]interface{}{
				"worker_name": strParam("要升级的从节点名称，必须与 worker_list 返回的名称完全一致"),
			}, "worker_name"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "skill_list_local",
			"description": "列出当前已加载的 Skill（含未就绪的及原因）。用户说「有哪些 skill」「已安装的技能」「技能列表」时使用",
			"parameters":  emptyParams(),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "skill_list_remote",
			"description": "从注册表拉取并列出可安装的 Skill。用户说「可安装的 skill」「从注册表看看」「能装哪些技能」时使用",
			"parameters":  emptyParams(),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "skill_install",
			"description": "安装 Skill：从注册表按名称安装，或从 zip/tar.gz 链接直接安装。用户说「安装 skill xxx」「装一个叫 xxx 的技能」「从链接安装」时使用",
			"parameters": objectParams(map[string]interface{}{
				"name_or_url": strParam("Skill 名称（从注册表安装）或完整的 http(s) 链接（zip/tar.gz 直接安装）"),
			}, "name_or_url"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "skill_prepare",
			"description": "为指定 Skill 安装依赖（如执行 brew install）。用户说「给 xxx 装依赖」「准备 xxx 环境」「修复 xxx 未就绪」时使用",
			"parameters": objectParams(map[string]interface{}{
				"name": strParam("Skill 名称，与 skill_list_local 返回的名称一致"),
			}, "name"),
		},
	},
	{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "skill_update",
			"description": "从注册表批量更新已安装的 Skill。用户说「更新所有 skill」「升级技能」「拉取最新 skill」时使用",
			"parameters":  emptyParams(),
		},
	},
}

func emptyParams() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func strParam(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func objectParams(props map[string]interface{}, required ...string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}
