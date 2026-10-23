package tools

import "encoding/json"

// ToolCatalogItem describes a native tool available in the agent framework.
type ToolCatalogItem struct {
	Name            string          `json:"name"`
	Category        string          `json:"category"`
	CategoryName    string          `json:"category_name"`
	Description     string          `json:"description"`
	Roles           []string        `json:"roles"`
	DefaultProfiles []string        `json:"default_profiles,omitempty"`
	Parameters      json.RawMessage `json:"parameters,omitempty"`
}

// GetNativeToolCatalog returns the full list of built-in native tools supported by the platform.
func GetNativeToolCatalog() []ToolCatalogItem {
	return []ToolCatalogItem{
		// Category: discovery (资产与端点探测)
		{
			Name:            "get_endpoint",
			Category:        "discovery",
			CategoryName:    "资产与端点探测",
			Description:     "抓取指定资产端点的 HTTP/HTTPS 响应内容并记录访问快照",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser", "http_readonly"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"asset_id": { "type": "string", "description": "资产的 UUID 标识" },
					"path": { "type": "string", "description": "相对端点路径（可选）" }
				},
				"required": ["asset_id"]
			}`),
		},
		{
			Name:            "extract_endpoint_links",
			Category:        "discovery",
			CategoryName:    "资产与端点探测",
			Description:     "从已执行成功的 HTML 响应中解析并提取新的 HTTP 端点超链接",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser", "http_readonly"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"source_tool_call_id": { "type": "string", "description": "关联的 get_endpoint 或 http_request 调用的工具执行 ID" },
					"base_url": { "type": "string", "description": "页面基准 URL，用于解析相对路径" }
				},
				"required": ["source_tool_call_id"]
			}`),
		},
		{
			Name:            "extract_api_endpoints",
			Category:        "discovery",
			CategoryName:    "资产与端点探测",
			Description:     "从响应文本、JS 脚本或 API 文档中智能提取 RESTful 路由与后端端点",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"source_tool_call_id": { "type": "string", "description": "关联的上游响应调用 ID" },
					"endpoints": {
						"type": "array",
						"items": { "type": "string" },
						"description": "提取出的 API 路径列表"
					}
				},
				"required": ["source_tool_call_id"]
			}`),
		},
		{
			Name:            "register_asset",
			Category:        "discovery",
			CategoryName:    "资产与端点探测",
			Description:     "将确认的新资产、网络服务或关键端点注册入库，纳入后续探索范围",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"type": { "type": "string", "enum": ["domain", "ip", "url", "endpoint"], "description": "资产类型" },
					"value": { "type": "string", "description": "资产标识（如域名、IP或URL路径）" },
					"metadata": { "type": "object", "description": "附加资产元数据与标签" }
				},
				"required": ["type", "value"]
			}`),
		},
		{
			Name:            "record_observation",
			Category:        "discovery",
			CategoryName:    "资产与端点探测",
			Description:     "记录单条针对资产或目标的结构化观察记录（状态、响应特征与上下文）",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser", "http_readonly", "sample"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"target": { "type": "string", "description": "目标端点或资产名称" },
					"content_summary": { "type": "string", "description": "观察简要结论（要求简体中文）" },
					"details": { "type": "object", "description": "详细技术指标与响应指纹" }
				},
				"required": ["target", "content_summary"]
			}`),
		},
		{
			Name:            "record_observation_batch",
			Category:        "discovery",
			CategoryName:    "资产与端点探测",
			Description:     "批量提交多条结构化观察项，提高探测记录吞吐效率",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"observations": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"target": { "type": "string" },
								"content_summary": { "type": "string" },
								"details": { "type": "object" }
							},
							"required": ["target", "content_summary"]
						}
					}
				},
				"required": ["observations"]
			}`),
		},
		{
			Name:            "read_sample",
			Category:        "discovery",
			CategoryName:    "资产与端点探测",
			Description:     "读取范围内的安全测试用例、已知漏洞复现样本或本地离线数据",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "sample"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"sample_id": { "type": "string", "description": "安全样本的唯一标识" }
				},
				"required": ["sample_id"]
			}`),
		},

		// Category: traffic (HTTP 与网络流量)
		{
			Name:            "http_request",
			Category:        "traffic",
			CategoryName:    "HTTP 与网络交互",
			Description:     "执行高度可定制的 HTTP/HTTPS 请求（支持各类请求方法、请求头与请求载荷）",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"url": { "type": "string", "description": "目标 URL" },
					"method": { "type": "string", "enum": ["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"], "default": "GET" },
					"headers": { "type": "object", "description": "自定义 HTTP 请求标头" },
					"body": { "type": "string", "description": "请求体内容" }
				},
				"required": ["url"]
			}`),
		},
		{
			Name:            "search_http_traffic",
			Category:        "traffic",
			CategoryName:    "HTTP 与网络交互",
			Description:     "按 URL 关键字、HTTP 方法、状态码或响应内容全文检索捕获的通信流量",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": { "type": "string", "description": "检索关键字" },
					"method": { "type": "string", "description": "过滤 HTTP 方法" },
					"status_code": { "type": "integer", "description": "过滤 HTTP 响应状态码" },
					"limit": { "type": "integer", "description": "返回条目上限", "default": 20 }
				}
			}`),
		},
		{
			Name:            "get_http_exchange",
			Category:        "traffic",
			CategoryName:    "HTTP 与网络交互",
			Description:     "获取特定网络交互的完整请求头、请求体、响应头与响应正文详情",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"exchange_id": { "type": "string", "description": "HTTP 交互记录 ID" }
				},
				"required": ["exchange_id"]
			}`),
		},

		// Category: knowledge (知识图谱与态势)
		{
			Name:            "record_fact",
			Category:        "knowledge",
			CategoryName:    "知识图谱与态势",
			Description:     "向全局探索知识图谱持久化关键事实，自动驱动 Planner 态势感知与画板联动",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"summary": { "type": "string", "description": "一句话核心事实结论（必须简体中文）" },
					"detail": { "type": "string", "description": "详细技术事实描述（简体中文）" },
					"target": { "type": "string", "description": "关联的目标资产或端点" }
				},
				"required": ["summary", "detail"]
			}`),
		},
		{
			Name:            "list_facts",
			Category:        "knowledge",
			CategoryName:    "知识图谱与态势",
			Description:     "列出当前任务知识图谱中已确立的事实项清单",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"limit": { "type": "integer", "description": "返回数量上限", "default": 50 }
				}
			}`),
		},
		{
			Name:         "record_hint",
			Category:     "knowledge",
			CategoryName: "知识图谱与态势",
			Description:  "记录横向探索线索、启发提示或待检验假设",
			Roles:        []string{"planner", "main_agent"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"hint": { "type": "string", "description": "线索内容与探索启发建议" }
				},
				"required": ["hint"]
			}`),
		},
		{
			Name:         "graph_overview",
			Category:     "knowledge",
			CategoryName: "知识图谱与态势",
			Description:  "获取当前探索链路拓扑图的宏观概览（节点数、边数、连通态势）",
			Roles:        []string{"planner", "main_agent"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		{
			Name:            "node_detail",
			Category:        "knowledge",
			CategoryName:    "知识图谱与态势",
			Description:     "获取探索链路图上特定节点的详细信息及其所有上下游连接",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"node_id": { "type": "string", "description": "图节点唯一标识 ID" }
				},
				"required": ["node_id"]
			}`),
		},

		// Category: finding (漏洞与安全发现)
		{
			Name:            "report_finding",
			Category:        "finding",
			CategoryName:    "漏洞与安全发现",
			Description:     "上报有实际影响及复现证据的漏洞或具体待验证线索；配置基线、版本指纹和通用错误页用 record_fact",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"title": { "type": "string", "description": "漏洞简明标题（必须简体中文）" },
					"category": { "type": "string", "description": "实际漏洞机理，例如 path_traversal 或 auth_bypass" },
					"source_kind": { "type": "string", "enum": ["empirical", "speculative"] },
					"severity": { "type": "string", "enum": ["low", "medium", "high", "critical"], "description": "严重等级" },
					"description": { "type": "string", "description": "漏洞详细成因及危害（简体中文）" },
					"remediation": { "type": "string", "description": "修复建议（简体中文）" },
					"asset_id": { "type": "string", "description": "受影响目标资产 UUID" }
				},
				"required": ["title", "category", "severity", "description", "asset_id", "source_kind"]
			}`),
		},
		{
			Name:         "run_security_check",
			Category:     "finding",
			CategoryName: "漏洞与安全发现",
			Description:  "可选的历史配置审查适配器，须显式启用；不属于默认自主探索流程",
			Roles:        []string{"worker"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"check_id": { "type": "string", "description": "已注册的配置审查规则 ID" },
					"asset_id": { "type": "string", "description": "被测资产 UUID" }
				},
				"required": ["check_id", "asset_id"]
			}`),
		},

		// Category: supervision (多 Agent 编排与协同)
		{
			Name:         "steer_work",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "对执行偏离目标或卡滞的 Worker 工作项进行动态航向纠偏与指导",
			Roles:        []string{"planner", "main_agent"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"work_item_id": { "type": "string", "description": "目标 WorkItem UUID" },
					"instruction": { "type": "string", "description": "纠偏指示说明" }
				},
				"required": ["work_item_id", "instruction"]
			}`),
		},
		{
			Name:         "kill_work",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "终止指定的 Worker 运行实例，释放并发执行资源",
			Roles:        []string{"planner", "main_agent"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"work_item_id": { "type": "string", "description": "目标 WorkItem UUID" },
					"reason": { "type": "string", "description": "终止原因" }
				},
				"required": ["work_item_id", "reason"]
			}`),
		},
		{
			Name:         "add_intent",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "分解并下发新的探索意图工作项（支持指定 Capability Profile 与输入参数）",
			Roles:        []string{"planner", "main_agent"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"title": { "type": "string", "description": "意图工作项标题" },
					"profile": { "type": "string", "description": "绑定的能力 Profile（如 explore, api_recon）" },
					"objective": { "type": "string", "description": "意图核心目标" }
				},
				"required": ["title", "objective"]
			}`),
		},
		{
			Name:         "mark_todo",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "更新 Planner 跨轮待办清单项的状态（待办/进行中/完成/废弃）",
			Roles:        []string{"planner"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"todo_id": { "type": "string", "description": "待办项标识" },
					"status": { "type": "string", "enum": ["pending", "in_progress", "done", "cancelled"] }
				},
				"required": ["todo_id", "status"]
			}`),
		},
		{
			Name:         "prove_goal",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "提交预设任务目标的达成证明与证据链条，将目标标记为已满足",
			Roles:        []string{"planner"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"goal_id": { "type": "string", "description": "目标 ID" },
					"proof": { "type": "string", "description": "证明依据与推导证据" }
				},
				"required": ["goal_id", "proof"]
			}`),
		},
		{
			Name:         "continue_work",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "在保留历史上下文会话的前提下，唤醒并延续该 WorkItem 继续深挖",
			Roles:        []string{"planner"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"work_item_id": { "type": "string", "description": "工作项 ID" }
				},
				"required": ["work_item_id"]
			}`),
		},
		{
			Name:         "retry_work",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "重新发起先前执行失败或不充分的工作项",
			Roles:        []string{"planner"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"work_item_id": { "type": "string", "description": "工作项 ID" }
				},
				"required": ["work_item_id"]
			}`),
		},
		{
			Name:         "grant_budget",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "为当前任务或特定 WorkItem 追加执行配额（模型调用轮次、工具调用上限）",
			Roles:        []string{"planner", "main_agent"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"additional_model_calls": { "type": "integer", "description": "追加的模型调用次数" },
					"additional_tool_calls": { "type": "integer", "description": "追加的工具调用次数" }
				}
			}`),
		},
		{
			Name:         "start_epoch",
			Category:     "supervision",
			CategoryName: "多 Agent 编排与协同",
			Description:  "推进当前的执行纪元 (Epoch)，使先前的过期指令与状态平滑归档",
			Roles:        []string{"planner"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},

		// Category: artifact (工件与执行轨迹)
		{
			Name:            "write_artifact",
			Category:        "artifact",
			CategoryName:    "工件与执行轨迹",
			Description:     "在任务持久化存储中保存扫描成果、结构化数据或 POC 验证脚本",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"name": { "type": "string", "description": "工件名称" },
					"type": { "type": "string", "description": "工件格式类型（如 json, text, script）" },
					"content": { "type": "string", "description": "工件正文内容" }
				},
				"required": ["name", "content"]
			}`),
		},
		{
			Name:            "list_artifacts",
			Category:        "artifact",
			CategoryName:    "工件与执行轨迹",
			Description:     "获取当前执行任务产生的所有持久化工件文件列表",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		{
			Name:            "read_artifact",
			Category:        "artifact",
			CategoryName:    "工件与执行轨迹",
			Description:     "按工件 ID 读取完整的大文本或数据产物内容",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"artifact_id": { "type": "string", "description": "工件唯一标识 ID" }
				},
				"required": ["artifact_id"]
			}`),
		},
		{
			Name:            "list_worker_traces",
			Category:        "artifact",
			CategoryName:    "工件与执行轨迹",
			Description:     "列出参与本任务的所有 Worker 的执行轨迹摘要",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"limit": { "type": "integer", "description": "返回条目上限", "default": 20 }
				}
			}`),
		},
		{
			Name:            "get_worker_trace",
			Category:        "artifact",
			CategoryName:    "工件与执行轨迹",
			Description:     "检索特定 Worker 的详细执行过程（包含各步模型思考、工具调用与结果反馈）",
			Roles:           []string{"worker", "planner", "main_agent"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"work_item_id": { "type": "string", "description": "目标 WorkItem UUID" }
				},
				"required": ["work_item_id"]
			}`),
		},
		{
			Name:         "get_worker_output",
			Category:     "artifact",
			CategoryName: "工件与执行轨迹",
			Description:  "提取 Worker 执行结束后的最终交付总结文本",
			Roles:        []string{"planner", "main_agent"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"work_item_id": { "type": "string", "description": "目标 WorkItem UUID" }
				},
				"required": ["work_item_id"]
			}`),
		},

		// Category: skill (技能与沙箱执行)
		{
			Name:            "list_skills",
			Category:        "skill",
			CategoryName:    "技能与沙箱执行",
			Description:     "列出平台内置的渗透评估专有技能手册与 SOP 标准操作流程",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		{
			Name:            "read_skill",
			Category:        "skill",
			CategoryName:    "技能与沙箱执行",
			Description:     "按技能名称读取专属评估流程指导细则与注意要点",
			Roles:           []string{"worker"},
			DefaultProfiles: []string{"explore", "api_recon", "active_probe", "browser"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"skill_name": { "type": "string", "description": "技能名称" }
				},
				"required": ["skill_name"]
			}`),
		},
		{
			Name:         "run_command",
			Category:     "skill",
			CategoryName: "技能与沙箱执行",
			Description:  "在隔离工作空间目录中执行安全的系统命令（需要任务启用命令执行权限）",
			Roles:        []string{"worker"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": { "type": "string", "description": "要执行的命令行指令" },
					"timeout_seconds": { "type": "integer", "description": "超时秒数", "default": 30 }
				},
				"required": ["command"]
			}`),
		},
		{
			Name:         "write_file",
			Category:     "skill",
			CategoryName: "技能与沙箱执行",
			Description:  "在工作空间中创建或覆盖指定文件",
			Roles:        []string{"worker"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": { "type": "string", "description": "相对文件路径" },
					"content": { "type": "string", "description": "文件写入内容" }
				},
				"required": ["path", "content"]
			}`),
		},
		{
			Name:         "read_file",
			Category:     "skill",
			CategoryName: "技能与沙箱执行",
			Description:  "读取工作空间中指定文件的文本内容",
			Roles:        []string{"worker"},
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": { "type": "string", "description": "相对文件路径" }
				},
				"required": ["path"]
			}`),
		},
	}
}
