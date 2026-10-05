/**
 * Unit tests for the currency utilities.
 *
 * Guards the system's monetary invariant: two-decimal precision with no
 * binary floating-point drift (BigInt arithmetic throughout), strict input
 * rejection, overflow safety, and a consistent locale-aware display contract.
 *
 * Security note: `formatMinorUnits` and every helper here return plain text.
 * When rendering, bind output via a text node / `textContent` only — never
 * via `innerHTML`/`dangerouslySetInnerHTML` (injection-prone, CWE-79 class).
 */
import {
  parseMinorUnits,
  toMinorUnits,
  formatMinorUnits,
  addMoney,
  subtractMoney,
  isNonNegative,
  isZero,
  setDefaultCurrency,
  setDefaultLocale,
  CurrencyParseError,
} from "../currency";

describe("parseMinorUnits", () => {
  it("parses two-decimal strings into exact minor units", () => {
    expect(parseMinorUnits("123.45")).toBe(12345n);
    expect(parseMinorUnits("0.10")).toBe(10n);
    expect(parseMinorUnits("1234")).toBe(123400n);
  });

  it("pads a single decimal digit to a tenth", () => {
    expect(parseMinorUnits("123.4")).toBe(12340n);
  });

  it("rejects three or more decimal places", () => {
    // Spec boundary: amounts are limited to two decimal places; a third digit
    // is rejected, never silently rounded to a value the user did not enter.
    expect(() => parseMinorUnits("12.345")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("0.123")).toThrow(CurrencyParseError);
  });

  it("rejects malformed and non-decimal input", () => {
    expect(() => parseMinorUnits("1.23.4")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("1e5")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("0x10")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("abc")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("")).toThrow(CurrencyParseError);
  });

  it("rejects non-numeric and whitespace-masked input strictly", () => {
    // Allowlist gate: no parseFloat/Number fallback and no isNaN-only check,
    // so the ledger can never receive coercion artifacts or empty payloads.
    expect(() => parseMinorUnits("Infinity")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("NaN")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("12 34")).toThrow(CurrencyParseError);
    expect(() => parseMinorUnits("   ")).toThrow(CurrencyParseError);
  });

  it("rejects oversized whole parts (overflow safety, never wraps)", () => {
    // Just over the 15-integer-digit maximum is rejected, not wrapped
    // (CWE-190-style): a crafted extreme amount must never reach the ledger.
    expect(() => parseMinorUnits("1234567890123456.00")).toThrow(
      CurrencyParseError
    );
  });

  it("handles an explicit leading sign", () => {
    expect(parseMinorUnits("-12.30")).toBe(-1230n);
    expect(parseMinorUnits("+12.30")).toBe(1230n);
  });
});

describe("toMinorUnits", () => {
  it("combines whole and fractional parts", () => {
    expect(toMinorUnits("12", "34")).toBe("1234");
    expect(toMinorUnits("5")).toBe("500");
    expect(toMinorUnits("0", "3")).toBe("30");
  });

  it("rejects malformed parts", () => {
    expect(() => toMinorUnits("12", "345")).toThrow(CurrencyParseError);
    expect(() => toMinorUnits("1e2", "00")).toThrow(CurrencyParseError);
  });
});

describe("addMoney / subtractMoney", () => {
  it("adds without binary floating-point drift", () => {
    // addMoney consumes minor-unit strings: "0.10"->10n, "0.20"->20n, sum
    // 30n. 0.10 + 0.20 is exactly 0.30, never 0.30000000000000004.
    expect(addMoney("0.10", "0.20")).toBe("30");
    expect(parseMinorUnits(addMoney("0.10", "0.20"))).toBe(30n);
    expect(formatMinorUnits(addMoney("0.10", "0.20"), "USD", "en-US")).toBe(
      "$0.30"
    );
    expect(addMoney("123", "456")).toBe("579");
  });

  it("adds zero values without introducing drift", () => {
    expect(addMoney("0.00", "0.00")).toBe("0");
    expect(addMoney("0", "0")).toBe("0");
  });

  it("never yields a negative from two non-negative addends", () => {
    // Negatives are only reachable via the explicit sign path (subtractMoney
    // with a < b, or parseMinorUnits("-...")), per the "adjustments/refunds
    // with reason codes" rule. A plain add can never show a negative balance.
    expect(parseMinorUnits(addMoney("0", "0"))).not.toBeLessThan(0n);
    expect(parseMinorUnits(addMoney("10", "20"))).not.toBeLessThan(0n);
  });

  it("subtracts and preserves the sign", () => {
    expect(subtractMoney("150", "200")).toBe("-50");
    expect(subtractMoney("200", "150")).toBe("50");
  });
});

describe("overflow safety (large amounts)", () => {
  it("accepts the maximum representable amount exactly", () => {
    // 15 integer digits is the AMOUNT_RE ceiling; BigInt holds it losslessly.
    expect(parseMinorUnits("999999999999999.99")).toBe(99999999999999999n);
  });

  it("adds large amounts with no precision loss", () => {
    expect(addMoney("500000000000000", "500000000000000")).toBe(
      "1000000000000000"
    );
  });

  it("formats the maximum amount losslessly through Intl", () => {
    expect(formatMinorUnits("999999999999999.99", "USD", "en-US")).toBe(
      "$999,999,999,999,999.99"
    );
  });
});

describe("formatMinorUnits", () => {
  it("formats with grouping and exactly two decimals", () => {
    expect(formatMinorUnits("123456", "USD", "en-US")).toBe("$1,234.56");
    expect(formatMinorUnits("500", "USD", "en-US")).toBe("$5.00");
  });

  it("emits canonical zero and negative sign glyph", () => {
    expect(formatMinorUnits("0", "USD", "en-US")).toBe("$0.00");
    const negative = formatMinorUnits("-500", "USD", "en-US");
    expect(negative).not.toContain("NaN");
    // Has a sign indicator (minus or parentheses) plus two decimals.
    expect(negative).toMatch(/(^-|\(.*\))/);
    expect(negative).toMatch(/\.00$/);
  });

  it("falls back to the session currency for unknown codes", () => {
    setDefaultCurrency("EUR");
    const result = formatMinorUnits("1000", "??", "en-US");
    expect(result).toContain("€");
    expect(result).toMatch(/10\.00/);
  });

  it("uses the session locale when none is provided", () => {
    setDefaultLocale("en-US");
    setDefaultCurrency("USD");
    expect(formatMinorUnits("1500", "USD")).toBe("$15.00");
  });
});

describe("predicates and defaults", () => {
  it("isZero and isNonNegative", () => {
    expect(isZero("0")).toBe(true);
    expect(isZero("5")).toBe(false);
    expect(isNonNegative("0")).toBe(true);
    expect(isNonNegative("-5")).toBe(false);
    expect(() => isZero("abc")).toThrow(CurrencyParseError);
  });

  it("setDefaultCurrency validates shape", () => {
    expect(() => setDefaultCurrency("eur")).toThrow(CurrencyParseError);
    expect(() => setDefaultCurrency("123")).toThrow(CurrencyParseError);
    setDefaultCurrency("USD");
  });

  it("formatMinorUnits stays byte-identical across repeated calls", () => {
    const first = formatMinorUnits("123456", "USD", "en-US");
    const second = formatMinorUnits("123456", "USD", "en-US");
    expect(second).toBe(first);
  });
});