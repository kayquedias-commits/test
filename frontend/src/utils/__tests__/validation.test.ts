import { describe, it, expect } from "vitest";

import {
  categoryRequired,
  currencyCode,
  futureDateAllowed,
  maxLength,
  positiveAmount,
  rangeValid,
  required,
  validDate,
  validateForm,
} from "../validation";

describe("required", () => {
  it("rejects empty, whitespace and nullish values", () => {
    const v = required("Description");
    expect(v("")).toContain("Description is required");
    expect(v("   ")).toContain("Description is required");
    expect(v(null as unknown as string)).toContain("Description is required");
    expect(v(undefined as unknown as string)).toContain("Description is required");
  });

  it("accepts a non-empty value", () => {
    const v = required("Description");
    expect(v("Groceries")).toBeNull();
  });
});

describe("positiveAmount", () => {
  const v = positiveAmount();

  it("accepts 2-dp positive amounts", () => {
    expect(v("12.50")).toBeNull();
    expect(v("12.5")).toBeNull();
    expect(v("0.01")).toBeNull();
    expect(v("1")).toBeNull();
  });

  it("rejects negative values", () => {
    expect(v("-5")).toContain("up to 2 decimal places");
    expect(v("-12.50")).toContain("up to 2 decimal places");
  });

  it("rejects more than two decimal places", () => {
    expect(v("12.505")).toContain("up to 2 decimal places");
    expect(v("1.234")).toContain("up to 2 decimal places");
  });

  it("rejects zero and non-numeric garbage", () => {
    expect(v("0")).toBe("Amount must be greater than zero.");
    expect(v("0.00")).toBe("Amount must be greater than zero.");
    expect(v("0.0")).toBe("Amount must be greater than zero.");
    expect(v("12a")).toContain("up to 2 decimal places");
    expect(v("   ")).toContain("up to 2 decimal places");
  });

  it("rejects amounts exceeding the integer magnitude cap", () => {
    expect(v(("9".repeat(20)) + ".99")).toBe("Amount exceeds the maximum allowed value.");
    expect(v(("1" + "0".repeat(19)))).toBe("Amount exceeds the maximum allowed value.");
  });

  it("leaves empty values for the required validator", () => {
    expect(v("")).toBeNull();
    expect(v(null as unknown as string)).toBeNull();
    expect(v(undefined as unknown as string)).toBeNull();
  });
});

describe("validDate", () => {
  const v = validDate();

  it("accepts real calendar dates", () => {
    expect(v("2024-05-14")).toBeNull();
    expect(v("2024-02-29")).toBeNull();
    expect(v("2023-02-28")).toBeNull();
  });

  it("rejects impossible dates without rolling over", () => {
    expect(v("2023-02-30")).toContain("not a real calendar date");
    expect(v("2024-02-31")).toContain("not a real calendar date");
    expect(v("2024-13-01")).toContain("not a real calendar date");
  });

  it("rejects malformed strings", () => {
    expect(v("2024-1-1")).toContain("not a real calendar date");
    expect(v("not-a-date")).toContain("not a real calendar date");
    expect(v("20240514")).toContain("not a real calendar date");
  });

  it("leaves empty values for the required validator", () => {
    expect(v("")).toBeNull();
  });
});

describe("maxLength", () => {
  it("rejects over-long values with the current count", () => {
    const v = maxLength(255, "Description");
    const long = "x".repeat(300);
    const message = v(long);
    expect(message).toContain("Description must be 255 characters or fewer");
    expect(message).toContain("300");
  });

  it("rejects exactly at the boundary (256) that exceeds the limit", () => {
    const v = maxLength(255, "Description");
    const message = v("x".repeat(256));
    expect(message).toContain("Description must be 255 characters or fewer");
    expect(message).toContain("256");
  });

  it("accepts values at or under the limit", () => {
    const v = maxLength(255, "Description");
    expect(v("short")).toBeNull();
    expect(v("x".repeat(255))).toBeNull();
    expect(v("")).toBeNull();
    expect(v(null as unknown as string)).toBeNull();
  });
});

