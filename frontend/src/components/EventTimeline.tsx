import React from 'react';
import { RunEvent } from '@/lib/types';

interface EventTimelineProps {
  events: RunEvent[];
}

export function EventTimeline({ events }: EventTimelineProps) {
  if (!events || events.length === 0) {
    return (
      <div className="py-12 text-center text-gray-500 border border-dashed border-gray-800 rounded-xl bg-gray-900/20">
        <svg
          className="w-8 h-8 mx-auto mb-2 text-gray-600"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
        >
          <path
            strokeLinecap="round"
            strokeLinejoin="round"
            strokeWidth={1.5}
            d="M12 8v4l3 3m6-3a9 9 0 11-18 0 9 9 0 0118 0z"
          />
        </svg>
        <p className="text-sm">No events recorded yet.</p>
      </div>
    );
  }

  // Sort by sequence ascending
  const sortedEvents = [...events].sort((a, b) => a.seq - b.seq);

  return (
    <div className="relative pl-6 border-l-2 border-gray-800 space-y-6 my-4 ml-4">
      {sortedEvents.map((event) => {
        const payload = event.payload || {};

        const typeBadge = (() => {
          switch (event.type) {
            case 'state_change':
              return (
                <span className="text-xs px-2.5 py-0.5 rounded-full font-medium bg-indigo-500/10 text-indigo-400 border border-indigo-500/20">
                  state_change
                </span>
              );
            case 'progress':
              return (
                <span className="text-xs px-2.5 py-0.5 rounded-full font-medium bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">
                  progress
                </span>
              );
            case 'log':
            default:
              return (
                <span className="text-xs px-2.5 py-0.5 rounded-full font-medium bg-gray-500/10 text-gray-400 border border-gray-500/20">
                  {event.type}
                </span>
              );
          }
        })();

        const formattedTime = new Date(event.created_at).toLocaleTimeString(undefined, {
          hour: '2-digit',
          minute: '2-digit',
          second: '2-digit',
        });

        const renderEventDetails = () => {
          if (event.type === 'state_change') {
            const from =
              (payload.from as string) ??
              (payload.from_state as string) ??
              (payload.old_status as string) ??
              (payload.old_state as string) ??
              'unknown';
            const to =
              (payload.to as string) ??
              (payload.to_state as string) ??
              (payload.new_status as string) ??
              (payload.new_state as string) ??
              (payload.status as string) ??
              'unknown';
            const reason = payload.reason as string | undefined;

            return (
              <div className="mt-2 flex flex-wrap items-center gap-2 text-sm">
                <span className="font-mono text-xs px-2 py-0.5 bg-gray-950 text-gray-300 rounded border border-gray-800">
                  {from}
                </span>
                <span className="text-gray-500 font-bold">→</span>
                <span className="font-mono text-xs px-2 py-0.5 bg-gray-950 text-white font-semibold rounded border border-gray-700">
                  {to}
                </span>
                {reason && (
                  <span className="text-xs text-gray-400 italic ml-2">
                    ({reason})
                  </span>
                )}
              </div>
            );
          }

          if (event.type === 'progress') {
            const step = Number(
              payload.step ?? payload.current_step ?? payload.current ?? 0
            );
            const total = Number(
              payload.total ?? payload.total_steps ?? 10
            );
            const percent = total > 0 ? Math.min(100, Math.round((step / total) * 100)) : 0;
            const message = payload.message as string | undefined;

            return (
              <div className="mt-2.5 space-y-2">
                <div className="flex items-center justify-between text-xs">
                  <span className="font-medium text-emerald-400 font-mono">
                    Step {step} / {total}
                  </span>
                  <span className="text-gray-400">{percent}%</span>
                </div>
                <div className="w-full bg-gray-800 rounded-full h-2 overflow-hidden">
                  <div
                    className="bg-emerald-500 h-2 rounded-full transition-all duration-300 ease-out"
                    style={{ width: `${percent}%` }}
                  />
                </div>
                {message && (
                  <p className="text-xs text-gray-400 mt-1">{message}</p>
                )}
              </div>
            );
          }

          // 'log' or general payload
          const logMsg =
            payload.message ??
            payload.log ??
            payload.text ??
            (Object.keys(payload).length > 0 ? JSON.stringify(payload, null, 2) : 'Log event recorded');

          return (
            <div className="mt-2 text-xs text-gray-300 font-mono bg-gray-950 p-3 rounded-lg border border-gray-800/80 whitespace-pre-wrap break-words">
              {String(logMsg)}
            </div>
          );
        };

        return (
          <div key={event.id || event.seq} className="relative group">
            {/* Seq circle centered on vertical timeline */}
            <div className="absolute -left-[37px] top-1 w-6 h-6 rounded-full bg-gray-900 border-2 border-gray-700 group-hover:border-blue-500 flex items-center justify-center text-[10px] font-mono font-bold text-gray-300 transition-colors shadow-sm">
              {event.seq}
            </div>

            {/* Event Content Container */}
            <div className="bg-gray-900/60 border border-gray-800 rounded-xl p-4 shadow-sm hover:border-gray-700/80 transition-all">
              <div className="flex items-center justify-between">
                <div className="flex items-center space-x-2">
                  {typeBadge}
                  <span className="text-xs text-gray-500 font-mono">seq #{event.seq}</span>
                </div>
                <span
                  className="text-xs text-gray-500 font-mono"
                  title={new Date(event.created_at).toISOString()}
                >
                  {formattedTime}
                </span>
              </div>
              {renderEventDetails()}
            </div>
          </div>
        );
      })}
    </div>
  );
}

export default EventTimeline;
