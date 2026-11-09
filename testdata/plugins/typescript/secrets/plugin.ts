// A test plugin reading the secrets its plugin.yaml declares with secrets.get, like testdata/plugins/go/secrets:
// Create fails with "refused <value>" when the secret create starts with "fail", Foo(name) returns the secret name,
// Foo("@create") what Create read of the secret create, Validate(check) returns an info issue holding the value of
// the secret check.Mode names, and Fail(cmd) leaks a secret (print, error, panic: writes it to stderr, then runs
// out of memory, fetch).
const hoard: ArrayBuffer[] = [];

export function Create() {
  let created: string;
  try {
    created = secrets.get("create");
  } catch (e) {
    created = "error: " + (e as Error).message;
  }
  if (created.startsWith("fail")) {
    throw new Error("refused " + created);
  }
  return {
    Validate(check: { Mode: string }) {
      const value = secrets.get(check.Mode);
      return [{ severity: "info", code: "secrets." + value, message: "rejected " + value, field: value }];
    },
    Foo(name: string): string {
      return name === "@create" ? created : secrets.get(name);
    },
    Count: () => 0,
    async Fail(cmd: string): Promise<void> {
      const [action, name, url] = cmd.split(" ");
      const value = secrets.get(name);
      switch (action) {
        case "print":
          console.log("token " + value);
          console.error("stderr token " + value);
          return;
        case "error":
          throw new Error("rejected " + value);
        case "panic":
          console.error("dying with " + value);
          for (;;) {
            hoard.push(new ArrayBuffer(1 << 20));
          }
        case "fetch":
          await fetch(url + "/" + value);
          return;
      }
      throw new Error("unknown command " + action);
    },
    Boil: (c: number) => [c >= 100, (c * 9) / 5 + 32],
    Scalars: (...args: unknown[]) => args,
    Spin: (n: number) => n,
  };
}
