// The TypeScript counterpart of testdata/plugins/go/fooermain: a second Fooer, plain object with methods, whose
// Scalars returns its arguments unchanged.
export function Create() {
  return {
    Foo: (name: string) => "main says hello, " + name.trim(),
    Count: () => 0,
    Fail: (_msg: string) => undefined,
    Boil: (_c: number) => [false, 0],
    Scalars: (...args: unknown[]) => args,
    Spin: (n: number) => n,
  };
}
