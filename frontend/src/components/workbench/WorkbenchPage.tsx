'use client';

import React, { Suspense } from 'react';
import { WorkbenchApp } from './WorkbenchApp';

export function WorkbenchPage() {
  return (
    <Suspense fallback={<p className="px-4 py-6 text-sm text-gray-300">正在打开工作台</p>}>
      <WorkbenchApp />
    </Suspense>
  );
}
