// Exercise the shipped script with controllable fetch completion order.
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

test("late status response cannot replace a newer dashboard", async () => {
  const pending = [];
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
    document, window: { addEventListener() {} }, setInterval() {},
    fetch: () => new Promise(resolve => pending.push(resolve)),
  });
  const manual = document.querySelector("#refresh").click();
  function status(name) {
    return { ok: true, json: async () => ({ targets: [{ name, prober: "p" }] }) };
  }
  pending[1](status("new-target"));
  await manual;
  pending[0](status("old-target"));
  await new Promise(resolve => setImmediate(resolve));
  assert.match(document.querySelector("#targets").innerHTML, /new-target/);
  assert.doesNotMatch(document.querySelector("#targets").innerHTML, /old-target/);
});
