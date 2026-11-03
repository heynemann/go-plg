// The reference TypeScript test plugin: it implements testcontract.Fooer like testdata/plugins/go/fooer, across
// several files (relative imports, with and without extensions, and a .js import of a .ts file).
import { greeting } from "./greeting";
import { fahrenheit } from "./lib/units.js";

class Fooer {
  private calls = 0;

  Foo(name: string): string {
    return greeting(name);
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
    return [c >= 100, fahrenheit(c)];
  }

  Scalars(
    b: boolean, i: number, i8: number, i16: number, i32: number, i64: number,
    u: number, u8: number, u16: number, u32: number, u64: number, f32: number, f64: number, s: string,
  ): [boolean, number, number, number, number, number, number, number, number, number, number, number, number, string] {
    return [!b, i + 1, i8 + 1, i16 + 1, i32 + 1, i64 + 1, u + 1, u8 + 1, u16 + 1, u32 + 1, u64 + 1, f32 * 2, f64 * 2,
      s.toUpperCase()];
  }

  Spin(n: number): number {
    let count = 0;
    while (count < n) {
      count++;
    }
    return count;
  }
}

export function Create(): Fooer {
  return new Fooer();
}
