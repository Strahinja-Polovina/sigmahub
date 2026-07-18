import { describe, it, expect } from "vitest";
import {
  hashInviteToken,
  newInviteToken,
  inviteUsable,
  inviteRejection,
  inviteRejectionMessage,
  normalizeOrgRole,
  parseProjectGrants,
  serializeProjectGrants,
  inviteUrl,
  sameEmail,
  INVITE_TTL_MS,
} from "./invite";

describe("hashInviteToken", () => {
  it("is deterministic and hex (never returns the raw token)", () => {
    const raw = "s3cr3t-token";
    const h = hashInviteToken(raw);
    expect(h).toBe(hashInviteToken(raw));
    expect(h).toMatch(/^[0-9a-f]{64}$/);
    expect(h).not.toContain(raw);
  });
  it("differs per input", () => {
    expect(hashInviteToken("a")).not.toBe(hashInviteToken("b"));
  });
});

describe("newInviteToken", () => {
  it("returns a URL-safe raw token whose hash matches", () => {
    const { raw, hash } = newInviteToken();
    expect(raw).toMatch(/^[A-Za-z0-9_-]+$/); // base64url, no +/=
    expect(hash).toBe(hashInviteToken(raw));
  });
  it("is unique per call", () => {
    expect(newInviteToken().raw).not.toBe(newInviteToken().raw);
  });
});

describe("inviteUsable / inviteRejection", () => {
  const now = new Date("2027-03-01T00:00:00Z");
  const future = new Date(now.getTime() + INVITE_TTL_MS);
  const past = new Date(now.getTime() - 1000);

  it("pending + unexpired is usable, nothing else", () => {
    expect(inviteUsable({ status: "pending", expiresAt: future }, now)).toBe(true);
    expect(inviteUsable({ status: "pending", expiresAt: past }, now)).toBe(false);
    expect(inviteUsable({ status: "accepted", expiresAt: future }, now)).toBe(false);
    expect(inviteUsable({ status: "revoked", expiresAt: future }, now)).toBe(false);
  });

  it("classifies the rejection reason precisely", () => {
    expect(inviteRejection(null, now)).toBe("not-found");
    expect(inviteRejection({ status: "revoked", expiresAt: future }, now)).toBe("revoked");
    expect(inviteRejection({ status: "accepted", expiresAt: future }, now)).toBe("accepted");
    expect(inviteRejection({ status: "pending", expiresAt: past }, now)).toBe("expired");
    expect(inviteRejection({ status: "pending", expiresAt: future }, now)).toBeNull();
  });

  it("has copy for every rejection reason", () => {
    for (const r of ["not-found", "revoked", "accepted", "expired"] as const) {
      expect(inviteRejectionMessage(r)).toBeTruthy();
    }
  });
});

describe("normalizeOrgRole", () => {
  it("passes known roles and floors unknown to Developer", () => {
    expect(normalizeOrgRole("Org Admin")).toBe("Org Admin");
    expect(normalizeOrgRole("Project Admin")).toBe("Project Admin");
    expect(normalizeOrgRole("Developer")).toBe("Developer");
    expect(normalizeOrgRole("root")).toBe("Developer");
    expect(normalizeOrgRole("")).toBe("Developer");
  });
});

describe("project grants round-trip + validation", () => {
  it("keeps only well-formed grants", () => {
    const grants = parseProjectGrants(
      JSON.stringify([
        { projectId: "proj_a", role: "Project Admin" },
        { projectId: "proj_b", role: "Developer" },
        { projectId: "proj_c", role: "Org Admin" }, // not a project role → dropped
        { projectId: 42, role: "Developer" }, // bad id → dropped
        { role: "Developer" }, // missing id → dropped
        "nonsense",
      ])
    );
    expect(grants).toEqual([
      { projectId: "proj_a", role: "Project Admin" },
      { projectId: "proj_b", role: "Developer" },
    ]);
  });

  it("collapses malformed JSON to an empty list (never throws)", () => {
    expect(parseProjectGrants("not json")).toEqual([]);
    expect(parseProjectGrants("{}")).toEqual([]);
    expect(parseProjectGrants("[]")).toEqual([]);
  });

  it("serialize drops invalid entries and round-trips", () => {
    const json = serializeProjectGrants([
      { projectId: "proj_a", role: "Developer" },
      { projectId: "proj_b", role: "root" }, // invalid role → dropped
    ]);
    expect(parseProjectGrants(json)).toEqual([{ projectId: "proj_a", role: "Developer" }]);
  });
});

describe("inviteUrl", () => {
  it("joins base + token, tolerating a trailing slash", () => {
    expect(inviteUrl("https://app.example.com", "abc")).toBe(
      "https://app.example.com/invite/abc"
    );
    expect(inviteUrl("https://app.example.com/", "abc")).toBe(
      "https://app.example.com/invite/abc"
    );
  });
  it("encodes the token", () => {
    expect(inviteUrl("https://x", "a/b?c")).toBe("https://x/invite/a%2Fb%3Fc");
  });
});

describe("sameEmail", () => {
  it("matches case- and whitespace-insensitively", () => {
    expect(sameEmail("Ada@Example.com", "  ada@example.com ")).toBe(true);
    expect(sameEmail("a@x.com", "b@x.com")).toBe(false);
  });
});
