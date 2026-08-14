import { afterEach, describe, expect, it } from "vitest";

import { configuredMailTransport, defaultSmtpPort, mailDelivers } from "./mail";

// One switch decides whether this deployment delivers mail, and both the senders
// and the copy on /forgot, /signup and the invite dialog read it (SIGMA-307).
// So the switch itself is worth pinning: a half-configured deployment must keep
// saying "not delivered" rather than start promising an inbox (SIGMA-365).

const vars = ["SMTP_HOST", "SMTP_FROM", "SMTP_PORT"] as const;

afterEach(() => {
  for (const v of vars) delete process.env[v];
});

describe("configuredMailTransport", () => {
  it("is the log transport when nothing is configured", () => {
    expect(configuredMailTransport()).toEqual({ kind: "log" });
    expect(mailDelivers()).toBe(false);
  });

  it("needs BOTH a host and an envelope sender", () => {
    process.env.SMTP_HOST = "smtp.example.com";
    expect(mailDelivers()).toBe(false);

    delete process.env.SMTP_HOST;
    process.env.SMTP_FROM = "SigmaHub <no-reply@example.com>";
    expect(mailDelivers()).toBe(false);
  });

  it("delivers over SMTP once host and sender are set, defaulting to submission", () => {
    process.env.SMTP_HOST = "smtp.example.com";
    process.env.SMTP_FROM = "no-reply@example.com";

    expect(configuredMailTransport()).toEqual({
      kind: "smtp",
      host: "smtp.example.com",
      from: "no-reply@example.com",
      port: defaultSmtpPort,
    });
    expect(mailDelivers()).toBe(true);
  });

  it("takes an explicit port and ignores an unparseable one", () => {
    process.env.SMTP_HOST = "smtp.example.com";
    process.env.SMTP_FROM = "no-reply@example.com";

    process.env.SMTP_PORT = "465";
    expect(configuredMailTransport()).toMatchObject({ port: 465 });

    process.env.SMTP_PORT = "not-a-port";
    expect(configuredMailTransport()).toMatchObject({ port: defaultSmtpPort });
  });

  it("carries no credentials — the descriptor is readable by UI code", () => {
    process.env.SMTP_HOST = "smtp.example.com";
    process.env.SMTP_FROM = "no-reply@example.com";
    process.env.SMTP_PASSWORD = "hunter2";

    expect(JSON.stringify(configuredMailTransport())).not.toContain("hunter2");
    delete process.env.SMTP_PASSWORD;
  });
});
