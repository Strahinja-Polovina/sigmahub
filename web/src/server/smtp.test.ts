// The SMTP client, driven against a real socket (SIGMA-365).
//
// smtp.ts is hand-written wire protocol on the critical path for password resets
// and invites, and its failure mode is silence: a locked-out user cannot report
// that the mail never came, and the operator sees one log line. So it is tested
// against an actual server speaking actual SMTP over an actual TCP socket rather
// than against a mocked transport — the bugs worth catching here (a multiline
// reply, a reply split across packets, dot-stuffing, a promise that never
// settles) all live precisely in the layer a mock would replace.
//
// The fake server is deliberately pedantic about the wire format: it answers only
// what it was scripted to answer and records every command verbatim, so a test
// asserts the bytes SigmaHub actually put on the socket.

import net from "node:net";
import { afterEach, describe, expect, it } from "vitest";

import { sendSmtpMail } from "./smtp";


type FakeOptions = {
  /** Extension lines returned after EHLO, without the 250 prefix. */
  extensions?: string[];
  /** Override the reply to a specific command prefix, e.g. { "RCPT TO": "550 no" }. */
  replies?: Record<string, string>;
  /** Write the greeting one byte at a time, to exercise split reads. */
  dribble?: boolean;
  /** Never answer anything — for the timeout test. */
  silent?: boolean;
};

type Fake = {
  port: number;
  commands: string[];
  /** Everything received inside DATA, before the terminating dot. */
  body: string;
  close: () => Promise<void>;
};

/** A single-connection SMTP server, just complete enough to be honest. */
async function startFakeSmtp(opts: FakeOptions = {}): Promise<Fake> {
  const commands: string[] = [];
  let body = "";

  const server = net.createServer((sock) => {
    sock.setEncoding("utf8");
    if (opts.silent) return; // accept and say nothing at all
    let inData = false;
    let buf = "";

    const write = (s: string) => {
      if (opts.dribble) {
        for (const ch of s) sock.write(ch);
      } else {
        sock.write(s);
      }
    };

    write("220 fake.test ESMTP ready\r\n");

    sock.on("data", (chunk: string) => {
      buf += chunk;
      for (;;) {
        const nl = buf.indexOf("\r\n");
        if (nl < 0) break;
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + 2);

        if (inData) {
          if (line === ".") {
            inData = false;
            write("250 2.0.0 queued\r\n");
          } else {
            body += line + "\n";
          }
          continue;
        }

        commands.push(line);
        const override = Object.entries(opts.replies ?? {}).find(([prefix]) =>
          line.toUpperCase().startsWith(prefix.toUpperCase())
        );
        if (override) {
          write(override[1] + "\r\n");
          continue;
        }

        const upper = line.toUpperCase();
        if (upper.startsWith("EHLO")) {
          // A multiline reply: continuation lines use `250-`, the last uses `250 `.
          const ext = opts.extensions ?? ["PIPELINING", "8BITMIME"];
          for (const e of ext) write(`250-${e}\r\n`);
          write("250 SIZE 10240000\r\n");
        } else if (upper.startsWith("AUTH")) {
          write("235 2.7.0 accepted\r\n");
        } else if (upper.startsWith("MAIL FROM") || upper.startsWith("RCPT TO")) {
          write("250 2.1.0 ok\r\n");
        } else if (upper === "DATA") {
          inData = true;
          write("354 end with <CRLF>.<CRLF>\r\n");
        } else if (upper === "QUIT") {
          write("221 2.0.0 bye\r\n");
          sock.end();
        } else {
          write("250 2.0.0 ok\r\n");
        }
      }
    });
    sock.on("error", () => {});
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  const port = typeof addr === "object" && addr ? addr.port : 0;
  return {
    port,
    commands,
    get body() {
      return body;
    },
    close: () =>
      new Promise<void>((resolve) => {
        server.close(() => resolve());
      }),
  } as Fake;
}

const open: Fake[] = [];
afterEach(async () => {
  while (open.length) await open.pop()!.close();
});

async function fake(opts?: FakeOptions) {
  const f = await startFakeSmtp(opts);
  open.push(f);
  return f;
}

/** Wait for the server side to observe something.
 *
 *  The client resolving does not mean the server has processed the last write:
 *  both run in this process, and the socket's end() callback fires when the local
 *  side flushed, not when the peer's 'data' handler ran. Asserting immediately
 *  therefore races the event loop rather than testing the client. */
async function waitFor(cond: () => boolean, ms = 1000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!cond() && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
  }
}

