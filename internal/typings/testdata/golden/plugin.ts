// A plugin of every method of everything.Store, written against the golden declarations: it type-checks, so
// the declarations are valid TypeScript that a plugin can implement and that reads the values plg hands over.
import notes from "./notes.txt";

class Store implements everything.Store {
  Scalars(b: boolean, i: number, i8: number, i16: number, i32: number, i64: number, u: number, u8: number,
    u16: number, u32: number, u64: number, f32: number, f64: number, s: string): boolean {
    return b && i + i8 + i16 + i32 + i64 + u + u8 + u16 + u32 + u64 + f32 + f64 > s.length;
  }

  Bytes(raw: Uint8Array | null, blob: everything.Blob) {
    return raw ?? blob ?? new Uint8Array(0);
  }

  async Times(at: plg.Time, when: plg.Time | null, every: time.Duration) {
    console.log(at.seconds, when?.nanos, every);
    return { Prev: when, History: [at, new Date()], At: new Date(at.seconds * 1000) };
  }

  Collections(tags: string[] | null, matrix: (number[] | null)[] | null,
    byName: { [key: string]: everything.Item | null } | null, nested: { [key: string]: everything.Item[] | null } | null,
  ): everything.Tags {
    const first = byName?.["x"]?.["a-b"] ?? nested?.["y"]?.[0]?.["café"] ?? "";
    return [...(tags ?? []), first, String(matrix?.[0]?.[0] ?? 0), notes];
  }

  async Records(item: everything.Item, ptr: everything.Item | null, point: { X: number; Y: number }, empty: {}) {
    const tree: everything.Tree = [[], null];
    const copy = { ...item, seen: new Date(), parent: ptr, children: [ptr, null], end: String(point.X + point.Y) };
    return [copy, tree] as [plg.Out<everything.Item | null>, everything.Tree];
  }

  Nothing() {}

  Variadic(first: string, rest: number[] | null) {
    return first.length + (rest?.length ?? 0);
  }

  async Unnamed(p1: string, p2: number): Promise<everything.Tier> {
    const [found, ok] = await host.Lookup(p1, new Date());
    await host.Notify([{ ...found!, seen: { seconds: p2, nanos: 0 } }]);
    await host.Ping();
    return ok ? found?.name ?? "" : "";
  }

  Keywords(function_: string, end_: number, p3_: boolean, p3: everything.number_) {}

  Embedding(e: everything.Embedder): everything.Count {
    return e.Counter.N + e.Alias.N;
  }

  Validate(input: everything.Limits): plg.Issue[] {
    return input.maxItems > 1 ? [{ severity: "warning", code: "store.limit", field: "maxItems" }] : [];
  }

  async Close() {
    const response = await fetch("https://example.com", { method: "POST", body: "x", headers: { a: "b" } });
    console.error(response.status, await response.text(), new Headers(response.headers).get("a"));
  }
}

export function Create(): everything.Store {
  return new Store();
}
