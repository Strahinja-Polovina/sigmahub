import { cpBaseOrNull, cpProvisionToken } from "@/server/cp";

// The dashboard's deploy gate (SIGMA-265).
//
// A rollout has to be able to ask "can this container do its job?", and until
// this route existed the only thing CI could ask was `GET /` — the marketing
// home page, which renders for an anonymous request out of static sections and
// never reads SIGMAHUB_CP_URL, never mints a token, never dials the control
// plane. Every failure of the web→CP path therefore looked identical to a
// healthy deploy: 200, sections rendered, green tick, and every logged-in page
// throwing on its first control-plane call.
//
// `?require=cp` is how the caller states what it expects. A demo deployment
// runs with no control plane ON PURPOSE and is not broken, so a bare
// /api/health reports `cp: "disabled"` and stays 200; the staging rollout,
// which knows a control plane must be there, polls `?require=cp` and gets a 503
// the moment the dashboard cannot reach it.
export const dynamic = "force-dynamic";

type CpHealth = "ok" | "disabled" | "unconfigured" | "unauthorized" | "unreachable";

/** How long to wait on the control plane before calling it unreachable. The
 *  rollout polls this route every two seconds, so a probe that hangs is a probe
 *  that piles up. */
const PROBE_TIMEOUT_MS = 3_000;

/** Round-trip to the control plane with the dashboard's real credential.
 *
 *  The probe is `POST /v1/orgs` with an empty body. That endpoint is the one
 *  the provision token opens, and it validates the credential BEFORE it looks
 *  at the body — so a valid credential answers 400 ("orgId is required") and an
 *  invalid one answers 401, which is exactly the distinction being checked.
 *  Nothing is created: an empty orgId never reaches the token issuer. A health
 *  check must not have side effects, and provisioning a throwaway org on every
 *  poll would be a side effect that accumulates for as long as the box is up. */
async function probeControlPlane(base: string): Promise<CpHealth> {
  let res: Response;
  try {
    res = await fetch(`${base}/v1/orgs`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${cpProvisionToken()}`,
        "Content-Type": "application/json",
      },
      body: "{}",
      cache: "no-store",
      signal: AbortSignal.timeout(PROBE_TIMEOUT_MS),
    });
  } catch {
    return "unreachable";
  }
  if (res.status === 401 || res.status === 403) return "unauthorized";
  // Anything the control plane answers below 500 means it is up and it accepted
  // who we are; 5xx means it is up but not serving.
  return res.status < 500 ? "ok" : "unreachable";
}

/** Optional shared secret for the CP probe. Unset (the default, and every
 *  existing rollout) leaves the probe open, as it was.
 *
 *  Deliberately no response memoisation: this is a deploy gate, and a gate that
 *  answers from a cache can report a control plane that has since gone away. The
 *  amplification concern is addressed by not probing at all unless asked, and by
 *  this token on hosted deployments. */
function probeAuthorized(request: Request): boolean {
  const want = (process.env.HEALTH_PROBE_TOKEN ?? "").trim();
  if (!want) return true;
  const got = (request.headers.get("authorization") ?? "").replace(/^Bearer\s+/i, "").trim();
  return got === want;
}

export async function GET(request: Request) {
  const required = new URL(request.url).searchParams.get("require")?.split(",") ?? [];
  const wantCp = required.includes("cp");

  // Bare liveness: "this container is serving". No control-plane round-trip and
  // no posture disclosure — an anonymous caller learns nothing about whether a
  // control plane is configured, reachable, or accepting our credential.
  if (!wantCp) {
    return Response.json(
      { status: "ok" },
      { status: 200, headers: { "Cache-Control": "no-store" } }
    );
  }

  if (!probeAuthorized(request)) {
    return Response.json(
      { status: "unauthorized" },
      { status: 401, headers: { "Cache-Control": "no-store" } }
    );
  }

  const base = cpBaseOrNull();
  const cp: CpHealth = base ? await probeControlPlane(base) : "unconfigured";
  const healthy = cp === "ok";
  return Response.json(
    { status: healthy ? "ok" : "degraded", cp },
    {
      status: healthy ? 200 : 503,
      headers: { "Cache-Control": "no-store" },
    }
  );
}
