'use client';

import React, { useState } from 'react';
import { api } from '@/lib/api';
import { Task } from '@/lib/types';

interface TaskFormProps {
  onSuccess: (task: Task) => void;
  onCancel: () => void;
}

export function TaskForm({ onSuccess, onCancel }: TaskFormProps) {
  const [name, setName] = useState('');
  const [objective, setObjective] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) {
      setError('Task name is required');
      return;
    }

    setLoading(true);
    setError(null);

    try {
      const task = await api.createTask({
        name: name.trim(),
        objective: objective.trim(),
      });
      onSuccess(task);
    } catch (err: unknown) {
      if (err instanceof Error) {
        setError(err.message);
      } else {
        setError('Failed to create task');
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <form
      onSubmit={handleSubmit}
      className="bg-gray-900 border border-gray-800 rounded-xl p-6 shadow-xl mb-8"
    >
      <div className="flex items-center justify-between pb-4 mb-4 border-b border-gray-800">
        <div>
          <h2 className="text-lg font-semibold text-white">Create New Task</h2>
          <p className="text-xs text-gray-400 mt-0.5">
            Define a task name and optional objective for execution
          </p>
        </div>
        <button
          type="button"
          onClick={onCancel}
          disabled={loading}
          aria-label="Close"
          className="text-gray-400 hover:text-gray-200 transition-colors p-1 rounded-md hover:bg-gray-800"
        >
          <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      </div>

      {error && (
        <div className="mb-4 p-3 bg-red-500/10 border border-red-500/30 text-red-400 rounded-lg text-sm flex items-start space-x-2">
          <svg className="w-5 h-5 flex-shrink-0 text-red-400 mt-0.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
          </svg>
          <span>{error}</span>
        </div>
      )}

      <div className="space-y-4">
        <div>
          <label htmlFor="task-name" className="block text-sm font-medium text-gray-300 mb-1.5">
            Name <span className="text-red-400">*</span>
          </label>
          <input
            id="task-name"
            type="text"
            required
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={loading}
            placeholder="e.g. Summarize incident reports"
            className="w-full px-3.5 py-2.5 bg-gray-950 border border-gray-800 rounded-lg text-gray-100 placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-all"
          />
        </div>

        <div>
          <label htmlFor="task-objective" className="block text-sm font-medium text-gray-300 mb-1.5">
            Objective <span className="text-gray-500 text-xs font-normal">(optional)</span>
          </label>
          <textarea
            id="task-objective"
            rows={3}
            value={objective}
            onChange={(e) => setObjective(e.target.value)}
            disabled={loading}
            placeholder="Describe the objective or steps the agent should accomplish..."
            className="w-full px-3.5 py-2.5 bg-gray-950 border border-gray-800 rounded-lg text-gray-100 placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-all"
          />
        </div>

        <div className="flex items-center justify-end space-x-3 pt-2">
          <button
            type="button"
            onClick={onCancel}
            disabled={loading}
            className="px-4 py-2 text-sm font-medium text-gray-300 hover:text-white bg-gray-800 hover:bg-gray-750 border border-gray-700/60 rounded-lg transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={loading || !name.trim()}
            className="px-4 py-2 text-sm font-medium text-white bg-blue-600 hover:bg-blue-500 rounded-lg transition-colors disabled:opacity-50 flex items-center space-x-2"
          >
            {loading && (
              <svg className="animate-spin h-4 w-4 text-white" viewBox="0 0 24 24" fill="none">
                <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
              </svg>
            )}
            <span>{loading ? 'Creating...' : 'Create Task'}</span>
          </button>
        </div>
      </div>
    </form>
  );
}

export default TaskForm;
