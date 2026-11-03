// The TypeScript test plugin making HTTP requests with fetch, like testdata/plugins/go/fetcher. Foo takes a
// command:
//
//   GET <url>             one GET
//   POST <url> <n>        one POST with an n-byte body
//   REPEAT <n> <url>      n GETs in a row
//   HOST <url> <host>     one GET with the Host header overridden
//   FLOOD <n> <size> <url> n POSTs of one size-byte body, all started before awaiting any
//   LOOP -                loops forever
//   HOG -                 allocates until it runs out of memory
//   HANG -                awaits a promise nothing settles
//
// and returns "<status code>|<X-Reply header>|<body>" of the last response; FLOOD returns "<responses>|<the
// distinct sorted reasons of the failed requests, joined by ;>".
async function get(url: string, host?: string): Promise<string> {
  const headers: Record<string, string> = { "X-Test": "fetcher" };
  if (host !== undefined) {
    headers.Host = host;
  }
  return describe(await fetch(url, { headers }));
}

async function describe(resp: Response): Promise<string> {
  return `${resp.status}|${resp.headers.get("X-Reply") ?? ""}|${await resp.text()}`;
}

export function Create() {
  return {
    async Foo(cmd: string): Promise<string> {
      const args = cmd.split(/\s+/).filter((arg) => arg !== "");
      if (args.length < 2) {
        throw new Error("bad command");
      }
      switch (args[0]) {
        case "GET":
          return get(args[1]);
        case "HOST":
          return get(args[1], args[2]);
        case "POST":
          return describe(await fetch(args[1], {
            method: "POST",
            headers: { "Content-Type": "text/plain" },
            body: "x".repeat(Number(args[2])),
          }));
        case "REPEAT": {
          let out = "";
          for (let i = 0; i < Number(args[1]); i++) {
            out = await get(args[2]);
          }
          return out;
        }
        case "FLOOD": {
          const body = new Uint8Array(Number(args[2]));
          const sent: Promise<Response>[] = [];
          for (let i = 0; i < Number(args[1]); i++) {
            sent.push(fetch(args[3], { method: "POST", body }));
          }
          let ok = 0;
          const reasons = new Set<string>();
          for (const got of await Promise.allSettled(sent)) {
            if (got.status === "fulfilled") {
              ok++;
            } else {
              reasons.add(String(got.reason.message));
            }
          }
          return `${ok}|${[...reasons].sort().join(";")}`;
        }
        case "LOOP":
          for (;;) {
            // spins until the call's deadline stops it
          }
        case "HOG": {
          const kept: Uint8Array[] = [];
          for (;;) {
            kept.push(new Uint8Array(1 << 20));
          }
        }
        case "HANG":
          return new Promise<string>(() => {});
        default:
          throw new Error("unknown command " + args[0]);
      }
    },
    Count: () => 0,
    Fail: () => undefined,
    Boil: () => [false, 0],
    Scalars: (...args: unknown[]) => args,
    Spin: () => 0,
  };
}
