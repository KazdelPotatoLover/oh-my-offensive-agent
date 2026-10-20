import React from 'react';
import { RunStatus } from '@/lib/types';

interface RunStatusBadgeProps {
  status: RunStatus | string;
  finishReason?: string;
  size?: 'sm' | 'md' | 'lg';
  className?: string;
}

const statusConfig: Record<string, { bg: string; text: string; border: string; dot: string; pulse?: boolean }> = {
  queued: {
    bg: 'bg-yellow-500/10',
    text: 'text-yellow-400',
    border: 'border-yellow-500/20',
    dot: 'bg-yellow-400',
  },
  running: {
    bg: 'bg-blue-500/10',
    text: 'text-blue-400',
    border: 'border-blue-500/20',
    dot: 'bg-blue-400',
    pulse: true,
  },
  stopping: {
    bg: 'bg-orange-500/10',
    text: 'text-orange-400',
    border: 'border-orange-500/20',
    dot: 'bg-orange-400',
  },
  completed: {
    bg: 'bg-green-500/10',
    text: 'text-green-400',
    border: 'border-green-500/20',
    dot: 'bg-green-400',
  },
  failed: {
    bg: 'bg-red-500/10',
    text: 'text-red-400',
    border: 'border-red-500/20',
    dot: 'bg-red-400',
  },
  stopped: {
    bg: 'bg-gray-500/10',
    text: 'text-gray-400',
    border: 'border-gray-500/20',
    dot: 'bg-gray-400',
  },
  exhausted: {
    bg: 'bg-amber-500/10',
    text: 'text-amber-300',
    border: 'border-amber-500/30',
    dot: 'bg-amber-400',
  },
  interrupted: {
    bg: 'bg-purple-500/10',
    text: 'text-purple-400',
    border: 'border-purple-500/20',
    dot: 'bg-purple-400',
  },
};

export function RunStatusBadge({ status, finishReason, size = 'sm', className = '' }: RunStatusBadgeProps) {
  const normalizedStatus = (status || '').toLowerCase();
  const config = statusConfig[normalizedStatus] || {
    bg: 'bg-gray-500/10',
    text: 'text-gray-400',
    border: 'border-gray-500/20',
    dot: 'bg-gray-400',
  };

  const sizeClasses = {
    sm: 'text-xs px-2.5 py-0.5 gap-1.5',
    md: 'text-sm px-3 py-1 gap-2',
    lg: 'text-base px-4 py-1.5 gap-2.5 font-semibold',
  }[size];

  const dotSizes = {
    sm: 'w-1.5 h-1.5',
    md: 'w-2 h-2',
    lg: 'w-2.5 h-2.5',
  }[size];

  return (
    <span
      className={`inline-flex items-center rounded-full font-medium border capitalize ${config.bg} ${config.text} ${config.border} ${sizeClasses} ${
        config.pulse ? 'animate-pulse' : ''
      } ${className}`}
    >
      <span className={`rounded-full ${config.dot} ${dotSizes}`} />
      <span>{normalizedStatus === 'stopped' && (finishReason === 'model_calls_exhausted' || finishReason === 'tool_calls_exhausted') ? '预算耗尽 · 部分完成' : status}</span>
    </span>
  );
}

export default RunStatusBadge;
