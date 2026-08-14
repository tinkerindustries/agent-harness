import { describe, expect, it } from "vitest";
import { clampPage, offsetFor, pageCount } from "./paging";

describe("pageCount", () => {
  it("is one page for an empty or non-positive total", () => {
    expect(pageCount(0, 20)).toBe(1);
    expect(pageCount(-5, 20)).toBe(1);
  });

  it("rounds partial pages up", () => {
    expect(pageCount(1, 20)).toBe(1);
    expect(pageCount(20, 20)).toBe(1);
    expect(pageCount(21, 20)).toBe(2);
    expect(pageCount(40, 20)).toBe(2);
    expect(pageCount(41, 20)).toBe(3);
    expect(pageCount(137, 20)).toBe(7);
  });
});

describe("clampPage", () => {
  it("never goes below 1", () => {
    expect(clampPage(0, 100, 20)).toBe(1);
    expect(clampPage(-3, 100, 20)).toBe(1);
  });

  it("never goes above the last page", () => {
    expect(clampPage(8, 137, 20)).toBe(7);
    expect(clampPage(100, 40, 20)).toBe(2);
  });

  it("is 1 when the result set is empty", () => {
    expect(clampPage(4, 0, 20)).toBe(1);
  });

  it("passes an in-range page through", () => {
    expect(clampPage(1, 137, 20)).toBe(1);
    expect(clampPage(7, 137, 20)).toBe(7);
  });
});

describe("offsetFor", () => {
  it("maps the 1-based page to the server's 0-based offset", () => {
    expect(offsetFor(1, 20)).toBe(0);
    expect(offsetFor(2, 20)).toBe(20);
    expect(offsetFor(7, 20)).toBe(120);
    expect(offsetFor(3, 5)).toBe(10);
  });
});
