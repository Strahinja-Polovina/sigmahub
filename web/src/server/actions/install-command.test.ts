// The one-line install command, and the outage it was reported for.
//
// The wizard used to render `curl -fsSL https://github.com/<owner>/<repo>/
// releases/download/<tag>/install.sh`, and install.sh then fetched four more
// assets from the same unauthenticated place. On a PRIVATE release repository
// all five 404: an operator could not onboard a single server, and made the
// repository public to get past it. The control plane serves the script and
// proxies the assets now, authenticating to GitHub server-side.
//
// So what this file pins is not a string's cosmetics. It is that the command
// names ONE host — the control plane the operator already has to reach — that
// it carries no credential of its own, and that the variable it hands the
// script is the variable the script reads. That last one is the interesting
// failure: SIGMAHUB_DOWNLOAD_BASE is defaulted inside install.sh, so a typo in
// the name here does not error anywhere. The script silently falls back to
// github.com and reproduces the reported bug exactly, with a command that still
// looks right on the page. The assertion below reads the shell off disk for the
// same reason lib/decommission.test.ts does: two languages, one fact, and
// neither can import the other.

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const CP_URL = "https://cp.example.com";
/** What cpPublicUrl() answers. Mutable because the scheme is now part of the
 *  contract — the command pipes this URL into `sudo bash`, so a test has to be
 *  able to hand it a plaintext one. */
let publicUrl = CP_URL;
const VERSION = "v0.3.0";
const TOKEN = "sbt_testtoken";
/** What the control plane says it installs, returned with the bootstrap token.
 *  Mutable because it is now the ONLY source of the version in the command —
 *  the tests below steer it to prove the dashboard has no opinion of its own. */
let release: { agentVersion: string; agentVersionError?: string } = { agentVersion: VERSION };

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
vi.mock("@/server/active-org", () => {
  const actor = { user: { id: "usr_you", name: "you" }, role: "Org Admin" };
  return {
    requireMembership: async () => actor,
    requireProjectAdmin: async () => actor,
    getActiveOrgId: async () => "org_demo",
  };
});
// CP mode is the branch that renders an install command at all, so cpEnabled is
// true here and every other export throws unless this file needs it — a call
// that slipped through would otherwise pass silently and cover nothing.
vi.mock("@/server/cp", () => {
  const forbidden = async () => {
    throw new Error("this action must not reach the control-plane client");
  };
  return {
    cpEnabled: () => true,
    cpPublicUrl: () => publicUrl,
    cpReissueBootstrapToken: async () => ({
      token: TOKEN,
      bootstrapPubkey: "ssh-ed25519 AAAA sigmahub-bootstrap",
      expiresAt: new Date(Date.now() + 900_000).toISOString(),
      ...release,
    }),
    cpIssueBootstrapToken: forbidden,
    cpProvisionServer: forbidden,
    cpSetHardening: forbidden,
    cpSetProxyRole: forbidden,
    cpDecommissionServer: forbidden,
    cpDeleteServer: forbidden,
    cpGetServer: forbidden,
    boundResourcesOf: () => [],
    cpRenameServer: forbidden,
    cpSetServerType: forbidden,
    cpUpdateServerAgent: forbidden,
  };
});

import { connectServer, reissueInstallCommand } from "./servers";

/** The rendered command. reissueInstallCommand is the leanest of the three
 *  actions that hand one out (provisionServer and connectServer's CP branch
 *  render the same helper), so it is the one driven here. The action answers
 *  with a result rather than a throw — a thrown server-action error is
 *  redacted in production — so a refusal comes back as `{ ok: false }`. */
async function renderedCommand(): Promise<string> {
  const res = await reissueInstallCommand({ serverId: "srv_notlocal" });
  if (!res.ok) throw new Error(res.error);
  return res.command;
}

/** A refusal's message, asserted off the result the dialog actually renders. */
async function refusal(): Promise<string> {
  const res = await reissueInstallCommand({ serverId: "srv_notlocal" });
  expect(res.ok, "expected the action to refuse, but it rendered a command").toBe(false);
  return res.ok ? "" : res.error;
}

const installScript = readFileSync(
  join(process.cwd(), "..", "agent", "packaging", "install.sh"),
  "utf8"
);

/** install.sh with its comments removed. The header documents the command this
 *  file renders, so a variable NAMED in a comment proves nothing about whether
 *  the script reads it — and "the script mentions it somewhere" is precisely
 *  the check that would pass while the fallback to github.com went unnoticed. */
const installScriptCode = installScript
  .split("\n")
  .filter((line) => !line.trimStart().startsWith("#"))
  .join("\n");

beforeEach(() => {
  release = { agentVersion: VERSION };
});

afterEach(() => {
  release = { agentVersion: VERSION };
  delete process.env.SIGMAHUB_AGENT_VERSION;
});

