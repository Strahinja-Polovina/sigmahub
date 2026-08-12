// SIGMA-353: turn the agent's raw failure string into a named cause and the one
// action worth taking next.
//
// The agent already reports a real error when an op fails (SIGMA-301) — "health
// gate timed out after 2m0s", "pull from ghcr.io/… needs a registry credential",
// "address already in use". The resource page showed that string verbatim under
// a single generic line, "Fix the cause, then press Deploy", which is true of
// every failure and therefore useful for none: a health-check timeout and a
// missing registry credential need completely different fixes, and the product
// already knows which one it was.
//
// This maps the string to a category and a specific remediation. It is
// deliberately substring-matching rather than a structured code from the agent:
// the errors are wrapped with %w through several layers, so the reliable signal
// is the phrase the leaf produced, and a new leaf error simply falls through to
// the honest generic case rather than being misclassified.

export type DeployFailureCategory =
  | "registry"
  | "image"
  | "build"
  | "health"
  | "port"
  | "volume"
  | "network"
  | "unknown";

export type DeployFailure = {
  category: DeployFailureCategory;
  /** A short label for the kind of failure, so terminal states are
   *  distinguishable rather than all reading "failed". */
  title: string;
  /** The single next action, phrased as a thing to do — not "an error
   *  occurred". */
  remediation: string;
};

const GENERIC: DeployFailure = {
  category: "unknown",
  title: "Deployment failed",
  remediation:
    "Check the build output and logs in the Deployments tab for the cause, fix it, then press Deploy to re-apply.",
};

/**
 * Classify a deploy/convergence failure string. Returns the generic case for an
 * empty or unrecognized error — never worse than the single line it replaces.
 */
export function classifyDeployFailure(error: string | null | undefined): DeployFailure {
  const e = (error ?? "").toLowerCase();
  if (!e.trim()) return GENERIC;

  // Order matters: the more specific phrase wins. A registry-credential failure
  // also contains "pull", so it is checked before the general image case.
  if (e.includes("registry credential") || e.includes("no way to fetch")) {
    return {
      category: "registry",
      title: "Registry credential missing",
      remediation:
        "This image is private and the server has no credential to pull it. Connect the registry in Settings → Integrations, or make the image public, then Deploy.",
    };
  }
  if (
    e.includes("manifest unknown") ||
    e.includes("not found") ||
    e.includes("pull access denied") ||
    (e.includes("pull") && e.includes("image"))
  ) {
    return {
      category: "image",
      title: "Image could not be pulled",
      remediation:
        "The image tag could not be fetched — check the image name and tag are correct and the registry is reachable, then Deploy.",
    };
  }
  if (
    e.includes("empty image tag") ||
    e.includes("build context") ||
    e.includes("build image") ||
    e.includes("dockerfile")
  ) {
    return {
      category: "build",
      title: "Build failed",
      remediation:
        "The image did not build. Open the build output in the Deployments tab — a bad Dockerfile path or a build error is the usual cause — fix it and push again.",
    };
  }
  if (e.includes("health") || e.includes("unhealthy")) {
    return {
      category: "health",
      title: "Health check never passed",
      remediation:
        "The container started but its health check never went green. Check the app listens on the declared port and the health path returns 2xx, then Deploy. Adjust the check in Settings if it is wrong.",
    };
  }
  if (
    e.includes("address already in use") ||
    e.includes("bind") ||
    e.includes("listen tcp") ||
    e.includes("port is allocated")
  ) {
    return {
      category: "port",
      title: "Port already in use",
      remediation:
        "A host port this app publishes is held by something SigmaHub does not manage. Change the published port, or free it on the server, then Deploy.",
    };
  }
  if (e.includes("volume")) {
    return {
      category: "volume",
      title: "Volume could not be prepared",
      remediation:
        "A data volume could not be created or mounted — usually a disk or permissions problem on the server. Check the host has free disk, then Deploy.",
    };
  }
  if (e.includes("network")) {
    return {
      category: "network",
      title: "Network could not be prepared",
      remediation:
        "The container network could not be created on the server. Check the agent is healthy on the host, then Deploy.",
    };
  }
  return GENERIC;
}
