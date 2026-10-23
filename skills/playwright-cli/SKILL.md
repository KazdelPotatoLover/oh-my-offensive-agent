---
name: playwright-cli
description: 无头浏览器交互自动化、动态单页应用探索、DOM元素定位与表单测试
---

# Browser Automation with playwright-cli

本技能用于现代 Web 应用（SPA、Vue/React 前端路由、登录表单、动态表格）的无头浏览器交互与动态攻击面探测。

## 适用场景
1. **单页应用（SPA）探索**：纯前端路由与异步渲染，常规静态抓取看不到内层接口与按钮。
2. **表单填写与认证会话**：模拟用户输入、表单提交、获取登录凭证或会话 Cookie。
3. **前端网络流量抓取与路由拦截**：记录前端发起的 outbound 请求，提取隐藏 API。
4. **DOM 操作与视觉验证**：按钮点击、弹窗处理、截屏取证 (`screenshot`)。

## 核心命令流

```bash
# 1. 打开无头浏览器并导航至目标
playwright-cli open https://target.local/login

# 2. 获取当前 DOM 结构快照（包含元素引用编号 e1, e2, ...）
playwright-cli snapshot

# 3. 元素交互（使用快照中的编号，如 e1, e2）
playwright-cli fill e1 "admin"
playwright-cli fill e2 "password"
playwright-cli click e3

# 4. 再次获取快照核对登录后页面
playwright-cli snapshot

# 5. 查找页面内容或正则匹配
playwright-cli find "Dashboard"
playwright-cli find --regex "/admin\/api\//i"

# 6. 保存或截取证据图片（保存至工作空间）
playwright-cli screenshot --filename=evidence.png

# 7. 关闭浏览器会话
playwright-cli close
```

## 会话与凭证管理

```bash
# 保存登录状态（Cookies 与 LocalStorage）
playwright-cli state-save auth.json

# 导出当前会话 Cookie
playwright-cli cookie-list

# 载入已有认证状态
playwright-cli state-load auth.json
```

## 与 Agent 执行环境集成

在具备 `run_command` 的隔离容器中：
1. 使用 `run_command(argv: ["playwright-cli", "open", "<url>"])` 启动。
2. 产物（截图、state json）必须保存在当前工作目录 `/workspace`，不可使用 `/tmp`。
3. 所有外发的 HTTP/HTTPS 请求均受受控网关记录，可通过 `search_http_traffic` 检索。
4. 探索完成后务必执行 `playwright-cli close` 释放浏览器进程。
