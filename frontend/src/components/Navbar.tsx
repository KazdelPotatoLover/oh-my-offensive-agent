import React from 'react';
import Link from 'next/link';

export function Navbar() {
  return (
    <header className="fixed top-0 left-0 right-0 h-16 bg-gray-900 border-b border-gray-800 z-50">
      <div className="w-full px-4 sm:px-6 h-full flex items-center justify-between">
        <div className="flex items-center space-x-8">
          <Link
            href="/tasks"
            className="flex items-center space-x-2.5 font-bold text-lg text-white hover:text-blue-400 transition-colors"
          >
            <div className="p-1.5 rounded-lg bg-blue-600/20 text-blue-400 border border-blue-500/30 flex items-center justify-center">
              <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  strokeWidth={2}
                  d="M13 10V3L4 14h7v7l9-11h-7z"
                />
              </svg>
            </div>
            <span>Agent Workbench</span>
          </Link>
          <nav className="flex items-center space-x-1">
            <Link
              href="/workbench"
              className="text-gray-300 hover:text-white px-3 py-1.5 rounded-md text-sm font-medium hover:bg-gray-800 transition-colors"
            >
              工作台
            </Link>
            <Link
              href="/tasks"
              className="text-gray-300 hover:text-white px-3 py-1.5 rounded-md text-sm font-medium hover:bg-gray-800 transition-colors"
            >
              Tasks
            </Link>
            <Link
              href="/assets"
              className="text-gray-300 hover:text-white px-3 py-1.5 rounded-md text-sm font-medium hover:bg-gray-800 transition-colors"
            >
              Assets
            </Link>
            <Link
              href="/tools"
              className="text-gray-300 hover:text-white px-3 py-1.5 rounded-md text-sm font-medium hover:bg-gray-800 transition-colors"
            >
              工具 & MCP
            </Link>
          </nav>
        </div>
      </div>
    </header>
  );
}

export default Navbar;
