# Oh My Offensive Agent (OMOA)

<div align="center">

![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)
![Next.js](https://img.shields.io/badge/Next.js-14-black?style=flat&logo=next.js)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-16-336791?style=flat&logo=postgresql)
![Docker](https://img.shields.io/badge/Docker-Enabled-2496ED?style=flat&logo=docker)
![License](https://img.shields.io/badge/License-Apache--2.0-green.svg?style=flat)

**面向实战对抗的自主多智能体（Multi-Agent）安全渗透测试与攻防协同系统**

[特性亮点](#-核心特性) • [系统架构](#-系统架构) • [快速开始](#-快速开始) • [实战工作流](#-实战工作流) • [技术栈](#-技术栈) • [安全合规与免责声明](#-安全合规与免责声明)

</div>

---

## 📖 项目简介

**Oh My Offensive Agent (OMOA)** 是一个基于**认知探索图**与**事件驱动调度机制**构建的工业级自主多智能体攻防对抗平台。

传统的自动化安全工具往往局限于固定的规则扫描，而初级 LLM 安全 Agent 经常面临“单线串行效率低、Token 爆炸导致任务中断、缺乏实证产生虚假漏洞幻觉、Worker 各自为战重复发包”等工程瓶颈。

OMOA 参考先进的自主攻防智能体架构理念（如 ARTEX 等），从底层重构了攻防任务的状态流转、多 Agent 分工协同与证据链存证体系，实现了**战略规划（Lead Planner）**、**战术渗透（Domain Worker）**与**统一调度（Main Agent）**的高效协同，形成从目标设定、资产侦查、漏洞验证到结构化复测的完整自动化闭环。

---

## ⚡ 核心痛点与 OMOA 解决思路

| 维度 | 传统自动化扫描 / 简易 Agent | Oh My Offensive Agent (OMOA) |
| :--- | :--- | :--- |
| **协作拓扑** | 单一线性流水线，或伪多智能体串行阻塞 | **事件驱动的 Lead Planner + N 槽位并发 Domain Worker**，解耦规划与执行 |
| **认知底座** | 简单的页面链接树，缺乏因果关联 | **物理资产图（Asset Graph）与认知探索图（Exploration Graph）解耦**的双图架构 |
| **漏洞真实性** | 仅凭模型文本自述，容易出现“200 OK 即漏洞”的幻觉 | **防幻觉事实网关（Fact Gate）与严格实证契约**：必须绑定真实成功的工具调用与资产一致性 |
| **流量协同** | Worker 日志孤岛，各自向目标重复发包，容易打草惊蛇 | **统一网关透明抓包**，支持全 Agent 共享 HTTP 流量与材料零冗余复用 |
| **执行安全** | 宿主机直接运行危险命令或 PoC 脚本 | **Execution Broker 隔离沙箱**，基于 Docker 容器与严格命令白名单（Python/Playwright 等） |
| **长任务容错** | 局部额度耗尽任务直接崩溃退出 | **分层预算控制与合成协议闭合（Synthetic Protocol Closure）**，支持跨 Attempt 会话无缝恢复 |
| **人在环路** | 仅能在任务结束后被动看报告 | **全生命周期受控交互**：支持运行时双屏障暂停、动态纠偏（Steer）、定点终止与预算追加 |

---

## 🌟 核心特性

### 1. 🤖 分层非对称多智能体协同（Planner-Worker Hierarchy）
- **Lead Planner（战略规划中枢）**：负责全局态势感知、目标拆解与动态派工。每轮规划采用全新轻量 Session，仅注入最新双图拓扑与持久化待办（Planner Todos），彻底告别历史上下文无限膨胀。具备事件防抖唤醒机制，避免高频空转。
- **Domain Worker（战术攻坚节点）**：具备 N 槽位独立并发执行能力，专注于特定意图（Intent）的纵深探测与漏洞利用。跨 Attempt 自动维护稳定会话。
- **共享模型准入池（Shared Model Admission）**：内置基于 Token Bucket 的并发限流机制，统一管理全系统对大语言模型的请求吞吐，并为规划调度中枢预留高优先级通道，防止多 Worker 并发时打爆上游 RPM/TPM 导致系统饥饿。

### 2. 🗺️ 物理资产图与认知探索图（Dual-Graph Architecture）
- **物理资产图（Asset Graph）**：严格归一化域名、IP、端口服务与 HTTP/API 端点物理实体。强制施加安全范围检查（Scope Gate），严禁向未经授权的资产发送探测流量。
- **认知探索图（Exploration Graph）**：以认知因果血缘为主线，维护 `Goal`（最终目标）、`Intent`（行动意图）、`Fact`（可复用事实）、`Hint`（线索假设）与 `Finding`（漏洞发现）的因果闭环。
- **全景可视化画布（Exploration Canvas）**：基于现代前端流图组件呈现，真实反映因果推导链路，不掩盖未验证节点，杜绝前端虚假连线。

### 3. 🛡️ 防幻觉事实网关与实证闭环（Anti-Hallucination & Verification）
- **事实置信度检验（Fact Gate）**：事实分为 `observed`（直接观察）与 `inferred`（推理猜测）。只有通过真实且成功的工具执行产出的事实才被标记为 `verified`，纯文本猜测一律降级。
- **四重实证校验矩阵**：智能体上报实证漏洞（Finding）时，系统自动核验：
  1. 必须引用真实且执行成功的工具调用；
  2. 请求参数中的资产与报告资产必须一致（防止将资产 A 的证据挪用至资产 B）；
  3. 执行代次（Execution Epoch）精确匹配，杜绝旧凭证充当新证据；
  4. 工具类型必须属于验证类操作。
- **独立结构化复测器（Retest Executor）**：由独立 Verifier 会话执行复测，拒绝基于自由文本匹配判定修复，通过状态码与报文特征结构化研判，完整保留审计历史。

### 4. 📡 跨智能体共享流量与材料复用（Shared HTTP Traffic）
- **透明流量存证与收据（Receipt）**：所有网络探测由统一网关透传捕获，对响应报文执行脱敏与 SHA-256 哈希存证，生成唯一的 `receipt_id`。
- **协同检索工具集**：提供 `search_http_traffic`、`get_http_exchange` 与 `read_artifact`。Worker A 在资产勘测时抓取的页面数据，Worker B 可直接在本地读取解析，无需二次发包，大幅减少对目标环境的冲击。

### 5. 🐳 安全沙箱与 SOP 领域技能树（Execution Broker & Skills）
- **隔离容器沙箱（OMOA Execution Broker）**：支持执行 Python、Node.js、cURL 以及 Headless Playwright 浏览器自动化。通过受限 Broker 服务中转，施加命令白名单、只读挂载与超时保护，绝不在宿主机裸跑外部脚本。
- **领域技能库（Skills System）**：内置标准化 SOP 规范（API 资产侦查、实证契约对齐、Playwright 交互指引等），智能体按需调阅，兼顾专业知识注入与上下文精简。

### 6. 🎛️ 人在环路（HITL）受控交互与统一工作台
- **运行时动态干预**：支持双屏障暂停（Pause）、安全恢复（Resume）、人工线索纠偏（Steer）、定点终止单个 Worker（Kill）以及动态追加全局或局部预算（Grant Budget）。
- **统一工作台（Workbench UI）**：基于 Next.js 打造的全景控制台，支持任务配置、实时拓扑图谱交互、事件流水监控、漏洞审核确认与复测追踪。

---

## 🏗️ 系统架构

```
┌────────────────────────────────────────────────────────────────────────┐
│                        统一攻防工作台 (Next.js 14 UI)                  │
│   任务大盘  •  探索拓扑画布  •  Worker 状态流  •  漏洞存证审核  •  HITL 控制台 │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │ HTTP / SSE
┌───────────────────────────────────▼────────────────────────────────────┐
│                  控制面与调度中枢 (Go Backend & Controller)              │
│  ┌──────────────────────┐  ┌────────────────────────────────────────┐  │
│  │   Lead Planner       │  │  Domain Worker Pool (N 并发槽位)        │  │
│  │ (态势感知/意图拆解)    │  │  (会话恢复 / 合成协议闭合 / 纵深利用)  │  │
│  └──────────┬───────────┘  └───────────────────┬────────────────────┘  │
│             │                                  │                       │
│  ┌──────────▼──────────────────────────────────▼─────────────────────┐  │
│  │    共享模型准入池 (Shared LLM Admission / Token Bucket / RPM 保护)   │  │
│  └──────────────────────────────────┬────────────────────────────────┘  │
│                                     │                                  │
│  ┌──────────────────────────────────▼────────────────────────────────┐  │
│  │    工具执行与存证网关 (Tool Registry / Traffic Capture / Gate)     │  │
│  │   • 范围拦截 (Scope Gate)            • 事实校验 (Fact Gate)         │  │
│  │   • 共享流量检索 (search_traffic)    • 存证哈希 (Artifact Receipt)  │  │
│  └──────────────┬───────────────────────────────────┬────────────────┘  │
└─────────────────┼───────────────────────────────────┼───────────────────┘
                  │                                   │
      ┌───────────▼───────────┐           ┌───────────▼───────────┐
      │   PostgreSQL 16 数据库 │           │  Execution Broker     │
      │ • 物理资产图 / 探索图  │           │  (受限 Docker 沙箱)   │
      │ • 状态机 / 审计轨迹   │           │ • Python3 / Node      │
      │ • 待办 / 事实 / 漏洞  │           │ • Playwright 无头浏览 │
      └───────────────────────┘           └───────────┬───────────┘
                                                      │
                                          ┌───────────▼───────────┐
                                          │ 授权测试目标 / Web 靶场 │
                                          └───────────────────────┘
```

---

## 🛠️ 技术栈

| 模块 | 核心技术 / 依赖 | 说明 |
| :--- | :--- | :--- |
| **后端架构** | Go 1.22+, Chi Router, pgx | 高性能并发控制、严格事务隔离与事件流驱动 |
| **前端平台** | Next.js 14 (App Router), React 18, Tailwind CSS | 现代化统一攻防工作台，响应式交互大盘 |
| **拓扑画布** | `@xyflow/react` (React Flow) | 双图联动与探索因果血缘全景可视化 |
| **数据持久化** | PostgreSQL 16 | 存储物理资产、认知因果图、执行快照与审计日志 |
| **执行沙箱** | Docker, Execution Broker, Playwright, Python 3 | 隔离执行 PoC、浏览器自动化与外部脚本 |
| **模型接入** | 标准 OpenAI API 规范接口 (Chat Completions) | 兼容主流大语言模型（如 GPT-4o、DeepSeek、Claude 等） |

---

## 🚀 快速开始

### 1. 环境准备
确保本机已安装以下基础环境：
- **Docker** 24.0+ 与 **Docker Compose** v2+
- **Python** 3.10+
- **Git**

### 2. 克隆项目与配置环境变量
```bash
git clone git@github.com:KazdelPotatoLover/oh-my-offensive-agent.git
cd oh-my-offensive-agent

# 复制环境变量配置模版
cp .env.example .env
```

编辑 `.env` 文件，填入你的大模型配置及端口（默认为安全的高位端口）：
```bash
# 模型接口配置（支持 OpenAI 兼容格式）
LLM_API_FORMAT=chat_completions
LLM_BASE_URL=https://api.openai.com/v1
LLM_MODEL=gpt-4o
LLM_API_KEY=your_actual_api_key_here

# 共享模型并发与流控
LLM_MAX_INFLIGHT=2
LLM_MAX_OUTPUT_TOKENS=4096
```

### 3. 初始化本地运行时凭据
运行脚本自动生成执行 Broker 所需的安全 Token 及宿主机工作目录：
```bash
python3 scripts/prepare-runtime.py
```

### 4. 一键构建并启动服务
使用项目提供的一键启动脚本启动所有容器服务（包含数据库、执行 Broker、沙箱镜像、后端 API 及工作台前端）：
```bash
./scripts/start-local.sh
```

也可以直接通过 Docker Compose 启动：
```bash
# 构建沙箱执行环境基础镜像
docker build -f deploy/execution/Dockerfile -t oma-executor:ma4 backend

# 启动整套环境
docker compose -p oh-my-offensive-agent up -d --build
```

### 5. 访问系统
服务启动完成后，可在浏览器中访问：
- 🖥️ **攻防工作台前端**：`http://localhost:23000`
- ⚙️ **后端 API 服务**：`http://localhost:28080`
- 🎯 **内置演练靶场服务（Fixture）**：`http://localhost:29090`
- 🗄️ **PostgreSQL 数据库**：`localhost:25432` (账号/密码: `postgres` / `postgres`)

### 6. 创建探索任务

打开工作台，点击“新任务”，填写任务目标和授权网站地址即可启动。网址可以包含路径；任务名会根据目标生成，模型默认使用服务器的 `.env` 配置。本地演示模式需要在“高级设置 → 开发与调试”中明确选择。

新探索任务默认使用 **3 个并发 Worker**，任务总时长默认不限。模型调用、工具调用、规划轮数和方向数量默认不设总上限。Worker 单轮 MaxTurns 默认不限，单轮时长默认 1200 秒；局部耗尽后进入独立收尾（默认 10 轮），再由 Planner 决定是否继续。专家仍可写明硬上限。

普通创建留空表示继承服务器设置。高级设置可以选择“不限时”，或填写具体分钟数。界面显示“不限时”或真实分钟数。

管理员可用可选环境变量调整**新探索任务**的默认值。正数是显式上限；已有 Run 保持创建时冻结的策略：

```dotenv
OMA_EXPLORATION_BUDGET='{"max_parallel_workers":5,"run_timeout_seconds":3600}'
OMA_AGENT_LIMITS='{"task_timeout_seconds":0}'
```

策略在创建时冻结为 `execution_input.execution_policy`（`agent_limits.v1`）。兼容的 `budget` 字段是它的投影。没有该版本的旧 Run 仍按原 BudgetConfig 执行。发现模式且未开启探索的任务继续使用历史有界授权。说明与回填命令见 [预算与探索血缘交付记录](docs/ARTEX预算与探索血缘纠偏-交付记录-2026-10-23.md)。

---

## 💻 本地调试开发模式

如果你希望在本地直接运行 Go 后端与 Next.js 前端进行代码调试：

```bash
# 1. 仅启动 PostgreSQL 数据库与隔离 Broker
docker compose up -d db fixture execution-broker

# 2. 启动 Go 后端 (终端 1)
cd backend
export DATABASE_URL='postgres://postgres:postgres@localhost:25432/agent_workbench?sslmode=disable'
export PORT=28080
export CORS_ORIGINS=http://localhost:23000
go run ./cmd/server

# 3. 启动前端 Dashboard (终端 2)
cd frontend
npm install
export NEXT_PUBLIC_API_URL=http://localhost:28080
npm run dev -- -p 23000
```

---

## 🔄 实战工作流

系统启动后，一个典型的自动化攻防实战任务流程如下：

```mermaid
sequenceDiagram
    autonumber
    actor User as 安全人员 (User)
    participant UI as 攻防工作台
    participant Controller as 控制中枢
    participant Planner as Lead Planner
    participant Worker as Domain Workers (N 并发)
    participant Broker as Execution Broker
    participant Target as 目标靶场环境

    User->>UI: 创建任务 (设置目标资产与授权 Scope)
    UI->>Controller: 初始化 Run (冻结输入快照与预算)
    Controller->>Planner: 触发初始战略规划
    Planner->>Planner: 拆解交付目标 (Goal) 与首批探测意图 (Intent)
    
    par 多 Worker 并发执行
        Controller->>Worker: 调度意图至空闲槽位
        Worker->>Broker: 发起探测请求 (HTTP / Playwright / 脚本)
        Broker->>Target: 发送受控探测流量 (受限于 Scope Gate)
        Target-->>Broker: 返回网络响应
        Broker-->>Worker: 透明抓包存证 (生成 Receipt & Artifact)
        Worker->>Worker: 提取新资产 / 沉淀可复用事实 (Fact)
    and 跨 Agent 材料复用
        Worker->>Worker: 通过 search_http_traffic 检索共享流量 (免重复发包)
    end

    Note over Worker: 发现高危漏洞行为
    Worker->>Controller: 提交实证漏洞 (ReportFinding)
    Controller->>Controller: 经过四重实证校验矩阵 (校验工具调用、代次与资产一致性)
    Controller-->>UI: 探索图实时更新 (标记 Verified Finding 并证明 Goal)

    opt 人在环路动态纠偏
        User->>UI: 发现探测走偏，点击 Steer / Pause
        UI->>Controller: 下发纠偏提示词或冻结执行
    end

    Controller->>Planner: 唤醒收尾与达成检查 (suggest_completion)
    Planner->>Controller: 确认所有目标已证明，结项
    UI->>User: 导出完整渗透测试报告与实证日志
```

---

## 📂 项目结构

```
oh-my-offensive-agent/
├── backend/                     # Go 后端核心引擎
│   ├── cmd/server/              # 服务入口与路由挂载
│   ├── internal/
│   │   ├── agent/               # Agent 运行时代次与会话控制
│   │   ├── controlplane/        # 控制面控制器 (Main Agent / 监督机制)
│   │   ├── execution/           # 执行环境安全与 Docker 沙箱交互
│   │   ├── handler/             # HTTP REST API 处理器
│   │   ├── model/               # 领域模型定义 (Task, Run, Finding, Fact 等)
│   │   ├── orchestration/       # Lead Planner 提示词、规划与待办管理
│   │   ├── provider/            # LLM Provider 接入与准入池流控
│   │   ├── service/             # 业务逻辑与状态机
│   │   ├── tools/               # 工具集 (HTTP、流量检索、漏洞验证、技能)
│   │   └── worker/              # Worker 管理器、调度器与会话恢复引擎
│   └── migrations/              # PostgreSQL 数据库迁移文件
├── frontend/                    # Next.js 14 前端工作台
│   ├── src/
│   │   ├── app/                 # App Router 路由 (工作台、任务、资产)
│   │   ├── components/          # UI 组件库 (拓扑画布、审查器、任务表单)
│   │   └── lib/                 # API 客户端与状态流管理
├── deploy/                      # 部署与容器化配置
│   └── execution/               # 隔离 Broker 与沙箱 Dockerfile
├── skills/                      # SOP 领域技能库 (API 勘查、漏洞验证、浏览器操作)
├── scripts/                     # 运维辅助与本地一键启动脚本
├── docker-compose.yml           # 全组件编排文件
└── .env.example                 # 环境变量配置模板
```

---

## 🔒 安全合规与免责声明

> [!WARNING]
> **使用前提：阅读并无条件遵守本声明**  
> **Oh My Offensive Agent (OMOA)** 是一款具备自主推理与自动化执行能力的攻防安全研究系统。在下载、部署、运行或修改本项目前，使用者必须详细阅读并严格遵守以下全部条款。任何使用行为均视为已完全理解并无保留接受本声明的全部约束。

### 1. 明确合法的书面授权（Mandatory Prior Authorization）
- **授权前置**：严禁在未取得目标系统、网络、服务及资产合法所有者或运营方**明确、正式的书面测试授权（Scope of Engagement / Written Authorization）**的情况下，使用本系统针对任何外部目标进行侦查、扫描、渗透或攻击活动。
- **作用域责任**：系统内部提供的 `Scope Gate`（作用域校验）和白名单机制属于工程防护辅助手段，**无法替代使用者的合规核查责任**。使用者必须对录入系统的所有测试资产边界承担完全的法律与事实核验义务，严防越界探测第三方非授权资产（如共享 CDN、云厂商公用基础设施、第三方 API 服务商等）。

### 2. 法律法规与监管合规义务（Legal Compliance）
使用者必须严格遵守所在司法管辖区以及目标系统所在国家与地区的现行法律法规，包括但不限于：
- 《中华人民共和国网络安全法》
- 《中华人民共和国数据安全法》
- 《中华人民共和国个人信息保护法》
- 《中华人民共和国反电信网络诈骗法》
- 《中华人民共和国刑法》（特别包含第 285 条【非法侵入计算机信息系统罪、非法获取计算机信息系统数据、非法控制计算机信息系统罪】、第 286 条【破坏计算机信息系统罪】及相关司法解释）
- 国际及地区的《计算机欺诈与滥用法案》（CFAA）、《通用数据保护条例》（GDPR）等相关网络安全与数据隐私法规。

**任何利用本系统从事违法犯罪活动的行为，均与本项目及全体贡献者无关，由此产生的一切法律责任由行为人全额承担。**

### 3. 技术初衷与防御研究导向（Defensive Purpose & Dual-Use）
- **防御赋能**：OMOA 作为具备双重用途（Dual-Use）特性的安全工程验证与自主 Agent 探索平台，研发初衷完全立足于**防守方视角（Blue / Purple Teaming）**。
- **核心目的**：旨在协助合规企业安全运营团队、学术科研机构及安全研究人员开展常态化自动化安全风险排查、防御体系健壮性验证、攻防智能体协同机制推演以及大语言模型安全边界学术探索。严禁将本系统转化为未授权武器化打击工具。

### 4. 自主智能体的概率性与不可预测风险（Autonomous Agent Hazards）
- **自主决策特性**：由于大语言模型（LLM）基于概率生成机制进行动态意图拆解与载荷生成，智能体在面对未知架构、非标准响应或动态靶场时，可能自主衍生出未经预设的测试路径。
- **可用性与稳定性影响**：尽管系统内置了并发限流（Token Bucket）与沙箱命令白名单，但在并发发包、无头浏览器深度渲染、自动化表单提交或重试机制下，仍存在对目标系统造成**性能抖动、队列积压、服务拒绝（DoS）、脏数据写入、数据库锁定或进程崩溃**等次生灾害的潜在风险。
- **生产环境严禁盲测**：强烈建议仅在隔离的专用演练靶场、开发测试环境或具备高可用与容灾保障的受控环境中使用本系统；在生产系统开展渗透演练前，必须制定详尽的应急回滚方案与人工双人复核流程。

### 5. 最小危害原则与无害载荷保证（Payload Safety）
- **严禁破坏性载荷**：严禁向本系统挂载或诱导智能体执行任何具有破坏性、自传播、勒索加密、特权维持后门（Rootkit/Backdoor）、系统破坏（如格式化、删库）或持久化破坏特性的利用载荷。
- **PoC 最小化验证**：漏洞验证应始终贯彻“最小化证明（Minimal Proof-of-Concept）”原则，仅以确证漏洞存在性（如读取安全上下文信息、打印预设标记）为界限，严禁横向越权窃取、篡改或外发目标系统的敏感商业数据、用户隐私或系统凭据。

### 6. 负责任的漏洞披露与伦理操守（Coordinated Vulnerability Disclosure）
在获得授权的测试过程中，若本系统发现或触碰至目标系统的未公开高危安全漏洞（0-day / 1-day），使用者应当严格遵循**负责任的协调漏洞披露（CVD）流程**与网络安全行业伦理准则：
- 及时、私下、完整地将漏洞详情与缓解建议同步至目标系统权属单位的安全响应中心（SRC）或官方技术团队；
- 在责任方完成修复前，绝不向第三方泄露、公开售卖或扩散漏洞细节与利用代码。

### 7. 无担保与完全免责声明（Disclaimer of Warranties & Limitation of Liability）
- **按原样提供（AS-IS）**：本项目以开源软件形式按“现状（AS-IS）”和“现有（AS-AVAILABLE）”状态提供，不附带任何明示或暗示的担保条件，包括但不限于适销性、特定用途适用性、无漏洞、无缺陷、不侵权或连续运行之保证。
- **责任免除**：在任何情况下，本项目的发起人、核心维护者、代码贡献者及关联雇佣单位，均不对任何因使用、无法使用、滥用、误配置本系统，或因第三方利用本系统衍生产物所导致的任何直接、间接、附带、惩罚性或后果性损失（包括但不限于利润损失、商业中断、商誉受损、数据泄露、法律诉讼或行政处罚）承担任何法律责任，无论该责任系基于侵权、合同、严格责任或其他法理提出。使用者自愿承担使用本系统的全部风险。