describe("the install command the connect wizard renders", () => {
  it("fetches install.sh from the control plane, never from github.com", async () => {
    const command = await renderedCommand();
    expect(command.startsWith(`curl -fsSL ${CP_URL}/install.sh | `)).toBe(true);
    expect(command).not.toContain("github.com");
  });

  it("sends the script's own asset downloads back to the control plane", async () => {
    // Without this the script keeps its github.com default and four of the five
    // downloads 404 on a private repository — after the operator's terminal has
    // already run the first one successfully, which is what made the original
    // report so confusing.
    const command = await renderedCommand();
    expect(command).toContain(`SIGMAHUB_DOWNLOAD_BASE=${CP_URL}/dl/${VERSION}`);
  });

  it("names one host, so an operator who can reach the dashboard can onboard", async () => {
    const command = await renderedCommand();
    const urls = command.match(/https?:\/\/[^\s|]+/g) ?? [];
    expect(urls.length).toBeGreaterThan(0);
    for (const url of urls) {
      expect(url.startsWith(`${CP_URL}/`) || url === CP_URL).toBe(true);
    }
  });

  it("carries the bootstrap token and nothing else secret", async () => {
    // The control plane holds the GitHub credential; the command is pasted into
    // a terminal, screenshotted into tickets and left in shell history, so the
    // one-time bootstrap token — which expires, and is spent on first use — must
    // remain the only secret in it.
    const command = await renderedCommand();
    expect(command).toContain(`SIGMAHUB_BOOTSTRAP_TOKEN=${TOKEN}`);
    for (const leak of ["ghp_", "github_pat_", "Authorization", "CP_GITHUB", "token="]) {
      expect(command).not.toContain(leak);
    }
  });

  it("still pins the release, because the version is a path segment now", async () => {
    const command = await renderedCommand();
    expect(command).toContain(`SIGMAHUB_VERSION=${VERSION}`);
    expect(command).toContain(`SIGMAHUB_ENDPOINT=${CP_URL}`);
  });

  it("sets only variables agent/packaging/install.sh actually reads", async () => {
    const command = await renderedCommand();
    const set = [...command.matchAll(/\b(SIGMAHUB_[A-Z_]+)=/g)].map((m) => m[1]);
    expect(set.length).toBeGreaterThan(0);
    for (const name of set) {
      const read = new RegExp("\\$\\{" + name + "[:}]").test(installScriptCode);
      expect(
        read,
        `the install command sets ${name}, but install.sh never expands \${${name}} — ` +
          "a variable the script does not read is a setting that silently does nothing, " +
          "and for SIGMAHUB_DOWNLOAD_BASE that means falling back to github.com"
      ).toBe(true);
    }
  });

  it("refuses to render a command when the control plane is pinned to no release", async () => {
    // An empty version is how the control plane says "I cannot serve an
    // installer": /install.sh would answer 503 and every /dl path would 404, so
    // the only honest thing to render is nothing.
    release = { agentVersion: "", agentVersionError: "CP_AGENT_VERSION is not a released tag" };
    expect(await refusal()).toMatch(/CP_AGENT_VERSION/);
  });

  it("passes the control plane's own refusal through instead of a paraphrase", async () => {
    // The setting to change lives on the control plane, so the sentence naming
    // it has to come from there too — a second wording here is a second thing
    // to keep true.
    release = {
      agentVersion: "",
      agentVersionError: "this control plane is not configured to serve the agent installer. Set CP_RELEASE_REPO to …",
    };
    expect(await refusal()).toMatch(/CP_RELEASE_REPO/);
  });
});

// The version in the command and the version the control plane actually serves
// were, until SIGMA-217's follow-up, two settings: the dashboard read
// SIGMAHUB_AGENT_VERSION to build the /dl/{version} paths while the control
// plane served GET /install.sh from its own CP_AGENT_VERSION. Both halves were
// individually correct, and a deployment that set one and not the other — or set
// them to different tags — handed the operator a command that installed a
// version nobody chose. It is this project's recurring defect: two sides of one
// question, each answering it honestly.
//
// What these two tests pin is that there is only one answer left. The version
// arrives with the bootstrap token, from the control plane that will serve every
// URL in the line, and this process has no opinion to contribute.
describe("the release in the command is the control plane's, and only the control plane's", () => {
  it("renders whatever version came back with the token", async () => {
    release = { agentVersion: "v9.9.9-rc.1" };
    const command = await renderedCommand();
    expect(command).toContain("SIGMAHUB_VERSION=v9.9.9-rc.1");
    expect(command).toContain(`SIGMAHUB_DOWNLOAD_BASE=${CP_URL}/dl/v9.9.9-rc.1`);
    // One version, in both places it appears — the failure this whole change
    // exists to make impossible is a command whose script and assets disagree.
    expect(command).not.toContain(VERSION);
  });

  it("ignores SIGMAHUB_AGENT_VERSION, the second source that used to decide this", async () => {
    process.env.SIGMAHUB_AGENT_VERSION = "v0.0.1";
    release = { agentVersion: VERSION };
    const command = await renderedCommand();
    expect(command).toContain(`SIGMAHUB_VERSION=${VERSION}`);
    expect(command).not.toContain("v0.0.1");
  });
});

