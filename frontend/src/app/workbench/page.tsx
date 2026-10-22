import { redirect } from 'next/navigation';
import { WorkbenchPage } from '@/components/workbench/WorkbenchPage';
import { workbenchHref } from '@/lib/workbench/links';

export default function Page({
  searchParams,
}: {
  searchParams: { task?: string; run?: string; work?: string };
}) {
  if (searchParams.task) {
    redirect(workbenchHref({
      taskId: searchParams.task,
      runId: searchParams.run,
      workItemId: searchParams.work,
    }));
  }
  return <WorkbenchPage />;
}
