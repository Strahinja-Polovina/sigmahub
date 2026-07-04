import { describe, it, expect } from "vitest";
import {
  validateEmail,
  validatePassword,
  validateName,
  validateConfirm,
  validateOtp,
} from "./validators";

describe("validateEmail", () => {
  it("returns null for a valid email", () => {
    expect(validateEmail("user@example.com")).toBeNull();
  });

  it("rejects empty string", () => {
    expect(validateEmail("")).toBe("Email is required.");
  });

  it("rejects whitespace-only string", () => {
    expect(validateEmail("   ")).toBe("Email is required.");
  });

  it("rejects value without @", () => {
    expect(validateEmail("userexample.com")).toBe("Enter a valid email address.");
  });

  it("rejects value without domain part", () => {
    expect(validateEmail("user@")).toBe("Enter a valid email address.");
  });

  it("rejects value without TLD", () => {
    expect(validateEmail("user@example")).toBe("Enter a valid email address.");
  });

  it("trims leading/trailing whitespace before validating", () => {
    expect(validateEmail("  user@example.com  ")).toBeNull();
  });
});

describe("validatePassword", () => {
  it("returns null for a valid password (8+ chars)", () => {
    expect(validatePassword("abcdefgh")).toBeNull();
  });

  it("rejects empty string", () => {
    expect(validatePassword("")).toBe("Password is required.");
  });

  it("rejects password shorter than 8 characters", () => {
    expect(validatePassword("abc")).toBe("Password must be at least 8 characters.");
  });

  it("accepts exactly 8 characters", () => {
    expect(validatePassword("12345678")).toBeNull();
  });

  it("rejects 7 characters", () => {
    expect(validatePassword("1234567")).toBe("Password must be at least 8 characters.");
  });
});

describe("validateName", () => {
  it("returns null for a valid name", () => {
    expect(validateName("John Doe")).toBeNull();
  });

  it("rejects empty string", () => {
    expect(validateName("")).toBe("Name is required.");
  });

  it("rejects whitespace-only string", () => {
    expect(validateName("   ")).toBe("Name is required.");
  });

  it("rejects single character name", () => {
    expect(validateName("J")).toBe("Enter your full name.");
  });

  it("accepts two character name", () => {
    expect(validateName("Jo")).toBeNull();
  });
});

describe("validateConfirm", () => {
  it("returns null when passwords match", () => {
    expect(validateConfirm("password1", "password1")).toBeNull();
  });

  it("rejects empty confirm", () => {
    expect(validateConfirm("password1", "")).toBe("Please confirm your password.");
  });

  it("rejects mismatched passwords", () => {
    expect(validateConfirm("password1", "password2")).toBe("Passwords do not match.");
  });
});

describe("validateOtp", () => {
  it("returns null for a valid 6-digit code", () => {
    expect(validateOtp("123456")).toBeNull();
  });

  it("rejects code shorter than 6 digits", () => {
    expect(validateOtp("12345")).toBe("Enter the 6-digit code.");
  });

  it("rejects code longer than 6 digits", () => {
    expect(validateOtp("1234567")).toBe("Enter the 6-digit code.");
  });

  it("rejects non-numeric 6-char string", () => {
    expect(validateOtp("abcdef")).toBe("Code must be 6 digits.");
  });

  it("rejects mixed alphanumeric", () => {
    expect(validateOtp("12ab56")).toBe("Code must be 6 digits.");
  });

  it("rejects empty string", () => {
    expect(validateOtp("")).toBe("Enter the 6-digit code.");
  });
});
