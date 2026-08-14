import { afterEach, describe, expect, it } from "vitest";

import {
  configuredMailTransport,
  defaultSmtpPort,
  envelopeAddress,
  mailDelivers,
} from "./mail";

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

  // The failure this closes was silent all the way to the user: a display-name
  // SMTP_FROM went on the wire inside the envelope's angle brackets, the server
  // answered 501, and the only trace was a line in the web container's log for a
  // reset mail nobody was watching for. The value is judged when it is read.
  it("refuses an SMTP_FROM that is not an address, naming the variable", () => {
    process.env.SMTP_HOST = "smtp.example.com";

    for (const bad of ["no-reply", "SigmaHub <no-reply>", "@example.com", "a b@c"]) {
      process.env.SMTP_FROM = bad;
      expect(() => configuredMailTransport()).toThrow(/SMTP_FROM/);
    }
  });

  it("accepts a display-name SMTP_FROM — the envelope split handles it", () => {
    process.env.SMTP_HOST = "smtp.example.com";
    process.env.SMTP_FROM = "SigmaHub <no-reply@example.com>";

    // Kept whole: buildMessage puts it in the From: HEADER, where a display name
    // belongs. Only the envelope is narrowed, at send time.
    expect(configuredMailTransport()).toMatchObject({
      from: "SigmaHub <no-reply@example.com>",
    });
    expect(envelopeAddress("SigmaHub <no-reply@example.com>")).toBe(
      "no-reply@example.com"
    );
  });
});

describe("envelopeAddress", () => {
  it("passes a bare address through", () => {
    expect(envelopeAddress("no-reply@example.com")).toBe("no-reply@example.com");
    expect(envelopeAddress("  no-reply@example.com  ")).toBe("no-reply@example.com");
  });

  it("unwraps the two forms an operator naturally writes", () => {
    expect(envelopeAddress("SigmaHub <no-reply@example.com>")).toBe(
      "no-reply@example.com"
    );
    expect(envelopeAddress("<no-reply@example.com>")).toBe("no-reply@example.com");
  });

  it("takes the LAST bracketed group, so a bracketed display name cannot win", () => {
    expect(envelopeAddress("<SigmaHub> <no-reply@example.com>")).toBe(
      "no-reply@example.com"
    );
  });

  it("strips CR/LF, so a crafted value cannot smuggle a command or a header", () => {
    expect(envelopeAddress("a@b.com\r\nRCPT TO:<victim@c.com>")).not.toContain("\r");
    expect(envelopeAddress("a@b.com\nBcc: victim@c.com")).not.toContain("\n");
  });
});
