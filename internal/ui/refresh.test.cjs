// Exercise the shipped script with controllable fetch completion order.
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

test("a slow status request survives overlapping timer and manual refreshes", async () => {
  const pending = [];
  let tick;
  const nodes = new Map();
  const document = {
    addEventListener() {},
    querySelector(selector) {
      if (!nodes.has(selector)) nodes.set(selector, {
        addEventListener(event, fn) { this[event] = fn; },
      });
      return nodes.get(selector);
    },
  };
  const script = fs.readFileSync(path.join(__dirname, "static/app.js"), "utf8");
  vm.runInNewContext(script, {
    document, window: { addEventListener() {} }, setInterval(fn) { tick = fn; },
    fetch: () => new Promise(resolve => pending.push(resolve)),
  });
  function status(name) {
    return { ok: true, json: async () => ({ targets: [{ name, prober: "p" }] }) };
  }
  tick();
  await document.querySelector("#refresh").click();
  tick();
  assert.equal(pending.length, 1);
  pending[0](status("first-target"));
  await new Promise(resolve => setImmediate(resolve));
  assert.match(document.querySelector("#targets").innerHTML, /first-target/);

  const manual = document.querySelector("#refresh").click();
  assert.equal(pending.length, 2);
  pending[1](status("next-target"));
  await manual;
  assert.match(document.querySelector("#targets").innerHTML, /next-target/);
});
