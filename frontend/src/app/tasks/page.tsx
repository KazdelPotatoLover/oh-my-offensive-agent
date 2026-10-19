'use client';

import React, { useEffect, useState } from 'react';
import Link from 'next/link';
import { api } from '@/lib/api';
import { Task } from '@/lib/types';
import { TaskForm } from '@/components/TaskForm';

export default function TasksPage() {
  const [tasks, setTasks] = useState<Task[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [showCreateForm, setShowCreateForm] = useState(false);

  const loadTasks = async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.getTasks();
      setTasks(data);
    } catch (err: unknown) {
      if (err instanceof Error) {
        setError(err.message);
      } else {
        setError('Failed to fetch tasks');
      }
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadTasks();
  }, []);

  const handleTaskCreated = (newTask: Task) => {
    setTasks((prev) => [newTask, ...prev]);
    setShowCreateForm(false);
  };

  return (
    <div className="space-y-6">
      {/* Header section */}
      <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4 pb-6 border-b border-gray-800">
        <div>
          <h1 className="text-2xl font-bold text-white tracking-tight">Tasks</h1>
          <p className="text-sm text-gray-400 mt-1">
            Manage your autonomous tasks and track execution runs
          </p>
        </div>
        <div>
          <button
            onClick={() => setShowCreateForm((prev) => !prev)}
            className="inline-flex items-center space-x-2 px-4 py-2 bg-blue-600 hover:bg-blue-500 text-white text-sm font-medium rounded-lg shadow transition-colors"
          >
            <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
            </svg>
            <span>{showCreateForm ? 'Close Form' : 'New Task'}</span>
          </button>
        </div>
      </div>

      {/* Inline Create Task Form */}
      {showCreateForm && (
        <TaskForm
          onSuccess={handleTaskCreated}
          onCancel={() => setShowCreateForm(false)}
        />
      )}

      {/* Error state */}
      {error && (
        <div className="p-4 bg-red-500/10 border border-red-500/20 rounded-xl text-red-400 flex items-center justify-between">
          <div className="flex items-center space-x-2">
            <svg className="w-5 h-5 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
            <span className="text-sm">{error}</span>
          </div>
          <button
            onClick={loadTasks}
            className="text-xs bg-red-500/20 hover:bg-red-500/30 text-red-300 px-3 py-1.5 rounded-lg transition-colors font-medium"
          >
            Retry
          </button>
        </div>
      )}

      {/* Loading state */}
      {loading ? (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5">
          {[...Array(6)].map((_, idx) => (
            <div
              key={idx}
              className="bg-gray-900/40 border border-gray-800 rounded-xl p-5 animate-pulse space-y-3"
            >
              <div className="h-5 bg-gray-800 rounded w-3/4" />
              <div className="space-y-2">
                <div className="h-3.5 bg-gray-800/60 rounded w-full" />
                <div className="h-3.5 bg-gray-800/60 rounded w-2/3" />
              </div>
              <div className="pt-2 border-t border-gray-800/60 flex justify-between">
                <div className="h-3 bg-gray-800/60 rounded w-20" />
              </div>
            </div>
          ))}
        </div>
      ) : tasks.length === 0 ? (
        /* Empty state */
        <div className="text-center py-16 px-4 bg-gray-900/30 border border-gray-800/80 rounded-2xl">
          <div className="w-12 h-12 rounded-full bg-gray-800/80 text-gray-500 mx-auto flex items-center justify-center mb-4">
            <svg className="w-6 h-6" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5} d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
            </svg>
          </div>
          <h3 className="text-lg font-medium text-white mb-1">No tasks yet</h3>
          <p className="text-sm text-gray-400 mb-6 max-w-sm mx-auto">
            Get started by creating your first task to execute with the agent worker.
          </p>
          <button
            onClick={() => setShowCreateForm(true)}
            className="inline-flex items-center space-x-2 px-4 py-2 bg-blue-600 hover:bg-blue-500 text-white text-sm font-medium rounded-lg shadow transition-colors"
          >
            <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
            </svg>
            <span>Create Task</span>
          </button>
        </div>
      ) : (
        /* Tasks Grid */
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5">
          {tasks.map((task) => {
            const formattedDate = new Date(task.created_at).toLocaleDateString(undefined, {
              year: 'numeric',
              month: 'short',
              day: 'numeric',
            });

            const truncatedObjective = task.objective
              ? task.objective.length > 100
                ? `${task.objective.slice(0, 100)}...`
                : task.objective
              : 'No objective defined';

            return (
              <Link
                key={task.id}
                href={`/tasks/${task.id}`}
                className="group flex flex-col justify-between bg-gray-900/60 hover:bg-gray-900 border border-gray-800 hover:border-gray-700/90 rounded-xl p-5 transition-all shadow-sm hover:shadow-lg hover:-translate-y-0.5"
              >
                <div>
                  <div className="flex items-start justify-between gap-2 mb-2">
                    <h2 className="text-base font-semibold text-white group-hover:text-blue-400 transition-colors line-clamp-1">
                      {task.name}
                    </h2>
                    <span className="text-gray-500 group-hover:text-gray-400 transition-colors">
                      <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
                      </svg>
                    </span>
                  </div>
                  <p className="text-xs text-gray-400 line-clamp-3 leading-relaxed mb-4">
                    {truncatedObjective}
                  </p>
                </div>

                <div className="pt-3 border-t border-gray-800/80 flex items-center justify-between text-xs text-gray-500">
                  <div className="flex items-center space-x-1.5">
                    <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5} d="M8 7V3m8 4V3m-9 8h10M5 21h14a2 2 0 002-2V7a2 2 0 00-2-2H5a2 2 0 00-2 2v12a2 2 0 002 2z" />
                    </svg>
                    <span>{formattedDate}</span>
                  </div>
                  <span className="font-mono text-[10px] text-gray-600 bg-gray-800/50 px-1.5 py-0.5 rounded">
                    {task.id.slice(0, 8)}
                  </span>
                </div>
              </Link>
            );
          })}
        </div>
      )}
    </div>
  );
}
