// Tiny dependency-free validation helpers for the auth prototype.
// No zod / react-hook-form in this project, so we validate inline.

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export function validateEmail(value: string): string | null {
  const v = value.trim();
  if (!v) return "Email is required.";
  if (!EMAIL_RE.test(v)) return "Enter a valid email address.";
  return null;
}

export function validatePassword(value: string): string | null {
  if (!value) return "Password is required.";
  if (value.length < 8) return "Password must be at least 8 characters.";
  return null;
}

export function validateName(value: string): string | null {
  if (!value.trim()) return "Name is required.";
  if (value.trim().length < 2) return "Enter your full name.";
  return null;
}

export function validateConfirm(
  password: string,
  confirm: string,
): string | null {
  if (!confirm) return "Please confirm your password.";
  if (password !== confirm) return "Passwords do not match.";
  return null;
}

export function validateOtp(value: string): string | null {
  if (value.length !== 6) return "Enter the 6-digit code.";
  if (!/^\d{6}$/.test(value)) return "Code must be 6 digits.";
  return null;
}

/**
 * A 2FA backup code (SIGMA-365).
 *
 * Deliberately NOT validateOtp. better-auth mints these as `xxxxx-xxxxx` —
 * eleven characters, letters and digits, with a hyphen — and the TOTP path is a
 * six-digit-only segmented input that strips every non-digit as it is typed. A
 * user holding a valid, unspent backup code therefore had nowhere in the entire
 * product to type it, which turned a lost phone into a permanent lockout of the
 * account (and, for a sole Org Admin, of the whole organization).
 *
 * Case-insensitive and hyphen-tolerant because the code is transcribed by hand
 * from wherever the user saved it, usually under some stress.
 */
export function validateBackupCode(value: string): string | null {
  const v = value.trim().toLowerCase();
  if (!v) return "Enter one of your backup codes.";
  if (!/^[a-z0-9]{5}-?[a-z0-9]{5}$/.test(v)) {
    return "Backup codes look like abcde-12345.";
  }
  return null;
}

/** The form better-auth stores, from whatever the user typed. */
export function normalizeBackupCode(value: string): string {
  const v = value.trim().toLowerCase().replace(/-/g, "");
  return `${v.slice(0, 5)}-${v.slice(5)}`;
}