describe("currencyCode", () => {
  const v = currencyCode();

  it("accepts codes case-insensitively", () => {
    expect(v("USD")).toBeNull();
    expect(v("usd")).toBeNull();
    expect(v("eur")).toBeNull();
  });

  it("rejects malformed codes", () => {
    expect(v("US")).toContain("valid ISO 4217 currency code");
    expect(v("USD1")).toContain("valid ISO 4217 currency code");
    expect(v("123")).toContain("valid ISO 4217 currency code");
    expect(v("")).toBeNull();
  });
});

describe("categoryRequired", () => {
  const v = categoryRequired();

  it("rejects an empty selection", () => {
    expect(v([])).toContain("Select at least one category");
    expect(v(null as unknown as string[])).toContain("Select at least one category");
  });

  it("accepts a non-empty selection", () => {
    expect(v(["cat-1"])).toBeNull();
  });
});

describe("futureDateAllowed", () => {
  it("rejects a future date unless scheduled", () => {
    const notScheduled = futureDateAllowed(false, { locale: "en-US", timeZone: "UTC" });
    expect(notScheduled("2999-01-01")).toContain("Enable the Scheduled option");

    const scheduled = futureDateAllowed(true, { locale: "en-US", timeZone: "UTC" });
    expect(scheduled("2999-01-01")).toBeNull();
  });

  it("accepts past and today dates", () => {
    const v = futureDateAllowed(false, { locale: "en-US", timeZone: "UTC" });
    expect(v("2000-01-01")).toBeNull();
  });

  it("leaves invalid dates for the validDate validator", () => {
    const v = futureDateAllowed(false, { locale: "en-US", timeZone: "UTC" });
    expect(v("not-a-date")).toBeNull();
    expect(v("")).toBeNull();
  });
});

describe("rangeValid", () => {
  it("rejects a start after the end", () => {
    expect(rangeValid("2024-05-14", "2024-05-13")).toContain(
      "start date must be on or before the end date"
    );
  });

  it("accepts valid and equal bounds", () => {
    expect(rangeValid("2024-05-14", "2024-05-20")).toBeNull();
    expect(rangeValid("2024-05-14", "2024-05-14")).toBeNull();
  });

  it("rejects unparsable bounds", () => {
    expect(rangeValid("garbage", "2024-05-20")).toContain("start date must be on or before");
    expect(rangeValid("2024-05-14", "2024-02-30")).toContain("start date must be on or before");
  });
});

describe("validateForm", () => {
  it("aggregates multiple field errors in schema order", () => {
    const schema = {
      amount: [required("Amount"), positiveAmount()],
      date: [required("Date"), validDate()],
      description: [required("Description"), maxLength(255, "Description")],
    };
    const errors = validateForm(
      { amount: "0", date: "2023-02-30", description: "" },
      schema
    );

    expect(errors).toEqual([
      { field: "amount", message: "Amount must be greater than zero." },
      { field: "date", message: "That is not a real calendar date. Enter a valid date, for example 2024-05-14." },
      { field: "description", message: "Description is required. Enter a value to continue." },
    ]);
  });

  it("records one error per field (first failing validator wins)", () => {
    const schema = {
      amount: [positiveAmount()],
    };
    const errors = validateForm({ amount: "-5" }, schema);
    expect(errors).toHaveLength(1);
    expect(errors[0].field).toBe("amount");
  });

  it("returns an empty array when every field is valid", () => {
    const schema = {
      amount: [required("Amount"), positiveAmount()],
      date: [required("Date"), validDate()],
    };
    const errors = validateForm(
      { amount: "12.50", date: "2024-05-14" },
      schema
    );
    expect(errors).toEqual([]);
  });
});