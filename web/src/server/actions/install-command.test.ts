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

import { reissueInstallCommand } from "./servers";

/** The rendered command. reissueInstallCommand is the leanest of the three
 *  actions that hand one out (provisionServer and connectServer's CP branch
 *  render the same helper), so it is the one driven here. */
async function renderedCommand(): Promise<string> {
  const { command } = await reissueInstallCommand({ serverId: "srv_notlocal" });
  return command;
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
  process.env.SIGMAHUB_AGENT_VERSION = VERSION;
});

afterEach(() => {
  process.env.SIGMAHUB_AGENT_VERSION = VERSION;
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

  it("refuses to render a command when no release is pinned", async () => {
    delete process.env.SIGMAHUB_AGENT_VERSION;
    await expect(renderedCommand()).rejects.toThrow(/SIGMAHUB_AGENT_VERSION/);
  });

  it("refuses \"latest\", which is a tag no release asset is published under", async () => {
    process.env.SIGMAHUB_AGENT_VERSION = "latest";
    await expect(renderedCommand()).rejects.toThrow(/released tag/);
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

  it("throws when the control plane's public URL is not https", async () => {
    for (const url of ["http://cp.example.com", "http://10.0.0.5:8080"]) {
      publicUrl = url;
      await expect(renderedCommand()).rejects.toThrow(/https/i);
    }
  });

  it("names the setting to change rather than the symptom", async () => {
    publicUrl = "http://cp.example.com";
    await expect(renderedCommand()).rejects.toThrow(/SIGMAHUB_CP_PUBLIC_URL/);
  });
});
