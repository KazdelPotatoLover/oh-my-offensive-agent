---
name: finding-verification
description: 漏洞实证核验规范、PoC复现证据链与traffic_refs角色绑定方法
---

# Finding Verification & Evidence Chain (漏洞实证核验与证据链)

本规范定义了漏洞上报（`report_finding`）时的实证核验标准与证据链要求。**严禁把“指纹匹配到 CVE”或“参数看起来可疑”当作已确认漏洞**，必须具备实际可复现的触发依据。

## 证据链核心角色 (Evidence Roles)

在上报实测漏洞时，建议在 `evidence.traffic_refs` 中按顺序组织并标记关键报文的角色：

| 角色 (Role) | 定义与作用 | 典型示例 |
| --- | --- | --- |
| **baseline** | 正常未利用状态下的基线响应 | 未携带恶意载荷的正常请求、未越权时的正常用户数据或 401 响应 |
| **proof** | 触发漏洞或越权利用时的核心响应 | 成功越权获取越权主体数据、SQL 注入成功回显、任意文件读取返回 |
| **verification** | 采用独立方法或不同载荷进行的复核验证 | 采用二次变形编码、不同身份或只读参数再次触发以排除误判 |
| **supporting** | 辅助证明材料 | 报错堆栈、相关静态 JS 脚本片段、接口定义依据 |

## 上报纪律

1. **真实请求绑定**：
   - `source_tool_call_id` 必须填写产生核心证据的真实工具调用 ID（`_execution.source_tool_call_id`）。
   - 服务端会基于可信收据（receipt）与响应哈希校验该调用的真实性。
2. **对抗式自检**：
   - 在调用 `report_finding` 之前，先以怀疑者视角复查：这个响应是不是通用 200？是不是被 WAF 软拦截了但返回了类似内容？
   - 必须有独立一次触发证实后方可定案。
3. **无法触发但有嫌疑时的处理**：
   - 若怀疑存在问题但未能构造出有效利用，**禁止强行 report_finding**。
   - 使用 `record_fact` 并标记 confidence 为 `inferred`，在 summary 和 detail 中记录嫌疑点与未触发原因，交由规划者评估后续方向。