const msg = {
  from: "no-reply@sigmahub.test",
  to: ["someone@example.com"],
  subject: "Reset your SigmaHub password",
  text: "Reset it here:\nhttps://sigmahub.test/reset?token=abc\n",
};

describe("sendSmtpMail", () => {
  it("completes a full submission and sends the commands in order", async () => {
    const f = await fake();
    await sendSmtpMail(
      { host: "127.0.0.1", port: f.port, requireTls: false, timeoutMs: 5000 },
      msg
    );

    await waitFor(() => f.commands.some((c) => c.toUpperCase() === "QUIT"));
    const verbs = f.commands.map((c) => c.split(" ")[0].toUpperCase());
    expect(verbs).toEqual(["EHLO", "MAIL", "RCPT", "DATA", "QUIT"]);
    expect(f.commands[1]).toBe(`MAIL FROM:<${msg.from}>`);
    expect(f.commands[2]).toBe(`RCPT TO:<${msg.to[0]}>`);
    // Headers and body actually arrived.
    expect(f.body).toContain(`Subject: ${msg.subject}`);
    expect(f.body).toContain(`From: ${msg.from}`);
    expect(f.body).toContain("https://sigmahub.test/reset?token=abc");
  });

  it("puts a bare addr-spec in the envelope while the display name stays in From:", async () => {
    // `SMTP_FROM="SigmaHub <no-reply@…>"` is the natural thing for an operator to
    // write, and interpolating it verbatim produced
    // `MAIL FROM:<SigmaHub <no-reply@…>>` — a 501 from every conforming server,
    // on the password-reset path, seen by nobody (SIGMA-365).
    const f = await fake();
    await sendSmtpMail(
      { host: "127.0.0.1", port: f.port, requireTls: false, timeoutMs: 5000 },
      {
        ...msg,
        from: "SigmaHub <no-reply@sigmahub.test>",
        to: ["Someone <someone@example.com>"],
      }
    );

    expect(f.commands[1]).toBe("MAIL FROM:<no-reply@sigmahub.test>");
    expect(f.commands[2]).toBe("RCPT TO:<someone@example.com>");
    // ...and the display name is not lost — it belongs in the header, which is
    // the half of the split that makes the mail look like it came from a product.
    expect(f.body).toContain("From: SigmaHub <no-reply@sigmahub.test>");
    expect(f.body).toContain("To: Someone <someone@example.com>");
  });

  it("parses a multiline EHLO reply and one dribbled a byte at a time", async () => {
    // Both are ordinary on the wire and both break a naive reader: the multiline
    // reply must not be treated as complete at its first line, and a reply split
    // across reads must not be parsed twice or dropped.
    const f = await fake({ dribble: true, extensions: ["PIPELINING", "SIZE 1000", "ENHANCEDSTATUSCODES"] });
    await expect(
      sendSmtpMail({ host: "127.0.0.1", port: f.port, requireTls: false, timeoutMs: 5000 }, msg)
    ).resolves.toBeUndefined();
    expect(f.commands.map((c) => c.split(" ")[0].toUpperCase())).toContain("DATA");
  });

  it("authenticates with AUTH PLAIN using the NUL-framed base64 the RFC specifies", async () => {
    const f = await fake({ extensions: ["AUTH PLAIN LOGIN"] });
    await sendSmtpMail(
      {
        host: "127.0.0.1",
        port: f.port,
        username: "postmaster@sigmahub.test",
        password: "s3cr3t",
        requireTls: false,
        timeoutMs: 5000,
      },
      msg
    );
    const auth = f.commands.find((c) => c.toUpperCase().startsWith("AUTH PLAIN"));
    expect(auth).toBeDefined();
    const token = auth!.split(" ")[2];
    expect(Buffer.from(token, "base64").toString("utf8")).toBe(
      "\0postmaster@sigmahub.test\0s3cr3t"
    );
  });

  it("falls back to AUTH LOGIN when the server does not offer PLAIN", async () => {
    const f = await fake({
      extensions: ["AUTH LOGIN"],
      replies: { "AUTH LOGIN": "334 VXNlcm5hbWU6" },
    });
    // The two base64 lines after AUTH LOGIN are answered by the default 250/235
    // branch; what matters is that the credentials are base64 and in order.
    await sendSmtpMail(
      {
        host: "127.0.0.1",
        port: f.port,
        username: "user",
        password: "pass",
        requireTls: false,
        timeoutMs: 5000,
      },
      msg
    ).catch(() => {
      /* the fake's canned 334 flow may end early; the assertion below is the point */
    });
    expect(f.commands.some((c) => c.toUpperCase() === "AUTH LOGIN")).toBe(true);
    expect(f.commands).toContain(Buffer.from("user", "utf8").toString("base64"));
  });

  it("dot-stuffs a body line that would otherwise terminate DATA", async () => {
    const f = await fake();
    await sendSmtpMail(
      { host: "127.0.0.1", port: f.port, requireTls: false, timeoutMs: 5000 },
      { ...msg, text: "line one\n.\nline three\n" }
    );
    // The server strips one leading dot per RFC 5321; a client that failed to
    // double it would have ended the message at the lone dot, truncating the mail
    // and leaving "line three" to be parsed as a command.
    expect(f.body).toContain("line three");
    expect(f.commands.map((c) => c.toUpperCase())).not.toContain("LINE THREE");
  });

  it("strips CR/LF from headers so a crafted subject cannot inject one", async () => {
    const f = await fake();
    await sendSmtpMail(
      { host: "127.0.0.1", port: f.port, requireTls: false, timeoutMs: 5000 },
      { ...msg, subject: "Hello\r\nBcc: attacker@example.com" }
    );
    expect(f.body).not.toMatch(/^Bcc:/m);
    expect(f.body).toContain("Subject: Hello Bcc: attacker@example.com");
  });

  it("refuses to submit in cleartext when the server offers no STARTTLS", async () => {
    // The default. These messages carry reset links, which are bearer
    // credentials — handing them to a passive observer is worse than not sending.
    const f = await fake({ extensions: ["PIPELINING"] });
    await expect(
      sendSmtpMail({ host: "127.0.0.1", port: f.port, timeoutMs: 5000 }, msg)
    ).rejects.toThrow(/STARTTLS|cleartext/i);
  });

  it("surfaces a rejected recipient instead of reporting success", async () => {
    const f = await fake({ replies: { "RCPT TO": "550 5.1.1 no such user" } });
    await expect(
      sendSmtpMail({ host: "127.0.0.1", port: f.port, requireTls: false, timeoutMs: 5000 }, msg)
    ).rejects.toThrow(/550/);
  });

  it("rejects rather than hanging when the server never answers", async () => {
    // The failure that would be worst in production: a promise that never
    // settles pins the request that triggered the send.
    const f = await fake({ silent: true });
    await expect(
      sendSmtpMail({ host: "127.0.0.1", port: f.port, requireTls: false, timeoutMs: 300 }, msg)
    ).rejects.toThrow();
  }, 5000);

  it("rejects when nothing is listening at all", async () => {
    await expect(
      sendSmtpMail({ host: "127.0.0.1", port: 1, requireTls: false, timeoutMs: 1000 }, msg)
    ).rejects.toThrow();
  }, 5000);
});
