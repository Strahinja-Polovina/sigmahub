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
