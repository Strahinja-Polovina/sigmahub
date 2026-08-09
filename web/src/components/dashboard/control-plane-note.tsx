import { Cloud } from "lucide-react";

/**
 * What a capability does, and what it needs, said BEFORE it is offered.
 *
 * A handful of things in this product genuinely cannot be simulated: they are
 * the control plane executing something against real infrastructure — a backup
 * written to an S3 target, a MinIO reconfigured over the mesh, a Paddle
 * checkout, a webhook receiver, an alert actually delivered. Without one, the
 * old answer was to hide them, and hiding is not honest either: someone
 * evaluating SigmaHub offline concluded the product has no backups rather than
 * that they cannot watch one run here.
 *
 * So the surface stays, and it says the two things a reader needs — what this
 * would do, and why it is not doing it — instead of rendering a button whose
 * only behaviour is to throw. Every use of this passes a sentence about THIS
 * capability; a generic "requires the control plane" would be the hiding it
 * replaces, with extra words.
 */
export function ControlPlaneNote({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex gap-3 rounded-lg border border-dashed border-border bg-muted/30 p-4">
      <Cloud className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
      <div className="flex flex-col gap-1">
        <p className="text-sm font-medium text-foreground">{title}</p>
        <p className="text-xs leading-relaxed text-muted-foreground">{children}</p>
      </div>
    </div>
  );
}
