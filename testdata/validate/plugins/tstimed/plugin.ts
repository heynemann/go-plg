// A rule doubling its number: its Create returns at once.
class Double {
  Apply(n: number) {
    return n * 2;
  }
}

export function Create() {
  return new Double();
}
