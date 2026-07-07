"use client";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import type { AuditEntry } from "./settings-view";

// Derive a category badge from the action verb.
function categoryOf(action: string): { label: string; variant: "secondary" | "outline" } {
  const a = action.toLowerCase();
  if (a.includes("deploy")) return { label: "Deploy", variant: "secondary" };
  if (a.includes("server") || a.includes("agent")) return { label: "Server", variant: "outline" };
  if (a.includes("member") || a.includes("role") || a.includes("invit"))
    return { label: "Access", variant: "outline" };
  return { label: "Settings", variant: "outline" };
}

function relativeTime(input: string | Date) {
  const then = new Date(input).getTime();
  const diff = Math.max(0, Date.now() - then);
  const m = Math.floor(diff / 60000);
  if (m < 1) return "just now";
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} hr ago`;
  const d = Math.floor(h / 24);
  if (d === 1) return "yesterday";
  if (d < 30) return `${d} days ago`;
  return new Date(input).toLocaleDateString("en-GB", { day: "numeric", month: "short" });
}

export function AuditTab({ entries }: { entries: AuditEntry[] }) {
  return (
    <Card>
      <CardHeader className="border-b">
        <CardTitle>Audit log</CardTitle>
        <CardDescription>Recent audited actions across this organization.</CardDescription>
      </CardHeader>
      <CardContent className="px-0">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-4">Actor</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>Target</TableHead>
              <TableHead className="pr-4 text-right">When</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.length === 0 ? (
              <TableRow>
                <TableCell colSpan={4} className="py-10 text-center text-sm text-muted-foreground">
                  No audited actions yet. Actions like deploys, member changes and server
                  connections appear here.
                </TableCell>
              </TableRow>
            ) : (
              entries.map((e) => {
                const cat = categoryOf(e.action);
                return (
                  <TableRow key={e.id}>
                    <TableCell className="pl-4 font-medium text-foreground">{e.actor}</TableCell>
                    <TableCell>
                      <span className="flex items-center gap-2">
                        <Badge variant={cat.variant}>{cat.label}</Badge>
                        <span className="text-foreground">{e.action}</span>
                      </span>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {e.target || "—"}
                    </TableCell>
                    <TableCell className="pr-4 text-right text-muted-foreground tabular-nums">
                      {relativeTime(e.createdAt)}
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
