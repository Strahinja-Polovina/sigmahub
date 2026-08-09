/**
 * Ports, health checks and domains, as the wizard asks for them (SIGMA-210).
 *
 * All three were detected and none was ever shown. The consequence was not
 * cosmetic: the ports drive the rollout's exposed ports AND the default health
 * probe, so a detection the user could not see or correct was a first deploy
 * that failed its gate for reasons nothing in the UI had mentioned (SIGMA-160).
 */

/** One container port and how it is reached from the host. */
export type PortMapping = {
  id: string;
  container: number;
  /**
   * 0 means INTERNAL-ONLY, and it is the default.
   *
   * Publishing a host port is the wrong default twice over: it collides the
   * moment two apps want 3000, and it bypasses the proxy that terminates TLS
   * and routes domains. Anything that needs to be reachable gets a domain, and
   * Traefik reaches the container directly.
   */
  host: number;
};

let seq = 0;
function nextId(): string {
  seq += 1;
  return `port_${seq}`;
}

/** Detected container ports → editable mappings, internal-only. */
export function defaultPortMappings(ports: number[] | undefined | null): PortMapping[] {
  const seen = new Set<number>();
  const out: PortMapping[] = [];
  for (const p of ports ?? []) {
    if (!isValidPort(p) || seen.has(p)) continue;
    seen.add(p);
    out.push({ id: nextId(), container: p, host: 0 });
  }
  return out;
}

export function blankPortMapping(): PortMapping {
  return { id: nextId(), container: 0, host: 0 };
}

export function isValidPort(p: number): boolean {
  return Number.isInteger(p) && p > 0 && p < 65536;
}

/** Why a mapping cannot be used, or null. */
export function portMappingError(m: PortMapping): string | null {
  if (!isValidPort(m.container)) return "Container port must be between 1 and 65535.";
  if (m.host !== 0 && !isValidPort(m.host)) {
    return "Host port must be 0 (internal only) or between 1 and 65535.";
  }
  return null;
}

/** The first blocking problem across every mapping, or null. */
export function portMappingsError(mappings: PortMapping[]): string | null {
  for (const m of mappings) {
    const err = portMappingError(m);
    if (err) return err;
  }
  const published = mappings.filter((m) => m.host !== 0).map((m) => m.host);
  if (new Set(published).size !== published.length) {
    return "Two mappings publish the same host port — only one container can hold it.";
  }
  const containers = mappings.map((m) => m.container);
  if (new Set(containers).size !== containers.length) {
    return "The same container port is mapped twice.";
  }
  return null;
}

/** The `ports` block of the resource spec. Shape matches the reconciler's
 *  appResourceSpec.Ports exactly — a renamed field here silently unpublishes an
 *  app rather than failing. */
export function specPorts(
  mappings: PortMapping[]
): { container: number; host: number; protocol: string }[] {
  return mappings
    .filter((m) => isValidPort(m.container))
    .map((m) => ({ container: m.container, host: m.host, protocol: "tcp" }));
}

export type DetectedHealthCheck = {
  type?: string;
  path?: string;
  port?: number;
  intervalSec?: number;
  source?: string;
};

/**
 * The health path to pre-fill. A detected HTTP probe wins; otherwise "/", which
 * is what an operator overwhelmingly means and is trivially correctable — as
 * opposed to the previous behaviour, which offered nothing and silently shipped
 * a TCP probe.
 */
export function defaultHealthPath(hc: DetectedHealthCheck | null | undefined): string {
  const path = hc?.path?.trim();
  if (hc?.type === "http" && path) return path;
  return "/";
}

/** The port a probe should target: the detected one, else the first mapping. */
export function defaultHealthPort(
  hc: DetectedHealthCheck | null | undefined,
  mappings: PortMapping[]
): number {
  if (hc?.port && isValidPort(hc.port)) return hc.port;
  const first = mappings.find((m) => isValidPort(m.container));
  return first?.container ?? 0;
}

/**
 * The `healthCheck` block of the spec.
 *
 * An empty path means the user cleared it, which is a request for a TCP probe —
 * not a request for an HTTP probe against "". The interval is carried through
 * from detection so a repo that declared `--interval=30s` keeps it.
 */
export function healthCheckSpec(input: {
  path: string;
  port: number;
  detected?: DetectedHealthCheck | null;
}): { type: string; path?: string; port?: number; intervalSec?: number } {
  const path = input.path.trim();
  const intervalSec = input.detected?.intervalSec;
  if (!path) {
    return {
      type: "tcp",
      ...(isValidPort(input.port) ? { port: input.port } : {}),
      ...(intervalSec ? { intervalSec } : {}),
    };
  }
  return {
    type: "http",
    path: path.startsWith("/") ? path : `/${path}`,
    ...(isValidPort(input.port) ? { port: input.port } : {}),
    ...(intervalSec ? { intervalSec } : {}),
  };
}

/**
 * A domain the user may attach at create time.
 *
 * Kept deliberately permissive — the control plane owns DNS validation and the
 * dashboard already has a setup dialog for it. This only stops the obvious
 * paste of a whole URL, which is the mistake people actually make here.
 */
export function domainError(value: string): string | null {
  const raw = value.trim();
  if (!raw) return null;
  if (raw.includes("://")) return "Enter just the hostname, without http:// or https://.";
  if (raw.includes("/")) return "Enter just the hostname, without a path.";
  if (!/^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)+$/.test(raw)) {
    return "That doesn't look like a hostname (e.g. app.example.com).";
  }
  return null;
}

/**
 * Whether anything is reachable from outside.
 *
 * The wizard says this out loud because "host 0 = internal only" is the safe
 * default AND the surprising one: a user who published nothing and attached no
 * domain has deployed something they cannot reach, and should find that out
 * here rather than by curling it.
 */
export function reachability(
  mappings: PortMapping[],
  domain: string
): { reachable: boolean; summary: string } {
  if (domain.trim()) {
    return { reachable: true, summary: `Reachable at https://${domain.trim()}` };
  }
  const published = mappings.filter((m) => m.host !== 0);
  if (published.length > 0) {
    return {
      reachable: true,
      summary: `Published on host port${published.length > 1 ? "s" : ""} ${published
        .map((m) => m.host)
        .join(", ")}`,
    };
  }
  return {
    reachable: false,
    summary:
      "Internal only — reachable from other resources on the private network, not from the internet. Attach a domain to change that.",
  };
}
