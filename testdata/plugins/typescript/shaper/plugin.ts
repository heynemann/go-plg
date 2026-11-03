// The TypeScript test plugin for testcontract.Shaper: every data type crosses it. Struct fields are named by
// their json tag (else their Go name), times are {seconds, nanos}, []byte is a Uint8Array, and nil slices, maps,
// byte slices and pointers are null.
type Time = { seconds: number; nanos: number };
type Line = { sku: string; qty: number; price: number };
type Order = {
  Base: { id: string };
  When: Time;
  Lines: Line[] | null;
  ByCode: Record<string, Line | null> | null;
  Raw: Uint8Array | null;
  Origin: { x: number; y: number } | null;
  Tags: string[] | null;
  Notes: Record<string, string[] | null> | null;
  Grid: (number[] | null)[] | null;
};
type Tree = { Name: string; Kids?: Tree[] };

const size = (v: unknown[] | Uint8Array | object | null): number =>
  v === null ? -1 : Array.isArray(v) || v instanceof Uint8Array ? v.length : Object.keys(v).length;

// grow builds the same tree as the Go plugin's recursive grow, with an explicit stack: JavaScript frames are
// limited to about 3000 (JS_STACK_LIMIT in guestsrc/typescript/glue.c), fewer than the 3332 tree levels the wire
// allows.
function grow(name: string, depth: number, width: number): Tree {
  const root: Tree = { Name: name };
  const pending: [Tree, number][] = [[root, depth]];
  for (let next = pending.pop(); next !== undefined; next = pending.pop()) {
    const [tree, left] = next;
    if (left === 0) {
      continue;
    }
    const kids: Tree[] = [];
    for (let i = 0; i < width; i++) {
      const kid: Tree = { Name: `${tree.Name}.${i}` };
      kids.push(kid);
      pending.push([kid, left - 1]);
    }
    tree.Kids = kids;
  }
  return root;
}

export function Create() {
  return {
    Echo: (o: Order) => o,
    EchoPtr: (p: Order | null) => p,
    Inspect(o: Order) {
      return {
        ID: o.Base.id,
        Zone: "UTC",
        Unix: o.When.seconds,
        Nanos: o.When.nanos,
        Lines: size(o.Lines),
        ByCode: size(o.ByCode),
        Raw: size(o.Raw),
        Tags: size(o.Tags),
        Notes: size(o.Notes),
        Grid: size(o.Grid),
        Origin: o.Origin !== null,
        NilEntry: o.ByCode !== null && Object.values(o.ByCode).some((line) => line === null),
      };
    },
    Bump(o: Order, by: number) {
      for (const line of o.Lines ?? []) {
        line.qty += by;
      }
      return o;
    },
    Grow: (depth: number, width: number) => grow("root", depth, width),
    Index(lines: Line[] | null) {
      if (lines === null) {
        return null;
      }
      const index: Record<string, Line[]> = {};
      for (const line of lines) {
        (index[line.sku] ??= []).push(line);
      }
      return index;
    },
    Reverse: (b: Uint8Array | null) => (b === null ? null : b.slice().reverse()),
    Later: (t: Time, seconds: number): Time => ({ seconds: t.seconds + seconds, nanos: t.nanos }),
    Sum: (base: number, more: number[] | null) => (more ?? []).reduce((sum, n) => sum + n, base),
  };
}
