---
name: api-recon
description: 收集并侦察目标网站后端API接口、提取前端脚本与参数结构
---

# API Reconnaissance (前端与后端接口侦察)

在已授权范围内，尽可能完整地发现目标站点的后端 API 路由、HTTP 方法与参数面，将静态候选与动态流量转化为图谱资产。

## 核心任务与边界

| 允许行为 | 禁止与红线 |
| --- | --- |
| 静态 JS bundle 路由与端点提取 | 对生产数据执行批量删除或不可逆操作 |
| 解析 HTML script 标签与内联 fetch/axios | 凭空臆造未经实测的 API 接口 |
| 区分方法（GET/POST/PUT/DELETE）并关联资产 | 越权访问超出授权范围的第三方域名 |

## 阶段工作流

### 第一阶段：静态资产与脚本侦查
1. 使用 `http_request` 或 `get_endpoint` 获取页面初始 HTML。
2. 调用 `extract_api_endpoints`：
   - 自动提取页面中的 `<script src="...">` 资源并入图；
   - 扫描内联 JS 代码与 Axios/Fetch 调用，登记 `/api/...`、`/v1/...` 等候选端点；
   - 自动识别对应 HTTP 方法并写入资产元数据。
3. 对发现的 JavaScript 文件（如 `/assets/app.js`）发起探测，再次调用 `extract_api_endpoints` 挖掘全量前端 API 路由。

### 第二阶段：流量检索与端点核验
1. 在对目标端点进行主动请求前，**必须先调用** `search_http_traffic` 检查是否有同伴已请求过该资源。
2. 若已存在流量，调用 `get_http_exchange(receipt_id)` 读取脱敏正文与响应头，避免重复网络打靶。
3. 发现的新端点调用 `register_asset` 或 `extract_api_endpoints` 登记入图。

### 第三阶段：参数结构与结论写回
1. 提取 API 的参数结构（Query、JSON Body、Header Token）。
2. 将接口测绘结论汇总为一条结构化的 `record_fact`，并在 summary 中标明关键入口供规划者（Planner）分派下一步测试。