// install.sh is the ONE artifact cosign does not cover, because install.sh is
// what runs cosign. Its integrity has always rested on TLS — the command used
// to hard-code https://github.com/… — and moving the fetch to the control plane
// moved that trust without moving the requirement with it. The deployment guide
// shipped an http:// public URL and the control plane terminates no TLS itself,
// so the documented default piped plaintext into `sudo bash`: an on-path
// attacker goes from reading a bootstrap token to root on every host onboarded.
describe("the install command refuses to pipe plaintext into sudo bash", () => {
  afterEach(() => {
    publicUrl = CP_URL;
  });

  it("refuses when the control plane's public URL is not https", async () => {
    for (const url of ["http://cp.example.com", "http://10.0.0.5:8080"]) {
      publicUrl = url;
      expect(await refusal()).toMatch(/https/i);
    }
  });

  it("names the setting to change rather than the symptom", async () => {
    publicUrl = "http://cp.example.com";
    expect(await refusal()).toMatch(/SIGMAHUB_CP_PUBLIC_URL/);
  });
});

// SIGMA-315. There were two renderings of "the command an operator pastes into
// a root shell". The one above is cosign-verified, pinned to the control
// plane's release, and refuses over plaintext. connectServer's CP branch
// returned `sigmad --endpoint … --bootstrap-token …`: no fetch, no signature
// check, no version pin, and outside the https guard entirely — a command that
// does nothing at all on a fresh machine, because it assumes sigmad is already
// installed there.
//
// The connect dialog routes CP mode to provisionServer today, so the branch was
// reachable only by calling the action directly. That is a property of one
// caller, not of the action: the second renderer is a second thing that has to
// stay true, and it had already stopped being true.
describe("the command connectServer hands out is the same command", () => {
  const ISSUED = {
    token: TOKEN,
    serverId: "srv_connect",
    expiresAt: new Date(Date.now() + 900_000).toISOString(),
  };

  async function issuing(over: Partial<typeof release> = {}) {
    const mod = await import("@/server/cp");
    return vi
      .spyOn(mod, "cpIssueBootstrapToken")
      .mockResolvedValue({ ...ISSUED, ...release, ...over });
  }

  afterEach(() => {
    publicUrl = CP_URL;
    vi.restoreAllMocks();
  });

  const connect = () =>
    connectServer({ orgId: "org_demo", type: "general", hostIp: "203.0.113.7" });

  it("fetches and verifies the installer instead of assuming sigmad is on the host", async () => {
    await issuing();
    const res = await connect();
    if (res.mode !== "cp") throw new Error("expected the control-plane branch");
    expect(res.command).toBe(
      `curl -fsSL ${CP_URL}/install.sh | SIGMAHUB_ENDPOINT=${CP_URL} ` +
        `SIGMAHUB_BOOTSTRAP_TOKEN=${TOKEN} SIGMAHUB_VERSION=${VERSION} ` +
        `SIGMAHUB_DOWNLOAD_BASE=${CP_URL}/dl/${VERSION} sudo -E bash`
    );
  });

  it("refuses to render over plaintext, and mints nothing when it refuses", async () => {
    publicUrl = "http://cp:8080";
    const issue = await issuing();
    // The refusal is a result, not a throw: production redacts thrown
    // server-action errors, and this message names the setting to change.
    const res = await connect();
    expect(res.mode).toBe("error");
    if (res.mode === "error") expect(res.error).toMatch(/SIGMAHUB_CP_PUBLIC_URL/);
    // Same shape as SIGMA-300: a refusal that arrives after the control plane
    // has pre-created a row and burned a one-time token leaves the operator
    // retrying against a growing column of Provisioning hosts.
    expect(issue).not.toHaveBeenCalled();
  });

  it("refuses when the control plane is pinned to no release, and takes the row back", async () => {
    // This refusal cannot be pre-empted — the release comes back with the
    // token — so the row the control plane pre-created is undone instead.
    const mod = await import("@/server/cp");
    await issuing({ agentVersion: "", agentVersionError: "CP_AGENT_VERSION is not a released tag" });
    const remove = vi.spyOn(mod, "cpDeleteServer").mockResolvedValue(undefined);
    const res = await connect();
    expect(res.mode).toBe("error");
    if (res.mode === "error") expect(res.error).toMatch(/CP_AGENT_VERSION/);
    expect(remove).toHaveBeenCalledWith("org_demo", ISSUED.serverId, expect.anything());
  });
});
