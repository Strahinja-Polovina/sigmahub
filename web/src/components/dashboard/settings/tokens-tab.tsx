"use client";

import * as React from "react";
import { toast } from "sonner";
import { KeyRound, Loader2, RefreshCw, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
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
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  listServiceTokens,
  rotateServiceToken,
  revokeServiceToken,
} from "@/server/actions/service-tokens";
import type { CpServiceToken } from "@/server/cp";

// TokensTab manages an org's control-plane service tokens (Org Admin only):
// list, rotate (revoke + reissue, plaintext shown once), and revoke. Service
// tokens only exist when the control plane is wired, so the tab explains the
// empty state in demo mode.
export function TokensTab({ orgId, isAdmin }: { orgId: string; isAdmin: boolean }) {
  const [tokens, setTokens] = React.useState<CpServiceToken[] | null>(null);
  const [rotated, setRotated] = React.useState<{ name: string; token: string } | null>(null);
  const [pending, startTransition] = React.useTransition();

  const load = React.useCallback(() => {
    startTransition(async () => {
      try {
        setTokens(await listServiceTokens(orgId));
      } catch (err) {
        toast.error("Couldn’t load tokens", {
          description: err instanceof Error ? err.message : "Please try again.",
        });
        setTokens([]);
      }
    });
  }, [orgId]);

  React.useEffect(() => {
    if (isAdmin) load();
  }, [isAdmin, load]);

  if (!isAdmin) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Service tokens</CardTitle>
          <CardDescription>Only organization admins can manage service tokens.</CardDescription>
        </CardHeader>
      </Card>
    );
  }

  function rotate(t: CpServiceToken) {
    startTransition(async () => {
      try {
        const res = await rotateServiceToken({ orgId, tokenId: t.id });
        setRotated({ name: res.name, token: res.token });
        load();
      } catch (err) {
        toast.error("Couldn’t rotate", { description: err instanceof Error ? err.message : "Please try again." });
      }
    });
  }

  function revoke(t: CpServiceToken) {
    startTransition(async () => {
      try {
        await revokeServiceToken({ orgId, tokenId: t.id, name: t.name });
        toast.success(`Revoked “${t.name}”`);
        load();
      } catch (err) {
        toast.error("Couldn’t revoke", { description: err instanceof Error ? err.message : "Please try again." });
      }
    });
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-5" />
          Service tokens
        </CardTitle>
        <CardDescription>
          Control-plane credentials for this organization. Rotating reissues the token with the same role and
          reveals the new value once.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {tokens === null ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : tokens.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No service tokens. They appear here once the control plane is connected.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Role</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tokens.map((t) => (
                <TableRow key={t.id}>
                  <TableCell className="font-medium">{t.name || "—"}</TableCell>
                  <TableCell>{t.role}</TableCell>
                  <TableCell>{t.revokedAt ? "Revoked" : "Active"}</TableCell>
                  <TableCell className="flex justify-end gap-2">
                    <Button variant="outline" size="sm" disabled={pending || Boolean(t.revokedAt)} onClick={() => rotate(t)}>
                      <RefreshCw className="size-4" />
                      Rotate
                    </Button>
                    <Button variant="destructive" size="sm" disabled={pending || Boolean(t.revokedAt)} onClick={() => revoke(t)}>
                      <Trash2 className="size-4" />
                      Revoke
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>

      <Dialog open={rotated !== null} onOpenChange={(next) => !next && setRotated(null)}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>New token for “{rotated?.name}”</DialogTitle>
            <DialogDescription>
              Copy this now — it is shown once and cannot be retrieved again. The previous token is revoked.
            </DialogDescription>
          </DialogHeader>
          <p className="break-all rounded-md border border-border bg-muted/40 p-3 font-mono text-xs">
            {rotated?.token}
          </p>
          <DialogFooter>
            <DialogClose render={<Button type="button">Done</Button>} />
          </DialogFooter>
        </DialogContent>
      </Dialog>
      {pending && <Loader2 className="mt-2 size-4 animate-spin text-muted-foreground" />}
    </Card>
  );
}
