import { cn } from "@/lib/utils";

export function Logo({
  className,
  showWord = true,
}: {
  className?: string;
  showWord?: boolean;
}) {
  return (
    <span className={cn("inline-flex items-center gap-2", className)}>
      <span className="grid size-7 place-items-center rounded-md bg-primary font-mono text-sm font-bold text-primary-foreground">
        Σ
      </span>
      {showWord && (
        <span className="text-[15px] font-semibold tracking-tight text-foreground">
          SigmaHub
        </span>
      )}
    </span>
  );
}
