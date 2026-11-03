// A broken TypeScript plugin: Create rejects (it is async) with an error.
export async function Create() {
  await Promise.resolve();
  throw new Error("cannot start: no configuration");
}
