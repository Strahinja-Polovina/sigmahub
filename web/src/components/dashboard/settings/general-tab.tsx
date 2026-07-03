"use client";

import * as React from "react";
import { toast } from "sonner";

import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import type { Org } from "@/lib/mock";

const PLAN_LABELS: Record<Org["plan"], string> = {
  free: "Free",
  cloud: "Cloud",
};

export function GeneralTab({ org }: { org: Org }) {
  // Reset local edits whenever the active org changes.
  const [name, setName] = React.useState(org.name);
  React.useEffect(() => setName(org.name), [org.id, org.name]);

  const dirty = name.trim() !== org.name && name.trim().length > 0;

  function handleSave(e: React.FormEvent) {
    e.preventDefault();
    if (!dirty) return;
    toast.success("Organization settings saved", {
      description: `Name updated to “${name.trim()}”.`,
    });
  }

  return (
    <form onSubmit={handleSave}>
      <Card>
        <CardHeader className="border-b">
          <CardTitle>General</CardTitle>
          <CardDescription>
            Basic details for this organization.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-5 py-1">
          <div className="grid gap-2 sm:max-w-sm">
            <Label htmlFor="org-name">Organization name</Label>
            <Input
              id="org-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Acme Cloud"
            />
          </div>

          <div className="grid gap-2 sm:max-w-sm">
            <Label htmlFor="org-slug">Organization ID</Label>
            <Input
              id="org-slug"
              value={org.slug}
              readOnly
              disabled
              className="font-mono"
            />
            <p className="text-xs text-muted-foreground">
              Used in URLs and the CLI. Contact support to change it.
            </p>
          </div>

          <div className="grid gap-2">
            <Label>Plan</Label>
            <div className="flex items-center gap-2">
              <Badge
                variant={org.plan === "cloud" ? "default" : "outline"}
                className="capitalize"
              >
                {PLAN_LABELS[org.plan]}
              </Badge>
              <span className="text-xs text-muted-foreground">
                Single-meter pricing · manage usage in Billing.
              </span>
            </div>
          </div>
        </CardContent>
        <CardFooter className="justify-end gap-2">
          <Button
            type="button"
            variant="outline"
            disabled={!dirty}
            onClick={() => setName(org.name)}
          >
            Reset
          </Button>
          <Button type="submit" disabled={!dirty}>
            Save changes
          </Button>
        </CardFooter>
      </Card>
    </form>
  );
}
