// The hostile-corpus TypeScript plugin for failures at call time: Fail(msg) misbehaves as msg says ("throw",
// "reject", "oom", "recurse", "loop", "never", "print") and Foo("huge") returns a result larger than the
// instance's memory. Its other methods behave like the reference fooer's, so each instance is healthy until
// provoked.
let calls = 0;
const hoard: ArrayBuffer[] = [];

function deeper(msg: string): never {
  throw new Error(msg);
}

function recurse(n: number): number {
  return recurse(n + 1) + 1;
}

export function Create() {
  return {
    Foo(name: string): string {
      if (name === "huge") {
        return "x".repeat(64 << 20);
      }
      return "hello, " + name;
    },
    Count(): number {
      return ++calls;
    },
    async Fail(msg: string): Promise<void> {
      switch (msg) {
        case "throw":
          deeper("hostile throw");
          break;
        case "reject":
          await Promise.resolve();
          throw new TypeError("hostile rejection");
        case "oom":
          // 1 MiB at a time, in one allocation: filling an array runs a guest loop iteration per element, and every
          // iteration exits to the host to check the call's context, seconds per hoard on a loaded machine.
          for (;;) {
            hoard.push(new ArrayBuffer(1 << 20));
          }
        case "recurse":
          recurse(0);
          break;
        case "loop":
          for (;;) {}
        case "never":
          await new Promise(() => {});
          break;
        case "print":
          console.log("hostile says hello on stdout", { n: 1 });
          console.error("and on stderr");
          deeper("after printing");
      }
      throw new Error(msg);
    },
    Boil: (c: number) => [c >= 100, (c * 9) / 5 + 32],
    Scalars: (...args: unknown[]) => args,
    Spin: (n: number) => n,
  };
}
