// A test plugin reading the secrets its plugin.yaml declares with secrets.get, like testdata/plugins/go/secrets:
// Foo(name) returns the secret name, Foo("@create") what Create read of the secret create, and Fail(cmd) leaks a
// secret (print, error, panic: writes it to stderr, then runs out of memory, fetch).
const hoard: ArrayBuffer[] = [];

export function Create() {
  let created: string;
  try {
    created = secrets.get("create");
  } catch (e) {
    created = "error: " + (e as Error).message;
  }
  return {
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
