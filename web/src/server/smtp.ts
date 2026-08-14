import "server-only";

import net from "node:net";
import tls from "node:tls";

import { envelopeAddress } from "@/lib/mail";

// A minimal SMTP submission client (SIGMA-365).
//
// No SMTP library is bundled deliberately: the product needs exactly one thing
// from SMTP — submit a short plain-text message to a submission server — and the
// control plane already proves the same conversation out in Go
// (cp/internal/alerts/sender.go). A dependency for this would be a supply-chain
// surface for ~150 lines of well-specified protocol.
//
// What it does: EHLO, STARTTLS when offered (or implicit TLS on 465), AUTH
// PLAIN/LOGIN when credentials are given, then MAIL FROM / RCPT TO / DATA. What
// it deliberately does NOT do: pipelining, 8BITMIME negotiation, attachments,
// HTML bodies, connection reuse. Every message this product sends is a few lines
// of text carrying one link.

export type SmtpCredentials = {
  host: string;
  port: number;
  username?: string;
  password?: string;
  /** TLS from the first byte (submission port 465). Otherwise STARTTLS. */
  implicitTls?: boolean;
  /**
   * Refuse to send in cleartext when the server offers no STARTTLS. On by
   * default: the messages carry password-reset and invite links, which are
   * bearer credentials — handing them to a passive observer is worse than not
   * sending them at all.
   */
  requireTls?: boolean;
  timeoutMs?: number;
};

export type SmtpMessage = {
  from: string;
  to: string[];
  subject: string;
  text: string;
};

const defaultTimeoutMs = 15_000;

/** Strip CR/LF so a crafted subject or address cannot inject headers. */
function sanitizeHeader(v: string): string {
  return v.replace(/[\r\n]+/g, " ").trim();
}

/**
 * An SMTP reply is complete when a line arrives whose 3-digit code is followed
 * by a space rather than a hyphen (RFC 5321 §4.2: `250-EXTENSION` continues,
 * `250 OK` ends). Returns the byte length of the complete reply, or -1.
 */
function completeReplyLength(buf: string): number {
  let offset = 0;
  for (;;) {
    const nl = buf.indexOf("\n", offset);
    if (nl < 0) return -1;
    const line = buf.slice(offset, nl);
    if (/^\d{3} /.test(line)) return nl + 1;
    if (!/^\d{3}-/.test(line)) return nl + 1; // malformed — let the caller judge the code
    offset = nl + 1;
  }
}

/** Reads complete SMTP replies off a socket, one await at a time. */
class ReplyReader {
  private buf = "";
  private waiter: {
    resolve: (reply: string) => void;
    reject: (err: Error) => void;
  } | null = null;
  private failure: Error | null = null;

  constructor(private socket: net.Socket | tls.TLSSocket) {
    socket.setEncoding("utf8");
    socket.on("data", (chunk: string) => {
      this.buf += chunk;
      this.flush();
    });
    socket.on("error", (err: Error) => this.fail(err));
    socket.on("close", () => this.fail(new Error("smtp: connection closed")));
  }

  /** Hand the socket to a new reader (after a STARTTLS upgrade). */
  detach() {
    this.socket.removeAllListeners("data");
    this.socket.removeAllListeners("error");
    this.socket.removeAllListeners("close");
  }

  private fail(err: Error) {
    this.failure = err;
    const w = this.waiter;
    this.waiter = null;
    w?.reject(err);
  }

  private flush() {
    if (!this.waiter) return;
    const n = completeReplyLength(this.buf);
    if (n < 0) return;
    const reply = this.buf.slice(0, n);
    this.buf = this.buf.slice(n);
    const w = this.waiter;
    this.waiter = null;
    w.resolve(reply);
  }

  read(): Promise<string> {
    if (this.failure) return Promise.reject(this.failure);
    return new Promise<string>((resolve, reject) => {
      this.waiter = { resolve, reject };
      this.flush();
    });
  }
}

function replyCode(reply: string): number {
  return Number.parseInt(reply.slice(0, 3), 10);
}

