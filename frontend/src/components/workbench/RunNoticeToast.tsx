'use client';

import { useEffect, useState } from 'react';
import { AlertTriangle, X } from 'lucide-react';

export function RunNoticeToast({ noticeId, title, detail }: { noticeId: string; title: string; detail: string }) {
  const storageKey = `workbench:run-notice:${noticeId}`;
  const [visible, setVisible] = useState(false);

  useEffect(() => {
    try {
      setVisible(window.sessionStorage.getItem(storageKey) !== 'dismissed');
    } catch {
      setVisible(true);
    }
  }, [storageKey]);

  const dismiss = () => {
    setVisible(false);
    try {
      window.sessionStorage.setItem(storageKey, 'dismissed');
    } catch {
      // Closing the notice still works when browser storage is unavailable.
    }
  };

  if (!visible) return null;

  return (
    <aside
      role="status"
      aria-live="polite"
      aria-atomic="true"
      data-testid="run-termination-notice"
      className="fixed right-3 top-20 z-[60] flex w-[calc(100vw-1.5rem)] max-w-md items-start gap-3 rounded-lg border border-amber-800/70 bg-gray-900 p-3 text-sm text-amber-100 shadow-xl shadow-black/30 sm:right-5"
    >
      <AlertTriangle aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-amber-400" />
      <div className="min-w-0 flex-1 break-words">
        <p className="font-semibold leading-5 text-amber-200">{title}</p>
        {detail && <p className="mt-1 text-xs leading-5 text-gray-300">{detail}</p>}
      </div>
      <button
        type="button"
        aria-label="关闭任务提示"
        onClick={dismiss}
        className="-m-1 flex size-8 shrink-0 items-center justify-center rounded-md text-gray-400 transition-colors hover:bg-gray-800 hover:text-gray-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-amber-400"
      >
        <X aria-hidden="true" className="size-4" />
      </button>
    </aside>
  );
}
