import assert from "node:assert/strict";
import test from "node:test";

import { containerRequest } from "../src/proxy.mjs";

test("container ingress replaces client forwarding headers", () => {
  const request = new Request("https://example.com/path", {
    headers: {
      "cf-connecting-ip": "203.0.113.8",
      forwarded: "for=192.0.2.1",
      "x-forwarded-for": "192.0.2.2",
      "x-real-ip": "192.0.2.3"
    }
  });
  const forwarded = containerRequest(request);

  assert.equal(forwarded.headers.get("x-forwarded-for"), "203.0.113.8");
  assert.equal(forwarded.headers.has("forwarded"), false);
  assert.equal(forwarded.headers.has("x-real-ip"), false);
});

test("container ingress omits malformed connecting IPs", () => {
  const request = new Request("https://example.com", {
    headers: {
      "cf-connecting-ip": "203.0.113.8, 192.0.2.1",
      "x-forwarded-for": "192.0.2.2"
    }
  });
  assert.equal(containerRequest(request).headers.has("x-forwarded-for"), false);

  const invalidV6 = new Request("https://example.com", {
    headers: { "cf-connecting-ip": "2001:::1" }
  });
  assert.equal(containerRequest(invalidV6).headers.has("x-forwarded-for"), false);
});
