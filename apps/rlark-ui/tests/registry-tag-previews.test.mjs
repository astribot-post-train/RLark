import assert from "node:assert/strict";
import test from "node:test";
import { loadRegistryTagPreviews } from "../dist/test/utils/registryTagPreviews.js";

test("previews load with bounded concurrency and preserve individual failures", async () => {
  let active = 0;
  let maximum = 0;
  const results = new Map();
  const controller = new AbortController();
  await loadRegistryTagPreviews(
    ["one", "two", "three", "four", "five", "six"],
    async (repository, signal) => {
      assert.equal(signal, controller.signal);
      active += 1;
      maximum = Math.max(maximum, active);
      await new Promise((resolve) => setTimeout(resolve, 5));
      active -= 1;
      if (repository === "two") throw new Error("unavailable");
      return {
        items: [`${repository}-tag`],
        next: repository === "one" ? "next" : "",
      };
    },
    controller.signal,
    (repository, result) => results.set(repository, result),
  );
  assert.equal(maximum, 4);
  assert.equal(results.size, 6);
  assert.equal(results.get("one").page.next, "next");
  assert.match(results.get("two").error.message, /unavailable/);
  assert.deepEqual(results.get("six").page.items, ["six-tag"]);
  await loadRegistryTagPreviews(
    ["two"],
    async () => ({ items: ["recovered"], next: "" }),
    controller.signal,
    (name, result) => results.set(name, result),
  );
  assert.deepEqual(results.get("two").page.items, ["recovered"]);
});

test("cancelled preview loads do not start more requests or publish stale results", async () => {
  const controller = new AbortController();
  let started = 0;
  let published = 0;
  const pending = [];
  const work = loadRegistryTagPreviews(
    ["one", "two", "three", "four", "five"],
    () =>
      new Promise((resolve) => {
        started += 1;
        pending.push(resolve);
      }),
    controller.signal,
    () => {
      published += 1;
    },
  );
  assert.equal(started, 4);
  controller.abort();
  for (const resolve of pending) resolve({ items: ["v1"], next: "" });
  await work;
  assert.equal(started, 4);
  assert.equal(published, 0);
});
