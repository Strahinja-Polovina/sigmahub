/**
 * Environment variables: seeded from detection, marked, bulk-pasted (SIGMA-211).
 *
 * Detection has always returned the repository's own variable NAMES — from
 * .env.example, the Dockerfile's ENV/ARG and each compose service's environment
 * block. Retyping them is busywork, and worse, it is busywork that gets one
 * wrong: a missing DATABASE_URL is a container that starts and immediately dies
 * with a message nobody sees until they open the logs.
 */

/** Must match createSecretAction's server-side validation (secrets.ts). A key
 *  this rejects fails AFTER the resource has already been created, which
 *  surfaces as a misleading "Create failed" (SIGMA-151). */
export const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

export type EnvDraft = {
  id: string;
  key: string;
  value: string;
  /** Masked in the UI and stored as a secret. Defaults from the key's name. */
  secret: boolean;
  /**
   * The user has set `secret` by hand, so stop re-deriving it as they type.
   *
   * Without this the heuristic fights the user: unmark API_KEY, add a character
   * to the key, and it silently marks itself again — which reads as the toggle
   * being broken rather than as the guess being re-run.
   */
  touchedSecret?: boolean;
};

export function envKeyValid(key: string): boolean {
  const k = key.trim();
  return k === "" || ENV_KEY_RE.test(k);
}

/** Substrings that make a variable a credential. Deliberately generous: marking
 *  a harmless variable secret costs a reveal click, and failing to mark a real
 *  one puts a password in a screenshot. */
const SECRET_HINTS = [
  "SECRET",
  "PASSWORD",
  "PASSWD",
  "TOKEN",
  "APIKEY",
  "API_KEY",
  "PRIVATE",
  "CREDENTIAL",
  "DSN",
  "SALT",
  "SIGNING",
  "CERT",
];

/**
 * Whether a key names a credential.
 *
 * "_KEY" is matched as a suffix rather than as a substring, because KEYSPACE,
 * KEYCLOAK_URL and MONKEY_MODE are not credentials and a heuristic that masks
 * them teaches people to ignore the mask.
 */
export function isSecretKey(key: string): boolean {
  const k = key.trim().toUpperCase();
  if (!k) return false;
  if (SECRET_HINTS.some((hint) => k.includes(hint))) return true;
  return k.endsWith("_KEY") || k === "KEY";
}

let seq = 0;
function nextId(): string {
  seq += 1;
  return `env_${seq}`;
}

export function blankEnvDraft(): EnvDraft {
  return { id: nextId(), key: "", value: "", secret: false };
}

/**
 * Seed the Variables step from detected keys — values blank, secrets marked.
 *
 * A trailing blank row is deliberate: it is the affordance for adding one more,
 * and its absence is why the old wizard needed an "Add variable" click before
 * the first typed character.
 */
export function seedEnvVars(keys: string[] | undefined | null): EnvDraft[] {
  const seen = new Set<string>();
  const out: EnvDraft[] = [];
  for (const raw of keys ?? []) {
    const key = raw.trim();
    if (!key || seen.has(key) || !ENV_KEY_RE.test(key)) continue;
    seen.add(key);
    out.push({ id: nextId(), key, value: "", secret: isSecretKey(key) });
  }
  out.push(blankEnvDraft());
  return out;
}

export type ParsedEnv = {
  vars: { key: string; value: string }[];
  /** Lines that could not be read, by 1-based line number. Reported rather than
   *  dropped: a paste that silently loses three of forty variables is worse
   *  than one that refuses. */
  errors: { line: number; text: string; reason: string }[];
};

/**
 * Parse a pasted .env file.
 *
 * Handles what a real .env contains: comments, blank lines, `export ` prefixes,
 * quoted values, and values that themselves contain '=' (a DSN, a base64 key).
 * The FIRST '=' separates; everything after it is the value, verbatim.
 */
export function parseDotenv(text: string): ParsedEnv {
  const vars: { key: string; value: string }[] = [];
  const errors: ParsedEnv["errors"] = [];
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i].trim();
    if (!raw || raw.startsWith("#")) continue;
    const line = raw.startsWith("export ") ? raw.slice("export ".length).trim() : raw;
    const eq = line.indexOf("=");
    if (eq < 1) {
      errors.push({ line: i + 1, text: raw, reason: "expected KEY=value" });
      continue;
    }
    const key = line.slice(0, eq).trim();
    if (!ENV_KEY_RE.test(key)) {
      errors.push({ line: i + 1, text: raw, reason: `“${key}” is not a valid variable name` });
      continue;
    }
    vars.push({ key, value: unquote(line.slice(eq + 1).trim()) });
  }
  return { vars, errors };
}

/** Strip one matched pair of surrounding quotes. A value that is not quoted is
 *  returned untouched, including one that merely contains a quote. */
function unquote(value: string): string {
  if (value.length >= 2) {
    const first = value[0];
    const last = value[value.length - 1];
    if ((first === '"' || first === "'") && first === last) return value.slice(1, -1);
  }
  return value;
}

/**
 * Merge pasted variables into the current drafts.
 *
 * A pasted key that already exists OVERWRITES its value rather than appending a
 * duplicate: the whole point of pasting a .env over a seeded list is to fill in
 * the values for the keys detection already found, and two rows named
 * DATABASE_URL is a resource whose environment depends on iteration order.
 */
export function mergeEnvVars(existing: EnvDraft[], incoming: { key: string; value: string }[]): EnvDraft[] {
  const byKey = new Map<string, number>();
  const out = existing.map((e) => ({ ...e }));
  out.forEach((e, i) => {
    const k = e.key.trim();
    if (k) byKey.set(k, i);
  });

  for (const { key, value } of incoming) {
    const at = byKey.get(key);
    if (at !== undefined) {
      out[at].value = value;
      continue;
    }
    // Reuse a trailing blank row rather than leaving a gap in the middle.
    const blank = out.findIndex((e) => !e.key.trim() && !e.value);
    const draft: EnvDraft = {
      id: blank >= 0 ? out[blank].id : nextId(),
      key,
      value,
      secret: isSecretKey(key),
    };
    if (blank >= 0) {
      out[blank] = draft;
      byKey.set(key, blank);
    } else {
      out.push(draft);
      byKey.set(key, out.length - 1);
    }
  }
  if (!out.some((e) => !e.key.trim() && !e.value)) out.push(blankEnvDraft());
  return out;
}

/** Variables that will actually be created — a key with a value. */
export function submittableEnvVars(drafts: EnvDraft[]): EnvDraft[] {
  return drafts.filter((d) => d.key.trim() !== "" && d.value !== "");
}

/** The count the review screen shows. */
export function envVarCount(drafts: EnvDraft[]): number {
  return submittableEnvVars(drafts).length;
}

/** Whether the step may be left: every non-empty key has to be valid, because
 *  the server rejects the rest and by then the resource exists (SIGMA-151). */
export function envVarsValid(drafts: EnvDraft[]): boolean {
  return drafts.every((d) => envKeyValid(d.key));
}
