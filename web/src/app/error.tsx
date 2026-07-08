"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { AlertTriangle, RotateCw } from "lucide-react";

/** Root error boundary: catches failures thrown above the dashboard segment
 *  (notably the (app) layout's data fetching), which dashboard/error.tsx
 *  cannot see. Styled standalone — no app chrome is available up here. */
export default function RootError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  const router = useRouter();

  function retry() {
    React.startTransition(() => {
      router.refresh();
      reset();
    });
  }

  return (
    <div className="grid min-h-[60vh] place-items-center p-6">
      <div className="flex w-full max-w-md flex-col items-center gap-3 rounded-xl border border-border bg-card p-8 text-center shadow-sm">
        <span className="grid size-10 place-items-center rounded-full bg-destructive/10">
          <AlertTriangle className="size-5 text-destructive" />
        </span>
        <p className="text-sm font-medium text-foreground">Something went wrong</p>
        <p className="text-sm text-muted-foreground">
          {error.message || "An unexpected error occurred."}
        </p>
        <button
          type="button"
          onClick={retry}
          className="mt-2 inline-flex h-9 items-center gap-1.5 rounded-lg bg-primary px-4 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90"
        >
          <RotateCw className="size-4" />
          Try again
        </button>
      </div>
    </div>
  );
}
