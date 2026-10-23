'use client';

import React, { useEffect, useState, useMemo } from 'react';
import { api } from '@/lib/api';
import { MCPServer, MCPTool, NativeToolItem } from '@/lib/types';
import {
  Server,
  Wrench,
  Plus,
  RefreshCw,
  Trash2,
  Edit,
  Check,
  X,
  ChevronDown,
  ChevronRight,
  Terminal,
  Globe,
  Radio,
  Sliders,
  Search,
  Code,
  Copy,
  ExternalLink,
  Shield,
  Layers,
  Sparkles,
  AlertCircle,
  HelpCircle,
  Cpu,
} from 'lucide-react';

type TabType = 'mcp' | 'native';

interface EnvEntry {
  key: string;
  value: string;
}

export default function ToolsManagementPage() {
  const [activeTab, setActiveTab] = useState<TabType>('mcp');

  // MCP Servers State
  const [mcpServers, setMcpServers] = useState<MCPServer[]>([]);
  const [loadingMcp, setLoadingMcp] = useState(true);
  const [discoveringId, setDiscoveringId] = useState<number | null>(null);
  const [togglingId, setTogglingId] = useState<number | null>(null);
  const [expandedServers, setExpandedServers] = useState<Set<number>>(new Set());

  // Native Tools State
  const [nativeTools, setNativeTools] = useState<NativeToolItem[]>([]);
  const [loadingNative, setLoadingNative] = useState(true);
  const [nativeSearch, setNativeSearch] = useState('');
  const [selectedCategory, setSelectedCategory] = useState<string>('all');
  const [selectedRole, setSelectedRole] = useState<string>('all');
  const [expandedNativeTools, setExpandedNativeTools] = useState<Set<string>>(new Set());
  const [viewingSchemaTool, setViewingSchemaTool] = useState<string | null>(null);

  // Global Notification
  const [notification, setNotification] = useState<{
    type: 'success' | 'error' | 'info';
    message: string;
  } | null>(null);

  // Modal State for MCP Server (Create / Edit)
  const [serverModalOpen, setServerModalOpen] = useState(false);
  const [modalMode, setModalMode] = useState<'create' | 'edit'>('create');
  const [editingServerId, setEditingServerId] = useState<number | null>(null);
  const [formName, setFormName] = useState('');
  const [formTransport, setFormTransport] = useState<'stdio' | 'http' | 'sse'>('stdio');
  const [formCommand, setFormCommand] = useState('');
  const [formArgs, setFormArgs] = useState('');
  const [formUrl, setFormUrl] = useState('');
  const [formInsecure, setFormInsecure] = useState(false);
  const [formEnabled, setFormEnabled] = useState(true);
  const [formEnv, setFormEnv] = useState<EnvEntry[]>([]);
  const [formSubmitting, setFormSubmitting] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);

  // Delete Confirmation Modal
  const [deletingServer, setDeletingServer] = useState<MCPServer | null>(null);
  const [deleteSubmitting, setDeleteSubmitting] = useState(false);

  // Copy Feedback
  const [copiedText, setCopiedText] = useState<string | null>(null);

  const showNotification = (message: string, type: 'success' | 'error' | 'info' = 'success') => {
    setNotification({ type, message });
    setTimeout(() => {
      setNotification(prev => (prev?.message === message ? null : prev));
    }, 4000);
  };

  const handleCopy = (text: string) => {
    navigator.clipboard.writeText(text);
    setCopiedText(text);
    setTimeout(() => setCopiedText(null), 2000);
  };

  // Load Data
  const loadMcpServers = async () => {
    setLoadingMcp(true);
    try {
      const res = await api.listMCPServers();
      setMcpServers(res.servers || []);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '加载 MCP 服务列表失败';
      showNotification(msg, 'error');
    } finally {
      setLoadingMcp(false);
    }
  };

  const loadNativeTools = async () => {
    setLoadingNative(true);
    try {
      const res = await api.getNativeToolsCatalog();
      setNativeTools(res.tools || []);
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '加载系统原生工具目录失败';
      showNotification(msg, 'error');
    } finally {
      setLoadingNative(false);
    }
  };

  useEffect(() => {
    loadMcpServers();
    loadNativeTools();
  }, []);

  // Toggle Server Expand
  const toggleServerExpand = (id: number) => {
    setExpandedServers(prev => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  // Toggle Server Enabled
  const handleToggleEnabled = async (server: MCPServer, e?: React.MouseEvent) => {
    e?.stopPropagation();
    setTogglingId(server.id);
    try {
      const updated = await api.updateMCPServer(server.id, {
        enabled: !server.enabled,
      });
      setMcpServers(prev => prev.map(s => (s.id === server.id ? { ...s, enabled: updated.enabled } : s)));
      showNotification(
        `MCP 服务 "${server.name}" 已${updated.enabled ? '启用' : '禁用'}`,
        'success'
      );
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '更新状态失败';
      showNotification(msg, 'error');
    } finally {
      setTogglingId(null);
    }
  };

  // Discover Tools
  const handleDiscoverTools = async (server: MCPServer, e?: React.MouseEvent) => {
    e?.stopPropagation();
    setDiscoveringId(server.id);
    try {
      const res = await api.discoverMCPTools(server.id);
      const toolsCount = res.tools?.length ?? 0;
      showNotification(`成功为 "${server.name}" 发现并同步了 ${toolsCount} 个工具`, 'success');
      // 重新加载 MCP 服务以获取最新工具列表
      await loadMcpServers();
      // 默认展开该服务
      setExpandedServers(prev => new Set(prev).add(server.id));
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '发现工具失败，请检查服务连接';
      showNotification(`"${server.name}" 发现工具失败: ${msg}`, 'error');
    } finally {
      setDiscoveringId(null);
    }
  };

  // Open Create Modal
  const openCreateModal = () => {
    setModalMode('create');
    setEditingServerId(null);
    setFormName('');
    setFormTransport('stdio');
    setFormCommand('npx');
    setFormArgs('["@playwright/mcp", "--browser", "chromium", "--no-sandbox"]');
    setFormUrl('');
    setFormInsecure(false);
    setFormEnabled(true);
    setFormEnv([]);
    setFormError(null);
    setServerModalOpen(true);
  };

  // Open Edit Modal
  const openEditModal = (server: MCPServer, e?: React.MouseEvent) => {
    e?.stopPropagation();
    setModalMode('edit');
    setEditingServerId(server.id);
    setFormName(server.name);
    setFormTransport(server.transport);
    setFormCommand(server.command || '');
    setFormArgs(server.args && server.args.length > 0 ? JSON.stringify(server.args) : '');
    setFormUrl(server.url || '');
    setFormInsecure(server.insecure);
    setFormEnabled(server.enabled);

    const envs: EnvEntry[] = [];
    if (server.env) {
      for (const [k, v] of Object.entries(server.env)) {
        envs.push({ key: k, value: v });
      }
    }
    setFormEnv(envs);
    setFormError(null);
    setServerModalOpen(true);
  };

  // Preset Template Loader
  const applyPreset = (preset: 'playwright' | 'scopesentry' | 'python_cli') => {
    if (preset === 'playwright') {
      setFormName('browser');
      setFormTransport('stdio');
      setFormCommand('npx');
      setFormArgs('["@playwright/mcp", "--browser", "chromium", "--no-sandbox"]');
      setFormUrl('');
      setFormInsecure(false);
      setFormEnabled(true);
      setFormEnv([]);
    } else if (preset === 'scopesentry') {
      setFormName('ScopeSentry');
      setFormTransport('http');
      setFormCommand('');
      setFormArgs('');
      setFormUrl('http://127.0.0.1:8000/mcp');
      setFormInsecure(true);
      setFormEnabled(true);
      setFormEnv([{ key: 'X-API-Key', value: 'sentry-demo-token' }]);
    } else if (preset === 'python_cli') {
      setFormName('custom_recon');
      setFormTransport('stdio');
      setFormCommand('python3');
      setFormArgs('["-m", "recon_mcp_server"]');
      setFormUrl('');
      setFormInsecure(false);
      setFormEnabled(true);
      setFormEnv([{ key: 'PYTHONUNBUFFERED', value: '1' }]);
    }
  };

  // Submit MCP Server Form
  const handleServerFormSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError(null);

    const trimmedName = formName.trim();
    if (!trimmedName) {
      setFormError('服务名称为必填项');
      return;
    }
    if (!/^[a-zA-Z0-9_-]+$/.test(trimmedName)) {
      setFormError('服务名称只能包含英文字母、数字、下划线(_)或连字符(-)');
      return;
    }

    if (formTransport === 'stdio') {
      if (!formCommand.trim()) {
        setFormError('stdio 传输模式必须指定启动命令 (Command)');
        return;
      }
    } else {
      if (!formUrl.trim()) {
        setFormError(`${formTransport.toUpperCase()} 传输模式必须指定服务地址 (URL)`);
        return;
      }
      try {
        new URL(formUrl.trim());
      } catch {
        setFormError('请输入合法的 URL（例如 http://127.0.0.1:8000/mcp）');
        return;
      }
    }

    let parsedArgs: string[] = [];
    if (formArgs.trim()) {
      try {
        const parsed = JSON.parse(formArgs.trim());
        if (Array.isArray(parsed)) {
          parsedArgs = parsed.map(String);
        } else {
          parsedArgs = [formArgs.trim()];
        }
      } catch {
        // 允许以空格或换行分隔的普通输入
        parsedArgs = formArgs
          .split(/[\n\s]+/)
          .map(s => s.trim())
          .filter(Boolean);
      }
    }

    const envMap: Record<string, string> = {};
    for (const item of formEnv) {
      const k = item.key.trim();
      if (k) {
        envMap[k] = item.value;
      }
    }

    setFormSubmitting(true);
    try {
      if (modalMode === 'create') {
        await api.createMCPServer({
          name: trimmedName,
          transport: formTransport,
          command: formTransport === 'stdio' ? formCommand.trim() : undefined,
          args: parsedArgs,
          env: envMap,
          url: formTransport !== 'stdio' ? formUrl.trim() : undefined,
          insecure: formInsecure,
          enabled: formEnabled,
        });
        showNotification(`MCP 服务 "${trimmedName}" 创建成功`, 'success');
      } else if (editingServerId) {
        await api.updateMCPServer(editingServerId, {
          transport: formTransport,
          command: formTransport === 'stdio' ? formCommand.trim() : '',
          args: parsedArgs,
          env: envMap,
          url: formTransport !== 'stdio' ? formUrl.trim() : '',
          insecure: formInsecure,
          enabled: formEnabled,
        });
        showNotification(`MCP 服务 "${trimmedName}" 更新成功`, 'success');
      }
      setServerModalOpen(false);
      await loadMcpServers();
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '提交失败';
      setFormError(msg);
    } finally {
      setFormSubmitting(false);
    }
  };

  // Delete Server Submit
  const handleDeleteServerConfirm = async () => {
    if (!deletingServer) return;
    setDeleteSubmitting(true);
    try {
      await api.deleteMCPServer(deletingServer.id);
      showNotification(`MCP 服务 "${deletingServer.name}" 已删除`, 'success');
      setDeletingServer(null);
      await loadMcpServers();
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : '删除失败';
      showNotification(msg, 'error');
    } finally {
      setDeleteSubmitting(false);
    }
  };

  // Native Tools Categories
  const categories = useMemo(() => {
    const map = new Map<string, string>();
    map.set('all', '全部分类');
    nativeTools.forEach(tool => {
      if (tool.category && tool.category_name) {
        map.set(tool.category, tool.category_name);
      }
    });
    return Array.from(map.entries());
  }, [nativeTools]);

  // Filtered Native Tools
  const filteredNativeTools = useMemo(() => {
    return nativeTools.filter(tool => {
      // Category filter
      if (selectedCategory !== 'all' && tool.category !== selectedCategory) {
        return false;
      }
      // Role filter
      if (selectedRole !== 'all') {
        if (!tool.roles.includes(selectedRole)) {
          return false;
        }
      }
      // Search filter
      if (nativeSearch.trim()) {
        const q = nativeSearch.toLowerCase().trim();
        const matchName = tool.name.toLowerCase().includes(q);
        const matchDesc = tool.description.toLowerCase().includes(q);
        const matchCat = (tool.category_name || '').toLowerCase().includes(q);
        return matchName || matchDesc || matchCat;
      }
      return true;
    });
  }, [nativeTools, selectedCategory, selectedRole, nativeSearch]);

  const toggleNativeToolExpand = (name: string) => {
    setExpandedNativeTools(prev => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  };

  // Stats calculation
  const totalMcpCount = mcpServers.length;
  const enabledMcpCount = mcpServers.filter(s => s.enabled).length;
  const totalCachedMcpTools = mcpServers.reduce((acc, s) => acc + (s.tools?.length || 0), 0);
  const totalNativeCount = nativeTools.length;

  return (
    <div className="space-y-6 pb-16">
      {/* Toast Notification */}
      {notification && (
        <div
          className={`fixed bottom-6 right-6 z-50 flex items-center space-x-3 px-4 py-3 rounded-lg shadow-xl border text-sm transition-all duration-300 ${
            notification.type === 'success'
              ? 'bg-emerald-950/90 border-emerald-600/50 text-emerald-200'
              : notification.type === 'error'
              ? 'bg-red-950/90 border-red-600/50 text-red-200'
              : 'bg-blue-950/90 border-blue-600/50 text-blue-200'
          }`}
        >
          {notification.type === 'success' ? (
            <Check className="w-5 h-5 text-emerald-400 shrink-0" />
          ) : notification.type === 'error' ? (
            <AlertCircle className="w-5 h-5 text-red-400 shrink-0" />
          ) : (
            <HelpCircle className="w-5 h-5 text-blue-400 shrink-0" />
          )}
          <span>{notification.message}</span>
          <button
            onClick={() => setNotification(null)}
            className="text-gray-400 hover:text-white ml-2"
          >
            <X className="w-4 h-4" />
          </button>
        </div>
      )}

      {/* Page Header */}
      <div className="flex flex-col md:flex-row md:items-center md:justify-between gap-4 border-b border-gray-800 pb-5">
        <div>
          <div className="flex items-center space-x-3">
            <div className="p-2 rounded-lg bg-gradient-to-br from-blue-600/30 to-purple-600/30 border border-blue-500/40 text-blue-400">
              <Sliders className="w-6 h-6" />
            </div>
            <div>
              <h1 className="text-2xl font-bold tracking-tight text-white flex items-center gap-2">
                工具与 MCP 配置中心
              </h1>
              <p className="text-sm text-gray-400 mt-0.5">
                统一管理 Agent 原生工具库 (Native Tools) 与模型上下文协议 (Model Context Protocol, MCP) 扩展服务
              </p>
            </div>
          </div>
        </div>

        {/* Action Button for MCP */}
        <div className="flex items-center gap-3">
          {activeTab === 'mcp' && (
            <button
              onClick={openCreateModal}
              className="inline-flex items-center space-x-2 px-3.5 py-2 bg-blue-600 hover:bg-blue-500 text-white rounded-lg text-sm font-medium transition-colors shadow-sm"
            >
              <Plus className="w-4 h-4" />
              <span>添加 MCP 服务</span>
            </button>
          )}
          <button
            onClick={() => {
              if (activeTab === 'mcp') loadMcpServers();
              else loadNativeTools();
            }}
            className="inline-flex items-center space-x-1.5 px-3 py-2 bg-gray-800 hover:bg-gray-700 text-gray-200 rounded-lg text-sm transition-colors border border-gray-700"
            title="刷新数据"
          >
            <RefreshCw className={`w-4 h-4 ${loadingMcp || loadingNative ? 'animate-spin text-blue-400' : ''}`} />
            <span>刷新</span>
          </button>
        </div>
      </div>

      {/* Metrics Bar */}
      <div className="grid grid-cols-2 sm:grid-cols-4 gap-4">
        <div className="bg-gray-900/60 border border-gray-800 rounded-xl p-4 flex items-center space-x-3">
          <div className="p-2.5 rounded-lg bg-purple-500/10 text-purple-400 border border-purple-500/20">
            <Server className="w-5 h-5" />
          </div>
          <div>
            <div className="text-xs text-gray-400 font-medium">MCP 服务总数</div>
            <div className="text-xl font-bold text-white flex items-baseline gap-1.5 mt-0.5">
              <span>{totalMcpCount}</span>
              <span className="text-xs text-emerald-400 font-normal">({enabledMcpCount} 个已启用)</span>
            </div>
          </div>
        </div>

        <div className="bg-gray-900/60 border border-gray-800 rounded-xl p-4 flex items-center space-x-3">
          <div className="p-2.5 rounded-lg bg-blue-500/10 text-blue-400 border border-blue-500/20">
            <Wrench className="w-5 h-5" />
          </div>
          <div>
            <div className="text-xs text-gray-400 font-medium">已发现 MCP 工具</div>
            <div className="text-xl font-bold text-white mt-0.5">{totalCachedMcpTools}</div>
          </div>
        </div>

        <div className="bg-gray-900/60 border border-gray-800 rounded-xl p-4 flex items-center space-x-3">
          <div className="p-2.5 rounded-lg bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">
            <Cpu className="w-5 h-5" />
          </div>
          <div>
            <div className="text-xs text-gray-400 font-medium">系统原生工具</div>
            <div className="text-xl font-bold text-white mt-0.5">{totalNativeCount}</div>
          </div>
        </div>

        <div className="bg-gray-900/60 border border-gray-800 rounded-xl p-4 flex items-center space-x-3">
          <div className="p-2.5 rounded-lg bg-amber-500/10 text-amber-400 border border-amber-500/20">
            <Sparkles className="w-5 h-5" />
          </div>
          <div>
            <div className="text-xs text-gray-400 font-medium">协议版本支持</div>
            <div className="text-xs font-mono text-gray-300 mt-1">
              stdio (2024-11) / HTTP (2025-06)
            </div>
          </div>
        </div>
      </div>

      {/* Main Tabs Navigation */}
      <div className="flex border-b border-gray-800">
        <button
          onClick={() => setActiveTab('mcp')}
          className={`flex items-center space-x-2 px-5 py-3 text-sm font-medium border-b-2 transition-colors ${
            activeTab === 'mcp'
              ? 'border-blue-500 text-blue-400 bg-blue-500/5'
              : 'border-transparent text-gray-400 hover:text-gray-200 hover:border-gray-700'
          }`}
        >
          <Server className="w-4 h-4" />
          <span>MCP 扩展协议服务 (MCP Servers)</span>
          <span className="ml-1.5 px-2 py-0.5 text-xs rounded-full bg-gray-800 text-gray-300">
            {totalMcpCount}
          </span>
        </button>

        <button
          onClick={() => setActiveTab('native')}
          className={`flex items-center space-x-2 px-5 py-3 text-sm font-medium border-b-2 transition-colors ${
            activeTab === 'native'
              ? 'border-blue-500 text-blue-400 bg-blue-500/5'
              : 'border-transparent text-gray-400 hover:text-gray-200 hover:border-gray-700'
          }`}
        >
          <Cpu className="w-4 h-4" />
          <span>系统原生工具库 (Native Tools Catalog)</span>
          <span className="ml-1.5 px-2 py-0.5 text-xs rounded-full bg-gray-800 text-gray-300">
            {totalNativeCount}
          </span>
        </button>
      </div>

      {/* ===================== TAB 1: MCP SERVERS ===================== */}
      {activeTab === 'mcp' && (
        <div className="space-y-4">
          <div className="flex items-center justify-between">
            <p className="text-xs text-gray-400">
              MCP (Model Context Protocol) 允许通过标准协议将外部无头浏览器、安全资产库或外部命令行挂载为 Agent 工具。
              启用状态下的 MCP 工具会在各 Worker 启动时自动注入，以 <code className="text-blue-300 font-mono">mcp__&lt;server&gt;__&lt;tool&gt;</code> 命名提供。
            </p>
          </div>

          {loadingMcp && mcpServers.length === 0 ? (
            <div className="py-16 text-center text-gray-500">
              <RefreshCw className="w-8 h-8 animate-spin mx-auto mb-2 text-blue-500/50" />
              <span>正在获取已配置的 MCP 服务...</span>
            </div>
          ) : mcpServers.length === 0 ? (
            <div className="py-16 text-center bg-gray-900/30 border border-gray-800/80 rounded-xl p-8">
              <Server className="w-12 h-12 text-gray-600 mx-auto mb-3" />
              <h3 className="text-base font-medium text-gray-300">暂无已配置的 MCP 服务</h3>
              <p className="text-sm text-gray-500 mt-1 max-w-md mx-auto">
                您可以点击右上角“添加 MCP 服务”，接入 Playwright 浏览器自动化服务或 HTTP/SSE 远程 MCP 协议端点。
              </p>
              <button
                onClick={openCreateModal}
                className="mt-4 inline-flex items-center space-x-2 px-4 py-2 bg-blue-600 hover:bg-blue-500 text-white rounded-lg text-sm font-medium"
              >
                <Plus className="w-4 h-4" />
                <span>立即添加 MCP 服务</span>
              </button>
            </div>
          ) : (
            <div className="space-y-4">
              {mcpServers.map(server => {
                const isExpanded = expandedServers.has(server.id);
                const isDiscovering = discoveringId === server.id;
                const isToggling = togglingId === server.id;
                const toolsCount = server.tools?.length || 0;

                return (
                  <div
                    key={server.id}
                    className={`bg-gray-900/70 border rounded-xl transition-all duration-200 overflow-hidden ${
                      server.enabled
                        ? 'border-gray-800 hover:border-gray-700'
                        : 'border-gray-800/50 opacity-80'
                    }`}
                  >
                    {/* Server Card Main Header */}
                    <div className="p-4 sm:p-5 flex flex-col md:flex-row md:items-center justify-between gap-4">
                      <div className="flex items-start sm:items-center space-x-3.5">
                        <div
                          className={`p-2.5 rounded-lg border mt-0.5 sm:mt-0 ${
                            server.transport === 'stdio'
                              ? 'bg-blue-500/10 text-blue-400 border-blue-500/20'
                              : server.transport === 'http'
                              ? 'bg-emerald-500/10 text-emerald-400 border-emerald-500/20'
                              : 'bg-purple-500/10 text-purple-400 border-purple-500/20'
                          }`}
                        >
                          {server.transport === 'stdio' ? (
                            <Terminal className="w-5 h-5" />
                          ) : server.transport === 'http' ? (
                            <Globe className="w-5 h-5" />
                          ) : (
                            <Radio className="w-5 h-5" />
                          )}
                        </div>

                        <div>
                          <div className="flex items-center space-x-2.5 flex-wrap gap-y-1">
                            <span className="font-bold text-base text-white">{server.name}</span>
                            <span
                              className={`px-2 py-0.5 text-xs font-mono rounded uppercase font-semibold ${
                                server.transport === 'stdio'
                                  ? 'bg-blue-900/40 text-blue-300 border border-blue-700/50'
                                  : server.transport === 'http'
                                  ? 'bg-emerald-900/40 text-emerald-300 border border-emerald-700/50'
                                  : 'bg-purple-900/40 text-purple-300 border border-purple-700/50'
                              }`}
                            >
                              {server.transport}
                            </span>
                            {server.insecure && (
                              <span className="px-2 py-0.5 text-[11px] rounded bg-amber-950/60 text-amber-300 border border-amber-800/50">
                                跳过 TLS 证书检查
                              </span>
                            )}
                          </div>

                          {/* Connection Details */}
                          <div className="mt-1.5 text-xs text-gray-400 flex flex-wrap items-center gap-x-4 gap-y-1 font-mono">
                            {server.transport === 'stdio' ? (
                              <div className="flex items-center space-x-1.5">
                                <span className="text-gray-500">Command:</span>
                                <span className="text-gray-200 bg-gray-950 px-2 py-0.5 rounded border border-gray-800">
                                  {server.command} {server.args?.join(' ')}
                                </span>
                              </div>
                            ) : (
                              <div className="flex items-center space-x-1.5">
                                <span className="text-gray-500">Endpoint:</span>
                                <span className="text-gray-200 bg-gray-950 px-2 py-0.5 rounded border border-gray-800">
                                  {server.url || '(未设置)'}
                                </span>
                              </div>
                            )}

                            {server.env && Object.keys(server.env).length > 0 && (
                              <span className="text-gray-500">
                                Env: {Object.keys(server.env).length} 个变量
                              </span>
                            )}
                          </div>
                        </div>
                      </div>

                      {/* Controls and Actions */}
                      <div className="flex items-center space-x-2.5 self-end md:self-center">
                        {/* Toggle Enabled Switch */}
                        <button
                          type="button"
                          onClick={e => handleToggleEnabled(server, e)}
                          disabled={isToggling}
                          className={`flex items-center space-x-2 px-3 py-1.5 rounded-lg border text-xs font-medium transition-all ${
                            server.enabled
                              ? 'bg-emerald-950/50 border-emerald-600/40 text-emerald-300 hover:bg-emerald-900/50'
                              : 'bg-gray-800/60 border-gray-700 text-gray-400 hover:bg-gray-800'
                          }`}
                        >
                          <span
                            className={`w-2 h-2 rounded-full ${
                              server.enabled ? 'bg-emerald-400 shadow-[0_0_8px_rgba(52,211,153,0.8)]' : 'bg-gray-500'
                            }`}
                          />
                          <span>{server.enabled ? '已启用' : '已禁用'}</span>
                        </button>

                        {/* Discover Tools Button */}
                        <button
                          type="button"
                          onClick={e => handleDiscoverTools(server, e)}
                          disabled={isDiscovering}
                          className="flex items-center space-x-1.5 px-3 py-1.5 rounded-lg border border-gray-700 bg-gray-800 hover:bg-gray-700 text-gray-200 text-xs font-medium transition-colors"
                          title="主动向 MCP 服务发起连接并抓取最新的可用工具清单"
                        >
                          <RefreshCw className={`w-3.5 h-3.5 ${isDiscovering ? 'animate-spin text-blue-400' : ''}`} />
                          <span>{isDiscovering ? '发现中...' : '发现工具'}</span>
                        </button>

                        {/* Edit Button */}
                        <button
                          type="button"
                          onClick={e => openEditModal(server, e)}
                          className="p-1.5 rounded-lg border border-gray-700 bg-gray-800/80 hover:bg-gray-700 text-gray-300 hover:text-white transition-colors"
                          title="编辑配置"
                        >
                          <Edit className="w-4 h-4" />
                        </button>

                        {/* Delete Button */}
                        <button
                          type="button"
                          onClick={e => {
                            e.stopPropagation();
                            setDeletingServer(server);
                          }}
                          className="p-1.5 rounded-lg border border-gray-700 bg-gray-800/80 hover:bg-red-950 hover:border-red-800 text-gray-400 hover:text-red-300 transition-colors"
                          title="删除此 MCP 服务"
                        >
                          <Trash2 className="w-4 h-4" />
                        </button>

                        {/* Expand / Collapse Button */}
                        <button
                          type="button"
                          onClick={() => toggleServerExpand(server.id)}
                          className="flex items-center space-x-1 px-2.5 py-1.5 rounded-lg border border-gray-700/80 bg-gray-800/50 hover:bg-gray-800 text-xs text-gray-300 transition-colors"
                        >
                          <span>{toolsCount} 个工具</span>
                          {isExpanded ? (
                            <ChevronDown className="w-3.5 h-3.5" />
                          ) : (
                            <ChevronRight className="w-3.5 h-3.5" />
                          )}
                        </button>
                      </div>
                    </div>

                    {/* Tools Sublist (Expanded) */}
                    {isExpanded && (
                      <div className="border-t border-gray-800/80 bg-gray-950/70 p-4 sm:p-5 space-y-3">
                        <div className="flex items-center justify-between">
                          <h4 className="text-xs font-semibold text-gray-300 uppercase tracking-wider flex items-center gap-1.5">
                            <Wrench className="w-3.5 h-3.5 text-blue-400" />
                            <span>缓存的可用 MCP 工具列表 ({toolsCount})</span>
                          </h4>
                          {server.tools && server.tools.length > 0 && (
                            <span className="text-[11px] text-gray-500 font-mono">
                              最后同步: {new Date(server.tools[0].discovered_at).toLocaleString()}
                            </span>
                          )}
                        </div>

                        {toolsCount === 0 ? (
                          <div className="text-center py-6 border border-dashed border-gray-800 rounded-lg">
                            <p className="text-xs text-gray-500">当前尚未发现或缓存该服务的工具。</p>
                            <button
                              type="button"
                              onClick={e => handleDiscoverTools(server, e)}
                              disabled={isDiscovering}
                              className="mt-2 text-xs text-blue-400 hover:text-blue-300 underline font-medium"
                            >
                              点击此处发起首次发现
                            </button>
                          </div>
                        ) : (
                          <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                            {server.tools?.map((tool: MCPTool) => {
                              const standardizedName = `mcp__${server.name}__${tool.tool_name}`;
                              return (
                                <div
                                  key={tool.id}
                                  className="p-3 bg-gray-900/90 border border-gray-800 rounded-lg hover:border-gray-700 transition-colors"
                                >
                                  <div className="flex items-center justify-between">
                                    <div className="flex items-center space-x-2">
                                      <code className="text-xs font-mono font-bold text-blue-300 bg-blue-950/60 px-1.5 py-0.5 rounded border border-blue-800/40">
                                        {standardizedName}
                                      </code>
                                      <button
                                        onClick={() => handleCopy(standardizedName)}
                                        className="text-gray-500 hover:text-gray-300 transition-colors"
                                        title="复制完整工具调用名"
                                      >
                                        {copiedText === standardizedName ? (
                                          <Check className="w-3.5 h-3.5 text-emerald-400" />
                                        ) : (
                                          <Copy className="w-3.5 h-3.5" />
                                        )}
                                      </button>
                                    </div>
                                    <span className="text-[10px] text-gray-500 font-mono">
                                      原生名: {tool.tool_name}
                                    </span>
                                  </div>

                                  <p className="text-xs text-gray-400 mt-2 leading-relaxed">
                                    {tool.description || '暂无工具描述'}
                                  </p>

                                  {tool.schema && Object.keys(tool.schema).length > 0 && (
                                    <details className="mt-2 text-xs">
                                      <summary className="cursor-pointer text-gray-500 hover:text-gray-300 select-none">
                                        查看参数规范 (JSON Schema)
                                      </summary>
                                      <pre className="mt-1.5 p-2 bg-gray-950 rounded border border-gray-800/80 font-mono text-[11px] text-gray-300 overflow-x-auto max-h-40">
                                        {JSON.stringify(tool.schema, null, 2)}
                                      </pre>
                                    </details>
                                  )}
                                </div>
                              );
                            })}
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}

      {/* ===================== TAB 2: NATIVE TOOLS CATALOG ===================== */}
      {activeTab === 'native' && (
        <div className="space-y-4">
          <div className="bg-gray-900/60 border border-gray-800 rounded-xl p-4 text-xs text-gray-300 leading-relaxed flex items-start space-x-3">
            <Shield className="w-5 h-5 text-blue-400 shrink-0 mt-0.5" />
            <div>
              <p className="font-semibold text-white">关于系统原生工具 (Native Tools)</p>
              <p className="text-gray-400 mt-0.5">
                原生工具是由后端平台直接执行或监督的安全探测核心算子。在任务创建时，您可以通过{' '}
                <span className="text-blue-300 font-mono">Capability Profiles</span> (例如 explore、api_recon、active_probe 等)
                指定 Worker 拥有的工具子集。Planner 与 Main Agent 则自动装配态势感知与多 Agent 编排指令工具。
              </p>
            </div>
          </div>

          {/* Filters Bar */}
          <div className="flex flex-col md:flex-row md:items-center justify-between gap-3 bg-gray-900/40 p-3 rounded-xl border border-gray-800/80">
            {/* Search */}
            <div className="relative flex-1 max-w-md">
              <Search className="w-4 h-4 text-gray-500 absolute left-3 top-1/2 -translate-y-1/2" />
              <input
                type="text"
                placeholder="搜索原生工具名称、功能描述或分类..."
                value={nativeSearch}
                onChange={e => setNativeSearch(e.target.value)}
                className="w-full bg-gray-950 border border-gray-800 rounded-lg pl-9 pr-3 py-1.5 text-xs text-white placeholder-gray-500 focus:outline-none focus:border-blue-500"
              />
              {nativeSearch && (
                <button
                  onClick={() => setNativeSearch('')}
                  className="absolute right-2.5 top-1/2 -translate-y-1/2 text-gray-500 hover:text-gray-300"
                >
                  <X className="w-3.5 h-3.5" />
                </button>
              )}
            </div>

            {/* Role Filter */}
            <div className="flex items-center space-x-2">
              <span className="text-xs text-gray-500">角色:</span>
              <div className="flex rounded-lg bg-gray-950 p-0.5 border border-gray-800">
                {[
                  { id: 'all', label: '全部' },
                  { id: 'worker', label: 'Worker' },
                  { id: 'planner', label: 'Planner' },
                  { id: 'main_agent', label: 'Main Agent' },
                ].map(r => (
                  <button
                    key={r.id}
                    onClick={() => setSelectedRole(r.id)}
                    className={`px-2.5 py-1 text-xs rounded-md transition-colors ${
                      selectedRole === r.id
                        ? 'bg-blue-600 text-white font-medium shadow-sm'
                        : 'text-gray-400 hover:text-gray-200'
                    }`}
                  >
                    {r.label}
                  </button>
                ))}
              </div>
            </div>
          </div>

          {/* Category Chips */}
          <div className="flex items-center space-x-2 overflow-x-auto pb-1 scrollbar-thin">
            {categories.map(([catKey, catName]) => {
              const count =
                catKey === 'all'
                  ? nativeTools.length
                  : nativeTools.filter(t => t.category === catKey).length;
              return (
                <button
                  key={catKey}
                  onClick={() => setSelectedCategory(catKey)}
                  className={`px-3 py-1 rounded-full text-xs font-medium whitespace-nowrap transition-colors flex items-center space-x-1.5 border ${
                    selectedCategory === catKey
                      ? 'bg-blue-950 border-blue-500/60 text-blue-300'
                      : 'bg-gray-900/60 border-gray-800 text-gray-400 hover:text-gray-200 hover:border-gray-700'
                  }`}
                >
                  <span>{catName}</span>
                  <span className="text-[10px] px-1.5 py-0.2 rounded-full bg-gray-800 text-gray-300">
                    {count}
                  </span>
                </button>
              );
            })}
          </div>

          {/* Native Tools List */}
          {loadingNative && nativeTools.length === 0 ? (
            <div className="py-16 text-center text-gray-500">
              <RefreshCw className="w-8 h-8 animate-spin mx-auto mb-2 text-blue-500/50" />
              <span>正在获取系统原生工具目录...</span>
            </div>
          ) : filteredNativeTools.length === 0 ? (
            <div className="py-12 text-center text-gray-500 border border-dashed border-gray-800 rounded-xl">
              <Search className="w-8 h-8 mx-auto mb-2 text-gray-600" />
              <p className="text-sm">没有匹配筛选条件的原生工具</p>
            </div>
          ) : (
            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              {filteredNativeTools.map(tool => {
                const isExpanded = expandedNativeTools.has(tool.name);
                const hasParams = tool.parameters && Object.keys(tool.parameters).length > 0;

                return (
                  <div
                    key={tool.name}
                    className="bg-gray-900/70 border border-gray-800 hover:border-gray-700/80 rounded-xl p-4 transition-all flex flex-col justify-between"
                  >
                    <div>
                      {/* Tool Card Header */}
                      <div className="flex items-start justify-between gap-2">
                        <div className="flex items-center space-x-2 flex-wrap">
                          <code className="text-sm font-mono font-bold text-emerald-400 bg-emerald-950/60 px-2 py-0.5 rounded border border-emerald-800/40">
                            {tool.name}
                          </code>
                          <button
                            onClick={() => handleCopy(tool.name)}
                            className="text-gray-500 hover:text-gray-300"
                            title="复制工具名称"
                          >
                            {copiedText === tool.name ? (
                              <Check className="w-3.5 h-3.5 text-emerald-400" />
                            ) : (
                              <Copy className="w-3.5 h-3.5" />
                            )}
                          </button>
                          <span className="px-2 py-0.5 text-[11px] rounded bg-gray-800 text-gray-300 border border-gray-700">
                            {tool.category_name}
                          </span>
                        </div>

                        {/* Roles Badges */}
                        <div className="flex items-center space-x-1 shrink-0">
                          {tool.roles.map(r => (
                            <span
                              key={r}
                              className={`px-1.5 py-0.5 text-[10px] font-mono rounded uppercase font-semibold ${
                                r === 'worker'
                                  ? 'bg-blue-950 text-blue-300 border border-blue-800/50'
                                  : r === 'planner'
                                  ? 'bg-purple-950 text-purple-300 border border-purple-800/50'
                                  : 'bg-amber-950 text-amber-300 border border-amber-800/50'
                              }`}
                            >
                              {r === 'main_agent' ? 'MAIN' : r}
                            </span>
                          ))}
                        </div>
                      </div>

                      {/* Tool Description */}
                      <p className="text-xs text-gray-300 mt-2.5 leading-relaxed">
                        {tool.description}
                      </p>

                      {/* Profiles info */}
                      {tool.default_profiles && tool.default_profiles.length > 0 && (
                        <div className="mt-3 flex items-center space-x-1.5 flex-wrap text-[11px] text-gray-400">
                          <span className="text-gray-500">预装 Profile:</span>
                          {tool.default_profiles.map(p => (
                            <span
                              key={p}
                              className="px-1.5 py-0.2 rounded bg-gray-950 text-gray-300 border border-gray-800 font-mono text-[10px]"
                            >
                              {p}
                            </span>
                          ))}
                        </div>
                      )}
                    </div>

                    {/* Parameters Accordion */}
                    {hasParams && (
                      <div className="mt-3 pt-3 border-t border-gray-800/70">
                        <button
                          type="button"
                          onClick={() => toggleNativeToolExpand(tool.name)}
                          className="flex items-center space-x-1 text-xs text-gray-400 hover:text-gray-200 font-medium"
                        >
                          {isExpanded ? (
                            <ChevronDown className="w-3.5 h-3.5 text-blue-400" />
                          ) : (
                            <ChevronRight className="w-3.5 h-3.5" />
                          )}
                          <span>参数结构与规范</span>
                        </button>

                        {isExpanded && (
                          <div className="mt-2 space-y-2">
                            <pre className="p-2.5 bg-gray-950 rounded-lg border border-gray-800 font-mono text-[11px] text-gray-300 overflow-x-auto max-h-52">
                              {JSON.stringify(tool.parameters, null, 2)}
                            </pre>
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>
      )}

      {/* ===================== MODAL: CREATE / EDIT MCP SERVER ===================== */}
      {serverModalOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/75 backdrop-blur-sm overflow-y-auto">
          <div className="bg-gray-900 border border-gray-700 rounded-2xl w-full max-w-xl shadow-2xl overflow-hidden my-8 animate-in fade-in duration-200">
            {/* Modal Header */}
            <div className="px-6 py-4 border-b border-gray-800 flex items-center justify-between">
              <div className="flex items-center space-x-2.5">
                <div className="p-2 rounded-lg bg-blue-600/20 text-blue-400 border border-blue-500/30">
                  <Server className="w-5 h-5" />
                </div>
                <div>
                  <h3 className="text-base font-bold text-white">
                    {modalMode === 'create' ? '添加 MCP 扩展协议服务' : `编辑 MCP 服务: ${formName}`}
                  </h3>
                  <p className="text-xs text-gray-400">配置 Model Context Protocol 服务的连接与参数</p>
                </div>
              </div>
              <button
                onClick={() => setServerModalOpen(false)}
                className="text-gray-400 hover:text-white p-1 rounded-lg hover:bg-gray-800 transition-colors"
              >
                <X className="w-5 h-5" />
              </button>
            </div>

            {/* Modal Body */}
            <form onSubmit={handleServerFormSubmit} className="p-6 space-y-4">
              {/* Presets Bar (Only in Create Mode) */}
              {modalMode === 'create' && (
                <div className="p-3 bg-gray-950 rounded-xl border border-gray-800/80 space-y-1.5">
                  <div className="text-[11px] text-gray-400 font-medium flex items-center gap-1">
                    <Sparkles className="w-3.5 h-3.5 text-amber-400" />
                    <span>快捷填充官方推荐预设:</span>
                  </div>
                  <div className="flex flex-wrap gap-2">
                    <button
                      type="button"
                      onClick={() => applyPreset('playwright')}
                      className="px-2.5 py-1 text-xs rounded bg-gray-900 hover:bg-gray-800 border border-gray-700 text-blue-300 font-medium"
                    >
                      Playwright Browser (stdio)
                    </button>
                    <button
                      type="button"
                      onClick={() => applyPreset('scopesentry')}
                      className="px-2.5 py-1 text-xs rounded bg-gray-900 hover:bg-gray-800 border border-gray-700 text-emerald-300 font-medium"
                    >
                      ScopeSentry 资产同步 (http)
                    </button>
                    <button
                      type="button"
                      onClick={() => applyPreset('python_cli')}
                      className="px-2.5 py-1 text-xs rounded bg-gray-900 hover:bg-gray-800 border border-gray-700 text-purple-300 font-medium"
                    >
                      Python MCP 脚本 (stdio)
                    </button>
                  </div>
                </div>
              )}

              {/* Error Alert */}
              {formError && (
                <div className="p-3 bg-red-950/70 border border-red-700/50 rounded-lg text-xs text-red-200 flex items-start space-x-2">
                  <AlertCircle className="w-4 h-4 text-red-400 shrink-0 mt-0.5" />
                  <span>{formError}</span>
                </div>
              )}

              {/* Name Field */}
              <div>
                <label className="block text-xs font-semibold text-gray-300 mb-1">
                  服务标识名称 (Name) <span className="text-red-400">*</span>
                </label>
                <input
                  type="text"
                  placeholder="如: browser, scopesentry, custom_cli"
                  value={formName}
                  onChange={e => setFormName(e.target.value)}
                  disabled={modalMode === 'edit'}
                  className="w-full bg-gray-950 border border-gray-700 rounded-lg px-3 py-2 text-sm text-white placeholder-gray-500 focus:outline-none focus:border-blue-500 font-mono disabled:opacity-60"
                  required
                />
                <p className="text-[11px] text-gray-500 mt-1">
                  工具将被注册为 <code className="text-blue-300 font-mono">mcp__{'{name}'}__&lt;tool&gt;</code>。
                </p>
              </div>

              {/* Transport Selector */}
              <div>
                <label className="block text-xs font-semibold text-gray-300 mb-1.5">
                  通信传输协议 (Transport) <span className="text-red-400">*</span>
                </label>
                <div className="grid grid-cols-3 gap-2">
                  {[
                    { id: 'stdio', label: '标准输入输出 (stdio)', desc: '子进程直接通信' },
                    { id: 'http', label: 'HTTP (Stream/RPC)', desc: '远程端点通信' },
                    { id: 'sse', label: 'Server-Sent Events', desc: '流式服务端事件' },
                  ].map(item => (
                    <button
                      type="button"
                      key={item.id}
                      onClick={() => setFormTransport(item.id as 'stdio' | 'http' | 'sse')}
                      className={`p-2.5 rounded-lg border text-left transition-all ${
                        formTransport === item.id
                          ? 'bg-blue-600/20 border-blue-500 text-white shadow-sm'
                          : 'bg-gray-950 border-gray-800 text-gray-400 hover:border-gray-700'
                      }`}
                    >
                      <div className="font-semibold text-xs text-white">{item.label}</div>
                      <div className="text-[10px] text-gray-400 mt-0.5">{item.desc}</div>
                    </button>
                  ))}
                </div>
              </div>

              {/* Conditional: stdio configuration */}
              {formTransport === 'stdio' ? (
                <div className="space-y-3 p-3.5 bg-gray-950 rounded-xl border border-gray-800">
                  <div>
                    <label className="block text-xs font-semibold text-gray-300 mb-1">
                      执行指令 (Command) <span className="text-red-400">*</span>
                    </label>
                    <input
                      type="text"
                      placeholder="如: npx, node, python3, /usr/bin/tool"
                      value={formCommand}
                      onChange={e => setFormCommand(e.target.value)}
                      className="w-full bg-gray-900 border border-gray-700 rounded-lg px-3 py-1.5 text-sm text-white placeholder-gray-500 font-mono focus:outline-none focus:border-blue-500"
                    />
                  </div>

                  <div>
                    <label className="block text-xs font-semibold text-gray-300 mb-1">
                      指令参数 (Args)
                    </label>
                    <textarea
                      rows={2}
                      placeholder='JSON 数组格式如 ["@playwright/mcp", "--browser", "chromium"] 或空格分隔'
                      value={formArgs}
                      onChange={e => setFormArgs(e.target.value)}
                      className="w-full bg-gray-900 border border-gray-700 rounded-lg p-2 text-xs text-white placeholder-gray-500 font-mono focus:outline-none focus:border-blue-500"
                    />
                  </div>
                </div>
              ) : (
                /* Conditional: http / sse configuration */
                <div className="space-y-3 p-3.5 bg-gray-950 rounded-xl border border-gray-800">
                  <div>
                    <label className="block text-xs font-semibold text-gray-300 mb-1">
                      远程服务地址 (URL) <span className="text-red-400">*</span>
                    </label>
                    <input
                      type="text"
                      placeholder="如: http://127.0.0.1:8000/mcp 或 https://api.example.com/mcp"
                      value={formUrl}
                      onChange={e => setFormUrl(e.target.value)}
                      className="w-full bg-gray-900 border border-gray-700 rounded-lg px-3 py-1.5 text-sm text-white placeholder-gray-500 font-mono focus:outline-none focus:border-blue-500"
                    />
                  </div>

                  <div className="flex items-center space-x-2 pt-1">
                    <input
                      type="checkbox"
                      id="insecureCheck"
                      checked={formInsecure}
                      onChange={e => setFormInsecure(e.target.checked)}
                      className="rounded border-gray-700 text-blue-600 focus:ring-0 bg-gray-900"
                    />
                    <label htmlFor="insecureCheck" className="text-xs text-gray-300 select-none">
                      跳过 TLS 证书合法性检查 (Insecure Skip Verify - 适用于自签名内网证书)
                    </label>
                  </div>
                </div>
              )}

              {/* Environment / Headers Key-Values */}
              <div>
                <div className="flex items-center justify-between mb-1.5">
                  <label className="text-xs font-semibold text-gray-300">
                    环境变量 / 请求标头 (Environment / Headers)
                  </label>
                  <button
                    type="button"
                    onClick={() => setFormEnv([...formEnv, { key: '', value: '' }])}
                    className="text-xs text-blue-400 hover:text-blue-300 flex items-center space-x-1"
                  >
                    <Plus className="w-3.5 h-3.5" />
                    <span>添加键值</span>
                  </button>
                </div>

                {formEnv.length === 0 ? (
                  <p className="text-[11px] text-gray-500 italic">暂无环境变量设置（可点击右上角添加）</p>
                ) : (
                  <div className="space-y-2 max-h-36 overflow-y-auto pr-1">
                    {formEnv.map((item, idx) => (
                      <div key={idx} className="flex items-center space-x-2">
                        <input
                          type="text"
                          placeholder="键名 (如 X-API-Key)"
                          value={item.key}
                          onChange={e => {
                            const copy = [...formEnv];
                            copy[idx].key = e.target.value;
                            setFormEnv(copy);
                          }}
                          className="flex-1 bg-gray-950 border border-gray-700 rounded px-2 py-1 text-xs text-white font-mono"
                        />
                        <input
                          type="text"
                          placeholder="键值"
                          value={item.value}
                          onChange={e => {
                            const copy = [...formEnv];
                            copy[idx].value = e.target.value;
                            setFormEnv(copy);
                          }}
                          className="flex-1 bg-gray-950 border border-gray-700 rounded px-2 py-1 text-xs text-white font-mono"
                        />
                        <button
                          type="button"
                          onClick={() => setFormEnv(formEnv.filter((_, i) => i !== idx))}
                          className="p-1 text-gray-500 hover:text-red-400"
                        >
                          <Trash2 className="w-3.5 h-3.5" />
                        </button>
                      </div>
                    ))}
                  </div>
                )}
              </div>

              {/* Enable Switch */}
              <div className="flex items-center space-x-2 pt-2 border-t border-gray-800">
                <input
                  type="checkbox"
                  id="enabledCheck"
                  checked={formEnabled}
                  onChange={e => setFormEnabled(e.target.checked)}
                  className="rounded border-gray-700 text-blue-600 focus:ring-0 bg-gray-900"
                />
                <label htmlFor="enabledCheck" className="text-xs text-gray-200 select-none font-medium">
                  启用此 MCP 服务（Worker 启动时将自动装配其工具）
                </label>
              </div>

              {/* Modal Footer */}
              <div className="pt-3 border-t border-gray-800 flex justify-end space-x-3">
                <button
                  type="button"
                  onClick={() => setServerModalOpen(false)}
                  disabled={formSubmitting}
                  className="px-4 py-2 bg-gray-800 hover:bg-gray-700 text-gray-300 rounded-lg text-sm font-medium transition-colors"
                >
                  取消
                </button>
                <button
                  type="submit"
                  disabled={formSubmitting}
                  className="px-5 py-2 bg-blue-600 hover:bg-blue-500 text-white rounded-lg text-sm font-medium transition-colors shadow-sm disabled:opacity-50 flex items-center space-x-1.5"
                >
                  {formSubmitting && <RefreshCw className="w-4 h-4 animate-spin" />}
                  <span>{modalMode === 'create' ? '确认创建' : '保存更改'}</span>
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* ===================== DELETE CONFIRMATION MODAL ===================== */}
      {deletingServer && (
        <div className="fixed inset-0 z-50 flex items-center justify-center p-4 bg-black/80 backdrop-blur-sm">
          <div className="bg-gray-900 border border-gray-800 rounded-2xl w-full max-w-md p-6 shadow-2xl animate-in fade-in duration-200">
            <div className="flex items-center space-x-3 text-red-400 mb-3">
              <div className="p-2 rounded-lg bg-red-950/80 border border-red-800/50">
                <Trash2 className="w-6 h-6" />
              </div>
              <h3 className="text-lg font-bold text-white">删除 MCP 服务</h3>
            </div>
            <p className="text-sm text-gray-300 leading-relaxed">
              确定要删除 MCP 服务 <strong className="text-white font-mono">"{deletingServer.name}"</strong> 吗？
              这将同时清除其所有已发现缓存的工具记录。此操作不可逆。
            </p>
            <div className="mt-6 flex justify-end space-x-3">
              <button
                type="button"
                onClick={() => setDeletingServer(null)}
                disabled={deleteSubmitting}
                className="px-4 py-2 bg-gray-800 hover:bg-gray-700 text-gray-300 rounded-lg text-sm font-medium transition-colors"
              >
                取消
              </button>
              <button
                type="button"
                onClick={handleDeleteServerConfirm}
                disabled={deleteSubmitting}
                className="px-4 py-2 bg-red-600 hover:bg-red-500 text-white rounded-lg text-sm font-medium transition-colors shadow-sm disabled:opacity-50 flex items-center space-x-1.5"
              >
                {deleteSubmitting && <RefreshCw className="w-4 h-4 animate-spin" />}
                <span>确认删除</span>
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