/** Send one message. Resolves on a 250 for the final DATA, rejects otherwise. */
export async function sendSmtpMail(
  cfg: SmtpCredentials,
  msg: SmtpMessage
): Promise<void> {
  const timeoutMs = cfg.timeoutMs ?? defaultTimeoutMs;
  const requireTls = cfg.requireTls ?? true;

  let socket: net.Socket | tls.TLSSocket = cfg.implicitTls
    ? tls.connect({ host: cfg.host, port: cfg.port, servername: cfg.host })
    : net.connect({ host: cfg.host, port: cfg.port });
  let secure = Boolean(cfg.implicitTls);

  // One deadline for the whole exchange, like the CP's emailSendTimeout: a dead
  // host must not pin the request that triggered the send.
  const deadline = setTimeout(() => socket.destroy(new Error("smtp: timeout")), timeoutMs);

  let reader = new ReplyReader(socket);
  const expect = async (want: number, what: string) => {
    const reply = await reader.read();
    const code = replyCode(reply);
    if (code !== want) {
      throw new Error(`smtp: ${what} got ${reply.trim().slice(0, 200)}`);
    }
    return reply;
  };
  const send = (line: string) =>
    new Promise<void>((resolve, reject) => {
      socket.write(line + "\r\n", (err) => (err ? reject(err) : resolve()));
    });

  try {
    await new Promise<void>((resolve, reject) => {
      socket.once("error", reject);
      socket.once(cfg.implicitTls ? "secureConnect" : "connect", () => resolve());
    });
    await expect(220, "greeting");

    // EHLO twice when STARTTLS intervenes: the extension list is only
    // trustworthy once the channel is protected.
    const ehloName = "sigmahub";
    await send(`EHLO ${ehloName}`);
    let caps = await expect(250, "EHLO");

    if (!secure && /\bSTARTTLS\b/i.test(caps)) {
      await send("STARTTLS");
      await expect(220, "STARTTLS");
      reader.detach();
      socket = tls.connect({ socket: socket as net.Socket, servername: cfg.host });
      await new Promise<void>((resolve, reject) => {
        socket.once("error", reject);
        socket.once("secureConnect", () => resolve());
      });
      secure = true;
      reader = new ReplyReader(socket);
      await send(`EHLO ${ehloName}`);
      caps = await expect(250, "EHLO after STARTTLS");
    }

    if (!secure && requireTls) {
      throw new Error(
        "smtp: server offers no STARTTLS and SMTP_ALLOW_INSECURE is not set — " +
          "refusing to submit reset/invite links in cleartext"
      );
    }

    if (cfg.username && cfg.password) {
      if (/\bAUTH\b[^\n]*\bPLAIN\b/i.test(caps)) {
        const token = Buffer.from(`\0${cfg.username}\0${cfg.password}`, "utf8").toString("base64");
        await send(`AUTH PLAIN ${token}`);
        await expect(235, "AUTH PLAIN");
      } else {
        await send("AUTH LOGIN");
        await expect(334, "AUTH LOGIN");
        await send(Buffer.from(cfg.username, "utf8").toString("base64"));
        await expect(334, "AUTH LOGIN username");
        await send(Buffer.from(cfg.password, "utf8").toString("base64"));
        await expect(235, "AUTH LOGIN password");
      }
    }

    // Envelope, not header: the display name goes in the From:/To: lines that
    // buildMessage writes, never inside the angle brackets here.
    await send(`MAIL FROM:<${envelopeAddress(msg.from)}>`);
    await expect(250, "MAIL FROM");
    for (const rcpt of msg.to) {
      await send(`RCPT TO:<${envelopeAddress(rcpt)}>`);
      const reply = await reader.read();
      const code = replyCode(reply);
      // 251 is "not local, will forward" — also a success.
      if (code !== 250 && code !== 251) {
        throw new Error(`smtp: RCPT TO got ${reply.trim().slice(0, 200)}`);
      }
    }
    await send("DATA");
    await expect(354, "DATA");
    await send(buildMessage(msg));
    await expect(250, "message body");

    // Orderly shutdown. The message is already queued (the 250 above), so this
    // is politeness — but `destroy()` straight after writing QUIT can reset the
    // connection before the server has read it, which a strict MTA logs as an
    // aborted session. end() flushes the write and sends FIN; it does not wait
    // for the 221, so an unresponsive server cannot delay a send that already
    // succeeded. The destroy() below is then a no-op backstop.
    await send("QUIT");
    await new Promise<void>((resolve) => socket.end(() => resolve()));
  } finally {
    clearTimeout(deadline);
    socket.destroy();
  }
}

/** RFC 5322 plain-text message, dot-stuffed and terminated for DATA. */
function buildMessage(msg: SmtpMessage): string {
  const headers = [
    `From: ${sanitizeHeader(msg.from)}`,
    `To: ${msg.to.map(sanitizeHeader).join(", ")}`,
    `Subject: ${sanitizeHeader(msg.subject)}`,
    `Date: ${new Date().toUTCString()}`,
    "MIME-Version: 1.0",
    'Content-Type: text/plain; charset="UTF-8"',
  ].join("\r\n");
  // A body line of a single "." would end DATA early; RFC 5321 §4.5.2 says double
  // any leading dot.
  const body = msg.text
    .replace(/\r?\n/g, "\r\n")
    .split("\r\n")
    .map((line) => (line.startsWith(".") ? "." + line : line))
    .join("\r\n");
  return `${headers}\r\n\r\n${body}\r\n.`;
}
