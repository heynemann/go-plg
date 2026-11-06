// The TypeScript test plugin that validates itself (plg.WithValidation), like testdata/plugins/go/validator: it
// implements testcontract.Fooer with a Create awaiting host.Value, and its Validate does what the input's Mode says:
//
//   valid      no issue (undefined)
//   warn       a warning and an info issue
//   async      the same, as an async Validate's Promise
//   error      an error issue and a warning
//   unknown    an issue of a severity plg does not know
//   fail       throws
//   reject     returns a rejected Promise
//   panic      allocates until the instance's memory runs out
//   loop       never returns
//   pending    returns a Promise that never settles, running a job after another
//   stuck      returns a Promise that never settles, nothing left to run
//   malformed  returns an issue whose severity is not a string
//   host       an info issue "create=<host.Value() in Create> validate=<host.Value() in Validate>"
//   http       an info issue "<status code> <body>" of a GET of the input's URL
//
// Validate also marks its instance validated: Foo("validated") reports whether this instance was.
declare const host: {
  Value(): Promise<string>;
};

interface Check {
  Mode: string;
  URL: string;
}

interface Issue {
  severity: string;
  code: string;
  message?: string;
  field?: string;
}

const hoard: ArrayBuffer[] = [];

function warnings(): Issue[] {
  return [
    { severity: "warning", code: "validator.slow", message: "slow", field: "speed" },
    { severity: "info", code: "validator.note", message: "noted" },
  ];
}

// pending awaits forever: each turn queues the next, so a job is always left to run.
async function pending(): Promise<Issue[]> {
  for (;;) {
    await null;
  }
}

class Validator {
  private calls = 0;
  private validated = false;

  constructor(private readonly origin: string) {}

  Validate(check: Check): Issue[] | Promise<Issue[]> | undefined {
    this.validated = true;
    switch (check.Mode) {
      case "valid":
        return undefined;
      case "warn":
        return warnings();
      case "async":
        return (async () => {
          await null;
          return warnings();
        })();
      case "error":
        return [
          { severity: "error", code: "validator.broken", message: "broken", field: "max" },
          { severity: "warning", code: "validator.slow", message: "slow" },
        ];
      case "unknown":
        return [{ severity: "fatal", code: "validator.fatal", message: "fatal" }];
      case "fail":
        throw new Error("validation broke");
      case "reject":
        return Promise.reject(new Error("validation broke"));
      case "panic":
        // 1 MiB at a time, in one allocation (see testdata/hostile/typescript/hostile).
        for (;;) {
          hoard.push(new ArrayBuffer(1 << 20));
        }
      case "loop":
        for (;;) {}
      case "pending":
        return pending();
      case "stuck":
        return new Promise(() => {});
      case "malformed":
        return [{ severity: 3 } as unknown as Issue];
      case "host":
        return (async () => [
          { severity: "info", code: "validator.host", message: `create=${this.origin} validate=${await host.Value()}` },
        ])();
      case "http":
        return (async () => {
          const resp = await fetch(check.URL);
          return [{ severity: "info", code: "validator.http", message: `${resp.status} ${await resp.text()}` }];
        })();
    }
    throw new Error(`unknown mode ${check.Mode}`);
  }

  Foo(name: string): string {
    switch (name) {
      case "origin":
        return this.origin;
      case "validated":
        return String(this.validated);
    }
    return "hello, " + name.trim(); // testcontract.Greeting
  }

  Count(): number {
    this.calls++;
    return this.calls;
  }

  Fail(msg: string): void {
    if (msg !== "") {
      throw new Error(msg);
    }
  }

  Boil(c: number): [boolean, number] {
    return [c >= 100, (c * 9) / 5 + 32];
  }

  Scalars(
    b: boolean, i: number, i8: number, i16: number, i32: number, i64: number,
    u: number, u8: number, u16: number, u32: number, u64: number, f32: number, f64: number, s: string,
  ): [boolean, number, number, number, number, number, number, number, number, number, number, number, number, string] {
    return [b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s];
  }

  Spin(n: number): number {
    return n;
  }
}

// Create keeps what host.Value returns while Create runs.
export async function Create(): Promise<Validator> {
  return new Validator(await host.Value());
}
