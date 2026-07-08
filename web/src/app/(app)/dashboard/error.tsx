"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { AlertTriangle, RotateCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

/** Error boundary for the dashboard routes: shows the failure and offers a
 *  real retry instead of Next's unstyled default error screen. */
export default function DashboardError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  const router = useRouter();

  // Bare reset() only clears the boundary and re-renders the same errored
  // flight payload; refreshing the router re-fetches the server components.
  function retry() {
    React.startTransition(() => {
      router.refresh();
      reset();
    });
  }

  return (
    <div className="p-4 md:p-6">
      <Card className="mx-auto mt-10 max-w-md text-center">
        <CardHeader>
          <div className="mx-auto mb-2 grid size-10 place-items-center rounded-full bg-destructive/10">
            <AlertTriangle className="size-5 text-destructive" />
          </div>
          <CardTitle>Something went wrong</CardTitle>
          <CardDescription>
            {error.message || "An unexpected error occurred while loading this page."}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex justify-center gap-2">
          <Button onClick={retry} className="gap-1.5">
            <RotateCw className="size-4" />
            Try again
          </Button>
        </CardContent>
      </Card>
    </div>
  );
}
