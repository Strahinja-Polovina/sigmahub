"use client";

import * as React from "react";
import { toast } from "sonner";
import { Loader2 } from "lucide-react";

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
import { updateOrg } from "@/server/actions/org";
import type { SettingsOrg } from "./settings-view";
import { unwrap } from "@/lib/action-result";

const PLAN_LABELS: Record<string, string> = { free: "Free", cloud: "Cloud" };

export function GeneralTab({ org, isAdmin }: { org: SettingsOrg; isAdmin: boolean }) {
  const [name, setName] = React.useState(org.name);
  const [pending, startTransition] = React.useTransition();

  // Re-seed the field when the org identity or its persisted name changes
  // (switch, or after a successful save). Adjusting during render avoids the
  // setState-in-effect round-trip.
  const [prevOrg, setPrevOrg] = React.useState(`${org.id}:${org.name}`);
  if (prevOrg !== `${org.id}:${org.name}`) {
    setPrevOrg(`${org.id}:${org.name}`);
    setName(org.name);
  }

  const dirty = name.trim() !== org.name && name.trim().length > 0;

  function handleSave(e: React.FormEvent) {
    e.preventDefault();
    if (!dirty || !isAdmin) return;
    startTransition(async () => {
      try {
        unwrap(await updateOrg({ orgId: org.id, name: name.trim() }));
        toast.success("Organization settings saved", {
          description: `Name updated to “${name.trim()}”.`,
        });
      } catch (err) {
        toast.error("Couldn’t save settings", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
      }
    });
  }

  return (
    <form onSubmit={handleSave}>
      <Card>
        <CardHeader className="border-b">
          <CardTitle>General</CardTitle>
          <CardDescription>Basic details for this organization.</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-5 py-1">
          <div className="grid gap-2 sm:max-w-sm">
            <Label htmlFor="org-name">Organization name</Label>
            <Input
              id="org-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Acme Cloud"
              disabled={!isAdmin || pending}
              readOnly={!isAdmin}
            />
            {!isAdmin && (
              <p className="text-xs text-muted-foreground">
                Only organization admins can change these settings.
              </p>
            )}
          </div>

          <div className="grid gap-2 sm:max-w-sm">
            <Label htmlFor="org-slug">Organization ID</Label>
            <Input id="org-slug" value={org.slug} readOnly disabled className="font-mono" />
            <p className="text-xs text-muted-foreground">
              Used in URLs and the CLI. Contact support to change it.
            </p>
          </div>

          <div className="grid gap-2">
            <Label>Plan</Label>
            <div className="flex items-center gap-2">
              <Badge variant={org.plan === "cloud" ? "default" : "outline"} className="capitalize">
                {PLAN_LABELS[org.plan] ?? org.plan}
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
            disabled={!dirty || pending}
            onClick={() => setName(org.name)}
          >
            Reset
          </Button>
          <Button type="submit" disabled={!dirty || !isAdmin || pending}>
            {pending && <Loader2 className="size-4 animate-spin" />}
            Save changes
          </Button>
        </CardFooter>
      </Card>
    </form>
  );
}
