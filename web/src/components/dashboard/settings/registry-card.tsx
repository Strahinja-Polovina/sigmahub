"use client";

import * as React from "react";
import { toast } from "sonner";
import { Boxes, CircleCheck, Loader2, TriangleAlert } from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { setRegistry, removeRegistry } from "@/server/actions/registry";
import type { CpImageRegistry } from "@/server/cp";

/**
 * The org's container registry.
 *
 * It matters for exactly one reason, and the card says so rather than assuming
 * the user knows: a build produces an image on ONE machine, and the moment
 * something else has to run it — a dedicated build server, or any Kubernetes
 * cluster, where the scheduler picks the node — that image has to travel. A
 * registry is the only thing every host can pull from.
 */
export function RegistryCard({
  orgId,
  configured,
  registry,
  repository,
  canManage,
}: {
  orgId: string;
  configured: boolean;
  registry: CpImageRegistry | null;
  repository: string;
  canManage: boolean;
}) {
  const [host, setHost] = React.useState(registry?.host ?? "");
  const [namespace, setNamespace] = React.useState(registry?.namespace ?? "");
  const [username, setUsername] = React.useState(registry?.username ?? "");
  const [password, setPassword] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [current, setCurrent] = React.useState({ configured, repository });

  // What a pushed image will actually be called, updated as you type — the
  // fastest way to notice a pasted URL or a stray slash.
  const preview = React.useMemo(() => {
    const h = host.trim().replace(/\/+$/, "");
    if (!h) return "";
    const ns = namespace.trim().replace(/^\/+|\/+$/g, "");
    return `${ns ? `${h}/${ns}` : h}/<resource>:<commit>`;
  }, [host, namespace]);

  async function save(e: React.FormEvent) {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    try {
      const res = await setRegistry({
        orgId,
        host: host.trim(),
        namespace: namespace.trim(),
        username: username.trim(),
        password,
      });
      setCurrent({ configured: true, repository: res.repository });
      setPassword("");
      toast.success(`Images will be pushed to ${res.repository}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t save the registry");
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    if (busy) return;
    setBusy(true);
    try {
      await removeRegistry({ orgId });
      setCurrent({ configured: false, repository: "" });
      setHost("");
      setNamespace("");
      setUsername("");
      setPassword("");
      toast.success("Registry removed");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Couldn’t remove the registry");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <CardHeader className="border-b">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex flex-col gap-1.5">
            <CardTitle className="inline-flex items-center gap-2">
              <Boxes className="size-4" />
              Container registry
            </CardTitle>
            <CardDescription>
              Where built images are stored so any machine can run them. Needed for a
              dedicated build server and for every Kubernetes deploy — without one those
              deploys are refused instead of failing later with an unpullable image.
            </CardDescription>
          </div>
          {current.configured ? (
            <Badge variant="outline" className="gap-1.5 text-emerald-700 dark:text-emerald-400">
              <CircleCheck className="size-3.5" />
              {current.repository}
            </Badge>
          ) : (
            <Badge variant="outline" className="gap-1.5 text-amber-700 dark:text-amber-500">
              <TriangleAlert className="size-3.5" />
              Not configured
            </Badge>
          )}
        </div>
      </CardHeader>

      <CardContent className="pt-6">
        {!canManage ? (
          <p className="text-sm text-muted-foreground">
            {current.configured
              ? `Images are pushed to ${current.repository}. Only an org admin can change this.`
              : "No registry is configured. Ask an org admin to add one."}
          </p>
        ) : (
          <form className="flex flex-col gap-4" onSubmit={save}>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="flex flex-col gap-2">
                <Label htmlFor="registry-host">Host</Label>
                <Input
                  id="registry-host"
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                  placeholder="ghcr.io"
                  autoComplete="off"
                  required
                />
                <p className="text-xs text-muted-foreground">
                  Hostname only — no https://, no path.
                </p>
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="registry-namespace">Namespace</Label>
                <Input
                  id="registry-namespace"
                  value={namespace}
                  onChange={(e) => setNamespace(e.target.value)}
                  placeholder="your-org"
                  autoComplete="off"
                />
                <p className="text-xs text-muted-foreground">
                  Optional. A GitHub org, a Docker Hub user, or empty for a self-hosted
                  registry.
                </p>
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="registry-username">Username</Label>
                <Input
                  id="registry-username"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                  autoComplete="off"
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="registry-password">
                  {registry?.hasPassword ? "Password or token (stored)" : "Password or token"}
                </Label>
                <Input
                  id="registry-password"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  placeholder={registry?.hasPassword ? "Leave blank to keep the current one" : ""}
                  autoComplete="new-password"
                />
                <p className="text-xs text-muted-foreground">
                  Encrypted before it is stored, and only ever released to your own agents.
                </p>
              </div>
            </div>

            {preview && (
              <p className="text-xs text-muted-foreground">
                Images will be pushed as{" "}
                <span className="font-mono text-foreground">{preview}</span>
              </p>
            )}

            <div className="flex flex-wrap items-center gap-2">
              <Button type="submit" disabled={busy || !host.trim()}>
                {busy && <Loader2 className="size-4 animate-spin" />}
                {current.configured ? "Save changes" : "Connect registry"}
              </Button>
              {current.configured && (
                <Button type="button" variant="outline" disabled={busy} onClick={remove}>
                  Remove
                </Button>
              )}
            </div>
          </form>
        )}
      </CardContent>
    </Card>
  );
}
